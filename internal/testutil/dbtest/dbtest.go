// Package dbtest gives integration tests an isolated PostgreSQL schema.
//
// Tests run only when TEST_DATABASE_URL is set (they are skipped otherwise,
// except under CI, where a missing variable is a hard failure so the suite
// can never silently stop testing the database). Each call creates a
// uniquely named schema and points the pool's search_path at it, so tests
// are isolated from each other, safe to run in parallel, and never touch
// the public schema. The schema is dropped when the test finishes.
package dbtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"migo/internal/database"
	"migo/migrations"
)

func databaseURL(t testing.TB) string {
	t.Helper()
	u := os.Getenv("TEST_DATABASE_URL")
	if u == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("TEST_DATABASE_URL must be set in CI")
		}
		t.Skip("TEST_DATABASE_URL not set; skipping PostgreSQL integration test")
	}
	return u
}

// NewPool returns a pool bound to a fresh, empty schema (no migrations).
func NewPool(t testing.TB) *pgxpool.Pool {
	t.Helper()
	base := databaseURL(t)
	ctx := context.Background()

	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}

	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	schema := "t_" + hex.EncodeToString(b)
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		admin.Close(ctx)
		t.Fatalf("create schema: %v", err)
	}

	cfg, err := pgxpool.ParseConfig(base)
	if err != nil {
		admin.Close(ctx)
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	cfg.MaxConns = 25
	cfg.ConnConfig.RuntimeParams["search_path"] = schema

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		admin.Close(ctx)
		t.Fatalf("create pool: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+quoted+" CASCADE")
		admin.Close(context.Background())
	})
	return pool
}

// NewMigrated returns a pool bound to a fresh schema with all migrations applied.
func NewMigrated(t testing.TB) *pgxpool.Pool {
	t.Helper()
	pool := NewPool(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := database.Migrate(context.Background(), pool, migrations.FS, log); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}
