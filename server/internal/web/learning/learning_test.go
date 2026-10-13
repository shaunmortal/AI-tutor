package learning

// Handler tests against the live database in DATABASE_URL (or server/.env); skipped when it
// is unset. Handlers commit, so the throwaway users are real auth.users rows, deleted at the
// end (cascades take their rows; groups survive their creator, so those go first by hand).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/shaunmortal/AI-tutor/server/internal/app"
	"github.com/shaunmortal/AI-tutor/server/internal/auth"
	"github.com/shaunmortal/AI-tutor/server/internal/db"
	"github.com/shaunmortal/AI-tutor/server/internal/httpx"
	"github.com/shaunmortal/AI-tutor/server/internal/store"
)

type env struct {
	t        *testing.T
	mux      *http.ServeMux
	database *db.DB
	u1, u2   string
	// From the seeded default curriculum.
	curriculumID, subjectID string
}

func databaseURL() string {
	if url := os.Getenv("DATABASE_URL"); url != "" {
		return url
	}
	raw, _ := os.ReadFile("../../../.env")
	for _, line := range strings.Split(string(raw), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "DATABASE_URL="); ok {
			return strings.Trim(v, `"'`)
		}
	}
	return ""
}

func setup(t *testing.T) *env {
	url := databaseURL()
	if url == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	database, err := db.Open(ctx, url, false)
	must(t, err)
	e := &env{t: t, mux: http.NewServeMux(), database: database}

	var users []string
	newUser := func() string {
		var id string
		must(t, database.Admin.QueryRow(ctx,
			`with id as (select gen_random_uuid() as id)
			 insert into auth.users (id, email) select id, id || '@adigam.test' from id returning id::text`).Scan(&id))
		users = append(users, id)
		return id
	}
	t.Cleanup(func() {
		defer database.Close()
		_, err := database.Admin.Exec(ctx, `delete from study_groups where created_by = any($1::uuid[])`, users)
		check(t, err == nil, "delete groups: %v", err)
		_, err = database.Admin.Exec(ctx, `delete from auth.users where id = any($1::uuid[])`, users)
		check(t, err == nil, "delete users: %v", err)
		var left int
		must(t, database.Admin.QueryRow(ctx,
			`select (select count(*) from auth.users where id = any($1::uuid[]))
			      + (select count(*) from profiles where id = any($1::uuid[]))
			      + (select count(*) from learning_goals where user_id = any($1::uuid[]))
			      + (select count(*) from study_plans where user_id = any($1::uuid[]))
			      + (select count(*) from study_sessions where user_id = any($1::uuid[]))
			      + (select count(*) from group_members where user_id = any($1::uuid[]))
			      + (select count(*) from study_groups where created_by = any($1::uuid[]))`, users).Scan(&left))
		check(t, left == 0, "%d rows left behind", left)
	})
	e.u1, e.u2 = newUser(), newUser()

	must(t, database.Admin.QueryRow(ctx,
		`select c.id, s.id from curricula c join topics s on s.curriculum_id = c.id and s.parent_id is null
		 where c.slug = $1 and exists (select 1 from topics l where l.parent_id = s.id)
		 order by s.sort_order limit 1`, store.DefaultCurriculumSlug).Scan(&e.curriculumID, &e.subjectID))

	// The real routes, minus auth (the user comes from a test header) and rate limits.
	for _, r := range routes(&app.Deps{DB: database}) {
		h := httpx.Wrap(r.h)
		e.mux.Handle(r.pattern, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			h(w, req.WithContext(auth.WithUserID(req.Context(), req.Header.Get("X-Test-User"))))
		}))
	}
	return e
}

type reply struct {
	status int
	Data   map[string]any
	Error  *struct{ Code, Message string }
}

func (e *env) call(user, method, path, body string) reply {
	e.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("X-Test-User", user)
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	out := reply{status: rec.Code}
	must(e.t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out
}

// expect checks status and, for failures, the error code and (when given) message.
func (e *env) expect(r reply, status int, code, message string) {
	e.t.Helper()
	check(e.t, r.status == status, "status %d, want %d (%+v)", r.status, status, r.Error)
	if code != "" {
		check(e.t, r.Error != nil && r.Error.Code == code, "error %+v, want %s", r.Error, code)
	}
	if message != "" && r.Error != nil {
		check(e.t, r.Error.Message == message, "message %q, want %q", r.Error.Message, message)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func check(t *testing.T, ok bool, format string, args ...any) {
	t.Helper()
	if !ok {
		t.Errorf(format, args...)
	}
}

func inDays(n int) string { return time.Now().UTC().AddDate(0, 0, n).Format("2006-01-02") }

func (e *env) goalBody(examDate string) string {
	b, _ := json.Marshal(map[string]any{"title": "  Finals ", "curriculumId": e.curriculumID,
		"examDate": examDate, "dailyMinutes": 60, "subjectIds": []string{e.subjectID, e.subjectID}, "extra": 1})
	return string(b)
}

func TestLearning(t *testing.T) {
	e := setup(t)
	u1, u2 := e.u1, e.u2

	t.Run("goals", func(t *testing.T) {
		r := e.call(u1, "GET", "/goals", "")
		e.expect(r, 200, "", "")
		check(t, r.Data["goal"] == nil, "goal before setting one: %v", r.Data["goal"])

		e.expect(e.call(u1, "GET", "/goals/subjects", ""), 400, "INVALID_INPUT", "curriculumId is required and must be a uuid.")
		r = e.call(u1, "GET", "/goals/subjects?curriculumId="+e.curriculumID, "")
		e.expect(r, 200, "", "")
		check(t, len(r.Data["subjects"].([]any)) > 0, "no subjects")

		e.expect(e.call(u1, "POST", "/goals", ""), 400, "INVALID_INPUT", "Invalid input: expected object, received undefined")
		e.expect(e.call(u1, "POST", "/goals", `{}`), 400, "INVALID_INPUT", "title: Invalid input: expected string, received undefined")
		e.expect(e.call(u1, "POST", "/goals", `{"title":"x","curriculumId":"`+e.curriculumID+`","examDate":"2030-01-01","dailyMinutes":10.5,"subjectIds":[]}`),
			400, "INVALID_INPUT", "dailyMinutes: Invalid input: expected int, received number")
		e.expect(e.call(u1, "POST", "/goals", `{"title":"x","curriculumId":"`+e.curriculumID+`","examDate":"2030-01-01","dailyMinutes":10,"subjectIds":["nope"]}`),
			400, "INVALID_INPUT", "subjectIds.0: Invalid UUID")
		e.expect(e.call(u1, "POST", "/goals", e.goalBody("2020-01-01")), 400, "INVALID_INPUT",
			"The exam date must be in the future — there is nothing to plan otherwise.")

		r = e.call(u1, "POST", "/goals", e.goalBody(inDays(60)))
		e.expect(r, 201, "", "")
		goal := r.Data["goal"].(map[string]any)
		check(t, goal["title"] == "Finals" && goal["daysRemaining"] == 60.0 && goal["totalMinutesAvailable"] == 3600.0,
			"created goal %v", goal)
		check(t, len(goal["subjectIds"].([]any)) == 1 && goal["curriculumId"] == e.curriculumID, "scope %v", goal)

		r = e.call(u1, "GET", "/goals", "")
		check(t, r.Data["goal"].(map[string]any)["id"] == goal["id"], "active goal %v", r.Data["goal"])
	})

	t.Run("curricula", func(t *testing.T) {
		r := e.call(u1, "GET", "/curricula/default", "")
		e.expect(r, 200, "", "")
		check(t, r.Data["status"] == "cached" && r.Data["curriculum"].(map[string]any)["id"] == e.curriculumID, "default %v", r.Data)
		check(t, len(r.Data["subjects"].([]any)) > 0, "default subjects")

		e.expect(e.call(u1, "POST", "/curricula/resolve", `{"goalText":" a "}`), 400, "INVALID_INPUT",
			"goalText: Too small: expected string to have >=2 characters")
		e.expect(e.call(u1, "POST", "/curricula/resolve", `{"goalText":"class 10"}`), 503, "AI_UNAVAILABLE", "")
	})

	t.Run("learner", func(t *testing.T) {
		r := e.call(u1, "GET", "/learner/summary", "")
		e.expect(r, 200, "", "")
		check(t, r.Data["hasActivity"] == false, "summary %v", r.Data)

		r = e.call(u1, "GET", "/learner/activity", "")
		e.expect(r, 200, "", "")
		check(t, len(r.Data["calendar"].(map[string]any)["days"].([]any)) == 365, "calendar")
		check(t, r.Data["latestBadge"] == nil && len(r.Data["upcoming"].([]any)) > 0, "badges %v", r.Data)

		r = e.call(u1, "GET", "/learner/subjects", "")
		e.expect(r, 200, "", "")
		subjects := r.Data["subjects"].([]any)
		check(t, len(subjects) == 1 && subjects[0].(map[string]any)["id"] == e.subjectID, "subjects %v", subjects)

		r = e.call(u1, "GET", "/learner/profile", "")
		e.expect(r, 200, "", "")
		p := r.Data["profile"].(map[string]any)
		check(t, p["email"] == u1+"@adigam.test" && p["timezone"] == "Asia/Kolkata", "profile %v", p)

		e.expect(e.call(u1, "PATCH", "/learner/profile", `{"other":1}`), 400, "INVALID_INPUT", "send a displayName or a timezone")
		e.expect(e.call(u1, "PATCH", "/learner/profile", `{"timezone":"Mars/Phobos"}`), 400, "INVALID_INPUT",
			"timezone: is not a timezone this server recognises")
		r = e.call(u1, "PATCH", "/learner/profile", `{"displayName":" Ada ","timezone":"Europe/London"}`)
		e.expect(r, 200, "", "")
		p = r.Data["profile"].(map[string]any)
		check(t, p["displayName"] == "Ada" && p["timezone"] == "Europe/London", "patched %v", p)
	})

	t.Run("plan", func(t *testing.T) {
		r := e.call(u1, "GET", "/study-plan/current", "")
		e.expect(r, 200, "", "")
		check(t, r.Data["plan"] == nil && r.Data["adjustment"] == nil, "plan before generating %v", r.Data)

		e.expect(e.call(u2, "POST", "/study-plan/generate", `{}`), 400, "NO_ACTIVE_GOAL", "")

		r = e.call(u1, "POST", "/study-plan/generate", `{}`)
		e.expect(r, 201, "", "")
		plan := r.Data["plan"].(map[string]any)
		check(t, plan["version"] == 1.0 && plan["reason"] == "requested" && len(plan["days"].([]any)) > 0, "plan %v", plan)

		r = e.call(u1, "GET", "/study-plan/current", "")
		e.expect(r, 200, "", "")
		check(t, r.Data["plan"].(map[string]any)["id"] == plan["id"] && r.Data["adjustment"] == nil, "current %v", r.Data)
	})

	t.Run("readiness and revision", func(t *testing.T) {
		r := e.call(u2, "GET", "/readiness", "")
		e.expect(r, 200, "", "")
		check(t, r.Data["goal"] == nil && r.Data["readiness"] == nil, "no-goal readiness %v", r.Data)

		r = e.call(u1, "GET", "/readiness", "")
		e.expect(r, 200, "", "")
		check(t, r.Data["readiness"].(map[string]any)["band"] != nil && r.Data["thresholds"].(map[string]any)["ready"] == 75.0,
			"readiness %v", r.Data)

		r = e.call(u1, "GET", "/revision/due", "")
		e.expect(r, 200, "", "")
		check(t, len(r.Data["due"].([]any)) == 0 && r.Data["today"] != "", "due %v", r.Data)
	})

	t.Run("groups", func(t *testing.T) {
		r := e.call(u1, "GET", "/groups", "")
		e.expect(r, 200, "", "")
		check(t, len(r.Data["groups"].([]any)) == 0, "groups before creating")

		e.expect(e.call(u1, "POST", "/groups", `{"name":" a "}`), 400, "INVALID_INPUT",
			"name: Too small: expected string to have >=2 characters")
		r = e.call(u1, "POST", "/groups", `{"name":"Study buddies"}`)
		e.expect(r, 201, "", "")
		group := r.Data["group"].(map[string]any)
		check(t, group["role"] == "owner" && group["memberCount"] == 1.0, "created %v", group)
		id, code := group["id"].(string), group["inviteCode"].(string)

		e.expect(e.call(u2, "POST", "/groups/join", `{"inviteCode":"ab"}`), 400, "INVALID_INPUT",
			"inviteCode: Too small: expected string to have >=4 characters")
		e.expect(e.call(u2, "POST", "/groups/join", `{"inviteCode":"abcde"}`), 400, "INVALID_CODE", "")
		e.expect(e.call(u2, "GET", "/groups/"+id, ""), 404, "GROUP_NOT_FOUND", "")

		r = e.call(u2, "POST", "/groups/join", `{"inviteCode":" `+strings.ToLower(code[:3])+" "+code[3:]+`"}`)
		e.expect(r, 200, "", "")
		check(t, r.Data["group"].(map[string]any)["memberCount"] == 2.0, "joined %v", r.Data)

		r = e.call(u2, "GET", "/groups/"+id, "")
		e.expect(r, 200, "", "")
		detail := r.Data["group"].(map[string]any)
		check(t, detail["role"] == "member" && len(detail["comparison"].(map[string]any)["members"].([]any)) == 2, "detail %v", detail)

		e.expect(e.call(u2, "GET", "/groups/not-a-uuid", ""), 404, "GROUP_NOT_FOUND", "")
		e.expect(e.call(u2, "POST", "/groups/"+id+"/leave", `{}`), 200, "", "")
		e.expect(e.call(u2, "POST", "/groups/"+id+"/leave", `{}`), 404, "GROUP_NOT_FOUND", "")
	})
}
