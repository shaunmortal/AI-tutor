package store

import (
	"context"
	"time"

	"github.com/shaunmortal/AI-tutor/server/internal/learner"
)

// MasteryFormulaVersion is which version of the formula produced the stored numbers.
const MasteryFormulaVersion = 1

// Mastery writes the per-topic mastery cache. Needs Admin: learners may read but never write.
type Mastery struct{ q Q }

func NewMastery(q Q) *Mastery { return &Mastery{q} }

// FindScore is the stored mastery score, or nil when the topic has no row yet.
func (r *Mastery) FindScore(ctx context.Context, userID, topicID string) (*float64, error) {
	scores, err := colOf[float64](ctx, r.q, "mastery.findScore",
		`select mastery_score from concept_mastery where user_id = $1 and topic_id = $2`, userID, topicID)
	if err != nil || len(scores) == 0 {
		return nil, err
	}
	return &scores[0], nil
}

// Save writes the row whole rather than incrementing it, which keeps the "difficulty
// buckets add up to total_attempts" constraint satisfied by construction.
func (r *Mastery) Save(ctx context.Context, userID, topicID string, e learner.TopicEvidence, m learner.MasteryResult,
	lastAttemptAt time.Time, lastCorrectAt *time.Time) error {
	return exec(ctx, r.q, "mastery.save",
		`insert into concept_mastery (user_id, topic_id, mastery_score, total_attempts, correct_attempts,
		   recent_attempts, recent_correct, easy_attempts, easy_correct, medium_attempts, medium_correct,
		   hard_attempts, hard_correct, correct_streak, last_attempt_at, last_correct_at, computed_at, computed_version)
		 values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, now(), $17)
		 on conflict (user_id, topic_id) do update set
		   mastery_score = excluded.mastery_score, total_attempts = excluded.total_attempts,
		   correct_attempts = excluded.correct_attempts, recent_attempts = excluded.recent_attempts,
		   recent_correct = excluded.recent_correct, easy_attempts = excluded.easy_attempts,
		   easy_correct = excluded.easy_correct, medium_attempts = excluded.medium_attempts,
		   medium_correct = excluded.medium_correct, hard_attempts = excluded.hard_attempts,
		   hard_correct = excluded.hard_correct, correct_streak = excluded.correct_streak,
		   last_attempt_at = excluded.last_attempt_at, last_correct_at = excluded.last_correct_at,
		   computed_at = excluded.computed_at, computed_version = excluded.computed_version`,
		userID, topicID, m.Score, e.TotalAttempts, e.CorrectAttempts, e.RecentAttempts, e.RecentCorrect,
		e.Easy.Attempts, e.Easy.Correct, e.Medium.Attempts, e.Medium.Correct, e.Hard.Attempts, e.Hard.Correct,
		e.CorrectStreak, lastAttemptAt, lastCorrectAt, MasteryFormulaVersion)
}
