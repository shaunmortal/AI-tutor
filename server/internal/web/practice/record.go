package practice

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"

	"github.com/shaunmortal/AI-tutor/server/internal/app"
	"github.com/shaunmortal/AI-tutor/server/internal/store"
)

// Recorded is what RecordGraded wrote: the attempt and how the topic's mastery moved.
type Recorded struct {
	AttemptID string
	TopicID   string
	Before    *float64
	After     float64
}

// RecordGraded records one answer graded outside this package (the tutor's quick checks) down
// the same path as practice: attempt, mastery, revision schedule and profile, in one transaction.
func RecordGraded(ctx context.Context, d *app.Deps, userID string, q *store.QuestionForGrading, isCorrect bool,
	given json.RawMessage, source string) (Recorded, error) {
	days, err := examDays(ctx, d, userID)
	if err != nil {
		return Recorded{}, err
	}
	before, err := store.NewMastery(d.DB.Admin).FindScore(ctx, userID, q.TopicID)
	if err != nil {
		return Recorded{}, err
	}
	var out Recorded
	err = inTx(ctx, d, func(tx pgx.Tx) error {
		inserted, recs, err := recordAnswers(ctx, tx, userID, source,
			[]graded{{questionID: q.ID, given: given, verdict: verdict{IsCorrect: isCorrect}}}, days)
		if err != nil {
			return err
		}
		at := inserted[0]
		out = Recorded{AttemptID: at.ID, TopicID: at.TopicID, Before: before, After: recs[at.TopicID].score}
		return nil
	})
	return out, err
}
