package practice

import (
	"context"
	"math"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/shaunmortal/AI-tutor/server/internal/adaptive"
	"github.com/shaunmortal/AI-tutor/server/internal/app"
	"github.com/shaunmortal/AI-tutor/server/internal/assessment"
	"github.com/shaunmortal/AI-tutor/server/internal/auth"
	"github.com/shaunmortal/AI-tutor/server/internal/goals"
	"github.com/shaunmortal/AI-tutor/server/internal/httpx"
	"github.com/shaunmortal/AI-tutor/server/internal/store"
)

// The diagnostic. Elevated client throughout: marking reads the answer key, and the assessment
// tables grant learners SELECT only — so ownership is checked by user id on every read.
// It stores no "assessed level" of its own: every answer becomes an ordinary attempt
// (source 'assessment') and mastery is recomputed from the log, so there is one truth.

type assessmentView struct {
	ID              string                        `json:"id"`
	Status          string                        `json:"status"`
	QuestionCount   int                           `json:"questionCount"`
	DurationMinutes int                           `json:"durationMinutes"`
	StartedAt       time.Time                     `json:"startedAt"`
	CompletedAt     *time.Time                    `json:"completedAt"`
	Questions       []adaptive.PracticeQuestion   `json:"questions,omitzero"` // only while open, never with answers
	Result          *assessment.DiagnosticResult  `json:"result,omitzero"`    // only once completed
	Coverage        []coverageEntry               `json:"coverage,omitzero"`
	Uncovered       []assessment.UncoveredSubject `json:"uncovered,omitzero"`
}

type coverageEntry struct {
	SubjectName   string `json:"subjectName"`
	QuestionCount int    `json:"questionCount"`
}

var errNoAssessment = httpx.E(http.StatusNotFound, "ASSESSMENT_NOT_FOUND", "That assessment does not exist.")

func diagnosticLimits() map[string]int {
	return map[string]int{"min": assessment.DiagnosticMinQuestions, "max": assessment.DiagnosticMaxQuestions,
		"default": assessment.DiagnosticDefaultQuestions}
}

// requireGoal is the learner's active goal (read as the learner), or 400 NO_ACTIVE_GOAL.
func requireGoal(ctx context.Context, d *app.Deps, userID string) (*goals.View, error) {
	var goal *goals.View
	err := d.DB.AsUser(ctx, userID, func(tx pgx.Tx) error {
		var err error
		goal, err = goals.New(store.NewGoals(tx)).GetActive(ctx, userID)
		return err
	})
	if err == nil && goal == nil {
		err = httpx.E(http.StatusBadRequest, "NO_ACTIVE_GOAL",
			"Set a learning goal first — an assessment measures you against the subjects you chose.")
	}
	return goal, err
}

func getDiagnostic(d *app.Deps) httpx.Handler {
	return func(w http.ResponseWriter, r *http.Request) error {
		userID, ctx := auth.UserID(r), r.Context()
		goal, err := requireGoal(ctx, d, userID)
		if err != nil {
			return err
		}
		a := store.NewAssessments(d.DB.Admin)
		row, err := a.FindForGoal(ctx, userID, goal.ID)
		if err != nil {
			return err
		}
		var view *assessmentView
		if row != nil {
			if view, err = viewAssessment(ctx, d, row); err != nil {
				return err
			}
		}
		httpx.OK(w, http.StatusOK, map[string]any{"assessment": view, "limits": diagnosticLimits()})
		return nil
	}
}

func startDiagnostic(d *app.Deps) httpx.Handler {
	return func(w http.ResponseWriter, r *http.Request) error {
		userID, ctx := auth.UserID(r), r.Context()
		m, err := readObject(w, r)
		if err != nil {
			return err
		}
		count, err := optInt(m, "questionCount", assessment.DiagnosticMinQuestions, assessment.DiagnosticMaxQuestions)
		if err != nil {
			return err
		}
		requested := assessment.DiagnosticDefaultQuestions
		if count != nil {
			requested = *count
		}
		goal, err := requireGoal(ctx, d, userID)
		if err != nil {
			return err
		}

		a := store.NewAssessments(d.DB.Admin)
		topics, err := a.FindAssessableTopics(ctx, goal.ID)
		if err != nil {
			return err
		}
		if len(topics) == 0 {
			return nothingToAssess("There are no topics with questions in your subjects yet, so there is nothing to assess.")
		}
		dt := make([]assessment.DiagnosticTopic, len(topics))
		for i, t := range topics {
			dt[i] = assessment.DiagnosticTopic(t)
		}
		plan := assessment.PlanDiagnostic(dt, requested)
		if len(plan.Picks) == 0 {
			return nothingToAssess("None of your subjects have questions yet. Practise a little first and the assessment will have something to ask.")
		}
		var questionIDs []string
		for _, pick := range plan.Picks {
			id, err := a.FindOneQuestionForTopic(ctx, pick.TopicID)
			if err != nil {
				return err
			}
			if id != nil {
				questionIDs = append(questionIDs, *id)
			}
		}
		if len(questionIDs) == 0 {
			return nothingToAssess("None of your subjects have questions yet.")
		}

		// An unfinished one is abandoned rather than blocking a fresh start.
		if err := a.AbandonOpen(ctx, userID, goal.ID); err != nil {
			return err
		}
		// Counted from what was found, not what was asked for.
		row, err := a.Create(ctx, store.NewAssessment{UserID: userID, GoalID: goal.ID, QuestionCount: len(questionIDs)})
		if err != nil {
			return err
		}
		if err := a.AddQuestions(ctx, row.ID, questionIDs); err != nil {
			return err
		}
		view, err := viewAssessment(ctx, d, row)
		if err != nil {
			return err
		}
		view.Coverage = make([]coverageEntry, len(plan.Coverage))
		for i, c := range plan.Coverage {
			view.Coverage[i] = coverageEntry{c.SubjectName, c.QuestionCount}
		}
		if len(plan.Uncovered) > 0 {
			view.Uncovered = plan.Uncovered
		}
		httpx.OK(w, http.StatusCreated, map[string]any{"assessment": view})
		return nil
	}
}

func nothingToAssess(msg string) error {
	return httpx.E(http.StatusBadRequest, "NOTHING_TO_ASSESS", msg)
}

// findOwnAssessment is the path id, if it is a uuid the learner owns. A malformed id gets the
// same 404 as somebody else's, so the two cannot be told apart by probing.
func findOwnAssessment(r *http.Request, a *store.Assessments) (*store.AssessmentRow, error) {
	id := r.PathValue("id")
	if !isUUID(id) {
		return nil, errNoAssessment
	}
	row, err := a.Find(r.Context(), id, auth.UserID(r))
	if err == nil && row == nil {
		err = errNoAssessment
	}
	return row, err
}

func getAssessment(d *app.Deps) httpx.Handler {
	return func(w http.ResponseWriter, r *http.Request) error {
		row, err := findOwnAssessment(r, store.NewAssessments(d.DB.Admin))
		if err != nil {
			return err
		}
		view, err := viewAssessment(r.Context(), d, row)
		if err != nil {
			return err
		}
		httpx.OK(w, http.StatusOK, map[string]any{"assessment": view})
		return nil
	}
}

// submitAssessment marks the paper in one pass and lets it move mastery.
func submitAssessment(d *app.Deps) httpx.Handler {
	return func(w http.ResponseWriter, r *http.Request) error {
		userID, ctx := auth.UserID(r), r.Context()
		// Body first: in the TS, validation middleware ran before the controller's id check.
		m, err := readObject(w, r)
		if err != nil {
			return err
		}
		answers, err := parseAnswers(m, assessment.DiagnosticMaxQuestions)
		if err != nil {
			return err
		}

		admin := d.DB.Admin
		a, attempts := store.NewAssessments(admin), store.NewAttempts(admin)
		row, err := findOwnAssessment(r, a)
		if err != nil {
			return err
		}
		// Idempotent: a double-tapped submit returns the result instead of double-counting evidence.
		if row.Status != "completed" {
			questions, err := a.FindQuestions(ctx, row.ID)
			if err != nil {
				return err
			}
			given := answersByQuestion(answers)
			correct := 0
			var touched []string
			seen := map[string]bool{}
			for _, q := range questions {
				answer := given[q.QuestionID]
				if answer == nil {
					continue
				}
				// The answer key is read here, server side, for the first time in this paper's life.
				gq, err := attempts.FindForGrading(ctx, q.QuestionID)
				if err != nil {
					return err
				}
				isCorrect := gradeOrWrong(gq, answer)
				if isCorrect {
					correct++
				}
				attempt, err := attempts.Insert(ctx, store.NewAttempt{UserID: userID, QuestionID: q.QuestionID,
					IsCorrect: isCorrect, GivenAnswer: answer.json(), Source: "assessment"})
				if err != nil {
					return err
				}
				if err := a.LinkAttempt(ctx, row.ID, q.QuestionID, attempt.ID); err != nil {
					return err
				}
				if !seen[attempt.TopicID] {
					seen[attempt.TopicID] = true
					touched = append(touched, attempt.TopicID)
				}
			}
			// This is what makes the next plan personalised.
			for _, topicID := range touched {
				if _, err := recomputeMastery(ctx, admin, userID, topicID, "", ""); err != nil {
					return err
				}
			}
			if row, err = a.Complete(ctx, row.ID, correct); err != nil {
				return err
			}
		}
		view, err := viewAssessment(ctx, d, row)
		if err != nil {
			return err
		}
		httpx.OK(w, http.StatusOK, map[string]any{"assessment": view})
		return nil
	}
}

// viewAssessment: open papers carry their questions (no answers); completed ones the result.
func viewAssessment(ctx context.Context, d *app.Deps, row *store.AssessmentRow) (*assessmentView, error) {
	admin := d.DB.Admin
	a := store.NewAssessments(admin)
	view := &assessmentView{ID: row.ID, Status: row.Status, QuestionCount: row.QuestionCount,
		DurationMinutes: int(math.Max(1, jsRound(float64(row.QuestionCount*assessment.DiagnosticSecondsPerQuestion)/60))),
		StartedAt:       row.StartedAt, CompletedAt: row.CompletedAt}

	questions, err := a.FindQuestions(ctx, row.ID)
	if err != nil {
		return nil, err
	}
	if row.Status != "completed" {
		ids := make([]string, len(questions))
		for i, q := range questions {
			ids[i] = q.QuestionID
		}
		bodies, err := a.FindQuestionBodies(ctx, ids)
		if err != nil {
			return nil, err
		}
		view.Questions = inOrder(ids, bodies)
		return view, nil
	}

	var attemptIDs []string
	for _, q := range questions {
		if q.AttemptID != nil {
			attemptIDs = append(attemptIDs, *q.AttemptID)
		}
	}
	attempts, err := store.NewAttempts(admin).FindByIDs(ctx, attemptIDs)
	if err != nil {
		return nil, err
	}
	byID := map[string]store.AttemptOutcome{}
	var topicIDs []string
	seen := map[string]bool{}
	for _, at := range attempts {
		byID[at.ID] = at
		if !seen[at.TopicID] {
			seen[at.TopicID] = true
			topicIDs = append(topicIDs, at.TopicID)
		}
	}
	subjects, err := a.FindTopicSubjects(ctx, topicIDs)
	if err != nil {
		return nil, err
	}
	answers := make([]assessment.DiagnosticAnswer, len(questions))
	for i, q := range questions {
		ans := assessment.DiagnosticAnswer{SubjectID: "unknown", SubjectName: "Subject"}
		if q.AttemptID != nil {
			if at, ok := byID[*q.AttemptID]; ok {
				ans.TopicID, ans.IsCorrect, ans.Answered = at.TopicID, at.IsCorrect, true
				if s, ok := subjects[at.TopicID]; ok {
					ans.SubjectID, ans.SubjectName = s.SubjectID, s.SubjectName
				}
			}
		}
		answers[i] = ans
	}
	result := assessment.SummariseDiagnostic(answers)
	view.Result = &result
	return view, nil
}

// inOrder puts question bodies back in paper order, dropping any that could not be read.
func inOrder(ids []string, bodies []adaptive.PracticeQuestion) []adaptive.PracticeQuestion {
	byID := make(map[string]adaptive.PracticeQuestion, len(bodies))
	for _, b := range bodies {
		byID[b.ID] = b
	}
	out := []adaptive.PracticeQuestion{}
	for _, id := range ids {
		if b, ok := byID[id]; ok {
			out = append(out, b)
		}
	}
	return out
}
