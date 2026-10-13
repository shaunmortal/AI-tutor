package store

import (
	"context"
	"time"
)

// AccountProfile is the learner's own `profiles` row: what to call them and their timezone.
type AccountProfile struct {
	DisplayName *string   `db:"display_name" json:"display_name"`
	Email       *string   `db:"email" json:"email"`
	Timezone    *string   `db:"timezone" json:"timezone"`
	CreatedAt   time.Time `db:"created_at" json:"created_at"`
}

const accountProfileCols = `display_name, email, timezone, created_at`

// FindAccountProfile is nil when the row cannot be read.
func (r *Learners) FindAccountProfile(ctx context.Context, userID string) (*AccountProfile, error) {
	return rowOf[AccountProfile](ctx, r.q, "learners.findAccountProfile",
		`select `+accountProfileCols+` from profiles where id = $1`, userID)
}

// UpdateAccountProfile sets whichever of displayName and timezone are non-nil. Run it as the
// learner: the column grant on `profiles` is what keeps id and email out of reach.
func (r *Learners) UpdateAccountProfile(ctx context.Context, userID string, displayName, timezone *string) (*AccountProfile, error) {
	return single[AccountProfile](ctx, r.q, "learners.updateAccountProfile",
		`update profiles set display_name = coalesce($2, display_name), timezone = coalesce($3, timezone)
		 where id = $1 returning `+accountProfileCols, userID, displayName, timezone)
}
