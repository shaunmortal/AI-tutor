package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/shaunmortal/AI-tutor/server/internal/adaptive"
)

// MockTestRow is one mock test.
type MockTestRow struct {
	ID              string     `db:"id" json:"id"`
	PlanID          *string    `db:"plan_id" json:"plan_id"`
	Title           string     `db:"title" json:"title"`
	Source          string     `db:"source" json:"source"`
	Status          string     `db:"status" json:"status"`
	TotalQuestions  int        `db:"total_questions" json:"total_questions"`
	DurationMinutes int        `db:"duration_minutes" json:"duration_minutes"`
	CorrectCount    *int       `db:"correct_count" json:"correct_count"`
	ScorePercent    *float64   `db:"score_percent" json:"score_percent"`
	SecondsTaken    *int       `db:"seconds_taken" json:"seconds_taken"`
	StartedAt       time.Time  `db:"started_at" json:"started_at"`
	SubmittedAt     *time.Time `db:"submitted_at" json:"submitted_at"`
}

// MockTestQuestionRow is a question inside a test with what was answered. Never the answer key.
type MockTestQuestionRow struct {
	QuestionID  string          `db:"question_id" json:"question_id"`
	TopicID     *string         `db:"topic_id" json:"topic_id"`
	SortOrder   int             `db:"sort_order" json:"sort_order"`
	GivenAnswer json.RawMessage `db:"given_answer" json:"given_answer"`
	IsCorrect   *bool           `db:"is_correct" json:"is_correct"`
}

// PlanTopicCount is a topic the plan scheduled and how many sessions it got.
type PlanTopicCount struct {
	TopicID         string `json:"topicId"`
	PlannedSessions int    `json:"plannedSessions"`
}

// TopicDetail is a topic's name, subject and weight.
type TopicDetail struct {
	Name        string  `json:"name"`
	SubjectName *string `json:"subjectName"`
	Weight      float64 `json:"weight"`
}

// NewMockTest is a test to create.
type NewMockTest struct {
	UserID          string
	PlanID          *string
	Title           string
	Source          string // plan | goal
	TotalQuestions  int
	DurationMinutes int
}

// QuestionMark is the marking of one question.
type QuestionMark struct {
	QuestionID  string
	GivenAnswer json.RawMessage
	IsCorrect   bool
}

// MockTestResult is what SubmitTest records.
type MockTestResult struct {
	CorrectCount int
	ScorePercent float64
	SecondsTaken int
}

const mockTestCols = `id, plan_id, title, source, status, total_questions, duration_minutes, correct_count,
	score_percent, seconds_taken, started_at, submitted_at`

// MockTests holds mock tests and their questions. Writes need Admin: is_correct on a question
// row is the marking, so a learner able to write it could mark their own paper.
type MockTests struct{ q Q }

func NewMockTests(q Q) *MockTests { return &MockTests{q} }

// FindRecent is the learner's tests, newest first (TS default limit 10).
func (r *MockTests) FindRecent(ctx context.Context, userID string, limit int) ([]MockTestRow, error) {
	return rowsOf[MockTestRow](ctx, r.q, "mockTests.findRecent",
		`select `+mockTestCols+` from mock_tests where user_id = $1 order by started_at desc limit $2`, userID, limit)
}

// FindTest loads a test the learner owns.
func (r *MockTests) FindTest(ctx context.Context, testID, userID string) (*MockTestRow, error) {
	return rowOf[MockTestRow](ctx, r.q, "mockTests.findTest",
		`select `+mockTestCols+` from mock_tests where id = $1 and user_id = $2`, testID, userID)
}

// FindOpenTest is the newest unfinished test, if any.
func (r *MockTests) FindOpenTest(ctx context.Context, userID string) (*MockTestRow, error) {
	return rowOf[MockTestRow](ctx, r.q, "mockTests.findOpenTest",
		`select `+mockTestCols+` from mock_tests where user_id = $1 and status = 'in_progress'
		 order by started_at desc limit 1`, userID)
}

func (r *MockTests) FindTestQuestions(ctx context.Context, testID string) ([]MockTestQuestionRow, error) {
	return rowsOf[MockTestQuestionRow](ctx, r.q, "mockTests.findTestQuestions",
		`select question_id, topic_id, sort_order, given_answer, is_correct from mock_test_questions
		 where test_id = $1 order by sort_order`, testID)
}

// FindQuestionBodies is the questions without answers or explanations.
func (r *MockTests) FindQuestionBodies(ctx context.Context, questionIDs []string) ([]adaptive.PracticeQuestion, error) {
	return questionBodies(ctx, r.q, "mockTests.findQuestionBodies", questionIDs)
}

// FindQuestionsForTopic is verified question ids on a topic.
func (r *MockTests) FindQuestionsForTopic(ctx context.Context, topicID string, limit int) ([]string, error) {
	return colOf[string](ctx, r.q, "mockTests.findQuestionsForTopic",
		`select id::text from questions where topic_id = $1 and is_verified limit $2`, topicID, limit)
}

// CountQuestionsByTopic is how many verified questions each topic has (absent means none).
func (r *MockTests) CountQuestionsByTopic(ctx context.Context, topicIDs []string) (map[string]int, error) {
	counts := map[string]int{}
	if len(topicIDs) == 0 {
		return counts, nil
	}
	ids, err := colOf[string](ctx, r.q, "mockTests.countQuestionsByTopic",
		`select topic_id::text from questions where topic_id = any($1::uuid[]) and is_verified`, topicIDs)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		counts[id]++
	}
	return counts, nil
}

// FindPlanTopics is the topics the plan scheduled, with how many sessions each (pending and
// past alike), in first-seen order.
func (r *MockTests) FindPlanTopics(ctx context.Context, planID string) ([]PlanTopicCount, error) {
	ids, err := colOf[string](ctx, r.q, "mockTests.findPlanTopics",
		`select topic_id::text from study_sessions where plan_id = $1`, planID)
	if err != nil {
		return nil, err
	}
	out := []PlanTopicCount{}
	index := map[string]int{}
	for _, id := range ids {
		i, ok := index[id]
		if !ok {
			i = len(out)
			index[id] = i
			out = append(out, PlanTopicCount{TopicID: id})
		}
		out[i].PlannedSessions++
	}
	return out, nil
}

// FindTopicDetails is names, subjects and weights for a set of topics.
func (r *MockTests) FindTopicDetails(ctx context.Context, topicIDs []string) (map[string]TopicDetail, error) {
	details := map[string]TopicDetail{}
	if len(topicIDs) == 0 {
		return details, nil
	}
	type row struct {
		ID          string  `db:"id"`
		Name        string  `db:"name"`
		Weight      float64 `db:"weight"`
		SubjectName *string `db:"subject_name"`
	}
	rows, err := rowsOf[row](ctx, r.q, "mockTests.findTopicDetails",
		`select t.id, t.name, t.weight, p.name as subject_name from topics t left join topics p on p.id = t.parent_id
		 where t.id = any($1::uuid[])`, topicIDs)
	if err != nil {
		return nil, err
	}
	for _, t := range rows {
		details[t.ID] = TopicDetail{Name: t.Name, SubjectName: t.SubjectName, Weight: t.Weight}
	}
	return details, nil
}

func (r *MockTests) CreateTest(ctx context.Context, t NewMockTest) (*MockTestRow, error) {
	return single[MockTestRow](ctx, r.q, "mockTests.createTest",
		`insert into mock_tests (user_id, plan_id, title, source, total_questions, duration_minutes)
		 values ($1, $2, $3, $4, $5, $6) returning `+mockTestCols,
		t.UserID, t.PlanID, t.Title, t.Source, t.TotalQuestions, t.DurationMinutes)
}

// AddQuestions writes all the test's questions in one statement, sort_order from 0.
func (r *MockTests) AddQuestions(ctx context.Context, testID string, questionIDs []string) error {
	if len(questionIDs) == 0 {
		return nil
	}
	return exec(ctx, r.q, "mockTests.addQuestions",
		`insert into mock_test_questions (test_id, question_id, sort_order)
		 select $1, q, o - 1 from unnest($2::uuid[]) with ordinality as t(q, o)`, testID, questionIDs)
}

// MarkQuestions writes the marking of every question, one update each (as the TS code does,
// despite its comment about grouping).
func (r *MockTests) MarkQuestions(ctx context.Context, testID string, marks []QuestionMark) error {
	for _, m := range marks {
		if err := exec(ctx, r.q, "mockTests.markQuestions",
			`update mock_test_questions set given_answer = $3, is_correct = $4 where test_id = $1 and question_id = $2`,
			testID, m.QuestionID, m.GivenAnswer, m.IsCorrect); err != nil {
			return err
		}
	}
	return nil
}

func (r *MockTests) SubmitTest(ctx context.Context, testID string, res MockTestResult) (*MockTestRow, error) {
	return single[MockTestRow](ctx, r.q, "mockTests.submitTest",
		`update mock_tests set status = 'submitted', submitted_at = now(), correct_count = $2, score_percent = $3,
		   seconds_taken = $4
		 where id = $1 returning `+mockTestCols, testID, res.CorrectCount, res.ScorePercent, res.SecondsTaken)
}

// AbandonOpenTests retires unfinished tests so a fresh one can be generated.
func (r *MockTests) AbandonOpenTests(ctx context.Context, userID string) error {
	return exec(ctx, r.q, "mockTests.abandonOpenTests",
		`update mock_tests set status = 'abandoned' where user_id = $1 and status = 'in_progress'`, userID)
}
