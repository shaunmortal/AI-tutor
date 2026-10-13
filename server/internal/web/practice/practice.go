// Package practice serves answering questions and everything that grades them: single
// attempts, the next-session recommendation (and the question bank behind it), AI question
// generation, the diagnostic assessment and mock tests.
//
// Reads of the learner's own rows run AsUser (RLS applies); grading against the answer key and
// writes to tables learners only hold SELECT on run on Admin.
package practice

import (
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/shaunmortal/AI-tutor/server/internal/app"
	"github.com/shaunmortal/AI-tutor/server/internal/assessment"
	"github.com/shaunmortal/AI-tutor/server/internal/auth"
	"github.com/shaunmortal/AI-tutor/server/internal/dates"
	"github.com/shaunmortal/AI-tutor/server/internal/httpx"
	"github.com/shaunmortal/AI-tutor/server/internal/learner"
	"github.com/shaunmortal/AI-tutor/server/internal/ratelimit"
	"github.com/shaunmortal/AI-tutor/server/internal/store"
)

type route struct {
	pattern string
	h       httpx.Handler
	mw      []func(http.Handler) http.Handler
}

// routes is split from Register so tests can mount the same handlers without token checks.
func routes(d *app.Deps) []route {
	limit := func(name string, max int64, msg string) []func(http.Handler) http.Handler {
		return []func(http.Handler) http.Handler{ratelimit.Limit(d.Redis, name, max, time.Hour, msg)}
	}
	return []route{
		{"POST /attempts", submitAttempt(d), nil},
		{"GET /recommendations/next", nextRecommendation(d), nil},
		// Every call spends money: one generation request plus one verification per question.
		{"POST /questions/generate", generateQuestions(d),
			limit("questions-generate", 10, "You have reached the hourly limit for generating questions.")},
		{"GET /assessments/diagnostic", getDiagnostic(d), nil},
		{"POST /assessments/diagnostic", startDiagnostic(d),
			limit("assessments-start", 10, "You have started a lot of assessments in the last hour.")},
		{"GET /assessments/{id}", getAssessment(d), nil},
		{"POST /assessments/{id}/submit", submitAssessment(d), nil},
		{"GET /mock-tests", listTests(d), nil},
		{"POST /mock-tests", generateTest(d),
			limit("mock-tests-generate", 20, "You have generated a lot of tests in the last hour.")},
		{"GET /mock-tests/{id}", getTest(d), nil},
		{"POST /mock-tests/{id}/submit", submitTest(d), nil},
		{"GET /misconceptions", listMisconceptions(d), nil},
	}
}

func Register(d *app.Deps) {
	for _, rt := range routes(d) {
		d.Authed(rt.pattern, rt.h, rt.mw...)
	}
}

// --- POST /attempts ---

type attemptResult struct {
	AttemptID     string          `json:"attemptId"`
	IsCorrect     bool            `json:"isCorrect"`
	CorrectAnswer json.RawMessage `json:"correctAnswer"` // released only now the answer is committed
	Explanation   *string         `json:"explanation"`
	SourceChunkID *string         `json:"sourceChunkId"`
	// Feedback is the rubric grader's note on a typed answer; null for choice and numeric ones.
	Feedback *string `json:"feedback"`
	// Misconception is what a wrong answer showed, when it showed something specific.
	Misconception *string       `json:"misconception"`
	Mastery       masteryChange `json:"mastery"`
}

type masteryChange struct {
	TopicID         string   `json:"topicId"`
	Before          *float64 `json:"before"`
	After           float64  `json:"after"`
	Change          *float64 `json:"change"`
	AttemptsOnTopic int      `json:"attemptsOnTopic"`
}

// submitAttempt is grade -> record -> recompute mastery -> move the revision schedule.
// Elevated client throughout (grading reads the answer key; attempts and mastery are closed to
// learners); every query filters by the token's user id. The goal is read as the learner.
func submitAttempt(d *app.Deps) httpx.Handler {
	return func(w http.ResponseWriter, r *http.Request) error {
		userID := auth.UserID(r)
		in, err := parseAttempt(w, r)
		if err != nil {
			return err
		}
		ctx := r.Context()
		daysUntilExam, err := examDays(ctx, d, userID)
		if err != nil {
			return err
		}

		admin := d.DB.Admin
		question, err := store.NewAttempts(admin).FindForGrading(ctx, in.QuestionID)
		if err != nil {
			return err
		}
		// Graded before anything is written (a typed answer may need a model call), so a malformed
		// question leaves no half-recorded attempt.
		v, err := grade(ctx, d.AI, question, in.Answer)
		if err != nil {
			return err
		}
		before, err := store.NewMastery(admin).FindScore(ctx, userID, question.TopicID)
		if err != nil {
			return err
		}
		var attempt *store.InsertedAttempt
		var rec recomputed
		if err := inTx(ctx, d, func(tx pgx.Tx) error {
			inserted, recs, err := recordAnswers(ctx, tx, userID, in.Source, []graded{{questionID: in.QuestionID,
				given: in.Answer.json(), seconds: in.TimeTakenSeconds, verdict: v}}, daysUntilExam)
			if err == nil {
				attempt, rec = inserted[0], recs[inserted[0].TopicID]
			}
			return err
		}); err != nil {
			return err
		}

		res := attemptResult{AttemptID: attempt.ID, IsCorrect: v.IsCorrect, CorrectAnswer: question.CorrectAnswer,
			Explanation: question.Explanation, SourceChunkID: question.SourceChunkID, Feedback: v.Feedback,
			Misconception: v.Misconception,
			Mastery:       masteryChange{TopicID: attempt.TopicID, Before: before, After: rec.score, AttemptsOnTopic: rec.total}}
		if before != nil {
			change := jsRound((rec.score-*before)*100) / 100
			res.Mastery.Change = &change
		}
		httpx.OK(w, http.StatusCreated, res)
		return nil
	}
}

func gradable(q *store.QuestionForGrading) assessment.GradableQuestion {
	return assessment.GradableQuestion{QuestionType: assessment.QuestionType(q.QuestionType), CorrectAnswer: []byte(q.CorrectAnswer)}
}

type recomputed struct {
	score                       float64
	total                       int
	answeredToday, correctToday int
}

// recomputeMastery rebuilds one topic's mastery row from the attempt log. "Today" counts are
// in the learner's timezone; callers that do not need them pass "" for both.
func recomputeMastery(ctx context.Context, q store.Q, userID, topicID, today, timezone string) (recomputed, error) {
	rows, err := store.NewAttempts(q).FindTopicAttempts(ctx, userID, topicID)
	if err != nil {
		return recomputed{}, err
	}
	var out recomputed
	records := make([]learner.AttemptRecord, len(rows))
	var lastCorrect *time.Time
	for i, row := range rows {
		difficulty := "medium"
		if row.Questions != nil {
			difficulty = row.Questions.Difficulty
		}
		records[i] = learner.AttemptRecord{IsCorrect: row.IsCorrect, Difficulty: difficulty, AttemptedAt: row.AttemptedAt}
		if row.IsCorrect && lastCorrect == nil {
			lastCorrect = &rows[i].AttemptedAt
		}
		if today != "" && dates.TodayIn(timezone, row.AttemptedAt) == today {
			out.answeredToday++
			if row.IsCorrect {
				out.correctToday++
			}
		}
	}
	evidence := learner.SummariseAttempts(records, time.Now())
	mastery := learner.ComputeMastery(evidence)
	lastAttempt := time.Now()
	if len(rows) > 0 {
		lastAttempt = rows[0].AttemptedAt
	}
	if err := store.NewMastery(q).Save(ctx, userID, topicID, evidence, mastery, lastAttempt, lastCorrect); err != nil {
		return recomputed{}, err
	}
	out.score, out.total = mastery.Score, evidence.TotalAttempts
	return out, nil
}

// updateRevisionSchedule moves the topic in the spaced-repetition schedule, graded on today's
// answers: once per topic per day, and within a day the step may only collapse, never improve.
// Never fails the submission — a schedule that did not move is a topic that returns a bit late.
func updateRevisionSchedule(ctx context.Context, q store.Q, userID, topicID, today string,
	answeredToday, correctToday int, daysUntilExam *int) {
	err := softly(ctx, q, func(q store.Q) error {
		rev := store.NewRevisions(q)
		state, err := rev.FindState(ctx, userID, topicID)
		if err != nil {
			return err
		}
		accuracy := 0.0
		if answeredToday > 0 {
			accuracy = float64(correctToday) / float64(answeredToday) * 100
		}
		outcome := learner.ReviewOutcome{ReviewedOn: today, AccuracyPercent: accuracy,
			QuestionsAnswered: answeredToday, DaysUntilExam: daysUntilExam}

		if state != nil && state.LastReviewedOn != nil && *state.LastReviewedOn == today {
			// Already settled today: only a session that has turned into a failure may collapse it.
			if accuracy >= learner.LapseAccuracyPercent || state.IntervalDays <= 1 {
				return nil
			}
			return rev.Save(ctx, userID, topicID, learner.NextReview(state, outcome))
		}
		// No schedule and only today's first answer: being learned, not reviewed.
		if state == nil && answeredToday <= 1 {
			return rev.Save(ctx, userID, topicID, learner.FirstSchedule(today, daysUntilExam))
		}
		return rev.Save(ctx, userID, topicID, learner.NextReview(state, outcome))
	})
	if err != nil {
		slog.Warn("could not update the revision schedule", "topic", topicID, "err", err)
	}
}

func (a givenAnswer) given() assessment.GivenAnswer {
	return assessment.GivenAnswer{SelectedOptions: a.Options, Value: a.Value, Text: a.Text}
}

// jsRound is JavaScript's Math.round (halves towards +Infinity).
func jsRound(x float64) float64 { return math.Floor(x + 0.5) }
