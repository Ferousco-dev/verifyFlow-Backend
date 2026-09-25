package database_test

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"migo/internal/database"
	"migo/internal/testutil/dbtest"
	"migo/migrations"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestMigrateAppliesAndIsIdempotent(t *testing.T) {
	pool := dbtest.NewPool(t)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if err := database.Migrate(ctx, pool, migrations.FS, quiet); err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
	}

	entries, _ := migrations.FS.ReadDir(".")
	want := 0
	for _, e := range entries {
		if len(e.Name()) > 4 && e.Name()[len(e.Name())-4:] == ".sql" {
			want++
		}
	}
	var got int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("schema_migrations has %d rows, want %d", got, want)
	}
	for _, table := range []string{"users", "refresh_sessions", "password_reset_tokens"} {
		var reg *string
		if err := pool.QueryRow(ctx, `SELECT to_regclass($1)::text`, table).Scan(&reg); err != nil || reg == nil {
			t.Fatalf("table %s missing (err=%v)", table, err)
		}
	}
}

func TestMigrateConcurrentInstances(t *testing.T) {
	pool := dbtest.NewPool(t)
	ctx := context.Background()

	var wg sync.WaitGroup
	errs := make(chan error, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- database.Migrate(ctx, pool, migrations.FS, quiet)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent migrate failed: %v", err)
		}
	}
	entries, _ := migrations.FS.ReadDir(".")
	want := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			want++
		}
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&n); err != nil || n != want {
		t.Fatalf("expected %d recorded migrations, got %d (err=%v)", want, n, err)
	}
}

func TestMigrateRollsBackFailedMigration(t *testing.T) {
	pool := dbtest.NewPool(t)
	ctx := context.Background()

	fsys := fstest.MapFS{
		"0001_ok.sql":  {Data: []byte(`CREATE TABLE ok_table (id int);`)},
		"0002_bad.sql": {Data: []byte(`CREATE TABLE half_table (id int); SELECT * FROM does_not_exist;`)},
	}
	if err := database.Migrate(ctx, pool, fsys, quiet); err == nil {
		t.Fatal("expected migration failure")
	}

	var okReg, halfReg *string
	_ = pool.QueryRow(ctx, `SELECT to_regclass('ok_table')::text`).Scan(&okReg)
	_ = pool.QueryRow(ctx, `SELECT to_regclass('half_table')::text`).Scan(&halfReg)
	if okReg == nil {
		t.Fatal("first migration should have been kept")
	}
	if halfReg != nil {
		t.Fatal("failed migration must be rolled back atomically")
	}
	var versions []string
	rows, err := pool.Query(ctx, `SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var v string
		_ = rows.Scan(&v)
		versions = append(versions, v)
	}
	if len(versions) != 1 || versions[0] != "0001_ok.sql" {
		t.Fatalf("recorded versions = %v, want [0001_ok.sql]", versions)
	}
}
