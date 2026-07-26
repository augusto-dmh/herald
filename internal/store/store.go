// Package store is herald's data access layer: hand-written SQL over a
// pgx pool, one method per query.
//
// Two rules shape the API. Every read is scoped by tenant, and the
// scope is a parameter the caller cannot forget to pass, so a query
// issued on behalf of one tenant cannot return another's rows. And
// writes that must happen together take the caller's transaction
// rather than opening their own — ingest writes a message, its
// deliveries and their queue jobs in one transaction, which is only
// possible if the store lets the caller own it.
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound reports that no row matched — including the case where a
// row exists but belongs to another tenant, which callers must not
// distinguish.
var ErrNotFound = errors.New("store: not found")

// ErrConflict reports that a row would duplicate one that already
// exists, such as a second application with the same uid in a tenant.
var ErrConflict = errors.New("store: conflicting row")

// Store reads and writes herald's tables.
type Store struct {
	pool *pgxpool.Pool
}

// New returns a Store backed by pool.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Begin starts a transaction the caller owns. Ingest uses it to commit
// a message, its deliveries and their queue jobs together: the same
// transaction is passed to this package's Tx methods and to the queue's
// insert, so either everything is visible or nothing is.
func (s *Store) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: begin transaction: %w", err)
	}
	return tx, nil
}

// querier is the subset of pgx shared by a pool and a transaction, so a
// query can be written once and run in either.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// wrap turns a driver error into one callers can act on, without ever
// leaking why a row was invisible.
func wrap(op string, err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("store: %s: %w", op, ErrNotFound)
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
		return fmt.Errorf("store: %s: %w", op, ErrConflict)
	}
	return fmt.Errorf("store: %s: %w", op, err)
}

const uniqueViolation = "23505"
