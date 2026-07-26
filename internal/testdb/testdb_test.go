//go:build integration

package testdb_test

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/augusto-dmh/herald/internal/testdb"
)

func TestMain(m *testing.M) { os.Exit(testdb.RunMain(m)) }

// Every guarantee the rest of the suite leans on: a database starts
// empty, the schemas a test asks for are applied in the order given,
// and no test can see another's rows.
func TestNewDBHandsOutIsolatedDatabases(t *testing.T) {
	ctx := context.Background()

	createWidgets := func(ctx context.Context, pool *pgxpool.Pool) error {
		_, err := pool.Exec(ctx, `CREATE TABLE widgets (id int PRIMARY KEY)`)
		return err
	}
	fillWidgets := func(ctx context.Context, pool *pgxpool.Pool) error {
		_, err := pool.Exec(ctx, `INSERT INTO widgets (id) VALUES (1)`)
		return err
	}

	first := testdb.NewDB(t, createWidgets, fillWidgets)
	var count int
	if err := first.QueryRow(ctx, `SELECT count(*) FROM widgets`).Scan(&count); err != nil {
		t.Fatalf("read widgets after migration: %v", err)
	}
	if count != 1 {
		t.Errorf("widgets holds %d rows, want 1 from the second migrator", count)
	}

	second := testdb.NewDB(t)
	var exists bool
	if err := second.QueryRow(ctx,
		`SELECT to_regclass('public.widgets') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatalf("look for widgets in a second database: %v", err)
	}
	if exists {
		t.Errorf("a freshly created database already holds another test's table")
	}
}
