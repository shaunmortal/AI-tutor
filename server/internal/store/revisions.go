package store

import (
	"context"
	"slices"

	"github.com/shaunmortal/AI-tutor/server/internal/learner"
)

// DueTopic is a topic owed a review.
type DueTopic struct {
	TopicID            string   `json:"topicId"`
	Name               string   `json:"name"`
	SubjectName        *string  `json:"subjectName"`
	DueOn              string   `json:"dueOn"`
	IntervalDays       int      `json:"intervalDays"`
	Lapses             int      `json:"lapses"`
	ReviewCount        int      `json:"reviewCount"`
	LastReviewAccuracy *float64 `json:"lastReviewAccuracy"`
	MasteryScore       *float64 `json:"masteryScore"`
}

// Revisions is the revision schedule. Writes need Admin: learners get SELECT only, so nobody
// can push their own hard topics out of the schedule.
type Revisions struct{ q Q }

func NewRevisions(q Q) *Revisions { return &Revisions{q} }

func (r *Revisions) FindState(ctx context.Context, userID, topicID string) (*learner.ReviewState, error) {
	type row struct {
		IntervalDays   int     `db:"interval_days"`
		EaseFactor     float64 `db:"ease_factor"`
		ReviewCount    int     `db:"review_count"`
		Lapses         int     `db:"lapses"`
		LastReviewedOn *string `db:"last_reviewed_on"`
	}
	s, err := rowOf[row](ctx, r.q, "revisions.findState",
		`select interval_days, ease_factor, review_count, lapses, last_reviewed_on::text as last_reviewed_on
		 from revision_schedule where user_id = $1 and topic_id = $2`, userID, topicID)
	if err != nil || s == nil {
		return nil, err
	}
	return &learner.ReviewState{IntervalDays: s.IntervalDays, EaseFactor: s.EaseFactor, ReviewCount: s.ReviewCount,
		Lapses: s.Lapses, LastReviewedOn: s.LastReviewedOn}, nil
}

// Save writes the schedule for a topic, creating the row on the first review.
func (r *Revisions) Save(ctx context.Context, userID, topicID string, u learner.ScheduleUpdate) error {
	return exec(ctx, r.q, "revisions.save",
		`insert into revision_schedule (user_id, topic_id, due_on, interval_days, ease_factor, review_count,
		   lapses, last_reviewed_on, last_review_accuracy)
		 values ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		 on conflict (user_id, topic_id) do update set
		   due_on = excluded.due_on, interval_days = excluded.interval_days, ease_factor = excluded.ease_factor,
		   review_count = excluded.review_count, lapses = excluded.lapses,
		   last_reviewed_on = excluded.last_reviewed_on, last_review_accuracy = excluded.last_review_accuracy`,
		userID, topicID, u.DueOn, u.IntervalDays, u.EaseFactor, u.ReviewCount, u.Lapses, u.LastReviewedOn, u.LastReviewAccuracy)
}

// FindDue lists topics owed a review, longest overdue first, restricted to scopeIDs (a topic
// or its parent subject; empty means everything). As in the TS, limit applies to the schedule
// rows before the scope filter, so fewer than limit can come back.
func (r *Revisions) FindDue(ctx context.Context, userID, today string, scopeIDs []string, limit int) ([]DueTopic, error) {
	type row struct {
		TopicID            string   `db:"topic_id"`
		DueOn              string   `db:"due_on"`
		IntervalDays       int      `db:"interval_days"`
		Lapses             int      `db:"lapses"`
		ReviewCount        int      `db:"review_count"`
		LastReviewAccuracy *float64 `db:"last_review_accuracy"`
		Name               string   `db:"name"`
		ParentID           *string  `db:"parent_id"`
		SubjectName        *string  `db:"subject_name"`
		MasteryScore       *float64 `db:"mastery_score"`
	}
	// Inner join on the topic: a schedule row whose topic cannot be read is dropped, as before.
	rows, err := rowsOf[row](ctx, r.q, "revisions.findDue",
		`select r.topic_id, r.due_on, r.interval_days, r.lapses, r.review_count, r.last_review_accuracy,
		        t.name, t.parent_id, p.name as subject_name, m.mastery_score
		 from (select topic_id, due_on::text as due_on, interval_days, lapses, review_count, last_review_accuracy
		       from revision_schedule where user_id = $1 and due_on <= $2 order by due_on limit $3) r
		 join topics t on t.id = r.topic_id
		 left join topics p on p.id = t.parent_id
		 left join concept_mastery m on m.user_id = $1 and m.topic_id = r.topic_id
		 order by r.due_on`, userID, today, limit)
	if err != nil {
		return nil, err
	}

	out := []DueTopic{}
	for _, row := range rows {
		inScope := len(scopeIDs) == 0 || slices.Contains(scopeIDs, row.TopicID) ||
			(row.ParentID != nil && slices.Contains(scopeIDs, *row.ParentID))
		if !inScope {
			continue
		}
		var subject *string
		if row.ParentID != nil {
			subject = row.SubjectName
		}
		out = append(out, DueTopic{TopicID: row.TopicID, Name: row.Name, SubjectName: subject, DueOn: row.DueOn,
			IntervalDays: row.IntervalDays, Lapses: row.Lapses, ReviewCount: row.ReviewCount,
			LastReviewAccuracy: row.LastReviewAccuracy, MasteryScore: row.MasteryScore})
	}
	return out, nil
}
