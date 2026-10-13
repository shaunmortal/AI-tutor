package practice

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/shaunmortal/AI-tutor/server/internal/adaptive"
	"github.com/shaunmortal/AI-tutor/server/internal/app"
	"github.com/shaunmortal/AI-tutor/server/internal/assessment"
	"github.com/shaunmortal/AI-tutor/server/internal/auth"
	"github.com/shaunmortal/AI-tutor/server/internal/httpx"
	"github.com/shaunmortal/AI-tutor/server/internal/store"
)

// Mock tests: generated from the plan, served without answers, marked in one pass at the end.
// The rule: the answer key never leaves the server before submission. Elevated client
// throughout (marking reads correct_answer and writes is_correct, both closed to learners), so
// the user id filter in the repository is what guards a test id.

type mockTestView struct {
	ID              string                      `json:"id"`
	Title           string                      `json:"title"`
	Source          string                      `json:"source"`
	Status          string                      `json:"status"`
	TotalQuestions  int                         `json:"totalQuestions"`
	DurationMinutes int                         `json:"durationMinutes"`
	StartedAt       time.Time                   `json:"startedAt"`
	SubmittedAt     *time.Time                  `json:"submittedAt"`
	CorrectCount    *int                        `json:"correctCount"`
	ScorePercent    *float64                    `json:"scorePercent"`
	Questions       []adaptive.PracticeQuestion `json:"questions,omitzero"` // only while open
	Result          *mockResult                 `json:"result,omitzero"`    // only once submitted
	Coverage        []mockCoverage              `json:"coverage,omitzero"`
	Omitted         []assessment.OmittedTopic   `json:"omitted,omitzero"`
}

type mockResult struct {
	assessment.MockTestResult
	ByTopic []namedTopicScore `json:"byTopic"`
}

type namedTopicScore struct {
	assessment.TopicScore
	Name string `json:"name"`
}

type mockCoverage struct {
	Name          string  `json:"name"`
	SubjectName   *string `json:"subjectName"`
	QuestionCount int     `json:"questionCount"`
	Reason        string  `json:"reason"`
}

var errNoTest = httpx.E(http.StatusNotFound, "TEST_NOT_FOUND", "That test does not exist.")

func nothingToTest(msg string) error { return httpx.E(http.StatusBadRequest, "NOTHING_TO_TEST", msg) }

func toMockView(t store.MockTestRow) mockTestView {
	return mockTestView{ID: t.ID, Title: t.Title, Source: t.Source, Status: t.Status, TotalQuestions: t.TotalQuestions,
		DurationMinutes: t.DurationMinutes, StartedAt: t.StartedAt, SubmittedAt: t.SubmittedAt,
		CorrectCount: t.CorrectCount, ScorePercent: t.ScorePercent}
}

func listTests(d *app.Deps) httpx.Handler {
	return func(w http.ResponseWriter, r *http.Request) error {
		userID, ctx := auth.UserID(r), r.Context()
		tests := store.NewMockTests(d.DB.Admin)
		rows, err := tests.FindRecent(ctx, userID, 10)
		if err != nil {
			return err
		}
		views := make([]mockTestView, len(rows))
		for i, row := range rows {
			views[i] = toMockView(row)
		}
		var open *mockTestView
		if row, err := tests.FindOpenTest(ctx, userID); err != nil {
			return err
		} else if row != nil {
			if open, err = viewTest(ctx, tests, userID, row.ID); err != nil {
				return err
			}
		}
		httpx.OK(w, http.StatusOK, map[string]any{"tests": views, "open": open, "limits": map[string]int{
			"min": assessment.MockMinQuestions, "max": assessment.MockMaxQuestions, "default": assessment.MockDefaultQuestions}})
		return nil
	}
}

func getTest(d *app.Deps) httpx.Handler {
	return func(w http.ResponseWriter, r *http.Request) error {
		id := r.PathValue("id")
		if !isUUID(id) {
			return errNoTest // same answer as a test that is not yours
		}
		view, err := viewTest(r.Context(), store.NewMockTests(d.DB.Admin), auth.UserID(r), id)
		if err != nil {
			return err
		}
		httpx.OK(w, http.StatusOK, map[string]any{"test": view})
		return nil
	}
}

// viewTest is one test: question bodies (no answers) while open; the result, and not the
// questions, once submitted.
func viewTest(ctx context.Context, tests *store.MockTests, userID, testID string) (*mockTestView, error) {
	test, err := tests.FindTest(ctx, testID, userID)
	if err != nil {
		return nil, err
	}
	if test == nil {
		return nil, errNoTest
	}
	rows, err := tests.FindTestQuestions(ctx, test.ID)
	if err != nil {
		return nil, err
	}
	view := toMockView(*test)

	if test.Status == "submitted" {
		marked := make([]assessment.MarkedQuestion, len(rows))
		for i, row := range rows {
			marked[i] = assessment.MarkedQuestion{TopicID: deref(row.TopicID), IsCorrect: row.IsCorrect != nil && *row.IsCorrect,
				Answered: len(row.GivenAnswer) > 0 && string(row.GivenAnswer) != "null"}
		}
		result, err := markRows(ctx, tests, rows, marked)
		if err != nil {
			return nil, err
		}
		view.Result = result
		return &view, nil
	}

	ids := make([]string, len(rows))
	for i, row := range rows {
		ids[i] = row.QuestionID
	}
	bodies, err := tests.FindQuestionBodies(ctx, ids)
	if err != nil {
		return nil, err
	}
	view.Questions = inOrder(ids, bodies)
	return &view, nil
}

// markRows scores a sitting and names each topic in the breakdown.
func markRows(ctx context.Context, tests *store.MockTests, rows []store.MockTestQuestionRow,
	marked []assessment.MarkedQuestion) (*mockResult, error) {
	var topicIDs []string
	seen := map[string]bool{}
	for _, row := range rows {
		if id := deref(row.TopicID); id != "" && !seen[id] {
			seen[id] = true
			topicIDs = append(topicIDs, id)
		}
	}
	details, err := tests.FindTopicDetails(ctx, topicIDs)
	if err != nil {
		return nil, err
	}
	names := make(map[string]string, len(details))
	for id, dt := range details {
		names[id] = dt.Name
	}
	res := assessment.MarkMockTest(marked, names)
	out := &mockResult{MockTestResult: res, ByTopic: make([]namedTopicScore, len(res.ByTopic))}
	for i, t := range res.ByTopic {
		name, ok := names[t.TopicID]
		if !ok {
			name = "Topic"
		}
		out.ByTopic[i] = namedTopicScore{t, name}
	}
	return out, nil
}

// generateTest builds a test from the plan (or the goal when there is no plan). An open test
// is abandoned rather than blocking a new one.
func generateTest(d *app.Deps) httpx.Handler {
	return func(w http.ResponseWriter, r *http.Request) error {
		userID, ctx := auth.UserID(r), r.Context()
		m, err := readObject(w, r)
		if err != nil {
			return err
		}
		count, err := optInt(m, "questionCount", assessment.MockMinQuestions, assessment.MockMaxQuestions)
		if err != nil {
			return err
		}
		requested := assessment.MockDefaultQuestions
		if count != nil {
			requested = *count
		}

		admin := d.DB.Admin
		tests, plans := store.NewMockTests(admin), store.NewPlans(admin)
		plan, err := plans.FindActivePlan(ctx, userID)
		if err != nil {
			return err
		}
		var candidates []assessment.MockTestTopic
		if plan != nil {
			candidates, err = topicsFromPlan(ctx, tests, plans, userID, plan.ID)
		} else {
			candidates, err = topicsFromGoal(ctx, tests, plans, store.NewLearners(admin), userID)
		}
		if err != nil {
			return err
		}
		if len(candidates) == 0 {
			if plan != nil {
				return nothingToTest("Your plan has no topics with questions yet. Practise a little first, then come back.")
			}
			return nothingToTest("Set a learning goal and build a plan first — a test is drawn from what you are scheduled to study.")
		}

		profile, err := store.NewLearners(admin).FindProfile(ctx, userID)
		if err != nil {
			return err
		}
		pace := 0.0
		if profile != nil && profile.AvgSecondsPerQuestion != nil {
			pace = *profile.AvgSecondsPerQuestion
		}
		built := assessment.PlanMockTest(candidates, requested, pace)
		const tooThin = "There are not enough questions on your topics to make a test yet."
		if len(built.Allocations) == 0 {
			return nothingToTest(tooThin)
		}

		// Round-robin the per-topic lists, so running out of time costs a few questions across
		// the breakdown rather than whole topics.
		perTopic := make([][]string, len(built.Allocations))
		longest := 0
		for i, al := range built.Allocations {
			if perTopic[i], err = tests.FindQuestionsForTopic(ctx, al.TopicID, al.QuestionCount); err != nil {
				return err
			}
			longest = max(longest, len(perTopic[i]))
		}
		var questionIDs []string
		for i := range longest {
			for _, group := range perTopic {
				if i < len(group) {
					questionIDs = append(questionIDs, group[i])
				}
			}
		}
		if len(questionIDs) == 0 {
			return nothingToTest(tooThin)
		}

		if err := tests.AbandonOpenTests(ctx, userID); err != nil {
			return err
		}
		source, planID := "goal", (*string)(nil)
		if plan != nil {
			source, planID = "plan", &plan.ID
		}
		plural := "s"
		if len(built.Allocations) == 1 {
			plural = ""
		}
		test, err := tests.CreateTest(ctx, store.NewMockTest{UserID: userID, PlanID: planID,
			Title:  fmt.Sprintf("%d-question test across %d topic%s", len(questionIDs), len(built.Allocations), plural),
			Source: source, TotalQuestions: len(questionIDs), DurationMinutes: built.DurationMinutes})
		if err != nil {
			return err
		}
		if err := tests.AddQuestions(ctx, test.ID, questionIDs); err != nil {
			return err
		}
		view, err := viewTest(ctx, tests, userID, test.ID)
		if err != nil {
			return err
		}
		view.Coverage = make([]mockCoverage, len(built.Allocations))
		for i, al := range built.Allocations {
			view.Coverage[i] = mockCoverage{al.Name, al.SubjectName, al.QuestionCount, al.Reason}
		}
		if len(built.Omitted) > 0 {
			view.Omitted = built.Omitted
		}
		httpx.OK(w, http.StatusCreated, map[string]any{"test": view})
		return nil
	}
}

// topicsFromPlan is the plan's topics with mastery, names and bank sizes.
func topicsFromPlan(ctx context.Context, tests *store.MockTests, plans *store.Plans, userID, planID string) ([]assessment.MockTestTopic, error) {
	planned, err := tests.FindPlanTopics(ctx, planID)
	if err != nil || len(planned) == 0 {
		return nil, err
	}
	ids := make([]string, len(planned))
	for i, p := range planned {
		ids[i] = p.TopicID
	}
	details, err := tests.FindTopicDetails(ctx, ids)
	if err != nil {
		return nil, err
	}
	counts, err := tests.CountQuestionsByTopic(ctx, ids)
	if err != nil {
		return nil, err
	}
	performance, err := plans.FindTopicPerformance(ctx, userID, ids)
	if err != nil {
		return nil, err
	}
	mastery := map[string]*float64{}
	for _, p := range performance {
		mastery[p.TopicID] = p.MasteryScore
	}
	var out []assessment.MockTestTopic
	for _, p := range planned {
		dt, ok := details[p.TopicID]
		if !ok {
			continue
		}
		out = append(out, assessment.MockTestTopic{TopicID: p.TopicID, Name: dt.Name, SubjectName: dt.SubjectName,
			Weight: dt.Weight, MasteryScore: mastery[p.TopicID], PlannedSessions: p.PlannedSessions,
			AvailableQuestions: counts[p.TopicID]})
	}
	return out, nil
}

// topicsFromGoal is the fallback with no plan: the goal's own topics, none scheduled.
func topicsFromGoal(ctx context.Context, tests *store.MockTests, plans *store.Plans, learners *store.Learners, userID string) ([]assessment.MockTestTopic, error) {
	subjects, err := learners.FindGoalSubjects(ctx, userID)
	if err != nil || len(subjects) == 0 {
		return nil, err
	}
	scope := make([]string, len(subjects))
	for i, s := range subjects {
		scope[i] = s.ID
	}
	plannable, err := plans.FindPlannableTopics(ctx, userID, scope)
	if err != nil || len(plannable) == 0 {
		return nil, err
	}
	ids := make([]string, len(plannable))
	for i, t := range plannable {
		ids[i] = t.TopicID
	}
	counts, err := tests.CountQuestionsByTopic(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]assessment.MockTestTopic, len(plannable))
	for i, t := range plannable {
		out[i] = assessment.MockTestTopic{TopicID: t.TopicID, Name: t.Name, SubjectName: t.SubjectName, Weight: t.Weight,
			MasteryScore: t.MasteryScore, AvailableQuestions: counts[t.TopicID]}
	}
	return out, nil
}

// submitTest marks every answer at once. Questions not sent are skipped, not wrong.
func submitTest(d *app.Deps) httpx.Handler {
	return func(w http.ResponseWriter, r *http.Request) error {
		userID, ctx := auth.UserID(r), r.Context()
		// Body first: in the TS, validation middleware ran before the controller's id check.
		m, err := readObject(w, r)
		if err != nil {
			return err
		}
		answers, err := parseAnswers(m, assessment.MockMaxQuestions)
		if err != nil {
			return err
		}
		v, present := m["secondsTaken"]
		if !present {
			return expected("secondsTaken", "number", nil, false)
		}
		secondsTaken, err := intIn("secondsTaken", v, 0, 86_400)
		if err != nil {
			return err
		}
		testID := r.PathValue("id")
		if !isUUID(testID) {
			return errNoTest
		}

		admin := d.DB.Admin
		tests, attempts := store.NewMockTests(admin), store.NewAttempts(admin)
		test, err := tests.FindTest(ctx, testID, userID)
		if err != nil {
			return err
		}
		if test == nil {
			return errNoTest
		}
		// Idempotent: re-marking would write a second set of attempts.
		if test.Status != "submitted" {
			rows, err := tests.FindTestQuestions(ctx, test.ID)
			if err != nil {
				return err
			}
			given := answersByQuestion(answers)
			var marks []store.QuestionMark
			marked := make([]assessment.MarkedQuestion, len(rows))
			for i, row := range rows {
				marked[i].TopicID = deref(row.TopicID)
				answer := given[row.QuestionID]
				if answer == nil {
					continue
				}
				// The answer key is read here, server side, for the first time in this test's life.
				q, err := attempts.FindForGrading(ctx, row.QuestionID)
				if err != nil {
					return err
				}
				isCorrect := gradeOrWrong(q, answer)
				marks = append(marks, store.QuestionMark{QuestionID: row.QuestionID, GivenAnswer: answer.json(), IsCorrect: isCorrect})
				marked[i].IsCorrect, marked[i].Answered = isCorrect, true
				// A real attempt with its own source, so the test counts as evidence.
				if _, err := attempts.Insert(ctx, store.NewAttempt{UserID: userID, QuestionID: row.QuestionID,
					IsCorrect: isCorrect, GivenAnswer: answer.json(), Source: "mock_test"}); err != nil {
					return err
				}
			}
			if err := tests.MarkQuestions(ctx, test.ID, marks); err != nil {
				return err
			}
			result, err := markRows(ctx, tests, rows, marked)
			if err != nil {
				return err
			}
			if _, err := tests.SubmitTest(ctx, test.ID, store.MockTestResult{CorrectCount: result.CorrectCount,
				ScorePercent: result.ScorePercent, SecondsTaken: int(math.Max(0, math.Min(86_400, float64(secondsTaken))))}); err != nil {
				return err
			}
		}
		view, err := viewTest(ctx, tests, userID, test.ID)
		if err != nil {
			return err
		}
		httpx.OK(w, http.StatusOK, map[string]any{"test": view})
		return nil
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
