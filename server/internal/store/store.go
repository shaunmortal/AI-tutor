// Package store holds the SQL for every table, one file per aggregate.
//
// Each repository takes a Q, and the caller decides which one it gets — exactly as the
// TypeScript chose between userDb and adminDb:
//
//	db.AsUser(ctx, userID, func(tx pgx.Tx) error { ... NewGoals(tx) ... })   RLS applies
//	NewGoals(d.DB.Admin)                                                       bypasses RLS
//
// Queries through Admin must filter by user id by hand, because nothing else will.
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Q is satisfied by *pgxpool.Pool and pgx.Tx.
type Q interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// IsNotFound reports whether err is "no rows", so callers can turn it into a 404.
func IsNotFound(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// rowsOf runs sql and scans every row into a T by column name (db tags). Never nil, so an
// empty result encodes as [] like the TS did.
func rowsOf[T any](ctx context.Context, q Q, op, sql string, args ...any) ([]T, error) {
	rows, _ := q.Query(ctx, sql, args...)
	out, err := pgx.CollectRows(rows, pgx.RowToStructByName[T])
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	return out, nil
}

// rowOf is rowsOf for at most one row: (nil, nil) when there is none, which is how the TS
// `.maybeSingle()` answered "not found".
func rowOf[T any](ctx context.Context, q Q, op, sql string, args ...any) (*T, error) {
	rows, _ := q.Query(ctx, sql, args...)
	out, err := pgx.CollectOneRow(rows, pgx.RowToAddrOfStructByName[T])
	if IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	return out, nil
}

// colOf scans a single-column result.
func colOf[T any](ctx context.Context, q Q, op, sql string, args ...any) ([]T, error) {
	rows, _ := q.Query(ctx, sql, args...)
	out, err := pgx.CollectRows(rows, pgx.RowTo[T])
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	return out, nil
}

func exec(ctx context.Context, q Q, op, sql string, args ...any) error {
	if _, err := q.Exec(ctx, sql, args...); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	return nil
}

// distinct keeps the first occurrence of each id, in order — JS's [...new Set(ids)].
func distinct(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	out := []string{}
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

func wrap(op string, err error) error { return fmt.Errorf("%s: %w", op, err) }

// single is rowOf for statements that must produce a row (an insert or update ... returning),
// as `.single()` did: no row is an error that IsNotFound recognises.
func single[T any](ctx context.Context, q Q, op, sql string, args ...any) (*T, error) {
	rows, _ := q.Query(ctx, sql, args...)
	out, err := pgx.CollectOneRow(rows, pgx.RowToAddrOfStructByName[T])
	if err != nil {
		return nil, wrap(op, err)
	}
	return out, nil
}
