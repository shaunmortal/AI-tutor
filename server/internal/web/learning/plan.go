package learning

// The study plan: builds, stores and reads plans (planService.ts). The engine decides what a
// plan is; this decides when one is written and keeps only one current.

import (
	"context"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/shaunmortal/AI-tutor/server/internal/auth"
	"github.com/shaunmortal/AI-tutor/server/internal/dates"
	"github.com/shaunmortal/AI-tutor/server/internal/goals"
	"github.com/shaunmortal/AI-tutor/server/internal/httpx"
	"github.com/shaunmortal/AI-tutor/server/internal/planning"
	"github.com/shaunmortal/AI-tutor/server/internal/store"
)

type planAdjustment struct {
	Reason      string `json:"reason"`
	Explanation string `json:"explanation"`
}

type planSession struct {
	ID               string  `json:"id"`
	TopicID          string  `json:"topicId"`
	TopicName        string  `json:"topicName"`
	PlannedMinutes   int     `json:"plannedMinutes"`
	PlannedQuestions int     `json:"plannedQuestions"`
	Kind             string  `json:"kind"`
	Reason           *string `json:"reason"`
	Status           string  `json:"status"`
}

type planDay struct {
	Date         string        `json:"date"`
	TotalMinutes int           `json:"totalMinutes"`
	Sessions     []planSession `json:"sessions"`
}

type planView struct {
	ID                  string                  `json:"id"`
	Version             int                     `json:"version"`
	ExamDate            string                  `json:"examDate"`
	DailyMinutes        int                     `json:"dailyMinutes"`
	DaysRemaining       int                     `json:"daysRemaining"`
	TotalMinutesPlanned int                     `json:"totalMinutesPlanned"`
	TopicsCovered       int                     `json:"topicsCovered"`
	Reason              string                  `json:"reason"`
	CreatedAt           time.Time               `json:"createdAt"`
	Days                []planDay               `json:"days"`
	OmittedTopics       []planning.OmittedTopic `json:"omittedTopics,omitempty"`
}

// planService runs on Admin: settling sessions and writing plans are writes learners hold no
// privilege for, on purpose — a learner who could mark their own sessions complete would make
// every re-plan built on them worthless.
type planService struct {
	plans     *store.Plans
	learners  *store.Learners
	revisions *store.Revisions
}

func newPlanService(q store.Q) *planService {
	return &planService{store.NewPlans(q), store.NewLearners(q), store.NewRevisions(q)}
}

// today is the learner's day; a failed timezone read falls back to the default zone.
func (s *planService) today(ctx context.Context, userID string) string {
	tz, err := s.learners.FindTimezone(ctx, userID)
	if err != nil {
		tz = nil
	}
	return dates.TodayIn(timezoneOf(tz), time.Now())
}

// refreshCurrent settles past sessions, rebuilds the plan if it stopped describing reality, and
// returns the plan the learner should see. Adjusting never fails the read: the existing plan is
// still correct enough to work from.
func (s *planService) refreshCurrent(ctx context.Context, userID string, goal *goals.View) (*planView, *planAdjustment, error) {
	plan, err := s.plans.FindActivePlan(ctx, userID)
	if err != nil || plan == nil {
		return nil, nil, err
	}
	today := s.today(ctx, userID)

	rebuilt, adjustment, err := s.adjust(ctx, userID, plan, goal, today)
	if err != nil {
		slog.Warn("could not adjust the plan", "user", userID, "err", err)
	} else if rebuilt != nil {
		return rebuilt, adjustment, nil
	}

	view, err := s.view(ctx, plan)
	return view, nil, err
}

func (s *planService) adjust(ctx context.Context, userID string, plan *store.PlanRow, goal *goals.View, today string) (*planView, *planAdjustment, error) {
	if err := s.settlePastSessions(ctx, userID, plan.ID, today); err != nil {
		return nil, nil, err
	}
	verdict, err := s.judgePlan(ctx, userID, plan, today)
	if err != nil {
		return nil, nil, err
	}
	if !verdict.Replan || verdict.Reason == "" || goal == nil {
		return nil, nil, nil
	}
	rebuilt, err := s.generate(ctx, userID, goal, string(verdict.Reason))
	if err != nil {
		return nil, nil, err
	}
	return rebuilt, &planAdjustment{string(verdict.Reason), verdict.Explanation}, nil
}

// settlePastSessions records the outcome of sessions before today (today's is still live) from
// the answers actually given.
func (s *planService) settlePastSessions(ctx context.Context, userID, planID, today string) error {
	pending, err := s.plans.FindUnreconciledSessions(ctx, planID, today)
	if err != nil || len(pending) == 0 {
		return err
	}
	days := make([]string, len(pending))
	for i, p := range pending {
		days[i] = p.ScheduledDate
	}
	slices.Sort(days)
	attempts, err := s.plans.FindAttemptsByTopicAndDay(ctx, userID, days[0], days[len(days)-1])
	if err != nil {
		return err
	}
	settlements := make([]store.Settlement, len(pending))
	for i, p := range pending {
		actual := attempts[p.TopicID+"|"+p.ScheduledDate]
		settlements[i] = store.Settlement{
			ID: p.ID,
			Status: planning.ReconcileStatus(planning.ReconciledSession{
				ScheduledDate: p.ScheduledDate, TopicID: p.TopicID, PlannedQuestions: p.PlannedQuestions,
				QuestionsAnswered: actual.Answered, QuestionsCorrect: actual.Correct,
			}),
			Answered: actual.Answered,
			Correct:  actual.Correct,
		}
	}
	return s.plans.SettleSessions(ctx, settlements)
}

func (s *planService) judgePlan(ctx context.Context, userID string, plan *store.PlanRow, today string) (planning.ReplanVerdict, error) {
	settled, err := s.plans.FindSettledSessions(ctx, plan.ID, today)
	if err != nil {
		return planning.ReplanVerdict{}, err
	}
	var topicIDs []string
	past := make([]planning.PastSession, len(settled))
	for i, ss := range settled {
		if !slices.Contains(topicIDs, ss.TopicID) {
			topicIDs = append(topicIDs, ss.TopicID)
		}
		past[i] = planning.PastSession{
			ReconciledSession: planning.ReconciledSession{
				ScheduledDate: ss.ScheduledDate, TopicID: ss.TopicID, PlannedQuestions: ss.PlannedQuestions,
				QuestionsAnswered: ss.QuestionsAnswered, QuestionsCorrect: ss.QuestionsCorrect,
			},
			Status: planning.SessionStatus(ss.Status),
		}
	}
	perf, err := s.plans.FindTopicPerformance(ctx, userID, topicIDs)
	if err != nil {
		return planning.ReplanVerdict{}, err
	}
	topics := make([]planning.TopicPerformance, len(perf))
	for i, p := range perf {
		topics[i] = planning.TopicPerformance(p)
	}
	return planning.ShouldReplan(planning.ReplanInputs{
		Today: today, PlanCreatedAt: plan.CreatedAt, Now: time.Now(), PastSessions: past, Topics: topics,
	}), nil
}

// generate builds a plan and makes it current. reason is stored because it is the
// justification the learner is owed when their plan changes under them.
func (s *planService) generate(ctx context.Context, userID string, goal *goals.View, reason string) (*planView, error) {
	today := s.today(ctx, userID)
	daysRemaining, _ := dates.DaysBetween(today, goal.ExamDate)
	if daysRemaining <= 0 {
		return nil, httpx.E(http.StatusBadRequest, "EXAM_DATE_PASSED", "That exam date has passed. Set a new goal to plan again.")
	}

	scopeIDs, err := s.plans.FindGoalScope(ctx, goal.ID)
	if err != nil {
		return nil, err
	}
	topics, err := s.plans.FindPlannableTopics(ctx, userID, scopeIDs)
	if err != nil {
		return nil, err
	}
	if len(topics) == 0 {
		return nil, httpx.E(http.StatusBadRequest, "NOTHING_TO_PLAN",
			"There are no topics in the subjects you picked, so there is nothing to plan yet.")
	}
	profile, err := s.learners.FindProfile(ctx, userID)
	if err != nil {
		return nil, err
	}
	// Only the goal's own subjects: old goals' schedules are history, not work.
	due, err := s.revisions.FindDue(ctx, userID, today, scopeIDs, 20)
	if err != nil {
		return nil, err
	}

	in := planning.PlanInputs{StartDate: today, DaysRemaining: daysRemaining, DailyMinutes: goal.DailyMinutes, Topics: topics}
	// The TS tested truthiness, so 0 seconds counts as unknown too.
	if profile != nil && profile.AvgSecondsPerQuestion != nil && *profile.AvgSecondsPerQuestion != 0 {
		in.SecondsPerQuestion = profile.AvgSecondsPerQuestion
	}
	for _, d := range due {
		in.DueRevisions = append(in.DueRevisions, planning.DueRevision{TopicID: d.TopicID, Name: d.Name,
			SubjectName: d.SubjectName, DueOn: d.DueOn, Lapses: d.Lapses, LastReviewAccuracy: d.LastReviewAccuracy})
	}
	ids := make([]string, len(topics))
	for i, t := range topics {
		ids[i] = t.TopicID
	}
	if in.Prerequisites, err = s.plans.FindPrerequisites(ctx, ids); err != nil {
		return nil, err
	}
	built := planning.BuildPlan(in)
	if len(built.Sessions) == 0 {
		return nil, httpx.E(http.StatusBadRequest, "PLAN_EMPTY",
			"There is not enough time before the exam to plan a useful session. Try a later date or more minutes a day.")
	}

	version, err := s.plans.FindLatestVersion(ctx, goal.ID)
	if err != nil {
		return nil, err
	}
	// Retire the old plan first: one active plan per learner is a database constraint.
	if err := s.plans.SupersedeActive(ctx, userID); err != nil {
		return nil, err
	}
	stored, err := s.plans.CreatePlan(ctx, store.NewPlan{
		UserID: userID, GoalID: goal.ID, Version: version + 1, ExamDate: goal.ExamDate,
		DailyMinutes: goal.DailyMinutes, DaysRemaining: daysRemaining,
		TotalMinutesPlanned: built.TotalMinutesPlanned, TopicsCovered: built.TopicsCovered, Reason: reason,
	})
	if err != nil {
		return nil, err
	}
	if _, err := s.plans.CreateSessions(ctx, stored.ID, built.Sessions); err != nil {
		return nil, err
	}
	view, err := s.view(ctx, stored)
	if err != nil {
		return nil, err
	}
	// A plan that could not fit some subjects should say so, not look complete.
	if len(built.OmittedTopics) > 0 {
		view.OmittedTopics = built.OmittedTopics
	}
	return view, nil
}

// view groups a plan's sessions by date, the only way a plan is ever read.
func (s *planService) view(ctx context.Context, plan *store.PlanRow) (*planView, error) {
	sessions, err := s.plans.FindSessions(ctx, plan.ID)
	if err != nil {
		return nil, err
	}
	v := &planView{
		ID: plan.ID, Version: plan.Version, ExamDate: plan.ExamDate, DailyMinutes: plan.DailyMinutes,
		DaysRemaining: plan.DaysRemaining, TotalMinutesPlanned: plan.TotalMinutesPlanned,
		TopicsCovered: plan.TopicsCovered, Reason: plan.Reason, CreatedAt: plan.CreatedAt, Days: []planDay{},
	}
	index := map[string]int{}
	for _, ss := range sessions {
		i, ok := index[ss.ScheduledDate]
		if !ok {
			i = len(v.Days)
			index[ss.ScheduledDate] = i
			v.Days = append(v.Days, planDay{Date: ss.ScheduledDate, Sessions: []planSession{}})
		}
		name := "Topic"
		if ss.Topics != nil {
			name = ss.Topics.Name
		}
		v.Days[i].TotalMinutes += ss.PlannedMinutes
		v.Days[i].Sessions = append(v.Days[i].Sessions, planSession{
			ID: ss.ID, TopicID: ss.TopicID, TopicName: name, PlannedMinutes: ss.PlannedMinutes,
			PlannedQuestions: ss.PlannedQuestions, Kind: ss.Kind, Reason: ss.Reason, Status: ss.Status,
		})
	}
	slices.SortStableFunc(v.Days, func(a, b planDay) int { return strings.Compare(a.Date, b.Date) })
	return v, nil
}

// GET /study-plan/current — the plan or null, adjusted first if it has drifted from reality.
func (h handlers) getCurrentPlan(w http.ResponseWriter, r *http.Request) error {
	goal, err := h.activeGoal(r)
	if err != nil {
		return err
	}
	plan, adjustment, err := newPlanService(h.d.DB.Admin).refreshCurrent(r.Context(), auth.UserID(r), goal)
	if err != nil {
		return err
	}
	httpx.OK(w, http.StatusOK, map[string]any{"plan": plan, "adjustment": adjustment})
	return nil
}

// POST /study-plan/generate — Admin, because learners hold SELECT only on plans: one who could
// write their own could write themselves an easy one.
func (h handlers) generatePlan(w http.ResponseWriter, r *http.Request) error {
	goal, err := h.activeGoal(r)
	if err != nil {
		return err
	}
	if goal == nil {
		return httpx.E(http.StatusBadRequest, "NO_ACTIVE_GOAL", "Set a learning goal first — the plan is built from it.")
	}
	plan, err := newPlanService(h.d.DB.Admin).generate(r.Context(), auth.UserID(r), goal, "requested")
	if err != nil {
		return err
	}
	httpx.OK(w, http.StatusCreated, map[string]any{"plan": plan})
	return nil
}
