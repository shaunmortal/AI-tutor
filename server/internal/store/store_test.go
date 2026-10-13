package store

// Integration tests against the live database in DATABASE_URL (or server/.env). Every test
// runs inside one transaction that is always rolled back, so nothing is left behind. Users
// are throwaway auth.users rows created inside that transaction, as database/tests/*.sql do.

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/shaunmortal/AI-tutor/server/internal/adaptive"
	"github.com/shaunmortal/AI-tutor/server/internal/curriculum"
	"github.com/shaunmortal/AI-tutor/server/internal/groups"
	"github.com/shaunmortal/AI-tutor/server/internal/httpx"
	"github.com/shaunmortal/AI-tutor/server/internal/learner"
	"github.com/shaunmortal/AI-tutor/server/internal/planning"
)

type fixture struct {
	ctx context.Context
	tx  pgx.Tx
	// Two throwaway learners.
	u1, u2 string
	// From the seeded default curriculum: a subject, a leaf topic under it with a verified
	// question, and that question.
	curriculumID, subjectID, topicID, questionID string
}

func databaseURL() string {
	if url := os.Getenv("DATABASE_URL"); url != "" {
		return url
	}
	raw, err := os.ReadFile("../../.env")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "DATABASE_URL="); ok {
			return strings.Trim(v, `"'`)
		}
	}
	return ""
}

func setup(t *testing.T) *fixture {
	t.Helper()
	url := databaseURL()
	if url == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	must(t, err)
	tx, err := conn.Begin(ctx)
	must(t, err)
	t.Cleanup(func() {
		_ = tx.Rollback(ctx) // always: this is a shared database
		_ = conn.Close(ctx)
	})

	f := &fixture{ctx: ctx, tx: tx}
	f.u1, f.u2 = f.newUser(t), f.newUser(t)
	must(t, tx.QueryRow(ctx,
		`select c.id, s.id, l.id, q.id from curricula c
		 join topics s on s.curriculum_id = c.id and s.parent_id is null
		 join topics l on l.parent_id = s.id
		 join questions q on q.topic_id = l.id and q.is_verified
		 where c.slug = $1 order by s.sort_order, l.sort_order, q.id limit 1`, DefaultCurriculumSlug).
		Scan(&f.curriculumID, &f.subjectID, &f.topicID, &f.questionID))
	return f
}

func (f *fixture) newUser(t *testing.T) string {
	t.Helper()
	var id string
	must(t, f.tx.QueryRow(f.ctx,
		`with id as (select gen_random_uuid() as id)
		 insert into auth.users (id, email) select id, id || '@adigam.test' from id returning id::text`).Scan(&id))
	return id
}

// asUser runs fn the way db.AsUser does — JWT claims plus `set local role authenticated` —
// inside a savepoint that is rolled back, so the outer test continues as the admin role.
func (f *fixture) asUser(t *testing.T, userID string, fn func(q Q)) {
	t.Helper()
	sp, err := f.tx.Begin(f.ctx)
	must(t, err)
	defer func() { _ = sp.Rollback(f.ctx) }()
	claims, _ := json.Marshal(map[string]string{"sub": userID, "role": "authenticated"})
	_, err = sp.Exec(f.ctx, `select set_config('request.jwt.claims', $1, true)`, string(claims))
	must(t, err)
	_, err = sp.Exec(f.ctx, `set local role authenticated`)
	must(t, err)
	fn(sp)
}

func (f *fixture) goal(t *testing.T, userID string) *GoalRow {
	t.Helper()
	g, err := NewGoals(f.tx).ReplaceActive(f.ctx, userID, NewGoal{Title: "Exam", ExamDate: "2099-01-01",
		DailyMinutes: 60, SubjectIDs: []string{f.subjectID}, CurriculumID: f.curriculumID})
	must(t, err)
	return g
}

func (f *fixture) attempt(t *testing.T, userID string, correct bool) *InsertedAttempt {
	t.Helper()
	a, err := NewAttempts(f.tx).Insert(f.ctx, NewAttempt{UserID: userID, QuestionID: f.questionID, IsCorrect: correct,
		GivenAnswer: json.RawMessage(`{"selectedOptions":[0]}`), TimeTakenSeconds: ptr(20), Source: "practice"})
	must(t, err)
	return a
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

func today() string { return time.Now().UTC().Format("2006-01-02") }

func TestGoals(t *testing.T) {
	f := setup(t)
	r := NewGoals(f.tx)

	subjects, err := r.FindSubjects(f.ctx, f.curriculumID)
	must(t, err)
	var total int
	must(t, f.tx.QueryRow(f.ctx, `select count(*) from topics where curriculum_id = $1`, f.curriculumID).Scan(&total))
	leaves := 0
	for _, s := range subjects {
		leaves += s.TopicCount
	}
	check(t, len(subjects) > 0 && len(subjects)+leaves == total, "subjects %d + leaves %d != %d topics", len(subjects), leaves, total)

	none, err := r.FindActive(f.ctx, f.u1)
	must(t, err)
	check(t, none == nil, "a new learner has no goal")

	first := f.goal(t, f.u1)
	second := f.goal(t, f.u1)
	active, err := r.FindActive(f.ctx, f.u1)
	must(t, err)
	check(t, active != nil && active.ID == second.ID && active.ExamDate == "2099-01-01" && active.Status == "active",
		"active goal should be the second, got %+v", active)
	check(t, first.ID != second.ID, "replace creates a new goal")
	scope, err := r.FindScope(f.ctx, second.ID)
	must(t, err)
	check(t, len(scope) == 1 && scope[0] == f.subjectID, "scope %v", scope)

	// RLS: through the user-scoped path a learner sees their own goal and nobody else's.
	f.asUser(t, f.u1, func(q Q) {
		g, err := NewGoals(q).FindActive(f.ctx, f.u1)
		must(t, err)
		check(t, g != nil && g.ID == second.ID, "u1 should see their own goal")
	})
	f.asUser(t, f.u2, func(q Q) {
		g, err := NewGoals(q).FindActive(f.ctx, f.u1)
		must(t, err)
		check(t, g == nil, "RLS should hide u1's goal from u2, got %+v", g)
		s, err := NewGoals(q).FindScope(f.ctx, second.ID)
		must(t, err)
		check(t, len(s) == 0, "RLS should hide u1's goal scope from u2, got %v", s)
		// And learners cannot write goals at all.
		_, err = NewGoals(q).ReplaceActive(f.ctx, f.u2, NewGoal{Title: "x", ExamDate: "2099-01-01", DailyMinutes: 60,
			SubjectIDs: []string{f.subjectID}, CurriculumID: f.curriculumID})
		check(t, isCode(err, "42501"), "authenticated must not write goals, got %v", err)
	})
}

func isCode(err error, code string) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == code
}

func TestAttempts(t *testing.T) {
	f := setup(t)
	r := NewAttempts(f.tx)

	q, err := r.FindForGrading(f.ctx, f.questionID)
	must(t, err)
	check(t, q.ID == f.questionID && len(q.CorrectAnswer) > 0 && q.TopicID == f.topicID, "grading row %+v", q)
	_, err = r.FindForGrading(f.ctx, "00000000-0000-4000-8000-000000000000")
	var he *httpx.Error
	check(t, errors.As(err, &he) && he.Code == "QUESTION_NOT_FOUND", "missing question should 404, got %v", err)

	ins := f.attempt(t, f.u1, true)
	check(t, ins.TopicID == f.topicID, "topic_id comes from the trigger, got %s", ins.TopicID)
	byID, err := r.FindByIDs(f.ctx, []string{ins.ID})
	must(t, err)
	check(t, len(byID) == 1 && byID[0].IsCorrect, "findByIds %+v", byID)

	rows, err := r.FindTopicAttempts(f.ctx, f.u1, f.topicID)
	must(t, err)
	check(t, len(rows) == 1 && rows[0].IsCorrect && rows[0].Questions != nil && rows[0].Questions.Difficulty != "",
		"topic attempts %+v", rows)

	f.asUser(t, f.u1, func(q Q) {
		rows, err := NewAttempts(q).FindTopicAttempts(f.ctx, f.u1, f.topicID)
		must(t, err)
		check(t, len(rows) == 1, "u1 should see their own attempt, got %d", len(rows))
	})
	f.asUser(t, f.u2, func(q Q) {
		rows, err := NewAttempts(q).FindTopicAttempts(f.ctx, f.u1, f.topicID)
		must(t, err)
		check(t, len(rows) == 0, "RLS should hide u1's attempts from u2, got %d", len(rows))
	})
	// correct_answer is not granted to authenticated: the grading read must fail as a learner,
	// while the learner-visible columns still work.
	f.asUser(t, f.u1, func(q Q) {
		_, err := NewAttempts(q).FindForGrading(f.ctx, f.questionID)
		check(t, isCode(err, "42501"), "correct_answer must be unreadable as authenticated, got %v", err)
	})
	f.asUser(t, f.u1, func(q Q) {
		qs, err := NewQuestions(q).FindForPractice(f.ctx, adaptive.PracticeQuery{TopicID: f.topicID, Limit: 5})
		must(t, err)
		check(t, len(qs) > 0, "learner should read practice questions")
	})
}

func TestMastery(t *testing.T) {
	f := setup(t)
	r := NewMastery(f.tx)
	none, err := r.FindScore(f.ctx, f.u1, f.topicID)
	must(t, err)
	check(t, none == nil, "no score yet")

	now := time.Now()
	attempts := []learner.AttemptRecord{{IsCorrect: true, Difficulty: "medium", AttemptedAt: now}}
	e := learner.SummariseAttempts(attempts, now)
	m := learner.ComputeMastery(e)
	must(t, r.Save(f.ctx, f.u1, f.topicID, e, m, now, &now))
	must(t, r.Save(f.ctx, f.u1, f.topicID, e, m, now, &now)) // upsert, not a duplicate
	score, err := r.FindScore(f.ctx, f.u1, f.topicID)
	must(t, err)
	check(t, score != nil && *score == m.Score, "score %v, want %v", score, m.Score)
}

func TestTopics(t *testing.T) {
	f := setup(t)
	r := NewTopics(f.tx)
	now := time.Now()

	all, err := r.FindCandidatesForLearner(f.ctx, f.u1, now, "")
	must(t, err)
	f.goal(t, f.u1)
	saveMastery(t, f, f.u1, f.topicID)
	scoped, err := r.FindCandidatesForLearner(f.ctx, f.u1, now, "")
	must(t, err)
	check(t, len(scoped) > 0 && len(scoped) < len(all), "goal scope %d should narrow all %d", len(scoped), len(all))
	var hit *adaptive.TopicCandidate
	for i := range scoped {
		if scoped[i].TopicID == f.topicID {
			hit = &scoped[i]
		}
	}
	check(t, hit != nil && hit.MasteryScore != nil && hit.TotalAttempts == 1 && hit.RecentAccuracy != nil &&
		*hit.RecentAccuracy == 1 && hit.DaysSinceLastAttempt != nil, "candidate %+v", hit)

	same, err := r.FindCandidatesForLearner(f.ctx, f.u1, now, f.subjectID)
	must(t, err)
	check(t, len(same) == len(scoped), "subject in goal keeps the scope")
	out, err := r.FindCandidatesForLearner(f.ctx, f.u1, now, f.topicID)
	must(t, err)
	check(t, len(out) == 0, "a subject outside the goal yields nothing, got %d", len(out))
}

func saveMastery(t *testing.T, f *fixture, userID, topicID string) learner.MasteryResult {
	t.Helper()
	now := time.Now()
	e := learner.SummariseAttempts([]learner.AttemptRecord{{IsCorrect: true, Difficulty: "medium", AttemptedAt: now}}, now)
	m := learner.ComputeMastery(e)
	must(t, NewMastery(f.tx).Save(f.ctx, userID, topicID, e, m, now, &now))
	return m
}

func TestLearners(t *testing.T) {
	f := setup(t)
	r := NewLearners(f.tx)

	tz, err := r.FindTimezone(f.ctx, f.u1)
	must(t, err)
	check(t, tz != nil && *tz == "Asia/Kolkata", "timezone %v", tz)

	f.attempt(t, f.u1, true)
	secs := 20.0
	must(t, r.RecordAttemptOnProfile(f.ctx, f.u1, true, today(), &secs))
	p, err := r.FindProfile(f.ctx, f.u1)
	must(t, err)
	check(t, p.TotalAttempts == 1 && p.CorrectAttempts == 1 && p.CurrentStreakDays == 1 && p.LongestStreakDays == 1 &&
		p.LastActivityDate != nil && *p.LastActivityDate == today() && p.AvgSecondsPerQuestion != nil &&
		p.ConsistencyScore != nil && p.OverallAccuracy != nil && *p.OverallAccuracy == 100, "profile %+v", p)

	act, err := r.FindDailyActivity(f.ctx, f.u1, today(), today(), func(at time.Time) string { return at.UTC().Format("2006-01-02") })
	must(t, err)
	check(t, act.Total == 1 && act.Correct == 1 && act.Counts[today()] == 1, "activity %+v", act)

	f.goal(t, f.u1)
	m := saveMastery(t, f, f.u1, f.topicID)
	subjects, err := r.FindGoalSubjects(f.ctx, f.u1)
	must(t, err)
	check(t, len(subjects) == 1 && subjects[0].ID == f.subjectID && subjects[0].TopicsStarted == 1 &&
		subjects[0].QuestionsAnswered == 1 && subjects[0].AverageMastery != nil && subjects[0].TopicCount > 0,
		"goal subjects %+v", subjects)

	counts, err := r.FindAchievementCounts(f.ctx, f.u1, 0)
	must(t, err)
	check(t, counts.TopicsMastered == 1 && counts.ReviewsCompleted == 0, "achievements %+v", counts)

	mastery, err := r.FindMastery(f.ctx, f.u1, 5)
	must(t, err)
	check(t, len(mastery) == 1 && mastery[0].Topics != nil && mastery[0].MasteryScore == m.Score, "mastery %+v", mastery)

	sum, err := learner.NewModelService(r).GetSummary(f.ctx, f.u1)
	must(t, err)
	check(t, sum.HasActivity && sum.Attempts.Total == 1 && len(sum.WeakestTopics) == 1 &&
		sum.WeakestTopics[0].Name == mastery[0].Topics.Name && sum.ComputedAt != nil, "summary %+v", sum)
}

func TestQuestions(t *testing.T) {
	f := setup(t)
	r := NewQuestions(f.tx)

	all, err := r.FindForPractice(f.ctx, adaptive.PracticeQuery{TopicID: f.topicID, Limit: 50})
	must(t, err)
	check(t, len(all) > 0 && all[0].Marks >= 1 && all[0].Body != "", "practice %+v", all)
	excl, err := r.FindForPractice(f.ctx, adaptive.PracticeQuery{TopicID: f.topicID, Exclude: []string{f.questionID}, Limit: 50})
	must(t, err)
	check(t, len(excl) == len(all)-1, "exclude drops one: %d vs %d", len(excl), len(all))
	only, err := r.FindForPractice(f.ctx, adaptive.PracticeQuery{TopicID: f.topicID, Only: []string{f.questionID}, Limit: 50})
	must(t, err)
	check(t, len(only) == 1 && only[0].ID == f.questionID, "only %+v", only)
	empty, err := r.FindForPractice(f.ctx, adaptive.PracticeQuery{TopicID: f.topicID, Only: []string{}, Limit: 50})
	must(t, err)
	check(t, len(empty) == 0, "empty only matches nothing")
	byLevel, err := r.FindForPractice(f.ctx, adaptive.PracticeQuery{TopicID: f.topicID, Difficulty: all[0].Difficulty, Limit: 50})
	must(t, err)
	for _, q := range byLevel {
		check(t, q.Difficulty == all[0].Difficulty, "difficulty filter")
	}

	f.attempt(t, f.u1, false)
	f.attempt(t, f.u1, true)
	ids, err := r.FindAttemptedIDs(f.ctx, f.u1, f.topicID)
	must(t, err)
	check(t, len(ids) == 1 && ids[0] == f.questionID, "attempted ids deduped %v", ids)

	ts, err := r.FindTopicWithSubject(f.ctx, f.topicID)
	must(t, err)
	check(t, ts != nil && ts.SubjectName != ts.Name, "topic with subject %+v", ts)
	subj, err := r.FindTopicWithSubject(f.ctx, f.subjectID)
	must(t, err)
	check(t, subj.SubjectName == subj.Name, "a subject is its own context")

	before, err := r.CountVerifiedForTopic(f.ctx, f.topicID)
	must(t, err)
	g := GeneratedQuestion{TopicID: f.topicID, QuestionType: "mcq", Body: "Store test question " + f.u1,
		Options: []string{"a", "b"}, CorrectOptionIndexes: []int{1}, Explanation: "because", Difficulty: "easy", IsVerified: true}
	id, err := r.InsertGenerated(f.ctx, g)
	must(t, err)
	check(t, id != nil, "inserted")
	dup, err := r.InsertGenerated(f.ctx, g)
	must(t, err)
	check(t, dup == nil, "duplicate returns nil")
	after, err := r.CountVerifiedForTopic(f.ctx, f.topicID)
	must(t, err)
	check(t, after == before+1, "count %d -> %d", before, after)
	bodies, err := r.FindBodiesForTopic(f.ctx, f.topicID, 50)
	must(t, err)
	check(t, len(bodies) == after, "bodies %d", len(bodies))
	graded, err := NewAttempts(f.tx).FindForGrading(f.ctx, *id)
	must(t, err)
	check(t, string(graded.CorrectAnswer) == "[1]", "correct answer stored as %s", graded.CorrectAnswer)
}

func TestRevisions(t *testing.T) {
	f := setup(t)
	r := NewRevisions(f.tx)
	none, err := r.FindState(f.ctx, f.u1, f.topicID)
	must(t, err)
	check(t, none == nil, "no schedule yet")

	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	u := learner.FirstSchedule(yesterday, nil)
	must(t, r.Save(f.ctx, f.u1, f.topicID, u))
	must(t, r.Save(f.ctx, f.u1, f.topicID, u))
	s, err := r.FindState(f.ctx, f.u1, f.topicID)
	must(t, err)
	check(t, s != nil && s.IntervalDays == u.IntervalDays && s.LastReviewedOn != nil && *s.LastReviewedOn == yesterday, "state %+v", s)

	saveMastery(t, f, f.u1, f.topicID)
	due, err := r.FindDue(f.ctx, f.u1, today(), []string{f.subjectID}, 20)
	must(t, err)
	check(t, len(due) == 1 && due[0].TopicID == f.topicID && due[0].SubjectName != nil && due[0].MasteryScore != nil &&
		due[0].DueOn <= today(), "due %+v", due)
	out, err := r.FindDue(f.ctx, f.u1, today(), []string{"00000000-0000-4000-8000-000000000000"}, 20)
	must(t, err)
	check(t, len(out) == 0, "out of scope")
}

func TestCurricula(t *testing.T) {
	f := setup(t)
	r := NewCurricula(f.tx)
	def, err := r.FindDefault(f.ctx)
	must(t, err)
	check(t, def != nil && def.ID == f.curriculumID && !def.IsAIGenerated, "default %+v", def)
	byID, err := r.FindByID(f.ctx, def.ID)
	must(t, err)
	check(t, byID.Slug == DefaultCurriculumSlug, "by id")

	slug := "store-test-" + f.u1[:8]
	c, err := r.Create(f.ctx, curriculum.Resolved{Slug: slug, Name: "Store Test", Subjects: []curriculum.ResolvedSubject{
		{Name: "Alpha", Weight: 2, Topics: []string{"A1", "A2"}},
		{Name: "Beta", Weight: 1, Topics: []string{"B1"}},
	}})
	must(t, err)
	check(t, c.IsAIGenerated && c.Description == nil, "created %+v", c)
	subjects, err := r.FindSubjects(f.ctx, c.ID)
	must(t, err)
	check(t, len(subjects) == 2 && subjects[0].Name == "Alpha" && subjects[0].TopicCount == 2 && subjects[0].Weight == 2 &&
		subjects[1].TopicCount == 1, "subjects %+v", subjects)
	again, err := r.FindBySlug(f.ctx, slug)
	must(t, err)
	check(t, again != nil && again.ID == c.ID, "by slug")
}

func TestAssessments(t *testing.T) {
	f := setup(t)
	r := NewAssessments(f.tx)
	goal := f.goal(t, f.u1)

	topics, err := r.FindAssessableTopics(f.ctx, goal.ID)
	must(t, err)
	check(t, len(topics) > 0 && topics[0].SubjectID == f.subjectID, "assessable %+v", topics)
	one, err := r.FindOneQuestionForTopic(f.ctx, f.u1, f.topicID)
	must(t, err)
	check(t, one != nil, "one question")
	subj, err := r.FindTopicSubjects(f.ctx, []string{f.topicID, f.subjectID})
	must(t, err)
	check(t, len(subj) == 1 && subj[f.topicID].SubjectID == f.subjectID, "topic subjects %+v", subj)

	a, err := r.Create(f.ctx, NewAssessment{UserID: f.u1, GoalID: goal.ID, QuestionCount: 1})
	must(t, err)
	must(t, r.AddQuestions(f.ctx, a.ID, []string{f.questionID}))
	att := f.attempt(t, f.u1, true)
	must(t, r.LinkAttempt(f.ctx, a.ID, f.questionID, att.ID))
	qs, err := r.FindQuestions(f.ctx, a.ID)
	must(t, err)
	check(t, len(qs) == 1 && qs[0].Position == 1 && qs[0].AttemptID != nil && *qs[0].AttemptID == att.ID, "questions %+v", qs)
	bodies, err := r.FindQuestionBodies(f.ctx, []string{f.questionID})
	must(t, err)
	check(t, len(bodies) == 1, "bodies")

	done, err := r.Complete(f.ctx, a.ID, 1)
	must(t, err)
	check(t, done.Status == "completed" && done.CompletedAt != nil && done.Accuracy != nil && *done.Accuracy == 100, "complete %+v", done)
	latest, err := r.FindForGoal(f.ctx, f.u1, goal.ID)
	must(t, err)
	check(t, latest != nil && latest.ID == a.ID, "for goal")
	other, err := r.Find(f.ctx, a.ID, f.u2)
	must(t, err)
	check(t, other == nil, "ownership checked")
	_, err = r.Complete(f.ctx, "00000000-0000-4000-8000-000000000000", 0)
	check(t, IsNotFound(err), "complete on a missing row is an error, got %v", err)

	b, err := r.Create(f.ctx, NewAssessment{UserID: f.u1, GoalID: goal.ID, QuestionCount: 1})
	must(t, err)
	must(t, r.AbandonOpen(f.ctx, f.u1, goal.ID))
	b2, err := r.Find(f.ctx, b.ID, f.u1)
	must(t, err)
	check(t, b2.Status == "abandoned", "abandoned")
}

func TestMockTests(t *testing.T) {
	f := setup(t)
	r := NewMockTests(f.tx)

	counts, err := r.CountQuestionsByTopic(f.ctx, []string{f.topicID})
	must(t, err)
	check(t, counts[f.topicID] > 0, "counts %v", counts)
	ids, err := r.FindQuestionsForTopic(f.ctx, f.u1, f.topicID, 2)
	must(t, err)
	check(t, len(ids) > 0 && len(ids) <= 2, "ids %v", ids)
	details, err := r.FindTopicDetails(f.ctx, []string{f.topicID, f.subjectID})
	must(t, err)
	check(t, details[f.topicID].SubjectName != nil && details[f.subjectID].SubjectName == nil, "details %+v", details)

	m, err := r.CreateTest(f.ctx, NewMockTest{UserID: f.u1, Title: "Mock", Source: "goal", TotalQuestions: 1, DurationMinutes: 10})
	must(t, err)
	must(t, r.AddQuestions(f.ctx, m.ID, []string{f.questionID}))
	open, err := r.FindOpenTest(f.ctx, f.u1)
	must(t, err)
	check(t, open != nil && open.ID == m.ID, "open test")
	must(t, r.MarkQuestions(f.ctx, m.ID, []QuestionMark{{QuestionID: f.questionID, GivenAnswer: json.RawMessage(`{"selectedOptions":[0]}`), IsCorrect: new(true)}}))
	qs, err := r.FindTestQuestions(f.ctx, m.ID)
	must(t, err)
	check(t, len(qs) == 1 && qs[0].SortOrder == 0 && qs[0].IsCorrect != nil && *qs[0].IsCorrect && qs[0].TopicID != nil, "questions %+v", qs)
	sub, err := r.SubmitTest(f.ctx, m.ID, MockTestResult{CorrectCount: 1, ScorePercent: 100, SecondsTaken: 30})
	must(t, err)
	check(t, sub.Status == "submitted" && sub.SubmittedAt != nil && *sub.ScorePercent == 100, "submitted %+v", sub)
	recent, err := r.FindRecent(f.ctx, f.u1, 10)
	must(t, err)
	check(t, len(recent) == 1, "recent")
	mine, err := r.FindTest(f.ctx, m.ID, f.u2)
	must(t, err)
	check(t, mine == nil, "ownership checked")

	_, err = r.CreateTest(f.ctx, NewMockTest{UserID: f.u1, Title: "Mock 2", Source: "goal", TotalQuestions: 1, DurationMinutes: 10})
	must(t, err)
	must(t, r.AbandonOpenTests(f.ctx, f.u1))
	open, err = r.FindOpenTest(f.ctx, f.u1)
	must(t, err)
	check(t, open == nil, "abandoned")
}

func TestPlans(t *testing.T) {
	f := setup(t)
	r := NewPlans(f.tx)
	goal := f.goal(t, f.u1)

	v, err := r.FindLatestVersion(f.ctx, goal.ID)
	must(t, err)
	check(t, v == 0, "no plans yet")
	p, err := r.CreatePlan(f.ctx, NewPlan{UserID: f.u1, GoalID: goal.ID, Version: 1, ExamDate: goal.ExamDate,
		DailyMinutes: 60, DaysRemaining: 30, TotalMinutesPlanned: 60, TopicsCovered: 1, Reason: "initial"})
	must(t, err)
	active, err := r.FindActivePlan(f.ctx, f.u1)
	must(t, err)
	check(t, active != nil && active.ID == p.ID && active.ExamDate == "2099-01-01", "active plan %+v", active)

	past := time.Now().UTC().AddDate(0, 0, -2).Format("2006-01-02")
	n, err := r.CreateSessions(f.ctx, p.ID, []planning.PlannedSession{
		{ScheduledDate: past, TopicID: f.topicID, SortOrder: 0, PlannedMinutes: 20, PlannedQuestions: 5, Kind: planning.KindLearn, Reason: "r"},
		{ScheduledDate: today(), TopicID: f.topicID, SortOrder: 0, PlannedMinutes: 20, PlannedQuestions: 5, Kind: planning.KindRevise, Reason: "r"},
	})
	must(t, err)
	check(t, n == 2, "created %d", n)
	sessions, err := r.FindSessions(f.ctx, p.ID)
	must(t, err)
	check(t, len(sessions) == 2 && sessions[0].ScheduledDate == past && sessions[0].Topics != nil, "sessions %+v", sessions)

	pending, err := r.FindUnreconciledSessions(f.ctx, p.ID, today())
	must(t, err)
	check(t, len(pending) == 1, "unreconciled %+v", pending)
	must(t, r.SettleSessions(f.ctx, []Settlement{{ID: pending[0].ID, Status: planning.StatusCompleted, Answered: 5, Correct: 4}}))
	settled, err := r.FindSettledSessions(f.ctx, p.ID, today())
	must(t, err)
	check(t, len(settled) == 1 && settled[0].Status == "completed" && settled[0].QuestionsCorrect == 4, "settled %+v", settled)

	f.attempt(t, f.u1, true)
	tally, err := r.FindAttemptsByTopicAndDay(f.ctx, f.u1, today(), today())
	must(t, err)
	check(t, tally[f.topicID+"|"+today()] == DayTally{Answered: 1, Correct: 1}, "tally %v", tally)

	saveMastery(t, f, f.u1, f.topicID)
	plannable, err := r.FindPlannableTopics(f.ctx, f.u1, []string{f.subjectID})
	must(t, err)
	check(t, len(plannable) > 0 && plannable[0].SubjectName != nil, "plannable %+v", plannable)
	perf, err := r.FindTopicPerformance(f.ctx, f.u1, []string{f.topicID})
	must(t, err)
	check(t, len(perf) == 1 && perf[0].Name != "Topic" && perf[0].RecentAccuracyPercent != nil, "performance %+v", perf)
	scope, err := r.FindGoalScope(f.ctx, goal.ID)
	must(t, err)
	check(t, len(scope) == 1, "scope")
	planTopics, err := NewMockTests(f.tx).FindPlanTopics(f.ctx, p.ID)
	must(t, err)
	check(t, len(planTopics) == 1 && planTopics[0].PlannedSessions == 2, "plan topics %+v", planTopics)

	must(t, r.SupersedeActive(f.ctx, f.u1))
	gone, err := r.FindActivePlan(f.ctx, f.u1)
	must(t, err)
	check(t, gone == nil, "superseded")
	v, err = r.FindLatestVersion(f.ctx, goal.ID)
	must(t, err)
	check(t, v == 1, "latest version %d", v)
}

func TestGroups(t *testing.T) {
	f := setup(t)
	r := NewGroups(f.tx)
	code := groups.MakeInviteCode(func(n int) []byte { b := make([]byte, n); _, _ = rand.Read(b); return b })

	g, err := r.CreateGroup(f.ctx, NewGroup{Name: "Study pals", InviteCode: code, CreatedBy: f.u1})
	must(t, err)
	must(t, r.AddMember(f.ctx, g.ID, f.u1, "owner"))
	must(t, r.AddMember(f.ctx, g.ID, f.u2, "member"))
	must(t, r.AddMember(f.ctx, g.ID, f.u2, "member")) // idempotent

	mine, err := r.FindMyGroups(f.ctx, f.u2)
	must(t, err)
	check(t, len(mine) == 1 && mine[0].Group.ID == g.ID && mine[0].Role == "member" && mine[0].MemberCount == 2, "my groups %+v", mine)
	byCode, err := r.FindByInviteCode(f.ctx, code)
	must(t, err)
	check(t, byCode != nil && byCode.ID == g.ID, "by code")
	found, err := r.FindGroup(f.ctx, g.ID)
	must(t, err)
	check(t, found != nil, "find group")
	members, err := r.FindMemberIDs(f.ctx, g.ID)
	must(t, err)
	check(t, len(members) == 2 && members[0].UserID == f.u1 && members[0].Role == "owner", "members %+v", members)

	f.attempt(t, f.u1, true)
	must(t, NewLearners(f.tx).RecordAttemptOnProfile(f.ctx, f.u1, true, today(), nil))
	saveMastery(t, f, f.u1, f.topicID)
	progress, err := r.FindSharedProgress(f.ctx, []string{f.u1, f.u2}, 0)
	must(t, err)
	check(t, progress[f.u1].QuestionsAnswered == 1 && progress[f.u1].TopicsMastered == 1 && progress[f.u1].CurrentStreakDays == 1 &&
		progress[f.u2].QuestionsAnswered == 0 && progress[f.u2].DisplayName != "", "progress %+v", progress)

	must(t, r.RemoveMember(f.ctx, g.ID, f.u2))
	in, err := r.IsMember(f.ctx, g.ID, f.u2)
	must(t, err)
	check(t, !in, "removed")
	in, err = r.IsMember(f.ctx, g.ID, f.u1)
	must(t, err)
	check(t, in, "still a member")
}

func TestAI(t *testing.T) {
	f := setup(t)
	r := NewAI(f.tx)

	c, err := r.CreateConversation(f.ctx, f.u1, "Why?", &f.topicID)
	must(t, err)
	_, err = r.AddMessage(f.ctx, NewMessage{ConversationID: c.ID, Role: "learner", Content: "Why?"})
	must(t, err)
	answered, err := r.FindAnsweredIDs(f.ctx, []string{c.ID})
	must(t, err)
	check(t, !answered[c.ID], "not answered yet")
	_, err = r.AddMessage(f.ctx, NewMessage{ConversationID: c.ID, Role: "tutor", Content: "Because.", InputTokens: ptr(10),
		OutputTokens: ptr(5), Model: ptr("test"), Citations: json.RawMessage(`[{"n":1}]`)})
	must(t, err)
	answered, err = r.FindAnsweredIDs(f.ctx, []string{c.ID})
	must(t, err)
	check(t, answered[c.ID], "answered")
	msgs, err := r.ListMessages(f.ctx, c.ID, 50)
	must(t, err)
	check(t, len(msgs) == 2 && msgs[0].Citations == nil && string(msgs[1].Citations) == `[{"n": 1}]`, "messages %+v", msgs)

	list, err := r.ListConversations(f.ctx, f.u1, 30)
	must(t, err)
	check(t, len(list) == 1 && *list[0].TopicID == f.topicID, "conversations %+v", list)
	_, err = r.FindConversation(f.ctx, c.ID, f.u2)
	var he *httpx.Error
	check(t, errors.As(err, &he) && he.Status == 404, "someone else's conversation is a 404, got %v", err)

	f.attempt(t, f.u1, false)
	saveMastery(t, f, f.u1, f.topicID)
	facts, err := r.FindTutorFacts(f.ctx, f.u1, f.topicID)
	must(t, err)
	check(t, facts.TopicName != nil && facts.SubjectName != nil && facts.MasteryScore != nil && facts.AttemptsOnTopic == 1 &&
		facts.RecentAccuracyPercent != nil && *facts.RecentAccuracyPercent == 100 && len(facts.RecentMistakes) == 1, "facts %+v", facts)
	empty, err := r.FindTutorFacts(f.ctx, f.u1, "")
	must(t, err)
	check(t, empty.TopicName == nil && empty.RecentMistakes != nil, "no topic")
}

func TestSources(t *testing.T) {
	f := setup(t)
	r := NewSources(f.tx)

	s, err := r.Create(f.ctx, NewSource{UserID: f.u1, Kind: "pdf", Title: "Notes", StoragePath: ptr(f.u1 + "/notes.pdf")})
	must(t, err)
	must(t, r.InsertChunks(f.ctx, *s, []NewChunk{
		{Modality: "text", Page: ptr(2), Content: "Normalisation removes redundancy from relational tables."},
		{Modality: "text", Page: ptr(1), Content: "A primary key identifies a row."},
	}))
	list, err := r.List(f.ctx, f.u1)
	must(t, err)
	check(t, len(list) == 1 && list[0].ChunkCount == 2, "list %+v", list)
	chunks, err := r.ListChunks(f.ctx, f.u1)
	must(t, err)
	check(t, len(chunks) == 2 && *chunks[0].Page == 1, "chunks in reading order %+v", chunks)

	hits, err := r.Search(f.ctx, f.u1, "normalisation redundancy", 6)
	must(t, err)
	check(t, len(hits) == 1 && hits[0].Rank != nil && *hits[0].Rank > 0, "search %+v", hits)

	must(t, r.TagChunks(f.ctx, []ChunkAssignment{{ChunkID: chunks[0].ID, TopicID: f.topicID}}))
	forTopic, err := r.ChunksForTopic(f.ctx, f.topicID, f.u1, 8)
	must(t, err)
	check(t, len(forTopic) == 1, "chunks for topic")
	got, err := r.FindChunk(f.ctx, chunks[0].ID, f.u1)
	must(t, err)
	check(t, got.TopicID != nil && *got.TopicID == f.topicID, "tagged")
	_, err = r.FindChunk(f.ctx, chunks[0].ID, f.u2)
	var he *httpx.Error
	check(t, errors.As(err, &he) && he.Code == "EXCERPT_NOT_FOUND", "someone else's excerpt is a 404, got %v", err)

	ready, err := r.HasReadySources(f.ctx, f.u1)
	must(t, err)
	check(t, !ready, "processing is not ready")
	must(t, r.SetStatus(f.ctx, s.ID, "ready", nil))
	ready, err = r.HasReadySources(f.ctx, f.u1)
	must(t, err)
	check(t, ready, "ready")
	must(t, r.AttachCurriculum(f.ctx, f.u1, f.curriculumID))
	many, err := r.FindMany(f.ctx, []string{s.ID})
	must(t, err)
	check(t, len(many) == 1 && many[0].CurriculumID != nil, "attached %+v", many)

	must(t, r.SetTopicDetails(f.ctx, f.topicID, []string{"keys"}))
	must(t, r.AddPrerequisites(f.ctx, []Prerequisite{{TopicID: f.topicID, PrerequisiteID: f.subjectID}}))
	must(t, r.AddPrerequisites(f.ctx, []Prerequisite{{TopicID: f.topicID, PrerequisiteID: f.subjectID}}))
	cm, err := r.CourseMap(f.ctx, f.curriculumID, f.u1)
	must(t, err)
	check(t, cm.ExcerptCounts[f.topicID] == 1 && len(cm.Prerequisites) >= 1 && len(cm.Topics) > 0, "course map %+v", cm.ExcerptCounts)

	removed, err := r.Remove(f.ctx, s.ID, f.u1)
	must(t, err)
	check(t, removed.StoragePath != nil, "remove returns the row")
	_, err = r.Find(f.ctx, s.ID, f.u1)
	check(t, errors.As(err, &he) && he.Code == "SOURCE_NOT_FOUND", "gone, got %v", err)
}

func TestFreshQuestionsComeFirst(t *testing.T) {
	f := setup(t)
	var n int
	must(t, f.tx.QueryRow(f.ctx, `select count(*) from questions where topic_id = $1 and is_verified`, f.topicID).Scan(&n))
	if n < 2 {
		t.Skip("topic has one question")
	}
	r := NewMockTests(f.tx)
	// u1 has answered f.questionID: it must never come before a question they have not seen.
	f.attempt(t, f.u1, true)
	for range 5 {
		ids, err := r.FindQuestionsForTopic(f.ctx, f.u1, f.topicID, n)
		must(t, err)
		check(t, len(ids) == n && ids[n-1] == f.questionID, "seen question not last: %v", ids)
	}
	// A question on a paper counts as seen even when it was skipped.
	ids, err := r.FindQuestionsForTopic(f.ctx, f.u2, f.topicID, 1)
	must(t, err)
	m, err := r.CreateTest(f.ctx, NewMockTest{UserID: f.u2, Title: "Mock", Source: "goal", TotalQuestions: 1, DurationMinutes: 10})
	must(t, err)
	must(t, r.AddQuestions(f.ctx, m.ID, ids))
	next, err := NewAssessments(f.tx).FindOneQuestionForTopic(f.ctx, f.u2, f.topicID)
	must(t, err)
	check(t, next != nil && *next != ids[0], "diagnostic repeated the paper's question %v", ids)
}

func TestMistakes(t *testing.T) {
	f := setup(t)
	r := NewMistakes(f.tx)
	first, second := f.attempt(t, f.u1, false), f.attempt(t, f.u1, false)
	must(t, r.Record(f.ctx, f.u1, f.topicID, "Confuses mass with weight", first.ID))
	must(t, r.Record(f.ctx, f.u1, f.topicID, "confuses mass with weight ", second.ID)) // same one, other case
	must(t, r.Record(f.ctx, f.u1, f.topicID, "thinks heavier falls faster", second.ID))
	must(t, r.Record(f.ctx, f.u1, f.topicID, "  ", second.ID)) // nothing to record

	top, err := r.Top(f.ctx, f.u1, nil, 1, 10)
	must(t, err)
	check(t, len(top) == 1 && top[0].Label == "Confuses mass with weight" && top[0].Occurrences == 2 && top[0].TopicName != "",
		"top %+v", top)
	all, err := r.Top(f.ctx, f.u1, []string{f.topicID}, 5, 10)
	must(t, err)
	check(t, len(all) == 2, "per topic %+v", all)
	none, err := r.Top(f.ctx, f.u1, []string{f.subjectID}, 5, 10)
	must(t, err)
	check(t, len(none) == 0, "filtered %+v", none)

	var evidence []string
	must(t, f.tx.QueryRow(f.ctx, `select evidence_attempt_ids::text[] from mistake_patterns
		where user_id = $1 and lower(misconception) = 'confuses mass with weight'`, f.u1).Scan(&evidence))
	check(t, len(evidence) == 2 && evidence[1] == second.ID, "evidence %v", evidence)
	for range maxEvidence + 5 {
		must(t, r.Record(f.ctx, f.u1, f.topicID, "Confuses mass with weight", second.ID))
	}
	must(t, f.tx.QueryRow(f.ctx, `select evidence_attempt_ids::text[] from mistake_patterns
		where user_id = $1 and lower(misconception) = 'confuses mass with weight'`, f.u1).Scan(&evidence))
	check(t, len(evidence) == maxEvidence, "evidence capped at %d, got %d", maxEvidence, len(evidence))

	// RLS: a learner reads their own patterns, nobody else's, and cannot write any.
	f.asUser(t, f.u1, func(q Q) {
		mine, err := NewMistakes(q).Top(f.ctx, f.u1, nil, 5, 10)
		must(t, err)
		check(t, len(mine) == 2, "own patterns %d", len(mine))
	})
	f.asUser(t, f.u2, func(q Q) {
		theirs, err := NewMistakes(q).Top(f.ctx, f.u1, nil, 5, 10)
		must(t, err)
		check(t, len(theirs) == 0, "read another learner's patterns")
	})
	f.asUser(t, f.u1, func(q Q) {
		check(t, NewMistakes(q).Record(f.ctx, f.u1, f.topicID, "self-written", first.ID) != nil, "learner wrote a pattern")
	})
	// The per-option misconceptions would say which options are right: never readable as a learner.
	f.asUser(t, f.u1, func(q Q) {
		_, err := q.Exec(f.ctx, `select option_misconceptions from questions limit 1`)
		check(t, err != nil, "learner read option_misconceptions")
	})
}

func TestTypedQuestionsAndCitations(t *testing.T) {
	f := setup(t)
	r := NewQuestions(f.tx)
	id, err := r.InsertGenerated(f.ctx, GeneratedQuestion{TopicID: f.topicID, QuestionType: "short", Body: "Short store test " + f.u1,
		AcceptedAnswers: []string{"inertia"}, Rubric: "Names inertia.", Explanation: "because", Difficulty: "easy", IsVerified: true})
	must(t, err)
	graded, err := NewAttempts(f.tx).FindManyForGrading(f.ctx, []string{*id, f.questionID})
	must(t, err)
	check(t, len(graded) == 2 && strings.Contains(string(graded[*id].CorrectAnswer), `"answers": ["inertia"]`),
		"short key %s", graded[*id].CorrectAnswer)
	mcq, err := r.InsertGenerated(f.ctx, GeneratedQuestion{TopicID: f.topicID, QuestionType: "mcq", Body: "Mcq store test " + f.u1,
		Options: []string{"a", "b"}, CorrectOptionIndexes: []int{0}, OptionMisconceptions: []string{"", "thinks b"},
		Explanation: "because", Difficulty: "easy"})
	must(t, err)
	unverified, err := NewAttempts(f.tx).FindManyForGrading(f.ctx, []string{*mcq})
	must(t, err)
	check(t, len(unverified) == 0, "an unverified question is not gradable")
	must(t, f.tx.QueryRow(f.ctx, `select count(*) from questions where id = $1 and option_misconceptions[2] = 'thinks b'`, *mcq).Scan(new(int)))

	src, err := NewSources(f.tx).Create(f.ctx, NewSource{UserID: f.u1, Kind: "pdf", Title: "Notes", StoragePath: ptr("test/notes.pdf")})
	must(t, err)
	must(t, NewSources(f.tx).InsertChunks(f.ctx, *src, []NewChunk{{Modality: "text", Page: ptr(4), Content: "Inertia is ..."}}))
	var chunkID string
	must(t, f.tx.QueryRow(f.ctx, `select id from source_chunks where source_id = $1`, src.ID).Scan(&chunkID))
	cited, err := NewSources(f.tx).FindCited(f.ctx, f.u1, []string{chunkID})
	must(t, err)
	c := cited[chunkID]
	check(t, c.SourceTitle == "Notes" && c.Kind == "pdf" && c.Location() == "p. 4", "cited %+v", c)
	other, err := NewSources(f.tx).FindCited(f.ctx, f.u2, []string{chunkID})
	must(t, err)
	check(t, len(other) == 0, "another learner's excerpt was cited")
	check(t, ChunkRow{StartSec: ptr(3725)}.Location() == "1:02:05", "clock")
}
