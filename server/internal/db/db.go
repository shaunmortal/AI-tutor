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
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type DB struct {
	Admin *pgxpool.Pool
}

// defaultMaxConns replaces pgx's default (max(4, CPUs)), which starves a request that makes
// several queries. Supabase's free session pooler refuses a 16th client (EMAXCONNSESSION), so
// stay under 15 with room for psql and migrations. On a bigger plan raise it with
// ?pool_max_conns=N in DATABASE_URL.
const defaultMaxConns = 12

func Open(ctx context.Context, url string) (*DB, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	if !strings.Contains(url, "pool_max_conns") {
		cfg.MaxConns = defaultMaxConns
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
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
		// One round trip for both settings: set_config('role', …, true) is SET LOCAL ROLE.
		if _, err := tx.Exec(ctx,
			`select set_config('request.jwt.claims', $1, true), set_config('role', 'authenticated', true)`,
			string(claims)); err != nil {
			return err
		}
		return fn(tx)
	})
}

func (d *DB) Close() { d.Admin.Close() }
