package store

import (
	"context"
	"time"

	"github.com/shaunmortal/AI-tutor/server/internal/adaptive"
)

// Topics builds the candidate list the adaptive engine ranks.
type Topics struct{ q Q }

func NewTopics(q Q) *Topics { return &Topics{q} }

// FindCandidatesForLearner lists the leaf topics in the active goal's scope (every leaf topic
// when there is no goal), with the learner's mastery on each. subjectID, when not "", narrows
// the scope to that subject — intersected with the goal, so a subject outside it yields none.
func (r *Topics) FindCandidatesForLearner(ctx context.Context, userID string, now time.Time, subjectID string) ([]adaptive.TopicCandidate, error) {
	goalScope, err := activeGoalScope(ctx, r.q, "topics.findActiveGoalScope", userID)
	if err != nil {
		return nil, err
	}
	scopeIDs := goalScope
	if subjectID != "" {
		scopeIDs = []string{}
		for _, id := range goalScope {
			if id == subjectID {
				scopeIDs = append(scopeIDs, id)
			}
		}
		if len(scopeIDs) == 0 {
			return []adaptive.TopicCandidate{}, nil
		}
	}

	type topicRow struct {
		ID     string  `db:"id"`
		Name   string  `db:"name"`
		Weight float64 `db:"weight"`
	}
	// A scope entry matches the topic itself or its parent subject. Subjects (no parent) are
	// containers, not something to practise.
	topics, err := rowsOf[topicRow](ctx, r.q, "topics.findLeafTopics",
		`select id, name, weight from topics
		 where parent_id is not null and (cardinality($1::uuid[]) = 0 or id = any($1) or parent_id = any($1))`, scopeIDs)
	if err != nil {
		return nil, err
	}
	if len(topics) == 0 {
		return []adaptive.TopicCandidate{}, nil
	}

	type masteryRow struct {
		TopicID        string     `db:"topic_id"`
		MasteryScore   float64    `db:"mastery_score"`
		RecentAccuracy *float64   `db:"recent_accuracy"`
		RecentAttempts int        `db:"recent_attempts"`
		TotalAttempts  int        `db:"total_attempts"`
		LastAttemptAt  *time.Time `db:"last_attempt_at"`
	}
	mastery, err := rowsOf[masteryRow](ctx, r.q, "topics.findMastery",
		`select topic_id, mastery_score, recent_accuracy, recent_attempts, total_attempts, last_attempt_at
		 from concept_mastery where user_id = $1`, userID)
	if err != nil {
		return nil, err
	}
	byTopic := make(map[string]masteryRow, len(mastery))
	for _, m := range mastery {
		byTopic[m.TopicID] = m
	}

	out := make([]adaptive.TopicCandidate, len(topics))
	for i, t := range topics {
		c := adaptive.TopicCandidate{TopicID: t.ID, Name: t.Name, Weight: t.Weight}
		if m, ok := byTopic[t.ID]; ok {
			c.MasteryScore = &m.MasteryScore
			if m.RecentAccuracy != nil {
				// Stored as a percentage; the engine works in 0 to 1.
				c.RecentAccuracy = ptr(*m.RecentAccuracy / 100)
			}
			c.RecentAttempts, c.TotalAttempts = m.RecentAttempts, m.TotalAttempts
			if m.LastAttemptAt != nil {
				c.DaysSinceLastAttempt = ptr(max(0, float64(now.Sub(*m.LastAttemptAt).Milliseconds())/86_400_000))
			}
		}
		out[i] = c
	}

	// What the planner and the engine also need: self-rated priors for unmeasured topics, and
	// prerequisites, so a weak basis is recommended before what builds on it.
	ids := make([]string, len(topics))
	for i, t := range topics {
		ids[i] = t.ID
	}
	priors, err := selfRatedPriors(ctx, r.q, userID, ids)
	if err != nil {
		return nil, err
	}
	prereqs, err := findPrerequisites(ctx, r.q, ids)
	if err != nil {
		return nil, err
	}
	for i := range out {
		if p, ok := priors[out[i].TopicID]; ok && out[i].MasteryScore == nil {
			out[i].PriorMastery = &p
		}
		out[i].Prerequisites = prereqs[out[i].TopicID]
	}
	return out, nil
}

// activeGoalScope is the topic ids the active goal names; empty means "no goal".
func activeGoalScope(ctx context.Context, q Q, op, userID string) ([]string, error) {
	return colOf[string](ctx, q, op,
		`select gt.topic_id::text from learning_goals g join learning_goal_topics gt on gt.goal_id = g.id
		 where g.user_id = $1 and g.status = 'active'`, userID)
}

func ptr[T any](v T) *T { return &v }
