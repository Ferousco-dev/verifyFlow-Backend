package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"migo/internal/testutil/dbtest"
	"migo/internal/user"
)

func newRealService(t *testing.T) (*Service, *user.Repository, *pgxpool.Pool) {
	t.Helper()
	pool := dbtest.NewMigrated(t)
	users := user.NewRepository(pool)
	svc, err := NewService(users, NewSessionRepository(pool), NewHasher(testParams, 4),
		NewTokenManager(testSecret(), "migo", 15*time.Minute), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return svc, users, pool
}

func TestIntegrationFullAuthFlow(t *testing.T) {
	svc, _, _ := newRealService(t)
	ctx := context.Background()
	meta := Meta{UserAgent: "it", IP: "198.51.100.1"}

	u, reg, err := svc.Register(ctx, RegisterInput{FullName: "Feranmi Oresajo", Email: " Fer@Example.com", Password: "a-long-password"}, meta)
	if err != nil {
		t.Fatal(err)
	}
	if u.Username != "feranmi-oresajo" || u.Email != "fer@example.com" {
		t.Fatalf("got %q / %q", u.Username, u.Email)
	}

	// Access token authenticates /me equivalent.
	uid, err := svc.tokens.Parse(reg.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if me, err := svc.Me(ctx, uid); err != nil || me.ID != u.ID {
		t.Fatalf("Me: %+v, %v", me, err)
	}

	// Login creates an independent second session.
	_, login, err := svc.Login(ctx, LoginInput{Email: "FER@example.com", Password: "a-long-password"}, meta)
	if err != nil {
		t.Fatal(err)
	}

	// Rotate session 1; replay of its old token kills only session 1's family.
	rotated, err := svc.Refresh(ctx, reg.RefreshToken, meta)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Refresh(ctx, reg.RefreshToken, meta); !errors.Is(err, ErrInvalidRefreshToken) {
		t.Fatalf("reuse should be rejected, got %v", err)
	}
	if _, err := svc.Refresh(ctx, rotated.RefreshToken, meta); !errors.Is(err, ErrInvalidRefreshToken) {
		t.Fatalf("family should be revoked after reuse, got %v", err)
	}
	if _, err := svc.Refresh(ctx, login.RefreshToken, meta); err != nil {
		t.Fatalf("the other device's session must survive: %v", err)
	}

	// Logout revokes.
	_, l2, _ := svc.Login(ctx, LoginInput{Email: "fer@example.com", Password: "a-long-password"}, meta)
	if err := svc.Logout(ctx, l2.RefreshToken); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Refresh(ctx, l2.RefreshToken, meta); !errors.Is(err, ErrInvalidRefreshToken) {
		t.Fatalf("refresh after logout: %v", err)
	}
}

func TestIntegrationDuplicateEmailAndBadLogin(t *testing.T) {
	svc, _, _ := newRealService(t)
	ctx := context.Background()
	if _, _, err := svc.Register(ctx, RegisterInput{FullName: "One", Email: "dup@example.com", Password: "a-long-password"}, Meta{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Register(ctx, RegisterInput{FullName: "Two", Email: "DUP@example.com", Password: "a-long-password"}, Meta{}); !errors.Is(err, user.ErrEmailTaken) {
		t.Fatalf("got %v", err)
	}
	if _, _, err := svc.Login(ctx, LoginInput{Email: "dup@example.com", Password: "wrong-password"}, Meta{}); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("got %v", err)
	}
}

// Concurrent sign-ups with the same name must all succeed with distinct
// usernames: the DB constraint arbitrates and the service retries.
func TestIntegrationConcurrentSameNameGetsUniqueUsernames(t *testing.T) {
	svc, _, _ := newRealService(t)
	const n = 15
	var wg sync.WaitGroup
	users := make([]user.User, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			users[i], _, errs[i] = svc.Register(context.Background(),
				RegisterInput{FullName: "Ada Lovelace", Email: fmt.Sprintf("ada%d@example.com", i), Password: "a-long-password"}, Meta{})
		}(i)
	}
	wg.Wait()

	seen := map[string]bool{}
	plain := 0
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("register %d: %v", i, errs[i])
		}
		if seen[users[i].Username] {
			t.Fatalf("duplicate username %q", users[i].Username)
		}
		seen[users[i].Username] = true
		if users[i].Username == "ada-lovelace" {
			plain++
		} else if !strings.HasPrefix(users[i].Username, "ada-lovelace-") {
			t.Fatalf("unexpected variant %q", users[i].Username)
		}
	}
	if plain != 1 {
		t.Fatalf("exactly one user should hold the plain username, got %d", plain)
	}
}

func TestIntegrationConcurrentSameEmailSingleWinner(t *testing.T) {
	svc, _, _ := newRealService(t)
	const n = 12
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, errs[i] = svc.Register(context.Background(),
				RegisterInput{FullName: fmt.Sprintf("Racer %d", i), Email: "same@example.com", Password: "a-long-password"}, Meta{})
		}(i)
	}
	wg.Wait()

	wins := 0
	for _, err := range errs {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, user.ErrEmailTaken):
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("want exactly 1 account, got %d", wins)
	}
}

func TestIntegrationLoginPersistsHashUpgrade(t *testing.T) {
	svc, users, _ := newRealService(t)
	ctx := context.Background()
	u, _, err := svc.Register(ctx, RegisterInput{FullName: "Up Grade", Email: "up@example.com", Password: "a-long-password"}, Meta{})
	if err != nil {
		t.Fatal(err)
	}
	stronger := testParams
	stronger.Time = 2
	svc.hasher = NewHasher(stronger, 2)

	if _, _, err := svc.Login(ctx, LoginInput{Email: "up@example.com", Password: "a-long-password"}, Meta{}); err != nil {
		t.Fatal(err)
	}
	got, err := users.GetByID(ctx, u.ID)
	if err != nil || got.PasswordHash == nil || !strings.Contains(*got.PasswordHash, "t=2") {
		t.Fatalf("upgraded hash not persisted: %v %v", got.PasswordHash, err)
	}
	// And the upgraded hash still verifies.
	if _, _, err := svc.Login(ctx, LoginInput{Email: "up@example.com", Password: "a-long-password"}, Meta{}); err != nil {
		t.Fatalf("login with upgraded hash: %v", err)
	}
}

func TestIntegrationDeactivatedUser(t *testing.T) {
	svc, _, pool := newRealService(t)
	ctx := context.Background()
	u, tok, err := svc.Register(ctx, RegisterInput{FullName: "Soon Gone", Email: "gone@example.com", Password: "a-long-password"}, Meta{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET is_active = false WHERE id = $1::uuid`, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Me(ctx, u.ID); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("Me for inactive user: %v", err)
	}
	if _, err := svc.Refresh(ctx, tok.RefreshToken, Meta{}); !errors.Is(err, ErrInvalidRefreshToken) {
		t.Fatalf("Refresh for inactive user: %v", err)
	}
	if _, _, err := svc.Login(ctx, LoginInput{Email: "gone@example.com", Password: "a-long-password"}, Meta{}); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("Login for inactive user: %v", err)
	}
}
