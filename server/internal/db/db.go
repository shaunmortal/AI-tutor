// Package db talks to Postgres directly and keeps row-level security in force.
//
// Two ways in, on purpose:
//
//	Admin    bypasses RLS. Only for work that genuinely needs it (grading against the
//	         answer key, writing attempts, updating mastery). Every query must filter by
//	         user id by hand, because nothing else will.
//	AsUser   runs inside a transaction that switches to the `authenticated` role and sets
//	         the JWT claims Supabase's auth.uid() reads. RLS then applies exactly as it
//	         would in the browser, so a forgotten WHERE clause cannot leak another
//	         learner's rows.
package db

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type DB struct {
	Admin *pgxpool.Pool
}

func Open(ctx context.Context, url string) (*DB, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	return &DB{Admin: pool}, nil
}

// AsUser runs fn in a transaction scoped to userID. The settings are LOCAL, so they
// end with the transaction and never leak to the next borrower of the connection.
func (d *DB) AsUser(ctx context.Context, userID string, fn func(pgx.Tx) error) error {
	claims, _ := json.Marshal(map[string]string{"sub": userID, "role": "authenticated"})
	return pgx.BeginFunc(ctx, d.Admin, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `select set_config('request.jwt.claims', $1, true)`, string(claims)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `set local role authenticated`); err != nil {
			return err
		}
		return fn(tx)
	})
}

func (d *DB) Close() { d.Admin.Close() }
