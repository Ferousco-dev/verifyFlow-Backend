package user

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"migo/internal/testutil/dbtest"
)

func newRepo(t *testing.T) *Repository {
	t.Helper()
	return NewRepository(dbtest.NewMigrated(t))
}

func TestRepositoryCreateAndGet(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()
	hash := "$argon2id$v=19$m=8,t=1,p=1$YQ$YQ"

	created, err := repo.Create(ctx, User{FullName: "Ada Lovelace", Username: "ada-lovelace", Email: "ada@example.com", PasswordHash: &hash})
	if err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || len(created.ID) != 36 {
		t.Fatalf("expected generated uuid, got %q", created.ID)
	}
	if !created.IsActive || created.EmailVerified {
		t.Fatalf("defaults wrong: active=%v verified=%v", created.IsActive, created.EmailVerified)
	}
	if time.Since(created.CreatedAt) > time.Minute || created.UpdatedAt.IsZero() {
		t.Fatalf("timestamps not set: %+v", created)
	}

	byEmail, err := repo.GetByEmail(ctx, "ada@example.com")
	if err != nil || byEmail.ID != created.ID {
		t.Fatalf("GetByEmail: %+v, %v", byEmail, err)
	}
	byID, err := repo.GetByID(ctx, created.ID)
	if err != nil || byID.Username != "ada-lovelace" || byID.PasswordHash == nil || *byID.PasswordHash != hash {
		t.Fatalf("GetByID: %+v, %v", byID, err)
	}
}

func TestRepositoryAllowsNullPasswordHash(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()
	u, err := repo.Create(ctx, User{FullName: "OAuth Only", Username: "oauth-only", Email: "oauth@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetByID(ctx, u.ID)
	if err != nil || got.PasswordHash != nil {
		t.Fatalf("expected NULL password hash, got %+v, %v", got.PasswordHash, err)
	}
}

func TestRepositoryNotFound(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()

	if _, err := repo.GetByEmail(ctx, "ghost@example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetByEmail: want ErrNotFound, got %v", err)
	}
	if _, err := repo.GetByID(ctx, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetByID: want ErrNotFound, got %v", err)
	}
	// A malformed id is a caller bug, not "not found"; it must not panic.
	if _, err := repo.GetByID(ctx, "not-a-uuid"); err == nil {
		t.Fatal("expected error for malformed uuid")
	}
}

func TestRepositoryUniquenessMapsToSentinels(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()
	if _, err := repo.Create(ctx, User{FullName: "A", Username: "taken", Email: "a@example.com"}); err != nil {
		t.Fatal(err)
	}

	if _, err := repo.Create(ctx, User{FullName: "B", Username: "other", Email: "a@example.com"}); !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("duplicate email: got %v", err)
	}
	if _, err := repo.Create(ctx, User{FullName: "C", Username: "taken", Email: "c@example.com"}); !errors.Is(err, ErrUsernameTaken) {
		t.Fatalf("duplicate username: got %v", err)
	}
}

func TestRepositoryDatabaseConstraints(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()

	cases := map[string]User{
		"uppercase email":    {FullName: "X", Username: "x1", Email: "Upper@Example.com"},
		"uppercase username": {FullName: "X", Username: "Bad", Email: "x2@example.com"},
		"space in username":  {FullName: "X", Username: "bad name", Email: "x3@example.com"},
		"leading hyphen":     {FullName: "X", Username: "-bad", Email: "x4@example.com"},
		"double hyphen":      {FullName: "X", Username: "a--b", Email: "x5@example.com"},
		"empty username":     {FullName: "X", Username: "", Email: "x6@example.com"},
	}
	for name, u := range cases {
		_, err := repo.Create(ctx, u)
		if err == nil {
			t.Errorf("%s: expected constraint violation", name)
			continue
		}
		if errors.Is(err, ErrEmailTaken) || errors.Is(err, ErrUsernameTaken) {
			t.Errorf("%s: CHECK violation must not map to a uniqueness sentinel: %v", name, err)
		}
	}
}

func TestRepositoryUpdatePasswordHash(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()
	old := "old-hash"
	u, err := repo.Create(ctx, User{FullName: "P", Username: "pw-user", Email: "pw@example.com", PasswordHash: &old})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := repo.UpdatePasswordHash(ctx, u.ID, "new-hash"); err != nil {
		t.Fatal(err)
	}
	got, _ := repo.GetByID(ctx, u.ID)
	if got.PasswordHash == nil || *got.PasswordHash != "new-hash" {
		t.Fatalf("hash not updated: %v", got.PasswordHash)
	}
	if !got.UpdatedAt.After(u.UpdatedAt) {
		t.Fatalf("updated_at should advance: %v -> %v", u.UpdatedAt, got.UpdatedAt)
	}
}

// The DB constraint is authoritative under a race: many concurrent inserts
// of the same email must yield exactly one winner and no other error type.
func TestRepositoryConcurrentDuplicateEmail(t *testing.T) {
	repo := newRepo(t)
	const n = 20
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = repo.Create(context.Background(), User{
				FullName: "Race", Username: fmt.Sprintf("race-%d", i), Email: "race@example.com",
			})
		}(i)
	}
	wg.Wait()

	wins := 0
	for _, err := range errs {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, ErrEmailTaken):
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("expected exactly 1 successful insert, got %d", wins)
	}
}

func TestRepositoryConcurrentDuplicateUsername(t *testing.T) {
	repo := newRepo(t)
	const n = 20
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = repo.Create(context.Background(), User{
				FullName: "Race", Username: "same-name", Email: fmt.Sprintf("u%d@example.com", i),
			})
		}(i)
	}
	wg.Wait()

	wins := 0
	for _, err := range errs {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, ErrUsernameTaken):
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("expected exactly 1 successful insert, got %d", wins)
	}
}
