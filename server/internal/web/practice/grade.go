package practice

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/shaunmortal/AI-tutor/server/internal/ai"
	"github.com/shaunmortal/AI-tutor/server/internal/app"
	"github.com/shaunmortal/AI-tutor/server/internal/assessment"
	"github.com/shaunmortal/AI-tutor/server/internal/dates"
	"github.com/shaunmortal/AI-tutor/server/internal/goals"
	"github.com/shaunmortal/AI-tutor/server/internal/store"
)

// One grading path and one recording path, shared by practice, the diagnostic and mock tests,
// so a paper moves the learner model exactly as the same answers given in practice would.

// verdict is the marking of one answer.
type verdict struct {
	IsCorrect bool
	// Feedback is the rubric grader's note on a typed answer; nil otherwise.
	Feedback *string
	// Misconception is what a wrong answer showed: the chosen option's distractor rationale, or
	// the rubric grader's diagnosis. Nil when the answer was right or showed nothing specific.
	Misconception *string
}

// rubricGrade is the model's mark for a typed answer (ai.ShortAnswerGradeSchema).
type rubricGrade struct {
	Correct       bool    `json:"correct"`
	Feedback      string  `json:"feedback"`
	Misconception *string `json:"misconception"`
}

// rubricGrader marks a typed answer against the key's rubric. A variable so tests can stand in
// for the model; nothing else reassigns it.
var rubricGrader = func(ctx context.Context, client *ai.Client, body string, key assessment.ShortKey, answer string) (rubricGrade, error) {
	if client == nil {
		return rubricGrade{}, errAIUnavailable
	}
	var g rubricGrade
	_, err := client.JSON(ctx, ai.Request{System: ai.ShortAnswerGraderSystem, Schema: ai.ShortAnswerGradeSchema,
		Prompt: ai.ShortAnswerGradePrompt(body, key.Answers, key.Rubric, answer), MaxTokens: 1500, Quality: ai.Standard}, &g)
	return g, err
}

// grade marks one answer. Choice and numeric answers are decided here; a typed answer that
// matches no accepted wording goes to the rubric grader.
func grade(ctx context.Context, client *ai.Client, q *store.QuestionForGrading, a givenAnswer) (verdict, error) {
	ok, err := assessment.GradeAnswer(gradable(q), a.given())
	if errors.Is(err, assessment.ErrNeedsJudgement) {
		key, _ := assessment.ParseShortKey(q.CorrectAnswer) // GradeAnswer has checked its shape
		g, err := rubricGrader(ctx, client, q.Body, key, *a.Text)
		if err != nil {
			return verdict{}, err
		}
		v := verdict{IsCorrect: g.Correct, Feedback: clip(g.Feedback, 1000)}
		if !g.Correct && g.Misconception != nil {
			v.Misconception = clip(*g.Misconception, 300)
		}
		return v, nil
	}
	if err != nil {
		return verdict{}, err
	}
	v := verdict{IsCorrect: ok}
	if !ok {
		v.Misconception = optionMisconception(q, a.Options)
	}
	return v, nil
}

// optionMisconception is the misconception behind the first wrong option chosen, if the question
// names one.
func optionMisconception(q *store.QuestionForGrading, chosen []int) *string {
	var key []int
	_ = json.Unmarshal(q.CorrectAnswer, &key)
	for _, i := range chosen {
		if i >= 0 && i < len(q.OptionMisconceptions) && !slices.Contains(key, i) {
			if m := clip(q.OptionMisconceptions[i], 300); m != nil {
				return m
			}
		}
	}
	return nil
}

// clip trims s and caps it at n characters; nil when nothing is left.
func clip(s string, n int) *string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	if utf8.RuneCountInString(s) > n {
		s = string([]rune(s)[:n])
	}
	return &s
}

// paperMark is one question of a paper after marking.
type paperMark struct {
	answer *givenAnswer
	// marked is false for a question that was answered but cannot be marked: it was withdrawn
	// (no longer verified) or its typed answer could not be graded. Left out of the score.
	marked bool
	verdict
}

// rubricConcurrency bounds the model calls one paper makes at once.
const rubricConcurrency = 4

// markPaper marks every answered question of a paper. It never fails because of one question:
// what can be graded is graded, and the rest is left unmarked, so a finished paper is never lost.
func markPaper(ctx context.Context, d *app.Deps, questionIDs []string, given map[string]*givenAnswer) (map[string]*paperMark, error) {
	var answered []string
	for _, id := range questionIDs {
		if given[id] != nil {
			answered = append(answered, id)
		}
	}
	// The answer key is read here, server side, for the first time in this paper's life.
	keys, err := store.NewAttempts(d.DB.Admin).FindManyForGrading(ctx, answered)
	if err != nil {
		return nil, err
	}
	marks := make(map[string]*paperMark, len(answered))
	var wg sync.WaitGroup
	var mu sync.Mutex
	slots := make(chan struct{}, rubricConcurrency)
	for _, id := range answered {
		m := &paperMark{answer: given[id]}
		marks[id] = m
		q := keys[id]
		if q == nil {
			slog.Warn("paper question can no longer be marked", "question", id)
			continue
		}
		wg.Go(func() {
			slots <- struct{}{}
			defer func() { <-slots }()
			v, err := grade(ctx, d.AI, q, *m.answer)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				m.marked, m.verdict = true, v
			case q.QuestionType == string(assessment.Short):
				// Nobody to grade it right now: unmarked rather than wrong.
				slog.Warn("typed answer could not be graded", "question", id, "err", err)
			default:
				// A malformed answer (a number for a choice question) is wrong, as before.
				m.marked = true
			}
		})
	}
	wg.Wait()
	return marks, nil
}

// graded is one marked answer, ready to record.
type graded struct {
	questionID string
	given      json.RawMessage
	seconds    *int
	verdict
}

// recordAnswers writes graded answers and moves the learner model the way one practice answer
// does: attempt rows, each touched topic's mastery recomputed from the log, its revision
// schedule, the profile's counters and habits, and any misconception an answer showed. It runs
// on q, so a caller passing a transaction gets all of it or none.
func recordAnswers(ctx context.Context, q store.Q, userID, source string, answers []graded, daysUntilExam *int) (
	[]*store.InsertedAttempt, map[string]recomputed, error) {
	learners := store.NewLearners(q)
	// The learner's own calendar day, read once so streak, schedule and grading agree.
	timezone := ""
	if tz, err := learners.FindTimezone(ctx, userID); err != nil {
		return nil, nil, err
	} else if tz != nil {
		timezone = *tz
	}
	today := dates.TodayIn(timezone, time.Now())

	attempts, mistakes := store.NewAttempts(q), store.NewMistakes(q)
	inserted := make([]*store.InsertedAttempt, len(answers))
	var topics []string
	correct := 0
	for i, a := range answers {
		at, err := attempts.Insert(ctx, store.NewAttempt{UserID: userID, QuestionID: a.questionID, IsCorrect: a.IsCorrect,
			GivenAnswer: a.given, TimeTakenSeconds: a.seconds, Source: source})
		if err != nil {
			return nil, nil, err
		}
		inserted[i] = at
		if !slices.Contains(topics, at.TopicID) {
			topics = append(topics, at.TopicID)
		}
		if a.IsCorrect {
			correct++
		}
		if a.Misconception != nil {
			if err := mistakes.Record(ctx, userID, at.TopicID, *a.Misconception, at.ID); err != nil {
				return nil, nil, err
			}
		}
	}

	recs := make(map[string]recomputed, len(topics))
	for _, topicID := range topics {
		rec, err := recomputeMastery(ctx, q, userID, topicID, today, timezone)
		if err != nil {
			return nil, nil, err
		}
		recs[topicID] = rec
		updateRevisionSchedule(ctx, q, userID, topicID, today, rec.answeredToday, rec.correctToday, daysUntilExam)
	}

	// Time per answer is known for a single practice answer; a paper reports only its total.
	var secs *float64
	if len(answers) == 1 && answers[0].seconds != nil {
		s := float64(*answers[0].seconds)
		secs = &s
	}
	if err := learners.RecordAnswersOnProfile(ctx, userID, len(answers), correct, today, secs); err != nil {
		return nil, nil, err
	}
	return inserted, recs, nil
}

// examDays is how many days the learner's active goal has left, read as the learner. It caps
// how far out a review can go; no goal is a normal state (nil).
func examDays(ctx context.Context, d *app.Deps, userID string) (*int, error) {
	var days *int
	err := d.DB.AsUser(ctx, userID, func(tx pgx.Tx) error {
		g, err := goals.New(store.NewGoals(tx)).GetActive(ctx, userID)
		if g != nil {
			days = &g.DaysRemaining
		}
		return err
	})
	return days, err
}

// inTx runs fn in one transaction on the elevated pool.
func inTx(ctx context.Context, d *app.Deps, fn func(tx pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, d.DB.Admin, fn)
}

// softly runs fn so that its failure cannot take the caller down with it: inside a transaction
// it gets a savepoint (a failed statement would otherwise abort the whole transaction).
func softly(ctx context.Context, q store.Q, fn func(store.Q) error) error {
	tx, ok := q.(pgx.Tx)
	if !ok {
		return fn(q)
	}
	return pgx.BeginFunc(ctx, tx, func(sp pgx.Tx) error { return fn(sp) })
}
