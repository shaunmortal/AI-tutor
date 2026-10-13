// Package learning serves goals, curricula, the learner's own numbers, the study plan,
// readiness, revision and study groups.
//
// Which database client each step uses is the security model: reads of the learner's own rows run AsUser (RLS applies); writes to tables learners
// only hold SELECT on, and reads that must see other learners' rows, run on Admin.
package learning

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/shaunmortal/AI-tutor/server/internal/ai"
	"github.com/shaunmortal/AI-tutor/server/internal/app"
	"github.com/shaunmortal/AI-tutor/server/internal/auth"
	"github.com/shaunmortal/AI-tutor/server/internal/curriculum"
	"github.com/shaunmortal/AI-tutor/server/internal/dates"
	"github.com/shaunmortal/AI-tutor/server/internal/goals"
	"github.com/shaunmortal/AI-tutor/server/internal/httpx"
	"github.com/shaunmortal/AI-tutor/server/internal/learner"
	"github.com/shaunmortal/AI-tutor/server/internal/ratelimit"
	"github.com/shaunmortal/AI-tutor/server/internal/readiness"
	"github.com/shaunmortal/AI-tutor/server/internal/store"
)

type route struct {
	pattern string
	h       httpx.Handler
	mw      []func(http.Handler) http.Handler
}

// routes is the table Register mounts; tests mount it without auth and rate limits.
func routes(d *app.Deps) []route {
	h := handlers{d}
	limit := func(name string, max int64, message string) []func(http.Handler) http.Handler {
		return []func(http.Handler) http.Handler{ratelimit.Limit(d.Redis, name, max, time.Hour, message)}
	}
	return []route{
		{"GET /goals/subjects", h.listSubjects, nil},
		{"GET /goals", h.getActiveGoal, nil},
		{"POST /goals", h.createGoal, nil},

		{"GET /curricula/default", h.getDefaultCurriculum, nil},
		{"POST /curricula/resolve", h.resolveCurriculum,
			limit("curricula-resolve", 20, "You have looked up a lot of goals in the last hour.")},

		{"GET /learner/summary", h.getLearnerSummary, nil},
		{"GET /learner/activity", h.getActivity, nil},
		{"GET /learner/subjects", h.getGoalSubjects, nil},
		{"GET /learner/profile", h.getProfile, nil},
		{"PATCH /learner/profile", h.updateProfile, nil},
		{"GET /learner/confidence", h.getConfidence, nil},
		{"PUT /learner/confidence", h.saveConfidence, nil},

		{"GET /study-plan/current", h.getCurrentPlan, nil},
		{"POST /study-plan/generate", h.generatePlan,
			limit("plan-generate", 20, "You have rebuilt your plan a lot in the last hour.")},

		{"GET /readiness", h.getReadiness, nil},
		{"GET /revision/due", h.getDueRevisions, nil},

		{"GET /groups", h.listGroups, nil},
		{"POST /groups", h.createGroup, nil},
		{"POST /groups/join", h.joinGroup,
			limit("groups-join", 30, "Too many join attempts. Wait a while and check the code you were given.")},
		{"GET /groups/{id}", h.getGroup, nil},
		{"POST /groups/{id}/leave", h.leaveGroup, nil},
	}
}

func Register(d *app.Deps) {
	for _, r := range routes(d) {
		d.Authed(r.pattern, r.h, r.mw...)
	}
}

type handlers struct{ d *app.Deps }

// asUser runs fn as the signed-in learner, so RLS applies.
func (h handlers) asUser(r *http.Request, fn func(tx pgx.Tx) error) error {
	return h.d.DB.AsUser(r.Context(), auth.UserID(r), fn)
}

// activeGoal is the learner's goal, read as the learner.
func (h handlers) activeGoal(r *http.Request) (goal *goals.View, err error) {
	err = h.asUser(r, func(tx pgx.Tx) error {
		goal, err = goals.New(store.NewGoals(tx)).GetActive(r.Context(), auth.UserID(r))
		return err
	})
	return goal, err
}

func timezoneOf(tz *string) string {
	if tz == nil {
		return ""
	}
	return *tz
}

// ---- goals ----

// GET /goals/subjects?curriculumId=… — subjects within one syllabus.
func (h handlers) listSubjects(w http.ResponseWriter, r *http.Request) error {
	ids := r.URL.Query()["curriculumId"]
	if len(ids) != 1 || !isUUID(ids[0]) {
		return invalid("curriculumId is required and must be a uuid.")
	}
	var subjects []store.SubjectRow
	err := h.asUser(r, func(tx pgx.Tx) (err error) {
		subjects, err = goals.New(store.NewGoals(tx)).ListSubjects(r.Context(), ids[0])
		return err
	})
	if err != nil {
		return err
	}
	httpx.OK(w, http.StatusOK, map[string]any{"subjects": subjects})
	return nil
}

// GET /goals — the active goal, or null.
func (h handlers) getActiveGoal(w http.ResponseWriter, r *http.Request) error {
	goal, err := h.activeGoal(r)
	if err != nil {
		return err
	}
	httpx.OK(w, http.StatusOK, map[string]any{"goal": goal})
	return nil
}

// POST /goals — Admin, because the goal tables grant learners SELECT only.
func (h handlers) createGoal(w http.ResponseWriter, r *http.Request) error {
	f, err := readObject(w, r)
	if err != nil {
		return err
	}
	in := goals.CreateInput{UserID: auth.UserID(r)}
	if in.Title, err = f.str("title", 1, 120); err != nil {
		return err
	}
	if in.CurriculumID, err = f.uuid("curriculumId"); err != nil {
		return err
	}
	examDate, ok := f["examDate"].(string)
	if !ok {
		return expected("examDate", "string", f["examDate"], hasKey(f, "examDate"))
	}
	if !datePattern(examDate) {
		return invalid("examDate: must be a date in YYYY-MM-DD form")
	}
	in.ExamDate = examDate
	if in.DailyMinutes, err = f.int("dailyMinutes", 10, 960); err != nil {
		return err
	}
	if in.SubjectIDs, err = f.uuids("subjectIds", 1, 20); err != nil {
		return err
	}

	// The insert runs on Admin, where RLS doesn't apply, so the curriculum rule is checked here:
	// a goal may only point at a shared syllabus or the learner's own course map. Otherwise a goal
	// would be a way to read somebody else's map.
	allowed, err := store.NewSources(h.d.DB.Admin).CanReadCourseMap(r.Context(), in.CurriculumID, in.UserID)
	if err != nil {
		return err
	}
	if !allowed {
		return httpx.E(http.StatusNotFound, "CURRICULUM_NOT_FOUND", "That syllabus does not exist.")
	}

	goal, err := goals.New(store.NewGoals(h.d.DB.Admin)).Create(r.Context(), in)
	if err != nil {
		return err
	}
	httpx.OK(w, http.StatusCreated, map[string]any{"goal": goal})
	return nil
}

func hasKey(f fields, key string) bool { _, ok := f[key]; return ok }

// datePattern is /^\d{4}-\d{2}-\d{2}$/.
func datePattern(s string) bool {
	if len(s) != 10 {
		return false
	}
	for i, c := range []byte(s) {
		if (i == 4 || i == 7) != (c == '-') || (c != '-' && (c < '0' || c > '9')) {
			return false
		}
	}
	return true
}

// ---- curricula ----

func curriculumView(c *store.CurriculumRow) map[string]any {
	return map[string]any{
		"id": c.ID, "slug": c.Slug, "name": c.Name, "description": c.Description, "isAiGenerated": c.IsAIGenerated,
	}
}

// outcome is a ResolveOutcome with a curriculum: status is "cached" or "created".
func outcome(ctx context.Context, curricula *store.Curricula, status string, c *store.CurriculumRow) (map[string]any, error) {
	subjects, err := curricula.FindSubjects(ctx, c.ID)
	if err != nil {
		return nil, err
	}
	return map[string]any{"status": status, "curriculum": curriculumView(c), "subjects": subjects}, nil
}

// GET /curricula/default — no AI call on this path, so it is read as the learner.
func (h handlers) getDefaultCurriculum(w http.ResponseWriter, r *http.Request) error {
	var out map[string]any
	err := h.asUser(r, func(tx pgx.Tx) error {
		curricula := store.NewCurricula(tx)
		c, err := curricula.FindDefault(r.Context())
		if err != nil {
			return err
		}
		if c == nil {
			return httpx.E(http.StatusInternalServerError, "NO_DEFAULT_CURRICULUM", "No default syllabus is available.")
		}
		out, err = outcome(r.Context(), curricula, "cached", c)
		return err
	})
	if err != nil {
		return err
	}
	httpx.OK(w, http.StatusOK, out)
	return nil
}

// POST /curricula/resolve — Admin, because it may write a curriculum. A rejection is a 200 with
// status "rejected": the request was fine, the text just is not a study goal.
func (h handlers) resolveCurriculum(w http.ResponseWriter, r *http.Request) error {
	f, err := readObject(w, r)
	if err != nil {
		return err
	}
	// Validation leaves at most 120 UTF-16 units after trimming, so the TS slice is a no-op.
	goalText, err := f.str("goalText", 2, 120)
	if err != nil {
		return err
	}
	// The TS built the provider before anything else, so a server without a key answered
	// AI_UNAVAILABLE even for a goal already stored.
	if h.d.AI == nil {
		return httpx.E(http.StatusServiceUnavailable, "AI_UNAVAILABLE", "AI features are not configured on this server.")
	}
	out, err := h.resolve(r.Context(), store.NewCurricula(h.d.DB.Admin), goalText)
	if err != nil {
		return err
	}
	httpx.OK(w, http.StatusOK, out)
	return nil
}

func (h handlers) resolve(ctx context.Context, curricula *store.Curricula, goalText string) (map[string]any, error) {
	// Step 1: free lookup before any paid call.
	if slug := curriculum.ToSlug(goalText); len(slug) >= 2 {
		existing, err := curricula.FindBySlug(ctx, slug)
		if err != nil {
			return nil, err
		}
		if existing != nil {
			return outcome(ctx, curricula, "cached", existing)
		}
	}

	// Step 2: one call that judges and builds.
	var reply json.RawMessage
	if _, err := h.d.AI.JSON(ctx, ai.Request{
		System: ai.CurriculumSystem, Prompt: ai.CurriculumPrompt(goalText), Schema: ai.CurriculumSchema,
		MaxTokens: 6000, Quality: ai.High,
	}, &reply); err != nil {
		return nil, err
	}

	// Step 3: the validator decides, not the model.
	o := curriculum.ValidateReply(reply, goalText)
	if !o.OK {
		slog.Warn("curriculum rejected", "goalText", goalText, "detail", o.LogDetail)
		return map[string]any{"status": "rejected", "message": o.LearnerMessage}, nil
	}

	// The model's slug can differ from the learner's phrasing, so check again before writing.
	settled, err := curricula.FindBySlug(ctx, o.Curriculum.Slug)
	if err != nil {
		return nil, err
	}
	if settled != nil {
		return outcome(ctx, curricula, "cached", settled)
	}

	// Step 4: store, then re-resolve so a concurrent writer does not turn into an error.
	created, createErr := curricula.Create(ctx, o.Curriculum)
	if createErr == nil {
		return outcome(ctx, curricula, "created", created)
	}
	raced, err := curricula.FindBySlug(ctx, o.Curriculum.Slug)
	if err != nil {
		return nil, err
	}
	if raced != nil {
		return outcome(ctx, curricula, "cached", raced)
	}
	return nil, createErr
}

// ---- learner ----

// GET /learner/summary
func (h handlers) getLearnerSummary(w http.ResponseWriter, r *http.Request) error {
	var summary learner.Summary
	err := h.asUser(r, func(tx pgx.Tx) (err error) {
		summary, err = learner.NewModelService(store.NewLearners(tx)).GetSummary(r.Context(), auth.UserID(r))
		return err
	})
	if err != nil {
		return err
	}
	httpx.OK(w, http.StatusOK, summary)
	return nil
}

// GET /learner/activity — a year of squares, the streaks read off them, and the badges.
func (h handlers) getActivity(w http.ResponseWriter, r *http.Request) error {
	userID, ctx := auth.UserID(r), r.Context()
	var out map[string]any
	err := h.asUser(r, func(tx pgx.Tx) error {
		learners := store.NewLearners(tx)
		tzp, err := learners.FindTimezone(ctx, userID)
		if err != nil {
			return err
		}
		tz := timezoneOf(tzp)
		today := dates.TodayIn(tz, time.Now())
		from := dates.AddDays(today, -(learner.CalendarDays - 1))

		activity, err := learners.FindDailyActivity(ctx, userID, from, today,
			func(at time.Time) string { return dates.TodayIn(tz, at) })
		if err != nil {
			return err
		}
		achievements, err := learners.FindAchievementCounts(ctx, userID, learner.MasteryBar)
		if err != nil {
			return err
		}
		profile, err := learners.FindProfile(ctx, userID)
		if err != nil {
			return err
		}

		calendar := learner.BuildCalendar(activity.Counts, today)
		in := learner.BadgeInputs{
			ActiveDates:      sortedKeys(activity.Counts),
			MaxStreak:        calendar.MaxStreak,
			TotalAnswered:    activity.Total,
			TopicsMastered:   achievements.TopicsMastered,
			ReviewsCompleted: achievements.ReviewsCompleted,
		}
		// Lifetime, not the window: a badge for a thousand answers is not lost to time.
		if profile != nil {
			in.TotalAnswered = profile.TotalAttempts
			in.AccuracyPercent = profile.OverallAccuracy
		}
		badges := learner.EarnedBadges(in)
		var latest *learner.Badge
		if len(badges) > 0 {
			latest = &badges[0]
		}
		out = map[string]any{
			"calendar": calendar, "badges": badges, "upcoming": learner.NextBadges(in), "latestBadge": latest,
		}
		return nil
	})
	if err != nil {
		return err
	}
	httpx.OK(w, http.StatusOK, out)
	return nil
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// GET /learner/subjects — the goal's subjects with progress; empty means no goal yet.
func (h handlers) getGoalSubjects(w http.ResponseWriter, r *http.Request) error {
	var subjects []store.GoalSubject
	err := h.asUser(r, func(tx pgx.Tx) (err error) {
		subjects, err = store.NewLearners(tx).FindGoalSubjects(r.Context(), auth.UserID(r))
		return err
	})
	if err != nil {
		return err
	}
	httpx.OK(w, http.StatusOK, map[string]any{"subjects": subjects})
	return nil
}

func profileView(p *store.AccountProfile) map[string]any {
	tz := dates.DefaultTimezone
	if p.Timezone != nil {
		tz = *p.Timezone
	}
	return map[string]any{"profile": map[string]any{
		"displayName": p.DisplayName, "email": p.Email, "timezone": tz, "createdAt": p.CreatedAt,
		"preferredLanguage": p.PreferredLanguage,
	}}
}

// GET /learner/profile
func (h handlers) getProfile(w http.ResponseWriter, r *http.Request) error {
	var p *store.AccountProfile
	err := h.asUser(r, func(tx pgx.Tx) (err error) {
		p, err = store.NewLearners(tx).FindAccountProfile(r.Context(), auth.UserID(r))
		return err
	})
	if err != nil {
		return err
	}
	if p == nil {
		// Created by a trigger on sign-up, so its absence is an account problem.
		return httpx.E(http.StatusNotFound, "PROFILE_NOT_FOUND", "Your profile could not be loaded.")
	}
	httpx.OK(w, http.StatusOK, profileView(p))
	return nil
}

// PATCH /learner/profile — as the learner: the column grant is what actually holds.
func (h handlers) updateProfile(w http.ResponseWriter, r *http.Request) error {
	f, err := readObject(w, r)
	if err != nil {
		return err
	}
	displayName, err := f.optStr("displayName", 1, 60)
	if err != nil {
		return err
	}
	timezone, err := f.optStr("timezone", 0, 60)
	if err != nil {
		return err
	}
	if timezone != nil && !knownTimezone(*timezone) {
		return invalid("timezone: is not a timezone this server recognises")
	}
	language, err := f.optStr("preferredLanguage", 0, 20)
	if err != nil {
		return err
	}
	if language != nil && *language != "english" && *language != "hindi" && *language != "hinglish" {
		return invalid("preferredLanguage: must be english, hindi or hinglish")
	}
	if displayName == nil && timezone == nil && language == nil {
		return invalid("send a displayName, a timezone or a preferredLanguage")
	}

	var p *store.AccountProfile
	err = h.asUser(r, func(tx pgx.Tx) (err error) {
		p, err = store.NewLearners(tx).UpdateAccountProfile(r.Context(), auth.UserID(r), displayName, timezone, language)
		return err
	})
	if err != nil {
		return err
	}
	httpx.OK(w, http.StatusOK, profileView(p))
	return nil
}

// knownTimezone: a zone this server can format, so streaks and revision dates work for it.
// "" and "Local" load in Go but Intl rejects them.
func knownTimezone(zone string) bool {
	if zone == "" || zone == "Local" {
		return false
	}
	_, err := time.LoadLocation(zone)
	return err == nil
}

// ---- readiness and revision ----

// GET /readiness — derived on every read, as the learner.
func (h handlers) getReadiness(w http.ResponseWriter, r *http.Request) error {
	userID, ctx := auth.UserID(r), r.Context()
	var out map[string]any
	err := h.asUser(r, func(tx pgx.Tx) error {
		goal, err := goals.New(store.NewGoals(tx)).GetActive(ctx, userID)
		if err != nil {
			return err
		}
		if goal == nil {
			out = map[string]any{"goal": nil, "readiness": nil}
			return nil
		}
		planTopics, err := store.NewPlans(tx).FindPlannableTopics(ctx, userID, goal.SubjectIDs)
		if err != nil {
			return err
		}
		mocks, err := store.NewMockTests(tx).FindRecent(ctx, userID, 20)
		if err != nil {
			return err
		}

		in := readiness.Input{Topics: []readiness.Topic{}, Mocks: []readiness.Mock{},
			DaysRemaining: goal.DaysRemaining, DailyMinutes: goal.DailyMinutes}
		for _, t := range planTopics {
			// Field by field: readiness counts evidence only, so a self-rated prior stays out.
			in.Topics = append(in.Topics, readiness.Topic{TopicID: t.TopicID, Name: t.Name, SubjectName: t.SubjectName,
				Weight: t.Weight, MasteryScore: t.MasteryScore, Attempts: t.Attempts})
		}
		// Only finished papers: an abandoned test says nothing about form.
		for _, m := range mocks {
			if m.Status != "submitted" || m.ScorePercent == nil {
				continue
			}
			at := m.StartedAt
			if m.SubmittedAt != nil {
				at = *m.SubmittedAt
			}
			// Fixed width, so ordering the strings orders the times.
			in.Mocks = append(in.Mocks, readiness.Mock{ScorePercent: *m.ScorePercent,
				SubmittedAt: at.UTC().Format("2006-01-02T15:04:05.000000Z07:00")})
		}

		out = map[string]any{
			"goal": map[string]any{
				"title": goal.Title, "examDate": goal.ExamDate,
				"daysRemaining": goal.DaysRemaining, "dailyMinutes": goal.DailyMinutes,
			},
			"readiness": readiness.Score(in),
			"thresholds": map[string]any{
				"ready": readiness.ReadyAt, "onTrack": readiness.OnTrackAt,
				"building": readiness.BuildingAt, "masteryTarget": readiness.MasteryTarget,
			},
		}
		return nil
	})
	if err != nil {
		return err
	}
	httpx.OK(w, http.StatusOK, out)
	return nil
}

type dueTopic struct {
	store.DueTopic
	DaysOverdue int `json:"daysOverdue"`
}

// GET /revision/due — topics owed a review, scoped to the current goal, as the learner.
func (h handlers) getDueRevisions(w http.ResponseWriter, r *http.Request) error {
	userID, ctx := auth.UserID(r), r.Context()
	var out map[string]any
	err := h.asUser(r, func(tx pgx.Tx) error {
		goal, err := goals.New(store.NewGoals(tx)).GetActive(ctx, userID)
		if err != nil {
			return err
		}
		scope := []string{}
		if goal != nil {
			scope = goal.SubjectIDs
		}
		tz, err := store.NewLearners(tx).FindTimezone(ctx, userID)
		if err != nil {
			return err
		}
		today := dates.TodayIn(timezoneOf(tz), time.Now())
		due, err := store.NewRevisions(tx).FindDue(ctx, userID, today, scope, 20)
		if err != nil {
			return err
		}
		list := make([]dueTopic, len(due))
		for i, t := range due {
			list[i] = dueTopic{t, learner.DaysOverdue(t.DueOn, today)}
		}
		out = map[string]any{"today": today, "due": list}
		return nil
	})
	if err != nil {
		return err
	}
	httpx.OK(w, http.StatusOK, out)
	return nil
}

// ---- self-rated confidence (onboarding) ----

// GET /learner/confidence — the learner's self-ratings, as the learner.
func (h handlers) getConfidence(w http.ResponseWriter, r *http.Request) error {
	var ratings []store.SelfRating
	err := h.asUser(r, func(tx pgx.Tx) (err error) {
		ratings, err = store.NewLearners(tx).FindSelfRatings(r.Context(), auth.UserID(r))
		return err
	})
	if err != nil {
		return err
	}
	httpx.OK(w, http.StatusOK, map[string]any{"ratings": ratings})
	return nil
}

// PUT /learner/confidence {ratings: [{topicId, confidence 1-5}]} — how sure the learner is of each
// area before any evidence. Only a prior for topics they have not answered on (see
// learner.SelfRatedPrior). Admin, because learners hold SELECT only on self_ratings.
func (h handlers) saveConfidence(w http.ResponseWriter, r *http.Request) error {
	var body struct {
		Ratings []store.SelfRating `json:"ratings"`
	}
	if err := httpx.Decode(w, r, &body); err != nil {
		return err
	}
	if len(body.Ratings) == 0 || len(body.Ratings) > 50 {
		return invalid("ratings: send between 1 and 50 ratings")
	}
	seen := map[string]bool{}
	for _, s := range body.Ratings {
		if !isUUID(s.TopicID) || s.Confidence < 1 || s.Confidence > 5 || seen[s.TopicID] {
			return invalid("ratings: each needs its own topicId and a confidence from 1 to 5")
		}
		seen[s.TopicID] = true
	}
	if err := store.NewLearners(h.d.DB.Admin).SaveSelfRatings(r.Context(), auth.UserID(r), body.Ratings); err != nil {
		return err
	}
	httpx.OK(w, http.StatusOK, map[string]any{"ratings": body.Ratings})
	return nil
}
