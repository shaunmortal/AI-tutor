package store

import (
	"context"
	"math"
	"time"

	"github.com/shaunmortal/AI-tutor/server/internal/learner"
)

// DailyActivity is answers per learner-local day over a window.
type DailyActivity struct {
	Counts  map[string]int `json:"counts"`
	Correct int            `json:"correct"`
	Total   int            `json:"total"`
}

// GoalSubject is a subject in the learner's goal with how far through it they are.
type GoalSubject struct {
	ID                string   `json:"id"`
	Name              string   `json:"name"`
	Weight            float64  `json:"weight"`
	TopicCount        int      `json:"topicCount"`
	TopicsStarted     int      `json:"topicsStarted"`
	TopicsMastered    int      `json:"topicsMastered"`
	QuestionsAnswered int      `json:"questionsAnswered"`
	AverageMastery    *float64 `json:"averageMastery"`
}

// AchievementCounts are the badge inputs.
type AchievementCounts struct {
	TopicsMastered   int `db:"topics_mastered" json:"topicsMastered"`
	ReviewsCompleted int `db:"reviews_completed" json:"reviewsCompleted"`
}

// goalSubjectMasteredBar is the mastery score a topic needs to count as mastered on the
// goal-subject progress bars.
const goalSubjectMasteredBar = 85

// Learners is all database access for the learner model. It satisfies learner.ModelSource.
type Learners struct{ q Q }

func NewLearners(q Q) *Learners { return &Learners{q} }

var _ learner.ModelSource = (*Learners)(nil)

// FindTimezone is the learner's chosen timezone, or nil when the profile row is missing.
func (r *Learners) FindTimezone(ctx context.Context, userID string) (*string, error) {
	tz, err := colOf[string](ctx, r.q, "learners.findTimezone", `select timezone from profiles where id = $1`, userID)
	if err != nil || len(tz) == 0 {
		return nil, err
	}
	return &tz[0], nil
}

func (r *Learners) FindProfile(ctx context.Context, userID string) (*learner.ProfileRow, error) {
	return rowOf[learner.ProfileRow](ctx, r.q, "learners.findProfile",
		`select total_attempts, correct_attempts, overall_accuracy, avg_seconds_per_question, consistency_score,
		        current_streak_days, longest_streak_days, last_activity_date::text as last_activity_date,
		        preferred_difficulty, computed_at
		 from learner_profiles where user_id = $1`, userID)
}

// RecordAttemptOnProfile bumps the learner-level counters after one answer and moves the
// habits (streak, speed, consistency) along with them. A missing profile row is a no-op.
func (r *Learners) RecordAttemptOnProfile(ctx context.Context, userID string, wasCorrect bool, today string, secondsTaken *float64) error {
	correct := 0
	if wasCorrect {
		correct = 1
	}
	return r.RecordAnswersOnProfile(ctx, userID, 1, correct, today, secondsTaken)
}

// RecordAnswersOnProfile is RecordAttemptOnProfile for a batch of answers given today (a marked
// paper): the counters move by the batch, the habits by one day's activity. secondsTaken, the
// time per answer, may be nil.
func (r *Learners) RecordAnswersOnProfile(ctx context.Context, userID string, answered, correctNow int, today string, secondsTaken *float64) error {
	if answered <= 0 {
		return nil
	}
	current, err := r.FindProfile(ctx, userID)
	if err != nil {
		return err
	}
	var state learner.HabitState
	var correct int
	if current != nil {
		state = learner.HabitState{
			CurrentStreakDays:     current.CurrentStreakDays,
			LongestStreakDays:     current.LongestStreakDays,
			LastActivityDate:      current.LastActivityDate,
			AvgSecondsPerQuestion: current.AvgSecondsPerQuestion,
			TotalAttempts:         current.TotalAttempts,
		}
		correct = current.CorrectAttempts
	}
	correct += correctNow
	habits := learner.NextHabits(state, learner.HabitEvent{
		Today:        today,
		SecondsTaken: secondsTaken,
		Practice:     r.findPracticeDays(ctx, userID),
	})

	// consistency_score is left alone rather than nulled when it could not be worked out.
	return exec(ctx, r.q, "learners.recordAttemptOnProfile",
		`update learner_profiles set total_attempts = $2, correct_attempts = $3, last_activity_date = $4,
		   current_streak_days = $5, longest_streak_days = $6, avg_seconds_per_question = $7,
		   consistency_score = coalesce($8, consistency_score), computed_at = now()
		 where user_id = $1`,
		userID, state.TotalAttempts+answered, correct, habits.LastActivityDate, habits.CurrentStreakDays,
		habits.LongestStreakDays, habits.AvgSecondsPerQuestion, habits.ConsistencyScore)
}

// findPracticeDays is the distinct (UTC) days this learner has practised on and the first of
// them, or nil when there are none or the query failed — consistency is not worth failing an
// answer over. Note: inside a transaction a failed query still aborts the transaction.
func (r *Learners) findPracticeDays(ctx context.Context, userID string) *learner.PracticeSpan {
	type row struct {
		DistinctDays int     `db:"distinct_days"`
		FirstDate    *string `db:"first_date"`
	}
	// UTC days, as the TS sliced PostgREST's UTC timestamps.
	span, err := rowOf[row](ctx, r.q, "learners.findPracticeDays",
		`select count(distinct (attempted_at at time zone 'UTC')::date)::int as distinct_days,
		        min((attempted_at at time zone 'UTC')::date)::text as first_date
		 from question_attempts where user_id = $1`, userID)
	if err != nil || span == nil || span.FirstDate == nil {
		return nil
	}
	return &learner.PracticeSpan{DistinctDays: span.DistinctDays, FirstDate: *span.FirstDate}
}

// FindDailyActivity counts answers per learner-local day. The query window is the UTC days
// fromDate..toDate; toLocalDate maps each attempt time to the learner's calendar day.
func (r *Learners) FindDailyActivity(ctx context.Context, userID, fromDate, toDate string, toLocalDate func(time.Time) string) (DailyActivity, error) {
	type row struct {
		AttemptedAt time.Time `db:"attempted_at"`
		IsCorrect   bool      `db:"is_correct"`
	}
	rows, err := rowsOf[row](ctx, r.q, "learners.findDailyActivity",
		`select attempted_at, is_correct from question_attempts
		 where user_id = $1 and attempted_at >= ($2 || 'T00:00:00Z')::timestamptz
		   and attempted_at <= ($3 || 'T23:59:59Z')::timestamptz`, userID, fromDate, toDate)
	if err != nil {
		return DailyActivity{}, err
	}
	out := DailyActivity{Counts: map[string]int{}, Total: len(rows)}
	for _, row := range rows {
		out.Counts[toLocalDate(row.AttemptedAt)]++
		if row.IsCorrect {
			out.Correct++
		}
	}
	return out, nil
}

// FindGoalSubjects lists the subjects the active goal selected, in sort order, each with
// coverage (topics with any attempt) and the average mastery over the topics attempted.
func (r *Learners) FindGoalSubjects(ctx context.Context, userID string) ([]GoalSubject, error) {
	subjectIDs, err := activeGoalScope(ctx, r.q, "learners.findGoalSubjects", userID)
	if err != nil {
		return nil, err
	}
	if len(subjectIDs) == 0 {
		return []GoalSubject{}, nil
	}

	type subjectRow struct {
		ID     string  `db:"id"`
		Name   string  `db:"name"`
		Weight float64 `db:"weight"`
	}
	subjects, err := rowsOf[subjectRow](ctx, r.q, "learners.findGoalSubjects",
		`select id, name, weight from topics where id = any($1::uuid[]) order by sort_order`, subjectIDs)
	if err != nil {
		return nil, err
	}
	type leafRow struct {
		ID       string `db:"id"`
		ParentID string `db:"parent_id"`
	}
	leaves, err := rowsOf[leafRow](ctx, r.q, "learners.findGoalSubjects",
		`select id, parent_id from topics where parent_id = any($1::uuid[])`, subjectIDs)
	if err != nil {
		return nil, err
	}
	subjectOf := map[string]string{}
	topicCount := map[string]int{}
	leafIDs := make([]string, len(leaves))
	for i, l := range leaves {
		subjectOf[l.ID] = l.ParentID
		topicCount[l.ParentID]++
		leafIDs[i] = l.ID
	}

	type masteryRow struct {
		TopicID       string  `db:"topic_id"`
		MasteryScore  float64 `db:"mastery_score"`
		TotalAttempts int     `db:"total_attempts"`
	}
	mastery, err := rowsOf[masteryRow](ctx, r.q, "learners.findGoalSubjects",
		`select topic_id, mastery_score, total_attempts from concept_mastery
		 where user_id = $1 and topic_id = any($2::uuid[])`, userID, leafIDs)
	if err != nil {
		return nil, err
	}
	type stat struct {
		started, mastered, answered int
		masterySum                  float64
	}
	stats := map[string]*stat{}
	for _, m := range mastery {
		subjectID, ok := subjectOf[m.TopicID]
		if !ok {
			continue
		}
		s := stats[subjectID]
		if s == nil {
			s = &stat{}
			stats[subjectID] = s
		}
		s.started++
		s.answered += m.TotalAttempts
		s.masterySum += m.MasteryScore
		if m.MasteryScore >= goalSubjectMasteredBar {
			s.mastered++
		}
	}

	out := make([]GoalSubject, len(subjects))
	for i, sub := range subjects {
		g := GoalSubject{ID: sub.ID, Name: sub.Name, Weight: sub.Weight, TopicCount: topicCount[sub.ID]}
		if s := stats[sub.ID]; s != nil {
			g.TopicsStarted, g.TopicsMastered, g.QuestionsAnswered = s.started, s.mastered, s.answered
			if s.started > 0 {
				g.AverageMastery = ptr(math.Round(s.masterySum/float64(s.started)*10) / 10)
			}
		}
		out[i] = g
	}
	return out, nil
}

// FindAchievementCounts is topics at or above masteryBar, and total completed reviews
// (summed review_count: a topic reviewed six times is six reviews).
func (r *Learners) FindAchievementCounts(ctx context.Context, userID string, masteryBar float64) (AchievementCounts, error) {
	c, err := rowOf[AchievementCounts](ctx, r.q, "learners.findAchievementCounts",
		`select (select count(*)::int from concept_mastery where user_id = $1 and mastery_score >= $2) as topics_mastered,
		        (select coalesce(sum(review_count), 0)::int from revision_schedule where user_id = $1) as reviews_completed`,
		userID, masteryBar)
	if err != nil {
		return AchievementCounts{}, err
	}
	return *c, nil
}

// FindMastery is per-topic mastery, weakest first.
func (r *Learners) FindMastery(ctx context.Context, userID string, limit int) ([]learner.TopicMasteryRow, error) {
	type row struct {
		TopicID        string     `db:"topic_id"`
		MasteryScore   float64    `db:"mastery_score"`
		TotalAttempts  int        `db:"total_attempts"`
		Accuracy       *float64   `db:"accuracy"`
		RecentAccuracy *float64   `db:"recent_accuracy"`
		LastAttemptAt  *time.Time `db:"last_attempt_at"`
		Name           *string    `db:"name"`
		Weight         *float64   `db:"weight"`
	}
	rows, err := rowsOf[row](ctx, r.q, "learners.findMastery",
		`select m.topic_id, m.mastery_score, m.total_attempts, m.accuracy, m.recent_accuracy, m.last_attempt_at,
		        t.name, t.weight
		 from concept_mastery m left join topics t on t.id = m.topic_id
		 where m.user_id = $1 order by m.mastery_score asc limit $2`, userID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]learner.TopicMasteryRow, len(rows))
	for i, m := range rows {
		out[i] = learner.TopicMasteryRow{TopicID: m.TopicID, MasteryScore: m.MasteryScore, TotalAttempts: m.TotalAttempts,
			Accuracy: m.Accuracy, RecentAccuracy: m.RecentAccuracy, LastAttemptAt: m.LastAttemptAt}
		if m.Name != nil {
			out[i].Topics = &learner.TopicRef{Name: *m.Name, Weight: *m.Weight}
		}
	}
	return out, nil
}
