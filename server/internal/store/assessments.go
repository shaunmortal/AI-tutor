package store

import (
	"context"
	"time"

	"github.com/shaunmortal/AI-tutor/server/internal/adaptive"
)

// AssessmentRow is one diagnostic.
type AssessmentRow struct {
	ID            string     `db:"id" json:"id"`
	GoalID        string     `db:"goal_id" json:"goal_id"`
	Kind          string     `db:"kind" json:"kind"`
	Status        string     `db:"status" json:"status"`
	QuestionCount int        `db:"question_count" json:"question_count"`
	CorrectCount  int        `db:"correct_count" json:"correct_count"`
	Accuracy      *float64   `db:"accuracy" json:"accuracy"`
	StartedAt     time.Time  `db:"started_at" json:"started_at"`
	CompletedAt   *time.Time `db:"completed_at" json:"completed_at"`
}

// AssessmentQuestionRow is one question of a paper and the attempt that answered it.
type AssessmentQuestionRow struct {
	QuestionID string  `db:"question_id" json:"question_id"`
	Position   int     `db:"position" json:"position"`
	AttemptID  *string `db:"attempt_id" json:"attempt_id"`
}

// AssessableTopic is a goal topic with its subject and verified-question count.
type AssessableTopic struct {
	TopicID            string  `db:"topic_id" json:"topicId"`
	Name               string  `db:"name" json:"name"`
	SubjectID          string  `db:"subject_id" json:"subjectId"`
	SubjectName        string  `db:"subject_name" json:"subjectName"`
	SubjectWeight      float64 `db:"subject_weight" json:"subjectWeight"`
	AvailableQuestions int     `db:"available_questions" json:"availableQuestions"`
}

// TopicSubject is the subject a topic sits under.
type TopicSubject struct {
	SubjectID   string `json:"subjectId"`
	SubjectName string `json:"subjectName"`
}

// NewAssessment is a diagnostic to create.
type NewAssessment struct {
	UserID        string
	GoalID        string
	QuestionCount int
}

const assessmentCols = `id, goal_id, kind, status, question_count, correct_count, accuracy, started_at, completed_at`

// Assessments holds diagnostics. Writes need Admin: learners get SELECT only.
type Assessments struct{ q Q }

func NewAssessments(q Q) *Assessments { return &Assessments{q} }

// FindForGoal is the latest diagnostic for a goal, if any.
func (r *Assessments) FindForGoal(ctx context.Context, userID, goalID string) (*AssessmentRow, error) {
	return rowOf[AssessmentRow](ctx, r.q, "assessments.findForGoal",
		`select `+assessmentCols+` from assessments where user_id = $1 and goal_id = $2 and kind = 'diagnostic'
		 order by started_at desc limit 1`, userID, goalID)
}

// Find loads an assessment the learner owns (ownership checked here, since Admin bypasses RLS).
func (r *Assessments) Find(ctx context.Context, assessmentID, userID string) (*AssessmentRow, error) {
	return rowOf[AssessmentRow](ctx, r.q, "assessments.find",
		`select `+assessmentCols+` from assessments where id = $1 and user_id = $2`, assessmentID, userID)
}

func (r *Assessments) FindQuestions(ctx context.Context, assessmentID string) ([]AssessmentQuestionRow, error) {
	return rowsOf[AssessmentQuestionRow](ctx, r.q, "assessments.findQuestions",
		`select question_id, position, attempt_id from assessment_questions where assessment_id = $1 order by position`,
		assessmentID)
}

// FindQuestionBodies is the questions without answers or explanations (not in paper order:
// the caller orders them).
func (r *Assessments) FindQuestionBodies(ctx context.Context, questionIDs []string) ([]adaptive.PracticeQuestion, error) {
	return questionBodies(ctx, r.q, "assessments.findQuestionBodies", questionIDs)
}

// FindAssessableTopics is the goal's leaf topics, each with its subject and how many
// verified questions it has.
func (r *Assessments) FindAssessableTopics(ctx context.Context, goalID string) ([]AssessableTopic, error) {
	return rowsOf[AssessableTopic](ctx, r.q, "assessments.findAssessableTopics",
		`select l.id as topic_id, l.name, s.id as subject_id, s.name as subject_name, s.weight as subject_weight,
		        (select count(*)::int from questions q where q.topic_id = l.id and q.is_verified) as available_questions
		 from learning_goal_topics g
		 join topics s on s.id = g.topic_id
		 join topics l on l.parent_id = s.id
		 where g.goal_id = $1`, goalID)
}

// FindOneQuestionForTopic is one verified question id on a topic, or nil.
func (r *Assessments) FindOneQuestionForTopic(ctx context.Context, topicID string) (*string, error) {
	ids, err := colOf[string](ctx, r.q, "assessments.findOneQuestionForTopic",
		`select id::text from questions where topic_id = $1 and is_verified limit 1`, topicID)
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	return &ids[0], nil
}

// FindTopicSubjects maps each topic that has a parent to its subject ("Subject" when the
// subject's name cannot be read).
func (r *Assessments) FindTopicSubjects(ctx context.Context, topicIDs []string) (map[string]TopicSubject, error) {
	out := map[string]TopicSubject{}
	if len(topicIDs) == 0 {
		return out, nil
	}
	type row struct {
		ID         string  `db:"id"`
		ParentID   string  `db:"parent_id"`
		ParentName *string `db:"parent_name"`
	}
	rows, err := rowsOf[row](ctx, r.q, "assessments.findTopicSubjects",
		`select t.id, t.parent_id, p.name as parent_name from topics t left join topics p on p.id = t.parent_id
		 where t.id = any($1::uuid[]) and t.parent_id is not null`, topicIDs)
	if err != nil {
		return nil, err
	}
	for _, t := range rows {
		name := "Subject"
		if t.ParentName != nil {
			name = *t.ParentName
		}
		out[t.ID] = TopicSubject{SubjectID: t.ParentID, SubjectName: name}
	}
	return out, nil
}

func (r *Assessments) Create(ctx context.Context, a NewAssessment) (*AssessmentRow, error) {
	return single[AssessmentRow](ctx, r.q, "assessments.create",
		`insert into assessments (user_id, goal_id, kind, question_count) values ($1, $2, 'diagnostic', $3)
		 returning `+assessmentCols, a.UserID, a.GoalID, a.QuestionCount)
}

// AddQuestions writes every question of the paper in one statement, positions from 1.
func (r *Assessments) AddQuestions(ctx context.Context, assessmentID string, questionIDs []string) error {
	if len(questionIDs) == 0 {
		return nil
	}
	return exec(ctx, r.q, "assessments.addQuestions",
		`insert into assessment_questions (assessment_id, question_id, position)
		 select $1, q, o from unnest($2::uuid[]) with ordinality as t(q, o)`, assessmentID, questionIDs)
}

// LinkAttempt links a question to the attempt that answered it.
func (r *Assessments) LinkAttempt(ctx context.Context, assessmentID, questionID, attemptID string) error {
	return exec(ctx, r.q, "assessments.linkAttempt",
		`update assessment_questions set attempt_id = $3 where assessment_id = $1 and question_id = $2`,
		assessmentID, questionID, attemptID)
}

func (r *Assessments) Complete(ctx context.Context, assessmentID string, correctCount int) (*AssessmentRow, error) {
	return single[AssessmentRow](ctx, r.q, "assessments.complete",
		`update assessments set status = 'completed', correct_count = $2, completed_at = now()
		 where id = $1 returning `+assessmentCols, assessmentID, correctCount)
}

// AbandonOpen retires an unfinished diagnostic so a fresh one can be started.
func (r *Assessments) AbandonOpen(ctx context.Context, userID, goalID string) error {
	return exec(ctx, r.q, "assessments.abandonOpen",
		`update assessments set status = 'abandoned' where user_id = $1 and goal_id = $2 and status = 'in_progress'`,
		userID, goalID)
}

// questionBodies is the learner-visible columns of a set of questions.
func questionBodies(ctx context.Context, q Q, op string, questionIDs []string) ([]adaptive.PracticeQuestion, error) {
	if len(questionIDs) == 0 {
		return []adaptive.PracticeQuestion{}, nil
	}
	return rowsOf[adaptive.PracticeQuestion](ctx, q, op,
		`select `+practiceCols+` from questions where id = any($1::uuid[])`, questionIDs)
}
