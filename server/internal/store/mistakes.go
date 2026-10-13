package store

import (
	"context"
	"strconv"
	"strings"
	"time"
)

// maxEvidence is how many attempt ids a pattern remembers: enough to point at recent examples,
// bounded so a misconception repeated for a year does not grow a row without limit.
const maxEvidence = 20

// MisconceptionRow is one recurring misconception, with where to revisit it.
type MisconceptionRow struct {
	TopicID      string    `db:"topic_id"`
	TopicName    string    `db:"topic_name"`
	MasteryScore *float64  `db:"mastery_score"`
	Label        string    `db:"label"`
	Occurrences  int       `db:"occurrences"`
	LastSeenAt   time.Time `db:"last_seen_at"`
	// SourceChunkID is the excerpt behind the latest question that showed it, or else any of the
	// learner's excerpts on the topic; nil when they have no material there.
	SourceChunkID *string `db:"source_chunk_id"`
}

// Mistakes is the learner's misconceptions (026_mistake_patterns.sql). Writes need Admin.
type Mistakes struct{ q Q }

func NewMistakes(q Q) *Mistakes { return &Mistakes{q} }

// Record counts one more occurrence of a misconception, shown by attemptID.
func (r *Mistakes) Record(ctx context.Context, userID, topicID, label, attemptID string) error {
	label = strings.TrimSpace(label)
	if label == "" {
		return nil
	}
	return exec(ctx, r.q, "mistakes.record",
		`insert into mistake_patterns (user_id, topic_id, misconception, evidence_attempt_ids)
		 values ($1, $2, $3, array[$4::uuid])
		 on conflict (user_id, topic_id, lower(misconception)) do update set
		   occurrences = mistake_patterns.occurrences + 1,
		   evidence_attempt_ids = (mistake_patterns.evidence_attempt_ids || excluded.evidence_attempt_ids)
		     [greatest(1, cardinality(mistake_patterns.evidence_attempt_ids) + 2 - `+strconv.Itoa(maxEvidence)+`):],
		   last_seen_at = now()`, userID, topicID, label, attemptID)
}

// Top is the learner's most frequent misconceptions, at most perTopic per topic, weakest topic
// first. topicIDs nil means every topic.
func (r *Mistakes) Top(ctx context.Context, userID string, topicIDs []string, perTopic, limit int) ([]MisconceptionRow, error) {
	return rowsOf[MisconceptionRow](ctx, r.q, "mistakes.top",
		`select topic_id, topic_name, mastery_score, label, occurrences, last_seen_at, source_chunk_id from (
		   select m.topic_id, t.name as topic_name, cm.mastery_score, m.misconception as label, m.occurrences,
		          m.last_seen_at,
		          coalesce(
		            (select q.source_chunk_id from question_attempts a join questions q on q.id = a.question_id
		              where a.id = any(m.evidence_attempt_ids) and q.source_chunk_id is not null
		              order by a.attempted_at desc limit 1),
		            (select c.id from source_chunks c join course_sources s on s.id = c.source_id
		              where c.topic_id = m.topic_id and s.user_id = m.user_id limit 1)
		          )::text as source_chunk_id,
		          row_number() over (partition by m.topic_id order by m.occurrences desc, m.last_seen_at desc) as rank
		   from mistake_patterns m
		   join topics t on t.id = m.topic_id
		   left join concept_mastery cm on cm.user_id = m.user_id and cm.topic_id = m.topic_id
		   where m.user_id = $1 and ($2::uuid[] is null or m.topic_id = any($2))
		 ) ranked
		 where rank <= $3
		 order by coalesce(mastery_score, 0), topic_id, rank
		 limit $4`, userID, topicIDs, perTopic, limit)
}
