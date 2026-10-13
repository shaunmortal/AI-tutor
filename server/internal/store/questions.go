package store

import (
	"context"

	"github.com/shaunmortal/AI-tutor/server/internal/adaptive"
)

// practiceCols are the learner-visible columns. `authenticated` has no privilege on
// correct_answer or explanation, so these are all a user-scoped Q can ask for.
const practiceCols = `id, topic_id, question_type, body, options, difficulty, marks`

// TopicWithSubject is a topic and its subject's name (its own name for a subject).
type TopicWithSubject struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	SubjectName string `json:"subjectName"`
}

// GeneratedQuestion is an AI-written question to store.
type GeneratedQuestion struct {
	TopicID              string
	QuestionType         string // mcq | msq | numeric | short
	Body                 string
	Options              []string // nil for numeric and short
	CorrectOptionIndexes []int    // mcq | msq
	// OptionMisconceptions is aligned with Options ("" for correct options), or nil.
	OptionMisconceptions []string
	NumericValue         float64 // numeric
	NumericTolerance     float64
	AcceptedAnswers      []string // short
	Rubric               string
	Explanation          string
	Difficulty           string // easy | medium | hard
	IsVerified           bool
	CreatedBy            *string
	SourceChunkID        *string
}

// Questions reads the question bank. It satisfies adaptive.QuestionBank.
type Questions struct{ q Q }

func NewQuestions(q Q) *Questions { return &Questions{q} }

var _ adaptive.QuestionBank = (*Questions)(nil)

// FindAttemptedIDs is the question ids this learner has attempted on a topic, most recently
// answered first, each once.
func (r *Questions) FindAttemptedIDs(ctx context.Context, userID, topicID string) ([]string, error) {
	ids, err := colOf[string](ctx, r.q, "questions.findAttemptedIds",
		`select question_id::text from question_attempts where user_id = $1 and topic_id = $2
		 order by attempted_at desc`, userID, topicID)
	if err != nil {
		return nil, err
	}
	return distinct(ids), nil
}

// FindForPractice is questions on a topic, optionally at one difficulty, excluding and/or
// restricted to given ids. A non-nil empty Only matches nothing.
func (r *Questions) FindForPractice(ctx context.Context, o adaptive.PracticeQuery) ([]adaptive.PracticeQuestion, error) {
	if o.Only != nil && len(o.Only) == 0 {
		return []adaptive.PracticeQuestion{}, nil
	}
	sql := `select ` + practiceCols + ` from questions where topic_id = $1
		and ($2 = '' or difficulty = $2)
		and not (id = any($3::uuid[]))
		and ($4::uuid[] is null or id = any($4))
		limit $5`
	return rowsOf[adaptive.PracticeQuestion](ctx, r.q, "questions.findForPractice", sql,
		o.TopicID, string(o.Difficulty), nonNil(o.Exclude), o.Only, o.Limit)
}

// FindTopicWithSubject is a topic with its subject's name, or nil when it does not exist.
func (r *Questions) FindTopicWithSubject(ctx context.Context, topicID string) (*TopicWithSubject, error) {
	type row struct {
		ID         string  `db:"id"`
		Name       string  `db:"name"`
		ParentName *string `db:"parent_name"`
	}
	t, err := rowOf[row](ctx, r.q, "questions.findTopicWithSubject",
		`select t.id, t.name, p.name as parent_name from topics t left join topics p on p.id = t.parent_id
		 where t.id = $1`, topicID)
	if err != nil || t == nil {
		return nil, err
	}
	// A subject has no parent, so it is its own context.
	subject := t.Name
	if t.ParentName != nil {
		subject = *t.ParentName
	}
	return &TopicWithSubject{ID: t.ID, Name: t.Name, SubjectName: subject}, nil
}

// FindBodiesForTopic is existing question texts on a topic (TS default limit 50).
func (r *Questions) FindBodiesForTopic(ctx context.Context, topicID string, limit int) ([]string, error) {
	return colOf[string](ctx, r.q, "questions.findBodiesForTopic",
		`select body from questions where topic_id = $1 limit $2`, topicID, limit)
}

// CountVerifiedForTopic is how many questions on this topic a learner could be shown.
func (r *Questions) CountVerifiedForTopic(ctx context.Context, topicID string) (int, error) {
	n, err := colOf[int](ctx, r.q, "questions.countVerifiedForTopic",
		`select count(*)::int from questions where topic_id = $1 and is_verified`, topicID)
	if err != nil {
		return 0, err
	}
	return n[0], nil
}

// InsertGenerated stores one generated question and returns its id, or nil when an identical
// question already exists on the topic. `on conflict do nothing` answers the duplicate case the
// TS caught as error 23505, without aborting a surrounding transaction.
func (r *Questions) InsertGenerated(ctx context.Context, g GeneratedQuestion) (*string, error) {
	marks := 2
	if g.Difficulty == "easy" {
		marks = 1
	}
	// The answer key's shape follows the type (003_questions.sql, 025_short_answer_questions.sql).
	var key any = nonNil(g.CorrectOptionIndexes)
	options := g.Options
	switch g.QuestionType {
	case "numeric":
		key, options = map[string]float64{"value": g.NumericValue, "tol": g.NumericTolerance}, nil
	case "short":
		key, options = map[string]any{"answers": g.AcceptedAnswers, "rubric": g.Rubric}, nil
	}
	ids, err := colOf[string](ctx, r.q, "questions.insertGenerated",
		`insert into questions (topic_id, question_type, body, options, correct_answer, explanation, difficulty,
		   marks, is_ai_generated, is_verified, created_by, source_chunk_id, option_misconceptions)
		 values ($1, $2, $3, $4, $5, $6, $7, $8, true, $9, $10, $11, $12)
		 on conflict do nothing returning id::text`,
		g.TopicID, g.QuestionType, g.Body, options, key, g.Explanation, g.Difficulty,
		marks, g.IsVerified, g.CreatedBy, g.SourceChunkID, g.OptionMisconceptions)
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	return &ids[0], nil
}

// nonNil turns a nil slice into an empty one, so it encodes as [] / '{}' rather than NULL.
func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
