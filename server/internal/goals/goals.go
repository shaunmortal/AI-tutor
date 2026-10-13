// Package goals creates and reads the learner's goal.
//
// The validation the database cannot do lives here. A CHECK cannot require the exam date to
// be in the future, because a CHECK also fires on UPDATE: once the exam had passed, any edit
// to that row would fail. So the rule belongs where a goal is created.
package goals

import (
	"context"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/shaunmortal/AI-tutor/server/internal/dates"
	"github.com/shaunmortal/AI-tutor/server/internal/httpx"
	"github.com/shaunmortal/AI-tutor/server/internal/store"
)

// CreateInput is a goal to create.
type CreateInput struct {
	UserID       string
	Title        string
	ExamDate     string // YYYY-MM-DD
	DailyMinutes int
	SubjectIDs   []string
	CurriculumID string
}

// View is a goal as the API returns it.
type View struct {
	ID           string   `json:"id"`
	Title        string   `json:"title"`
	ExamDate     string   `json:"examDate"`
	DailyMinutes int      `json:"dailyMinutes"`
	SubjectIDs   []string `json:"subjectIds"`
	CurriculumID *string  `json:"curriculumId"`
	// Whole days from today: zero on exam day, negative once it has passed.
	DaysRemaining int `json:"daysRemaining"`
	// DailyMinutes x days left, so the planner and the UI agree on the budget.
	TotalMinutesAvailable int `json:"totalMinutesAvailable"`
}

// Service creates and reads goals.
type Service struct{ goals *store.Goals }

func New(goals *store.Goals) *Service { return &Service{goals} }

func (s *Service) ListSubjects(ctx context.Context, curriculumID string) ([]store.SubjectRow, error) {
	return s.goals.FindSubjects(ctx, curriculumID)
}

// GetActive is the learner's active goal, or nil when they have none.
func (s *Service) GetActive(ctx context.Context, userID string) (*View, error) {
	goal, err := s.goals.FindActive(ctx, userID)
	if err != nil || goal == nil {
		return nil, err
	}
	subjectIDs, err := s.goals.FindScope(ctx, goal.ID)
	if err != nil {
		return nil, err
	}
	return toView(goal, subjectIDs, time.Now()), nil
}

// Create validates the goal, archives any active one, and stores this one.
func (s *Service) Create(ctx context.Context, in CreateInput) (*View, error) {
	now := time.Now()
	days, ok := DaysUntil(in.ExamDate, now)
	if !ok {
		return nil, invalid("The exam date is not a real date.")
	}
	if days < 1 {
		return nil, invalid("The exam date must be in the future — there is nothing to plan otherwise.")
	}
	// Two years out is not a study plan, it is a wish.
	if days > 730 {
		return nil, invalid("Pick an exam date within the next two years.")
	}
	if len(in.SubjectIDs) == 0 {
		return nil, invalid("Choose at least one subject to study.")
	}

	// The same subject twice would violate the scope table's primary key with a confusing error.
	var subjectIDs []string
	seen := map[string]bool{}
	for _, id := range in.SubjectIDs {
		if !seen[id] {
			seen[id] = true
			subjectIDs = append(subjectIDs, id)
		}
	}

	created, err := s.goals.ReplaceActive(ctx, in.UserID, store.NewGoal{
		Title:        strings.TrimFunc(in.Title, isJSSpace),
		ExamDate:     in.ExamDate,
		DailyMinutes: in.DailyMinutes,
		SubjectIDs:   subjectIDs,
		CurriculumID: in.CurriculumID,
	})
	if err != nil {
		return nil, err
	}
	return toView(created, subjectIDs, now), nil
}

func toView(g *store.GoalRow, subjectIDs []string, now time.Time) *View {
	days, _ := DaysUntil(g.ExamDate, now)
	return &View{
		ID:                    g.ID,
		Title:                 g.Title,
		ExamDate:              g.ExamDate,
		DailyMinutes:          g.DailyMinutes,
		SubjectIDs:            subjectIDs,
		CurriculumID:          g.CurriculumID,
		DaysRemaining:         days,
		TotalMinutesAvailable: max(0, days) * g.DailyMinutes,
	}
}

// DaysUntil is whole days from now's UTC date to isoDate (YYYY-MM-DD), both at UTC midnight
// so the difference is a clean multiple of a day. ok is false for a malformed date.
func DaysUntil(isoDate string, now time.Time) (days int, ok bool) {
	return dates.DaysBetween(now.UTC().Format("2006-01-02"), isoDate)
}

func invalid(msg string) error { return httpx.E(http.StatusBadRequest, "INVALID_INPUT", msg) }

// isJSSpace is what String.prototype.trim strips: Unicode white space plus the BOM, but not NEL.
func isJSSpace(r rune) bool { return r == '\uFEFF' || (unicode.IsSpace(r) && r != '\u0085') }
