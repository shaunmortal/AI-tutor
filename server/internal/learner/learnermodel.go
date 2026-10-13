package learner

import (
	"context"
	"time"
)

// The learner model service: reads the cached learner model and shapes it for the dashboard.
// No scoring happens here; mastery and consistency are computed by their own engines and
// stored. The row types live here rather than in store so store can import this package.

// ProfileRow is the learner-level cache row from 007_learner_profiles.
type ProfileRow struct {
	TotalAttempts         int      `db:"total_attempts" json:"total_attempts"`
	CorrectAttempts       int      `db:"correct_attempts" json:"correct_attempts"`
	OverallAccuracy       *float64 `db:"overall_accuracy" json:"overall_accuracy"`
	AvgSecondsPerQuestion *float64 `db:"avg_seconds_per_question" json:"avg_seconds_per_question"`
	ConsistencyScore      *float64 `db:"consistency_score" json:"consistency_score"`
	CurrentStreakDays     int      `db:"current_streak_days" json:"current_streak_days"`
	LongestStreakDays     int      `db:"longest_streak_days" json:"longest_streak_days"`
	// YYYY-MM-DD.
	LastActivityDate    *string   `db:"last_activity_date" json:"last_activity_date"`
	PreferredDifficulty *string   `db:"preferred_difficulty" json:"preferred_difficulty"`
	ComputedAt          time.Time `db:"computed_at" json:"computed_at"`
}

// TopicMasteryRow is one per-topic mastery row with the topic embedded, as PostgREST's
// `topics(name, weight)` returned it: Topics is nil when the topic could not be read.
type TopicMasteryRow struct {
	TopicID        string     `json:"topic_id"`
	MasteryScore   float64    `json:"mastery_score"`
	TotalAttempts  int        `json:"total_attempts"`
	Accuracy       *float64   `json:"accuracy"`
	RecentAccuracy *float64   `json:"recent_accuracy"`
	LastAttemptAt  *time.Time `json:"last_attempt_at"`
	Topics         *TopicRef  `json:"topics"`
}

// TopicRef is the embedded topic of a TopicMasteryRow.
type TopicRef struct {
	Name   string  `json:"name"`
	Weight float64 `json:"weight"`
}

// ModelSource is the slice of the learner repository (store.Learners) the summary reads.
type ModelSource interface {
	FindProfile(ctx context.Context, userID string) (*ProfileRow, error)
	FindMastery(ctx context.Context, userID string, limit int) ([]TopicMasteryRow, error)
}

// Summary is what the dashboard needs in one request.
type Summary struct {
	HasActivity bool `json:"hasActivity"`
	Attempts    struct {
		Total           int      `json:"total"`
		Correct         int      `json:"correct"`
		AccuracyPercent *float64 `json:"accuracyPercent"`
	} `json:"attempts"`
	Habits struct {
		CurrentStreakDays     int      `json:"currentStreakDays"`
		LongestStreakDays     int      `json:"longestStreakDays"`
		ConsistencyPercent    *float64 `json:"consistencyPercent"`
		LastActiveDate        *string  `json:"lastActiveDate"`
		AvgSecondsPerQuestion *float64 `json:"avgSecondsPerQuestion"`
	} `json:"habits"`
	PreferredDifficulty *string     `json:"preferredDifficulty"`
	WeakestTopics       []WeakTopic `json:"weakestTopics"`
	// When the cached numbers were last recalculated.
	ComputedAt *time.Time `json:"computedAt"`
}

// WeakTopic is one of the learner's weakest topics.
type WeakTopic struct {
	TopicID               string   `json:"topicId"`
	Name                  string   `json:"name"`
	MasteryScore          float64  `json:"masteryScore"`
	Attempts              int      `json:"attempts"`
	AccuracyPercent       *float64 `json:"accuracyPercent"`
	RecentAccuracyPercent *float64 `json:"recentAccuracyPercent"`
}

// weakTopicLimit is how many weak topics the summary carries.
const weakTopicLimit = 5

// ModelService assembles the learner summary.
type ModelService struct{ learners ModelSource }

func NewModelService(learners ModelSource) *ModelService { return &ModelService{learners} }

// GetSummary is the dashboard summary. A learner with no activity gets zeros and nils, not
// an error: it is a real state the UI shows as empty.
func (s *ModelService) GetSummary(ctx context.Context, userID string) (Summary, error) {
	var out Summary
	profile, err := s.learners.FindProfile(ctx, userID)
	if err != nil {
		return out, err
	}
	mastery, err := s.learners.FindMastery(ctx, userID, weakTopicLimit)
	if err != nil {
		return out, err
	}

	if profile != nil {
		out.HasActivity = profile.TotalAttempts > 0
		out.Attempts.Total = profile.TotalAttempts
		out.Attempts.Correct = profile.CorrectAttempts
		out.Attempts.AccuracyPercent = profile.OverallAccuracy
		out.Habits.CurrentStreakDays = profile.CurrentStreakDays
		out.Habits.LongestStreakDays = profile.LongestStreakDays
		out.Habits.ConsistencyPercent = profile.ConsistencyScore
		out.Habits.LastActiveDate = profile.LastActivityDate
		out.Habits.AvgSecondsPerQuestion = profile.AvgSecondsPerQuestion
		out.PreferredDifficulty = profile.PreferredDifficulty
		out.ComputedAt = &profile.ComputedAt
	}

	out.WeakestTopics = make([]WeakTopic, len(mastery))
	for i, row := range mastery {
		name := "Unknown topic"
		if row.Topics != nil {
			name = row.Topics.Name
		}
		out.WeakestTopics[i] = WeakTopic{TopicID: row.TopicID, Name: name, MasteryScore: row.MasteryScore,
			Attempts: row.TotalAttempts, AccuracyPercent: row.Accuracy, RecentAccuracyPercent: row.RecentAccuracy}
	}
	return out, nil
}
