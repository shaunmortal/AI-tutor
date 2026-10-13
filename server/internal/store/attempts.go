package store

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/shaunmortal/AI-tutor/server/internal/httpx"
)

// QuestionForGrading is a question with the parts only the server may see.
type QuestionForGrading struct {
	ID            string          `db:"id" json:"id"`
	TopicID       string          `db:"topic_id" json:"topic_id"`
	QuestionType  string          `db:"question_type" json:"question_type"`
	CorrectAnswer json.RawMessage `db:"correct_answer" json:"correct_answer"`
	Explanation   *string         `db:"explanation" json:"explanation"`
	Difficulty    string          `db:"difficulty" json:"difficulty"`
	IsVerified    bool            `db:"is_verified" json:"is_verified"`
	// The course excerpt the question was written from, if any.
	SourceChunkID *string `db:"source_chunk_id" json:"source_chunk_id"`
	// Body is what a rubric grader is shown; OptionMisconceptions names why each wrong option
	// tempts a learner ("" for correct options), or is nil.
	Body                 string   `db:"body" json:"-"`
	OptionMisconceptions []string `db:"option_misconceptions" json:"-"`
}

const gradingCols = `id, topic_id, question_type, correct_answer, explanation, difficulty, is_verified, source_chunk_id,
	body, option_misconceptions`

// AttemptRow is one attempt with the question's difficulty embedded, as PostgREST's
// `questions(difficulty)` returned it: Questions is nil when the question could not be read.
type AttemptRow struct {
	IsCorrect   bool           `json:"is_correct"`
	AttemptedAt time.Time      `json:"attempted_at"`
	Questions   *DifficultyRef `json:"questions"`
}

// DifficultyRef is the embedded `questions(difficulty)`.
type DifficultyRef struct {
	Difficulty string `json:"difficulty"`
}

// AttemptOutcome is an attempt's id, topic and result.
type AttemptOutcome struct {
	ID        string `db:"id" json:"id"`
	TopicID   string `db:"topic_id" json:"topic_id"`
	IsCorrect bool   `db:"is_correct" json:"is_correct"`
}

// NewAttempt is one answer to record. topic_id and attempt_number are filled by a trigger.
type NewAttempt struct {
	UserID           string
	QuestionID       string
	IsCorrect        bool
	GivenAnswer      json.RawMessage // nil stores SQL NULL
	TimeTakenSeconds *int
	// practice | assessment | topic_test | mock_test | revision
	Source string
}

// InsertedAttempt is what Insert returns.
type InsertedAttempt struct {
	ID      string `db:"id" json:"id"`
	TopicID string `db:"topic_id" json:"topicId"`
}

// Attempts is attempt storage and the privileged question read grading needs. Every method
// expects Admin, so the user id arguments are load-bearing: nothing else filters.
type Attempts struct{ q Q }

func NewAttempts(q Q) *Attempts { return &Attempts{q} }

// FindForGrading is the question plus its answer. Unverified questions are treated as not
// existing (404 QUESTION_NOT_FOUND).
func (r *Attempts) FindForGrading(ctx context.Context, questionID string) (*QuestionForGrading, error) {
	q, err := rowOf[QuestionForGrading](ctx, r.q, "attempts.findForGrading",
		`select `+gradingCols+` from questions where id = $1`, questionID)
	if err != nil {
		return nil, err
	}
	if q == nil || !q.IsVerified {
		return nil, httpx.E(http.StatusNotFound, "QUESTION_NOT_FOUND", "That question does not exist or is not available yet.")
	}
	return q, nil
}

// FindManyForGrading is FindForGrading for a whole paper, keyed by id. A question that is no
// longer verified is simply absent: the paper skips it rather than failing.
func (r *Attempts) FindManyForGrading(ctx context.Context, questionIDs []string) (map[string]*QuestionForGrading, error) {
	out := map[string]*QuestionForGrading{}
	if len(questionIDs) == 0 {
		return out, nil
	}
	rows, err := rowsOf[QuestionForGrading](ctx, r.q, "attempts.findManyForGrading",
		`select `+gradingCols+` from questions where id = any($1::uuid[]) and is_verified`, questionIDs)
	for i := range rows {
		out[rows[i].ID] = &rows[i]
	}
	return out, err
}

// FindByIDs loads attempts by id, for a paper that recorded which attempt answered what.
func (r *Attempts) FindByIDs(ctx context.Context, attemptIDs []string) ([]AttemptOutcome, error) {
	if len(attemptIDs) == 0 {
		return []AttemptOutcome{}, nil
	}
	return rowsOf[AttemptOutcome](ctx, r.q, "attempts.findByIds",
		`select id, topic_id, is_correct from question_attempts where id = any($1::uuid[])`, attemptIDs)
}

func (r *Attempts) Insert(ctx context.Context, a NewAttempt) (*InsertedAttempt, error) {
	return single[InsertedAttempt](ctx, r.q, "attempts.insert",
		`insert into question_attempts (user_id, question_id, is_correct, given_answer, time_taken_seconds, source)
		 values ($1, $2, $3, $4, $5, $6) returning id, topic_id`,
		a.UserID, a.QuestionID, a.IsCorrect, a.GivenAnswer, a.TimeTakenSeconds, a.Source)
}

// FindTopicAttempts is every attempt on one topic, newest first, with the question's
// difficulty. Mastery is recomputed from this rather than nudged.
func (r *Attempts) FindTopicAttempts(ctx context.Context, userID, topicID string) ([]AttemptRow, error) {
	// ponytail: a safety ceiling of 1000, not a real limit. Aggregate in SQL if it is ever hit.
	type flat struct {
		IsCorrect   bool      `db:"is_correct"`
		AttemptedAt time.Time `db:"attempted_at"`
		Difficulty  *string   `db:"difficulty"`
	}
	rows, err := rowsOf[flat](ctx, r.q, "attempts.findTopicAttempts",
		`select a.is_correct, a.attempted_at, q.difficulty
		 from question_attempts a left join questions q on q.id = a.question_id
		 where a.user_id = $1 and a.topic_id = $2
		 order by a.attempted_at desc limit 1000`, userID, topicID)
	if err != nil {
		return nil, err
	}
	out := make([]AttemptRow, len(rows))
	for i, row := range rows {
		out[i] = AttemptRow{IsCorrect: row.IsCorrect, AttemptedAt: row.AttemptedAt}
		if row.Difficulty != nil {
			out[i].Questions = &DifficultyRef{*row.Difficulty}
		}
	}
	return out, nil
}
