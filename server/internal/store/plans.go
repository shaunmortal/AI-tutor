package store

import (
	"context"
	"time"

	"github.com/shaunmortal/AI-tutor/server/internal/planning"
)

// PlanRow is one study plan.
type PlanRow struct {
	ID                  string    `db:"id" json:"id"`
	GoalID              string    `db:"goal_id" json:"goal_id"`
	Version             int       `db:"version" json:"version"`
	Status              string    `db:"status" json:"status"`
	ExamDate            string    `db:"exam_date" json:"exam_date"`
	DailyMinutes        int       `db:"daily_minutes" json:"daily_minutes"`
	DaysRemaining       int       `db:"days_remaining" json:"days_remaining"`
	TotalMinutesPlanned int       `db:"total_minutes_planned" json:"total_minutes_planned"`
	TopicsCovered       int       `db:"topics_covered" json:"topics_covered"`
	Reason              string    `db:"reason" json:"reason"`
	CreatedAt           time.Time `db:"created_at" json:"created_at"`
}

// SessionRow is one session of a plan with its topic name embedded, as PostgREST's
// `topics(name)` returned it: Topics is nil when the topic could not be read.
type SessionRow struct {
	ID                string        `json:"id"`
	TopicID           string        `json:"topic_id"`
	ScheduledDate     string        `json:"scheduled_date"`
	SortOrder         int           `json:"sort_order"`
	PlannedMinutes    int           `json:"planned_minutes"`
	PlannedQuestions  int           `json:"planned_questions"`
	Kind              string        `json:"kind"`
	Reason            *string       `json:"reason"`
	Status            string        `json:"status"`
	QuestionsAnswered int           `json:"questions_answered"`
	QuestionsCorrect  int           `json:"questions_correct"`
	Topics            *TopicNameRef `json:"topics"`
}

// TopicNameRef is the embedded `topics(name)`.
type TopicNameRef struct {
	Name string `json:"name"`
}

// NewPlan is a plan to create.
type NewPlan struct {
	UserID              string
	GoalID              string
	Version             int
	ExamDate            string
	DailyMinutes        int
	DaysRemaining       int
	TotalMinutesPlanned int
	TopicsCovered       int
	Reason              string
}

// UnreconciledSession is a past session whose outcome is still unrecorded.
type UnreconciledSession struct {
	ID               string `db:"id" json:"id"`
	TopicID          string `db:"topic_id" json:"topic_id"`
	ScheduledDate    string `db:"scheduled_date" json:"scheduled_date"`
	PlannedQuestions int    `db:"planned_questions" json:"planned_questions"`
}

// SettledSession is a past session with its settled outcome.
type SettledSession struct {
	ScheduledDate     string `db:"scheduled_date" json:"scheduled_date"`
	TopicID           string `db:"topic_id" json:"topic_id"`
	PlannedQuestions  int    `db:"planned_questions" json:"planned_questions"`
	QuestionsAnswered int    `db:"questions_answered" json:"questions_answered"`
	QuestionsCorrect  int    `db:"questions_correct" json:"questions_correct"`
	Status            string `db:"status" json:"status"`
}

// DayTally is answers on one topic on one day.
type DayTally struct {
	Answered int `json:"answered"`
	Correct  int `json:"correct"`
}

// Settlement is the outcome to write on one past session.
type Settlement struct {
	ID       string
	Status   planning.SessionStatus
	Answered int
	Correct  int
}

// TopicPerformance is recent form on one topic.
type TopicPerformance struct {
	TopicID               string   `json:"topicId"`
	Name                  string   `json:"name"`
	MasteryScore          *float64 `json:"masteryScore"`
	RecentAccuracyPercent *float64 `json:"recentAccuracyPercent"`
	RecentAttempts        int      `json:"recentAttempts"`
}

const planCols = `id, goal_id, version, status, exam_date::text as exam_date, daily_minutes, days_remaining,
	total_minutes_planned, topics_covered, reason, created_at`

// Plans holds plans and their sessions. Writes need Admin: learners get SELECT only.
type Plans struct{ q Q }

func NewPlans(q Q) *Plans { return &Plans{q} }

func (r *Plans) FindActivePlan(ctx context.Context, userID string) (*PlanRow, error) {
	return rowOf[PlanRow](ctx, r.q, "plans.findActivePlan",
		`select `+planCols+` from study_plans where user_id = $1 and status = 'active'`, userID)
}

// FindSessions is a plan's sessions in the order they should be done.
func (r *Plans) FindSessions(ctx context.Context, planID string) ([]SessionRow, error) {
	type row struct {
		ID                string  `db:"id"`
		TopicID           string  `db:"topic_id"`
		ScheduledDate     string  `db:"scheduled_date"`
		SortOrder         int     `db:"sort_order"`
		PlannedMinutes    int     `db:"planned_minutes"`
		PlannedQuestions  int     `db:"planned_questions"`
		Kind              string  `db:"kind"`
		Reason            *string `db:"reason"`
		Status            string  `db:"status"`
		QuestionsAnswered int     `db:"questions_answered"`
		QuestionsCorrect  int     `db:"questions_correct"`
		TopicName         *string `db:"topic_name"`
	}
	rows, err := rowsOf[row](ctx, r.q, "plans.findSessions",
		`select s.id, s.topic_id, s.scheduled_date::text as scheduled_date, s.sort_order, s.planned_minutes,
		        s.planned_questions, s.kind, s.reason, s.status, s.questions_answered, s.questions_correct,
		        t.name as topic_name
		 from study_sessions s left join topics t on t.id = s.topic_id
		 where s.plan_id = $1 order by s.scheduled_date, s.sort_order`, planID)
	if err != nil {
		return nil, err
	}
	out := make([]SessionRow, len(rows))
	for i, s := range rows {
		out[i] = SessionRow{ID: s.ID, TopicID: s.TopicID, ScheduledDate: s.ScheduledDate, SortOrder: s.SortOrder,
			PlannedMinutes: s.PlannedMinutes, PlannedQuestions: s.PlannedQuestions, Kind: s.Kind, Reason: s.Reason,
			Status: s.Status, QuestionsAnswered: s.QuestionsAnswered, QuestionsCorrect: s.QuestionsCorrect}
		if s.TopicName != nil {
			out[i].Topics = &TopicNameRef{*s.TopicName}
		}
	}
	return out, nil
}

// FindLatestVersion is the highest plan version used against a goal, 0 when none.
func (r *Plans) FindLatestVersion(ctx context.Context, goalID string) (int, error) {
	v, err := colOf[int](ctx, r.q, "plans.findLatestVersion",
		`select coalesce(max(version), 0) from study_plans where goal_id = $1`, goalID)
	if err != nil {
		return 0, err
	}
	return v[0], nil
}

// SupersedeActive retires the current plan; a partial unique index allows one active plan.
func (r *Plans) SupersedeActive(ctx context.Context, userID string) error {
	return exec(ctx, r.q, "plans.supersedeActive",
		`update study_plans set status = 'superseded' where user_id = $1 and status = 'active'`, userID)
}

func (r *Plans) CreatePlan(ctx context.Context, p NewPlan) (*PlanRow, error) {
	return single[PlanRow](ctx, r.q, "plans.createPlan",
		`insert into study_plans (user_id, goal_id, version, exam_date, daily_minutes, days_remaining,
		   total_minutes_planned, topics_covered, reason)
		 values ($1, $2, $3, $4, $5, $6, $7, $8, $9) returning `+planCols,
		p.UserID, p.GoalID, p.Version, p.ExamDate, p.DailyMinutes, p.DaysRemaining, p.TotalMinutesPlanned,
		p.TopicsCovered, p.Reason)
}

// CreateSessions writes every session of a plan in one statement and returns how many were
// written. user_id is filled by a trigger from the plan.
func (r *Plans) CreateSessions(ctx context.Context, planID string, sessions []planning.PlannedSession) (int, error) {
	if len(sessions) == 0 {
		return 0, nil
	}
	n := len(sessions)
	topics, dates, kinds, reasons := make([]string, n), make([]string, n), make([]string, n), make([]string, n)
	orders, minutes, questions := make([]int, n), make([]int, n), make([]int, n)
	for i, s := range sessions {
		topics[i], dates[i], kinds[i], reasons[i] = s.TopicID, s.ScheduledDate, s.Kind, s.Reason
		orders[i], minutes[i], questions[i] = s.SortOrder, s.PlannedMinutes, s.PlannedQuestions
	}
	tag, err := r.q.Exec(ctx,
		`insert into study_sessions (plan_id, topic_id, scheduled_date, sort_order, planned_minutes,
		   planned_questions, kind, reason)
		 select $1, * from unnest($2::uuid[], $3::date[], $4::int[], $5::int[], $6::int[], $7::text[], $8::text[])`,
		planID, topics, dates, orders, minutes, questions, kinds, reasons)
	if err != nil {
		return 0, wrap("plans.createSessions", err)
	}
	return int(tag.RowsAffected()), nil
}

// FindPlannableTopics is the leaf topics in scope (a scope id is a topic or its subject), with
// the learner's mastery on each. Topics never attempted come back with nil mastery.
func (r *Plans) FindPlannableTopics(ctx context.Context, userID string, scopeIDs []string) ([]planning.PlanTopic, error) {
	if len(scopeIDs) == 0 {
		return []planning.PlanTopic{}, nil
	}
	type row struct {
		ID       string  `db:"id"`
		Name     string  `db:"name"`
		Weight   float64 `db:"weight"`
		ParentID *string `db:"parent_id"`
	}
	rows, err := rowsOf[row](ctx, r.q, "plans.findPlannableTopics",
		`select id, name, weight, parent_id from topics where id = any($1::uuid[]) or parent_id = any($1)`, scopeIDs)
	if err != nil {
		return nil, err
	}
	// Subject names come only from subjects in the result, i.e. named in the scope.
	subjectNames := map[string]string{}
	var leaves []row
	leafIDs := []string{}
	for _, t := range rows {
		if t.ParentID == nil {
			subjectNames[t.ID] = t.Name
		} else {
			leaves = append(leaves, t)
			leafIDs = append(leafIDs, t.ID)
		}
	}
	if len(leaves) == 0 {
		return []planning.PlanTopic{}, nil
	}

	type masteryRow struct {
		TopicID       string  `db:"topic_id"`
		MasteryScore  float64 `db:"mastery_score"`
		TotalAttempts int     `db:"total_attempts"`
	}
	mastery, err := rowsOf[masteryRow](ctx, r.q, "plans.findPlannableTopics",
		`select topic_id, mastery_score, total_attempts from concept_mastery
		 where user_id = $1 and topic_id = any($2::uuid[])`, userID, leafIDs)
	if err != nil {
		return nil, err
	}
	byTopic := map[string]masteryRow{}
	for _, m := range mastery {
		byTopic[m.TopicID] = m
	}

	out := make([]planning.PlanTopic, len(leaves))
	for i, leaf := range leaves {
		t := planning.PlanTopic{TopicID: leaf.ID, Name: leaf.Name, Weight: leaf.Weight}
		if name, ok := subjectNames[*leaf.ParentID]; ok {
			t.SubjectName = &name
		}
		if m, ok := byTopic[leaf.ID]; ok {
			t.MasteryScore = &m.MasteryScore
			t.Attempts = m.TotalAttempts
		}
		out[i] = t
	}
	priors, err := selfRatedPriors(ctx, r.q, userID, leafIDs)
	if err != nil {
		return nil, err
	}
	for i := range out {
		if p, ok := priors[out[i].TopicID]; ok && out[i].MasteryScore == nil {
			out[i].PriorMastery = &p
		}
	}
	return out, nil
}

// FindUnreconciledSessions is pending sessions scheduled before `before` (YYYY-MM-DD).
func (r *Plans) FindUnreconciledSessions(ctx context.Context, planID, before string) ([]UnreconciledSession, error) {
	return rowsOf[UnreconciledSession](ctx, r.q, "plans.findUnreconciledSessions",
		`select id, topic_id, scheduled_date::text as scheduled_date, planned_questions from study_sessions
		 where plan_id = $1 and status = 'pending' and scheduled_date < $2`, planID, before)
}

// FindAttemptsByTopicAndDay tallies answers per "topicId|YYYY-MM-DD" over the UTC days
// fromDate..toDate. The day is the UTC date, as the TS sliced PostgREST's UTC timestamp.
func (r *Plans) FindAttemptsByTopicAndDay(ctx context.Context, userID, fromDate, toDate string) (map[string]DayTally, error) {
	type row struct {
		TopicID     string    `db:"topic_id"`
		IsCorrect   bool      `db:"is_correct"`
		AttemptedAt time.Time `db:"attempted_at"`
	}
	rows, err := rowsOf[row](ctx, r.q, "plans.findAttemptsByTopicAndDay",
		`select topic_id, is_correct, attempted_at from question_attempts
		 where user_id = $1 and attempted_at >= ($2 || 'T00:00:00Z')::timestamptz
		   and attempted_at <= ($3 || 'T23:59:59Z')::timestamptz`, userID, fromDate, toDate)
	if err != nil {
		return nil, err
	}
	out := map[string]DayTally{}
	for _, a := range rows {
		key := a.TopicID + "|" + a.AttemptedAt.UTC().Format("2006-01-02")
		t := out[key]
		t.Answered++
		if a.IsCorrect {
			t.Correct++
		}
		out[key] = t
	}
	return out, nil
}

// SettleSessions writes the settled outcome of past sessions, one statement per distinct
// (status, answered, correct). completed_at is set only for 'completed', as the CHECK requires.
func (r *Plans) SettleSessions(ctx context.Context, settlements []Settlement) error {
	type key struct {
		status            planning.SessionStatus
		answered, correct int
	}
	var order []key
	groups := map[key][]string{}
	for _, s := range settlements {
		k := key{s.Status, s.Answered, s.Correct}
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], s.ID)
	}
	for _, k := range order {
		if err := exec(ctx, r.q, "plans.settleSessions",
			`update study_sessions set status = $2, questions_answered = $3, questions_correct = $4,
			   completed_at = case when $2 = 'completed' then now() end
			 where id = any($1::uuid[])`, groups[k], string(k.status), k.answered, k.correct); err != nil {
			return err
		}
	}
	return nil
}

// FindSettledSessions is non-pending sessions scheduled before `before`.
func (r *Plans) FindSettledSessions(ctx context.Context, planID, before string) ([]SettledSession, error) {
	return rowsOf[SettledSession](ctx, r.q, "plans.findSettledSessions",
		`select scheduled_date::text as scheduled_date, topic_id, planned_questions, questions_answered,
		        questions_correct, status
		 from study_sessions where plan_id = $1 and status <> 'pending' and scheduled_date < $2`, planID, before)
}

// FindTopicPerformance is recent form on each topic that has a mastery row.
func (r *Plans) FindTopicPerformance(ctx context.Context, userID string, topicIDs []string) ([]TopicPerformance, error) {
	if len(topicIDs) == 0 {
		return []TopicPerformance{}, nil
	}
	type row struct {
		TopicID        string   `db:"topic_id"`
		MasteryScore   float64  `db:"mastery_score"`
		RecentAccuracy *float64 `db:"recent_accuracy"`
		RecentAttempts int      `db:"recent_attempts"`
		Name           *string  `db:"name"`
	}
	rows, err := rowsOf[row](ctx, r.q, "plans.findTopicPerformance",
		`select m.topic_id, m.mastery_score, m.recent_accuracy, m.recent_attempts, t.name
		 from concept_mastery m left join topics t on t.id = m.topic_id
		 where m.user_id = $1 and m.topic_id = any($2::uuid[])`, userID, topicIDs)
	if err != nil {
		return nil, err
	}
	out := make([]TopicPerformance, len(rows))
	for i, m := range rows {
		name := "Topic"
		if m.Name != nil {
			name = *m.Name
		}
		out[i] = TopicPerformance{TopicID: m.TopicID, Name: name, MasteryScore: &m.MasteryScore,
			RecentAccuracyPercent: m.RecentAccuracy, RecentAttempts: m.RecentAttempts}
	}
	return out, nil
}

// FindGoalScope is the subject ids a goal covers.
func (r *Plans) FindGoalScope(ctx context.Context, goalID string) ([]string, error) {
	return NewGoals(r.q).FindScope(ctx, goalID)
}
