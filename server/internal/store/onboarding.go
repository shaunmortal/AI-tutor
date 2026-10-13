package store

import (
	"context"

	"github.com/shaunmortal/AI-tutor/server/internal/learner"
)

// SelfRating is a learner's confidence (1-5) in an area (a subject or a topic).
type SelfRating struct {
	TopicID    string `db:"topic_id" json:"topicId"`
	Confidence int    `db:"confidence" json:"confidence"`
}

// FindSelfRatings is the learner's ratings.
func (r *Learners) FindSelfRatings(ctx context.Context, userID string) ([]SelfRating, error) {
	return rowsOf[SelfRating](ctx, r.q, "learners.findSelfRatings",
		`select topic_id, confidence from self_ratings where user_id = $1 order by topic_id`, userID)
}

// SaveSelfRatings upserts ratings. Admin: learners hold SELECT only.
func (r *Learners) SaveSelfRatings(ctx context.Context, userID string, ratings []SelfRating) error {
	ids, levels := make([]string, len(ratings)), make([]int, len(ratings))
	for i, s := range ratings {
		ids[i], levels[i] = s.TopicID, s.Confidence
	}
	return exec(ctx, r.q, "learners.saveSelfRatings",
		`insert into self_ratings (user_id, topic_id, confidence) select $1, * from unnest($2::uuid[], $3::int[])
		 on conflict (user_id, topic_id) do update set confidence = excluded.confidence`, userID, ids, levels)
}

// selfRatedPriors is the mastery prior for each of these topics the learner rated, directly or
// through its subject (the topic's own rating wins). See learner.SelfRatedPrior.
func selfRatedPriors(ctx context.Context, q Q, userID string, topicIDs []string) (map[string]float64, error) {
	type row struct {
		ID         string `db:"id"`
		Confidence int    `db:"confidence"`
	}
	rows, err := rowsOf[row](ctx, q, "selfRatedPriors",
		`select t.id, coalesce(own.confidence, sub.confidence)::int as confidence from topics t
		 left join self_ratings own on own.user_id = $1 and own.topic_id = t.id
		 left join self_ratings sub on sub.user_id = $1 and sub.topic_id = t.parent_id
		 where t.id = any($2::uuid[]) and coalesce(own.confidence, sub.confidence) is not null`, userID, nonNil(topicIDs))
	if err != nil {
		return nil, err
	}
	out := make(map[string]float64, len(rows))
	for _, r := range rows {
		out[r.ID] = learner.SelfRatedPrior(r.Confidence)
	}
	return out, nil
}

// FindPrerequisites maps each of these topics to the topics it builds on.
func (r *Plans) FindPrerequisites(ctx context.Context, topicIDs []string) (map[string][]string, error) {
	return findPrerequisites(ctx, r.q, topicIDs)
}

func findPrerequisites(ctx context.Context, q Q, topicIDs []string) (map[string][]string, error) {
	rows, err := rowsOf[Prerequisite](ctx, q, "findPrerequisites",
		`select topic_id, prerequisite_id from topic_prerequisites where topic_id = any($1::uuid[])
		 order by topic_id, prerequisite_id`, nonNil(topicIDs))
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, p := range rows {
		out[p.TopicID] = append(out[p.TopicID], p.PrerequisiteID)
	}
	return out, nil
}
