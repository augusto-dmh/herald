//go:build integration

// Package testdb owns the PostgreSQL testcontainer shared by a test
// package and hands each test its own freshly created database, so
// tests never observe each other's rows or schema state.
//
// Herald's schema and the queue's schema live in the same database but
// are applied by different packages, so the database this hands out is
// empty and the caller says which schemas it needs. That keeps the
// helper usable by the migration tests, which must start from nothing.
package testdb

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

var (
	adminURL string
	dbSeq    atomic.Int64
)

// Migrator applies a schema to a database. Both herald's Migrate and
// the queue's satisfy it, so a test names the schemas it needs by
// passing the functions themselves.
type Migrator func(context.Context, *pgxpool.Pool) error

// RunMain starts one PostgreSQL container for the package's tests, runs
// them, and tears the container down. Call it from TestMain:
//
//	func TestMain(m *testing.M) { os.Exit(testdb.RunMain(m)) }
func RunMain(m *testing.M) int {
	ctx := context.Background()

	container, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("postgres"),
		postgres.WithUsername("herald"),
		postgres.WithPassword("herald"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second)),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "testdb: start postgres container: %v\n", err)
		return 1
	}
	defer func() {
		if err := container.Terminate(context.Background()); err != nil {
			fmt.Fprintf(os.Stderr, "testdb: terminate container: %v\n", err)
		}
	}()

	adminURL, err = container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintf(os.Stderr, "testdb: connection string: %v\n", err)
		return 1
	}
	return m.Run()
}

// NewDB creates an empty database in the shared container and returns a
// pool connected to it, applying the given schemas in order. Pool and
// database are cleaned up with the test.
func NewDB(t *testing.T, migrators ...Migrator) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	admin, err := pgxpool.New(ctx, adminURL)
	if err != nil {
		t.Fatalf("testdb: connect admin pool: %v", err)
	}

	name := fmt.Sprintf("herald_test_%d", dbSeq.Add(1))
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		admin.Close()
		t.Fatalf("testdb: create database %s: %v", name, err)
	}

	dbURL, err := url.Parse(adminURL)
	if err != nil {
		admin.Close()
		t.Fatalf("testdb: parse admin url: %v", err)
	}
	dbURL.Path = "/" + name

	pool, err := pgxpool.New(ctx, dbURL.String())
	if err != nil {
		admin.Close()
		t.Fatalf("testdb: connect to %s: %v", name, err)
	}

	t.Cleanup(func() {
		pool.Close()
		if _, err := admin.Exec(context.Background(), "DROP DATABASE "+name); err != nil {
			t.Errorf("testdb: drop database %s: %v", name, err)
		}
		admin.Close()
	})

	for _, migrate := range migrators {
		if err := migrate(ctx, pool); err != nil {
			t.Fatalf("testdb: apply schema to %s: %v", name, err)
		}
	}
	return pool
}
