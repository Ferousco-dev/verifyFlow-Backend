package auth

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"migo/internal/testutil/dbtest"
	"migo/internal/user"
)

type resetDB struct {
	pool     *pgxpool.Pool
	users    *user.Repository
	sessions *SessionRepository
	resets   *ResetRepository
}

func newResetDB(t *testing.T) *resetDB {
	t.Helper()
	pool := dbtest.NewMigrated(t)
	return &resetDB{pool: pool, users: user.NewRepository(pool), sessions: NewSessionRepository(pool), resets: NewResetRepository(pool)}
}

func (d *resetDB) mkUser(t *testing.T, n string) user.User {
	t.Helper()
	old := "old-hash-" + n
	u, err := d.users.Create(context.Background(), user.User{FullName: "U " + n, Username: "u-" + n, Email: n + "@example.com", PasswordHash: &old})
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func (d *resetDB) token(t *testing.T, userID string, ttl time.Duration) []byte {
	t.Helper()
	_, h, _ := NewRefreshToken()
	now := time.Now()
	if err := d.resets.Create(context.Background(), userID, h, now.Add(ttl), "203.0.113.9", now); err != nil {
		t.Fatal(err)
	}
	return h
}

func (d *resetDB) login(t *testing.T, userID string) {
	t.Helper()
	_, h, _ := NewRefreshToken()
	if err := d.sessions.Create(context.Background(), NewSession{UserID: userID, TokenHash: h, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
}

func (d *resetDB) count(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	if err := d.pool.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestResetCreateStoresDigestAndReplacesUnused(t *testing.T) {
	d := newResetDB(t)
	u := d.mkUser(t, "a")
	first := d.token(t, u.ID, time.Hour)
	second := d.token(t, u.ID, time.Hour)

	if n := d.count(t, `SELECT count(*) FROM password_reset_tokens WHERE user_id = $1::uuid`, u.ID); n != 1 {
		t.Fatalf("only one live token per user, got %d", n)
	}
	ctx := context.Background()
	if ok, _ := d.resets.Valid(ctx, first, time.Now()); ok {
		t.Fatal("older token must be invalidated by a newer request")
	}
	if ok, _ := d.resets.Valid(ctx, second, time.Now()); !ok {
		t.Fatal("newest token must be valid")
	}
	var stored []byte
	var ip string
	_ = d.pool.QueryRow(ctx, `SELECT token_hash, requested_ip FROM password_reset_tokens`).Scan(&stored, &ip)
	if len(stored) != 32 || ip != "203.0.113.9" {
		t.Fatalf("stored digest len=%d ip=%q", len(stored), ip)
	}
	// A different user's token is untouched by this user's requests.
	other := d.mkUser(t, "b")
	otherTok := d.token(t, other.ID, time.Hour)
	d.token(t, u.ID, time.Hour)
	if ok, _ := d.resets.Valid(ctx, otherTok, time.Now()); !ok {
		t.Fatal("another user's token must be unaffected")
	}
}

func TestResetValid(t *testing.T) {
	d := newResetDB(t)
	ctx := context.Background()
	u := d.mkUser(t, "v")
	h := d.token(t, u.ID, time.Hour)

	if ok, err := d.resets.Valid(ctx, h, time.Now()); err != nil || !ok {
		t.Fatalf("fresh: %v %v", ok, err)
	}
	if ok, _ := d.resets.Valid(ctx, h, time.Now().Add(2*time.Hour)); ok {
		t.Fatal("expired must be invalid")
	}
	if ok, _ := d.resets.Valid(ctx, []byte("unknown-unknown-unknown-unknown!"), time.Now()); ok {
		t.Fatal("unknown must be invalid")
	}
	if _, err := d.pool.Exec(ctx, `UPDATE users SET is_active = false WHERE id = $1::uuid`, u.ID); err != nil {
		t.Fatal(err)
	}
	if ok, _ := d.resets.Valid(ctx, h, time.Now()); ok {
		t.Fatal("inactive user's token must be invalid")
	}
}

func TestResetConsumeAppliesEverythingAtomically(t *testing.T) {
	d := newResetDB(t)
	ctx := context.Background()
	u := d.mkUser(t, "c")
	bystander := d.mkUser(t, "bystander")
	d.login(t, u.ID)
	d.login(t, u.ID) // two devices
	d.login(t, bystander.ID)
	h := d.token(t, u.ID, time.Hour)

	// A sibling token (e.g. left over from an earlier request) must be removed too.
	_, sib, _ := NewRefreshToken()
	if _, err := d.pool.Exec(ctx, `INSERT INTO password_reset_tokens (user_id, token_hash, expires_at) VALUES ($1::uuid, $2, now() + interval '1 hour')`, u.ID, sib); err != nil {
		t.Fatal(err)
	}

	res, err := d.resets.Consume(ctx, h, "new-hash", time.Now())
	if err != nil || res.Outcome != ConsumeOK || res.UserID != u.ID {
		t.Fatalf("%+v %v", res, err)
	}

	got, _ := d.users.GetByID(ctx, u.ID)
	if got.PasswordHash == nil || *got.PasswordHash != "new-hash" || !got.EmailVerified {
		t.Fatalf("password/verified not applied: %+v", got)
	}
	if n := d.count(t, `SELECT count(*) FROM refresh_sessions WHERE user_id = $1::uuid AND revoked_at IS NULL`, u.ID); n != 0 {
		t.Fatalf("all sessions must be revoked, %d live", n)
	}
	if n := d.count(t, `SELECT count(*) FROM refresh_sessions WHERE user_id = $1::uuid AND revoked_reason = 'password_reset'`, u.ID); n != 2 {
		t.Fatalf("expected reason=password_reset on 2 sessions, got %d", n)
	}
	if n := d.count(t, `SELECT count(*) FROM refresh_sessions WHERE user_id = $1::uuid AND revoked_at IS NULL`, bystander.ID); n != 1 {
		t.Fatal("another user's session must be untouched")
	}
	if n := d.count(t, `SELECT count(*) FROM password_reset_tokens WHERE user_id = $1::uuid`, u.ID); n != 1 {
		t.Fatalf("only the spent token should remain, got %d rows", n)
	}
	if n := d.count(t, `SELECT count(*) FROM password_reset_tokens WHERE used_at IS NOT NULL`); n != 1 {
		t.Fatal("spent token must be marked used")
	}
}

func TestResetConsumeFailuresHaveNoSideEffects(t *testing.T) {
	d := newResetDB(t)
	ctx := context.Background()
	u := d.mkUser(t, "f")
	d.login(t, u.ID)

	live := func() bool {
		return d.count(t, `SELECT count(*) FROM refresh_sessions WHERE user_id = $1::uuid AND revoked_at IS NULL`, u.ID) == 1
	}
	unchanged := func() bool {
		g, _ := d.users.GetByID(ctx, u.ID)
		return g.PasswordHash != nil && *g.PasswordHash == "old-hash-f" && !g.EmailVerified
	}

	if res, _ := d.resets.Consume(ctx, []byte("unknown-unknown-unknown-unknown!"), "x", time.Now()); res.Outcome != ConsumeNotFound {
		t.Fatalf("not found: %+v", res)
	}
	h := d.token(t, u.ID, time.Hour)
	if res, _ := d.resets.Consume(ctx, h, "x", time.Now().Add(2*time.Hour)); res.Outcome != ConsumeExpired {
		t.Fatalf("expired: %+v", res)
	}
	if _, err := d.pool.Exec(ctx, `UPDATE users SET is_active = false WHERE id = $1::uuid`, u.ID); err != nil {
		t.Fatal(err)
	}
	if res, _ := d.resets.Consume(ctx, h, "x", time.Now()); res.Outcome != ConsumeInactive {
		t.Fatalf("inactive: %+v", res)
	}
	if !live() || !unchanged() {
		t.Fatal("failed attempts must not touch the password or sessions")
	}

	if _, err := d.pool.Exec(ctx, `UPDATE users SET is_active = true WHERE id = $1::uuid`, u.ID); err != nil {
		t.Fatal(err)
	}
	if res, _ := d.resets.Consume(ctx, h, "first", time.Now()); res.Outcome != ConsumeOK {
		t.Fatalf("first use: %+v", res)
	}
	if res, _ := d.resets.Consume(ctx, h, "second", time.Now()); res.Outcome != ConsumeUsed {
		t.Fatalf("second use must be rejected: %+v", res)
	}
	g, _ := d.users.GetByID(ctx, u.ID)
	if *g.PasswordHash != "first" {
		t.Fatalf("a replayed token must not overwrite the password, got %q", *g.PasswordHash)
	}
}

// Ten people race to use the same link: exactly one wins, and the stored
// password is the winner's.
func TestResetConcurrentConsumeSingleWinner(t *testing.T) {
	d := newResetDB(t)
	ctx := context.Background()
	u := d.mkUser(t, "race")
	h := d.token(t, u.ID, time.Hour)

	const n = 10
	res := make([]ConsumeResult, n)
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			res[i], errs[i] = d.resets.Consume(ctx, h, fmt.Sprintf("pw-%d", i), time.Now())
		}(i)
	}
	close(start)
	wg.Wait()

	winner := -1
	for i := range res {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		switch res[i].Outcome {
		case ConsumeOK:
			if winner != -1 {
				t.Fatal("more than one winner: the token was spent twice")
			}
			winner = i
		case ConsumeUsed:
		default:
			t.Fatalf("unexpected outcome %v", res[i].Outcome)
		}
	}
	if winner == -1 {
		t.Fatal("no winner")
	}
	g, _ := d.users.GetByID(ctx, u.ID)
	if *g.PasswordHash != fmt.Sprintf("pw-%d", winner) {
		t.Fatalf("stored password %q is not the winner's", *g.PasswordHash)
	}
}

// Deterministic proof that Consume locks the token row. A competing
// transaction spends the token but hasn't committed; Consume starts and must
// wait, then see the token as used. Without FOR UPDATE it would read the stale
// unused row and succeed, letting one link set the password twice.
func TestResetConsumeSerializesOnRowLock(t *testing.T) {
	d := newResetDB(t)
	ctx := context.Background()
	u := d.mkUser(t, "lock")
	h := d.token(t, u.ID, time.Hour)

	holder, err := d.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	var id string
	if err := holder.QueryRow(ctx, `SELECT id::text FROM password_reset_tokens WHERE token_hash = $1 FOR UPDATE`, h).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(ctx, `UPDATE password_reset_tokens SET used_at = now() WHERE id = $1::uuid`, id); err != nil {
		t.Fatal(err)
	}

	done := make(chan ConsumeResult, 1)
	go func() {
		r, _ := d.resets.Consume(ctx, h, "attacker-pw", time.Now())
		done <- r
	}()
	time.Sleep(400 * time.Millisecond)
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.Outcome != ConsumeUsed {
			t.Fatalf("Consume must observe the competing spend, got outcome %v (token spent twice?)", r.Outcome)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Consume did not finish")
	}
	g, _ := d.users.GetByID(ctx, u.ID)
	if *g.PasswordHash != "old-hash-lock" {
		t.Fatalf("password must be unchanged, got %q", *g.PasswordHash)
	}
}

func TestResetTokensCascadeAndSweep(t *testing.T) {
	d := newResetDB(t)
	ctx := context.Background()
	u := d.mkUser(t, "cas")
	d.token(t, u.ID, time.Hour)
	if _, err := d.pool.Exec(ctx, `DELETE FROM users WHERE id = $1::uuid`, u.ID); err != nil {
		t.Fatal(err)
	}
	if n := d.count(t, `SELECT count(*) FROM password_reset_tokens`); n != 0 {
		t.Fatalf("tokens should cascade-delete, %d remain", n)
	}

	// Long-expired rows from any user are swept on the next Create.
	a := d.mkUser(t, "sweepa")
	b := d.mkUser(t, "sweepb")
	_, old, _ := NewRefreshToken()
	if _, err := d.pool.Exec(ctx, `INSERT INTO password_reset_tokens (user_id, token_hash, expires_at) VALUES ($1::uuid, $2, now() - interval '3 days')`, a.ID, old); err != nil {
		t.Fatal(err)
	}
	d.token(t, b.ID, time.Hour)
	if n := d.count(t, `SELECT count(*) FROM password_reset_tokens WHERE user_id = $1::uuid`, a.ID); n != 0 {
		t.Fatal("expired rows should be swept")
	}
}
