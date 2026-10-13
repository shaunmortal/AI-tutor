package store

import (
	"context"
	"time"
)

// GoalRow is a learning goal as the API returns it.
type GoalRow struct {
	ID           string    `db:"id" json:"id"`
	Title        string    `db:"title" json:"title"`
	ExamDate     string    `db:"exam_date" json:"exam_date"` // YYYY-MM-DD
	DailyMinutes int       `db:"daily_minutes" json:"daily_minutes"`
	Status       string    `db:"status" json:"status"` // active | archived
	CreatedAt    time.Time `db:"created_at" json:"created_at"`
	// Nil on goals created before curricula existed.
	CurriculumID *string `db:"curriculum_id" json:"curriculum_id"`
}

const goalCols = `id, title, exam_date::text as exam_date, daily_minutes, status, created_at, curriculum_id`

// NewGoal is what ReplaceActive writes.
type NewGoal struct {
	Title        string
	ExamDate     string
	DailyMinutes int
	SubjectIDs   []string
	CurriculumID string
}

// Goals holds learning goals and the slice of syllabus each covers. Writes need Admin: the
// tables grant learners SELECT only.
type Goals struct{ q Q }

func NewGoals(q Q) *Goals { return &Goals{q} }

// FindSubjects lists the subjects a learner can pick from within one curriculum.
func (r *Goals) FindSubjects(ctx context.Context, curriculumID string) ([]SubjectRow, error) {
	return NewCurricula(r.q).FindSubjects(ctx, curriculumID)
}

func (r *Goals) FindActive(ctx context.Context, userID string) (*GoalRow, error) {
	return rowOf[GoalRow](ctx, r.q, "goals.findActive",
		`select `+goalCols+` from learning_goals where user_id = $1 and status = 'active'`, userID)
}

// FindScope is the subject ids a goal covers.
func (r *Goals) FindScope(ctx context.Context, goalID string) ([]string, error) {
	return colOf[string](ctx, r.q, "goals.findScope",
		`select topic_id::text from learning_goal_topics where goal_id = $1`, goalID)
}

// ReplaceActive archives any active goal, then creates the new one with its scope. Archive
// first is required: a partial unique index allows one active goal per learner.
func (r *Goals) ReplaceActive(ctx context.Context, userID string, g NewGoal) (*GoalRow, error) {
	if err := exec(ctx, r.q, "goals.replaceActive",
		`update learning_goals set status = 'archived' where user_id = $1 and status = 'active'`, userID); err != nil {
		return nil, err
	}
	created, err := single[GoalRow](ctx, r.q, "goals.replaceActive",
		`insert into learning_goals (user_id, title, exam_date, daily_minutes, curriculum_id)
		 values ($1, $2, $3, $4, $5) returning `+goalCols,
		userID, g.Title, g.ExamDate, g.DailyMinutes, g.CurriculumID)
	if err != nil {
		return nil, err
	}
	if err := exec(ctx, r.q, "goals.replaceActive",
		`insert into learning_goal_topics (goal_id, topic_id) select $1, unnest($2::uuid[])`,
		created.ID, g.SubjectIDs); err != nil {
		return nil, err
	}
	return created, nil
}
