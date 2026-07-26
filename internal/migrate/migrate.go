// Package migrate applies herald's embedded schema migrations. Each
// migration runs in its own transaction and is recorded in
// herald_migrations, which makes Migrate idempotent and safe to call on
// every start.
//
// The queue's own schema is applied separately by the queue library;
// the two share a database but never a table.
package migrate

import (
	"context"
	"embed"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

type migration struct {
	version int
	name    string
	sql     string
}

// Migrate brings the database schema up to the latest embedded
// migration version. Migrations already recorded are skipped, so
// running it against an up-to-date database is a no-op.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS herald_migrations (
			version    int PRIMARY KEY,
			name       text NOT NULL,
			applied_at timestamptz NOT NULL DEFAULT now()
		)`)
	if err != nil {
		return fmt.Errorf("herald: create migrations table: %w", err)
	}

	var current int
	err = pool.QueryRow(ctx,
		`SELECT COALESCE(MAX(version), 0) FROM herald_migrations`).Scan(&current)
	if err != nil {
		return fmt.Errorf("herald: read schema version: %w", err)
	}

	all, err := load()
	if err != nil {
		return err
	}
	for _, m := range all {
		if m.version <= current {
			continue
		}
		if err := apply(ctx, pool, m); err != nil {
			return err
		}
	}
	return nil
}

// apply runs one migration and records it in the same transaction, so a
// migration is never half-applied and never recorded without having
// been applied.
func apply(ctx context.Context, pool *pgxpool.Pool, m migration) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("herald: begin migration %04d: %w", m.version, err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	if _, err := tx.Exec(ctx, m.sql); err != nil {
		return fmt.Errorf("herald: apply migration %04d %s: %w", m.version, m.name, err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO herald_migrations (version, name) VALUES ($1, $2)`,
		m.version, m.name); err != nil {
		return fmt.Errorf("herald: record migration %04d: %w", m.version, err)
	}
	return tx.Commit(ctx)
}

func load() ([]migration, error) {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return nil, fmt.Errorf("herald: read embedded migrations: %w", err)
	}

	all := make([]migration, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		prefix, _, ok := strings.Cut(name, "_")
		if !ok {
			return nil, fmt.Errorf("herald: migration %q: name must be NNNN_description.sql", name)
		}
		version, err := strconv.Atoi(prefix)
		if err != nil {
			return nil, fmt.Errorf("herald: migration %q: invalid version prefix: %w", name, err)
		}
		sql, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return nil, fmt.Errorf("herald: read migration %q: %w", name, err)
		}
		all = append(all, migration{version: version, name: name, sql: string(sql)})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].version < all[j].version })
	return all, nil
}
