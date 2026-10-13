package practice

// Handler tests against the live database in DATABASE_URL (or server/.env). Handlers commit,
// so every throwaway auth.users row is deleted at the end (everything else cascades from it)
// and the test checks nothing is left. The seeded curriculum and questions are only read.
// Token verification is bypassed by putting the user id in the request context.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/shaunmortal/AI-tutor/server/internal/app"
	"github.com/shaunmortal/AI-tutor/server/internal/auth"
	"github.com/shaunmortal/AI-tutor/server/internal/db"
	"github.com/shaunmortal/AI-tutor/server/internal/httpx"
	"github.com/shaunmortal/AI-tutor/server/internal/store"
)

func envVar(key string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	raw, _ := os.ReadFile("../../../.env")
	for _, line := range strings.Split(string(raw), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), key+"="); ok {
			return strings.Trim(v, `"'`)
		}
	}
	return ""
}

type env struct {
	t   *testing.T
	ctx context.Context
	d   *app.Deps
	// A seeded subject of the default curriculum whose leaf topics have verified questions.
	subjectID, curriculumID string
}

func setup(t *testing.T) *env {
	t.Helper()
	url := envVar("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	must(t, err)
	t.Cleanup(pool.Close)
	redisURL := envVar("REDIS_URL")
	if redisURL == "" {
		redisURL = "redis://localhost:6379/0"
	}
	opts, err := redis.ParseURL(redisURL)
	must(t, err)
	rdb := redis.NewClient(opts) // rate limits fail open when this is unreachable
	t.Cleanup(func() { _ = rdb.Close() })

	e := &env{t: t, ctx: ctx, d: &app.Deps{DB: &db.DB{Admin: pool}, Redis: rdb, Mux: http.NewServeMux()}}
	for _, rt := range routes(e.d) {
		var h http.Handler = httpx.Wrap(rt.h)
		for i := len(rt.mw) - 1; i >= 0; i-- {
			h = rt.mw[i](h)
		}
		method, path, _ := strings.Cut(rt.pattern, " ")
		inner := h
		e.d.Mux.Handle(method+" /api/v1"+path, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			inner.ServeHTTP(w, r.WithContext(auth.WithUserID(r.Context(), r.Header.Get("X-Test-User"))))
		}))
	}
	must(t, pool.QueryRow(ctx,
		`select c.id, s.id from curricula c join topics s on s.curriculum_id = c.id and s.parent_id is null
		 where c.slug = $1 and exists (select 1 from topics l join questions q on q.topic_id = l.id and q.is_verified
		   and q.question_type = 'mcq' where l.parent_id = s.id)
		 order by s.sort_order limit 1`, store.DefaultCurriculumSlug).Scan(&e.curriculumID, &e.subjectID))
	return e
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// newUser creates a throwaway learner and deletes it (and, by cascade, everything it owns)
// when the test ends, then checks nothing is left behind.
func (e *env) newUser(withGoal bool) string {
	t := e.t
	t.Helper()
	var id string
	must(t, e.d.DB.Admin.QueryRow(e.ctx,
		`with id as (select gen_random_uuid() as id)
		 insert into auth.users (id, email) select id, id || '@adigam.test' from id returning id::text`).Scan(&id))
	t.Cleanup(func() {
		ctx := context.Background()
		if _, err := e.d.DB.Admin.Exec(ctx, `delete from auth.users where id = $1`, id); err != nil {
			t.Errorf("cleanup: %v", err)
		}
		for _, table := range []string{"question_attempts", "concept_mastery", "revision_schedule", "assessments",
			"mock_tests", "learning_goals", "learner_profiles", "mistake_patterns"} {
			var n int
			_ = e.d.DB.Admin.QueryRow(ctx, `select count(*) from `+table+` where user_id = $1`, id).Scan(&n)
			if n != 0 {
				t.Errorf("cleanup left %d rows in %s", n, table)
			}
		}
		var n int
		_ = e.d.DB.Admin.QueryRow(ctx, `select count(*) from auth.users where id = $1`, id).Scan(&n)
		if n != 0 {
			t.Errorf("cleanup left the auth.users row")
		}
		if keys, err := e.d.Redis.Keys(ctx, "rl:*:"+id).Result(); err == nil && len(keys) > 0 {
			e.d.Redis.Del(ctx, keys...)
		}
	})
	if withGoal {
		_, err := store.NewGoals(e.d.DB.Admin).ReplaceActive(e.ctx, id, store.NewGoal{Title: "Exam", ExamDate: "2099-01-01",
			DailyMinutes: 60, SubjectIDs: []string{e.subjectID}, CurriculumID: e.curriculumID})
		must(t, err)
	}
	return id
}

type envelope struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data"`
	Error   *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	status int
	raw    string
}

func (e *env) call(method, path, user string, body any) envelope {
	e.t.Helper()
	var reader *bytes.Reader
	switch b := body.(type) {
	case nil:
		reader = bytes.NewReader(nil)
	case string:
		reader = bytes.NewReader([]byte(b))
	default:
		raw, _ := json.Marshal(b)
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, "/api/v1"+path, reader)
	req.Header.Set("X-Test-User", user)
	rec := httptest.NewRecorder()
	e.d.Mux.ServeHTTP(rec, req)
	var env envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		e.t.Fatalf("%s %s: not an envelope: %s", method, path, rec.Body.String())
	}
	env.status, env.raw = rec.Code, rec.Body.String()
	return env
}

func (env envelope) expect(t *testing.T, status int, code string) envelope {
	t.Helper()
	if env.status != status || (code != "" && (env.Error == nil || env.Error.Code != code)) {
		t.Fatalf("want %d %s, got %d %s", status, code, env.status, env.raw)
	}
	return env
}

func (env envelope) into(t *testing.T, v any) {
	t.Helper()
	must(t, json.Unmarshal(env.Data, v))
}

// noAnswerKey fails if a response that must not carry answers does.
func noAnswerKey(t *testing.T, env envelope) {
	t.Helper()
	for _, leak := range []string{"correct_answer", "correctAnswer", "explanation", "correctOptionIndexes"} {
		if strings.Contains(env.raw, leak) {
			t.Fatalf("response leaks %s: %s", leak, env.raw)
		}
	}
}

// correctOptions reads a question's answer key straight from the database.
func (e *env) correctOptions(questionID string) []int {
	var ans []int
	must(e.t, e.d.DB.Admin.QueryRow(e.ctx, `select correct_answer from questions where id = $1`, questionID).Scan(&ans))
	return ans
}

// answerFor is a right or wrong answer to any question, read from its key. A wrong short answer
// is blank, which is marked without a model.
func (e *env) answerFor(questionID string, right bool) map[string]any {
	var qtype string
	var key json.RawMessage
	var options int
	must(e.t, e.d.DB.Admin.QueryRow(e.ctx, `select question_type, correct_answer, coalesce(cardinality(options), 0)
		from questions where id = $1`, questionID).Scan(&qtype, &key, &options))
	out := map[string]any{"questionId": questionID}
	switch qtype {
	case "numeric":
		var n struct{ Value, Tol float64 }
		must(e.t, json.Unmarshal(key, &n))
		if !right {
			n.Value += n.Tol*10 + 1
		}
		out["value"] = n.Value
	case "short":
		var k struct{ Answers []string }
		must(e.t, json.Unmarshal(key, &k))
		out["text"] = ""
		if right {
			out["text"] = k.Answers[0]
		}
	default:
		var idx []int
		must(e.t, json.Unmarshal(key, &idx))
		if !right {
			idx = []int{(idx[0] + 1) % options}
		}
		out["selectedOptions"] = idx
	}
	return out
}

func TestSubmitAttempt(t *testing.T) {
	e := setup(t)
	user := e.newUser(true)
	var qid, topicID string
	var optionCount int
	must(t, e.d.DB.Admin.QueryRow(e.ctx,
		`select q.id, q.topic_id, cardinality(q.options) from questions q join topics l on l.id = q.topic_id
		 where l.parent_id = $1 and q.is_verified and q.question_type = 'mcq' order by q.id limit 1`, e.subjectID).
		Scan(&qid, &topicID, &optionCount))
	right := e.correctOptions(qid)
	wrong := []int{(right[0] + 1) % optionCount}

	var first attemptResult
	e.call("POST", "/attempts", user, map[string]any{"questionId": qid, "selectedOptions": right, "timeTakenSeconds": 30}).
		expect(t, 201, "").into(t, &first)
	if !first.IsCorrect || first.Mastery.Before != nil || first.Mastery.Change != nil || first.Mastery.After <= 0 ||
		first.Mastery.AttemptsOnTopic != 1 || first.Mastery.TopicID != topicID {
		t.Fatalf("first attempt: %+v", first)
	}
	var key []int
	must(t, json.Unmarshal(first.CorrectAnswer, &key))
	if fmt.Sprint(key) != fmt.Sprint(right) {
		t.Fatalf("correctAnswer %v, want %v", key, right)
	}

	var second attemptResult
	e.call("POST", "/attempts", user, map[string]any{"questionId": qid, "selectedOptions": wrong, "source": "revision"}).
		expect(t, 201, "").into(t, &second)
	if second.IsCorrect || second.Mastery.Before == nil || second.Mastery.Change == nil || *second.Mastery.Change >= 0 ||
		second.Mastery.AttemptsOnTopic != 2 {
		t.Fatalf("second attempt: %+v", second)
	}

	var mastery float64
	var total int
	must(t, e.d.DB.Admin.QueryRow(e.ctx, `select mastery_score, total_attempts from concept_mastery
		where user_id = $1 and topic_id = $2`, user, topicID).Scan(&mastery, &total))
	if total != 2 || mastery != second.Mastery.After {
		t.Fatalf("concept_mastery %v/%d, response %v", mastery, total, second.Mastery.After)
	}
	var due string
	var reviews int
	must(t, e.d.DB.Admin.QueryRow(e.ctx, `select due_on::text, review_count from revision_schedule
		where user_id = $1 and topic_id = $2`, user, topicID).Scan(&due, &reviews))
	if due == "" {
		t.Fatal("no revision schedule")
	}
	var profileTotal int
	must(t, e.d.DB.Admin.QueryRow(e.ctx, `select total_attempts from learner_profiles where user_id = $1`, user).Scan(&profileTotal))
	if profileTotal != 2 {
		t.Fatalf("learner profile total %d", profileTotal)
	}

	// Validation, in schema order, with zod's wording.
	for _, c := range []struct {
		body any
		msg  string
	}{
		{map[string]any{}, "questionId: Invalid input: expected string, received undefined"},
		{map[string]any{"questionId": "nope", "value": 1}, "questionId: Invalid UUID"},
		{map[string]any{"questionId": qid, "selectedOptions": []any{1.5}}, "selectedOptions.0: Invalid input: expected int, received number"},
		{map[string]any{"questionId": qid, "selectedOptions": []int{51}}, "selectedOptions.0: Too big: expected number to be <=50"},
		{map[string]any{"questionId": qid, "value": 1, "timeTakenSeconds": 4000}, "timeTakenSeconds: Too big: expected number to be <=3600"},
		{map[string]any{"questionId": qid, "value": 1, "source": "x"}, `source: Invalid option: expected one of "practice"|"assessment"|"topic_test"|"mock_test"|"revision"`},
		{map[string]any{"questionId": qid}, errOneAnswer},
		{map[string]any{"questionId": qid, "value": 1, "selectedOptions": []int{0}}, errOneAnswer},
		{map[string]any{"questionId": qid, "value": 1, "text": "x"}, errOneAnswer},
		{map[string]any{"questionId": qid, "text": 5}, "text: Invalid input: expected string, received number"},
		{"", "Invalid input: expected object, received undefined"},
	} {
		res := e.call("POST", "/attempts", user, c.body).expect(t, 400, "INVALID_INPUT")
		if res.Error.Message != c.msg {
			t.Errorf("%v: got %q, want %q", c.body, res.Error.Message, c.msg)
		}
	}
	e.call("POST", "/attempts", user, map[string]any{"questionId": "00000000-0000-4000-8000-000000000000", "value": 1}).
		expect(t, 404, "QUESTION_NOT_FOUND")
	// A numeric answer to a choice question is rejected before anything is written.
	e.call("POST", "/attempts", user, map[string]any{"questionId": qid, "value": 1}).expect(t, 400, "INVALID_ANSWER")
}

func TestRecommendationsNext(t *testing.T) {
	e := setup(t)
	user := e.newUser(true)

	res := e.call("GET", "/recommendations/next?minutes=20", user, nil).expect(t, 200, "")
	noAnswerKey(t, res)
	var out struct {
		Recommendation *struct {
			Action        string `json:"action"`
			QuestionCount int    `json:"questionCount"`
			Topic         struct{ ID string }
		} `json:"recommendation"`
		Questions []struct{ ID, TopicID string } `json:"questions"`
	}
	res.into(t, &out)
	if out.Recommendation == nil || len(out.Questions) == 0 || out.Recommendation.QuestionCount != len(out.Questions) {
		t.Fatalf("recommendation: %s", res.raw)
	}
	if !strings.Contains(res.raw, `"bankNote"`) {
		t.Fatalf("bankNote missing: %s", res.raw)
	}

	// A subject outside the goal is an explained empty state, not an error.
	res = e.call("GET", "/recommendations/next?subject=00000000-0000-4000-8000-000000000000", user, nil).expect(t, 200, "")
	if !strings.Contains(res.raw, `"recommendation":null`) || !strings.Contains(res.raw, "That subject is not part of your current goal.") {
		t.Fatalf("empty state: %s", res.raw)
	}

	// A topic picked on the course map is practised exactly.
	var first struct {
		Questions []struct {
			TopicID string `json:"topic_id"`
		} `json:"questions"`
	}
	e.call("GET", "/recommendations/next?minutes=20", user, nil).expect(t, 200, "").into(t, &first)
	picked := first.Questions[0].TopicID
	res = e.call("GET", "/recommendations/next?topic="+picked, user, nil).expect(t, 200, "")
	var only struct {
		Questions []struct {
			TopicID string `json:"topic_id"`
		} `json:"questions"`
	}
	res.into(t, &only)
	for _, q := range only.Questions {
		if q.TopicID != picked {
			t.Fatalf("topic=%s returned a question on %s", picked, q.TopicID)
		}
	}
	res = e.call("GET", "/recommendations/next?topic=00000000-0000-4000-8000-000000000000", user, nil).expect(t, 200, "")
	if !strings.Contains(res.raw, "That topic is not part of your current goal.") {
		t.Fatalf("topic empty state: %s", res.raw)
	}

	for _, q := range []string{"minutes=0", "minutes=1.5", "minutes=601", "minutes=abc", "minutes=", "subject=nope", "topic=nope"} {
		res := e.call("GET", "/recommendations/next?"+q, user, nil).expect(t, 400, "INVALID_INPUT")
		if res.Error.Message != "minutes must be a whole number between 1 and 600, and subject and topic must be uuids." {
			t.Errorf("%s: %s", q, res.Error.Message)
		}
	}
}

func TestDiagnostic(t *testing.T) {
	e := setup(t)
	e.call("GET", "/assessments/diagnostic", e.newUser(false), nil).expect(t, 400, "NO_ACTIVE_GOAL")

	user := e.newUser(true)
	res := e.call("GET", "/assessments/diagnostic", user, nil).expect(t, 200, "")
	if !strings.Contains(res.raw, `"assessment":null`) || !strings.Contains(res.raw, `"limits":{"default":15,"max":25,"min":6}`) {
		t.Fatalf("before start: %s", res.raw)
	}
	res = e.call("POST", "/assessments/diagnostic", user, map[string]any{"questionCount": 3}).expect(t, 400, "INVALID_INPUT")
	if res.Error.Message != "questionCount: Too small: expected number to be >=6" {
		t.Fatal(res.Error.Message)
	}

	res = e.call("POST", "/assessments/diagnostic", user, map[string]any{}).expect(t, 201, "")
	noAnswerKey(t, res)
	var started struct {
		Assessment assessmentView `json:"assessment"`
	}
	res.into(t, &started)
	a := started.Assessment
	if a.Status != "in_progress" || len(a.Questions) == 0 || len(a.Questions) != a.QuestionCount || len(a.Coverage) == 0 {
		t.Fatalf("started: %s", res.raw)
	}

	// Answer the first question right, the second wrong, skip the rest.
	answers := []map[string]any{e.answerFor(a.Questions[0].ID, true)}
	if len(a.Questions) > 1 {
		answers = append(answers, e.answerFor(a.Questions[1].ID, false))
	}
	path := "/assessments/" + a.ID + "/submit"
	e.call("POST", path, user, map[string]any{"answers": []any{map[string]any{"questionId": "x"}}}).
		expect(t, 400, "INVALID_INPUT")
	res = e.call("POST", path, user, map[string]any{"answers": answers}).expect(t, 200, "")
	var done struct {
		Assessment assessmentView `json:"assessment"`
	}
	res.into(t, &done)
	r := done.Assessment.Result
	if done.Assessment.Status != "completed" || done.Assessment.Questions != nil || r == nil ||
		r.CorrectCount != 1 || r.AnsweredCount != len(answers) || r.TotalQuestions != a.QuestionCount {
		t.Fatalf("completed: %s", res.raw)
	}
	var masteryRows, attempts int
	must(t, e.d.DB.Admin.QueryRow(e.ctx, `select count(*) from concept_mastery where user_id = $1`, user).Scan(&masteryRows))
	if masteryRows == 0 {
		t.Fatal("diagnostic did not move mastery")
	}

	// Idempotent: a second submit returns the result and writes nothing.
	e.call("POST", path, user, map[string]any{"answers": answers}).expect(t, 200, "")
	must(t, e.d.DB.Admin.QueryRow(e.ctx, `select count(*) from question_attempts where user_id = $1 and source = 'assessment'`, user).Scan(&attempts))
	if attempts != len(answers) {
		t.Fatalf("%d attempts after resubmit, want %d", attempts, len(answers))
	}
	e.call("GET", "/assessments/"+a.ID, user, nil).expect(t, 200, "")
	e.call("GET", "/assessments/not-a-uuid", user, nil).expect(t, 404, "ASSESSMENT_NOT_FOUND")
	// Somebody else's paper looks exactly like a missing one.
	e.call("GET", "/assessments/"+a.ID, e.newUser(false), nil).expect(t, 404, "ASSESSMENT_NOT_FOUND")
}

func TestMockTest(t *testing.T) {
	e := setup(t)
	e.call("POST", "/mock-tests", e.newUser(false), map[string]any{}).expect(t, 400, "NOTHING_TO_TEST")

	user := e.newUser(true)
	res := e.call("POST", "/mock-tests", user, map[string]any{"questionCount": 99}).expect(t, 400, "INVALID_INPUT")
	if res.Error.Message != "questionCount: Too big: expected number to be <=30" {
		t.Fatal(res.Error.Message)
	}
	res = e.call("POST", "/mock-tests", user, map[string]any{"questionCount": 6}).expect(t, 201, "")
	noAnswerKey(t, res)
	var created struct {
		Test mockTestView `json:"test"`
	}
	res.into(t, &created)
	tv := created.Test
	if tv.Source != "goal" || tv.Status != "in_progress" || len(tv.Questions) == 0 || len(tv.Questions) != tv.TotalQuestions || len(tv.Coverage) == 0 {
		t.Fatalf("created: %s", res.raw)
	}

	res = e.call("GET", "/mock-tests", user, nil).expect(t, 200, "")
	noAnswerKey(t, res)
	var list struct {
		Tests []mockTestView `json:"tests"`
		Open  *mockTestView  `json:"open"`
	}
	res.into(t, &list)
	if len(list.Tests) != 1 || list.Open == nil || list.Open.ID != tv.ID || len(list.Open.Questions) == 0 {
		t.Fatalf("list: %s", res.raw)
	}

	path := "/mock-tests/" + tv.ID + "/submit"
	res = e.call("POST", path, user, map[string]any{"answers": []any{}}).expect(t, 400, "INVALID_INPUT")
	if res.Error.Message != "secondsTaken: Invalid input: expected number, received undefined" {
		t.Fatal(res.Error.Message)
	}
	answers := []map[string]any{e.answerFor(tv.Questions[0].ID, true)}
	res = e.call("POST", path, user, map[string]any{"answers": answers, "secondsTaken": 120}).expect(t, 200, "")
	var submitted struct {
		Test mockTestView `json:"test"`
	}
	res.into(t, &submitted)
	s := submitted.Test
	if s.Status != "submitted" || s.Questions != nil || s.Result == nil || s.Result.CorrectCount != 1 ||
		s.Result.AnsweredCount != 1 || len(s.Result.ByTopic) == 0 || s.Result.ByTopic[0].Name == "" || s.CorrectCount == nil {
		t.Fatalf("submitted: %s", res.raw)
	}
	var attempts int
	must(t, e.d.DB.Admin.QueryRow(e.ctx, `select count(*) from question_attempts where user_id = $1 and source = 'mock_test'`, user).Scan(&attempts))
	if attempts != 1 {
		t.Fatalf("%d mock_test attempts", attempts)
	}
	// Idempotent.
	e.call("POST", path, user, map[string]any{"answers": answers, "secondsTaken": 120}).expect(t, 200, "")
	e.call("GET", "/mock-tests/"+tv.ID, user, nil).expect(t, 200, "")
	e.call("GET", "/mock-tests/nope", user, nil).expect(t, 404, "TEST_NOT_FOUND")
}

func TestGenerateQuestionsWithoutAI(t *testing.T) {
	e := setup(t)
	user := e.newUser(false)
	res := e.call("POST", "/questions/generate", user, map[string]any{"topicId": "nope"}).expect(t, 400, "INVALID_INPUT")
	if res.Error.Message != "topicId: Invalid UUID" {
		t.Fatal(res.Error.Message)
	}
	res = e.call("POST", "/questions/generate", user, map[string]any{"topicId": e.subjectID, "count": 6}).expect(t, 400, "INVALID_INPUT")
	if res.Error.Message != "count: Too big: expected number to be <=5" {
		t.Fatal(res.Error.Message)
	}
	e.call("POST", "/questions/generate", user, map[string]any{"topicId": e.subjectID}).expect(t, 503, "AI_UNAVAILABLE")
}
