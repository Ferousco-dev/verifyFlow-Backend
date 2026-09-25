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

type sessEnv struct {
	pool     *pgxpool.Pool
	users    *user.Repository
	sessions *SessionRepository
}

func newSessEnv(t *testing.T) *sessEnv {
	t.Helper()
	pool := dbtest.NewMigrated(t)
	return &sessEnv{pool: pool, users: user.NewRepository(pool), sessions: NewSessionRepository(pool)}
}

func (e *sessEnv) mkUser(t *testing.T, n string) user.User {
	t.Helper()
	u, err := e.users.Create(context.Background(), user.User{
		FullName: "User " + n, Username: "user-" + n, Email: n + "@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// login starts a new session family and returns the raw token and its hash.
func (e *sessEnv) login(t *testing.T, userID string, ttl time.Duration) (string, []byte) {
	t.Helper()
	raw, hash, err := NewRefreshToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := e.sessions.Create(context.Background(), NewSession{
		UserID: userID, TokenHash: hash, ExpiresAt: time.Now().Add(ttl), UserAgent: "test-agent", IP: "203.0.113.7",
	}); err != nil {
		t.Fatal(err)
	}
	return raw, hash
}

func nextSession(t *testing.T, ttl time.Duration) NewSession {
	t.Helper()
	_, hash, err := NewRefreshToken()
	if err != nil {
		t.Fatal(err)
	}
	return NewSession{TokenHash: hash, ExpiresAt: time.Now().Add(ttl), UserAgent: "test-agent", IP: "203.0.113.7"}
}

func (e *sessEnv) live(t *testing.T, userID string) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM refresh_sessions WHERE user_id = $1::uuid AND revoked_at IS NULL`, userID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSessionCreateStoresOnlyDigest(t *testing.T) {
	e := newSessEnv(t)
	u := e.mkUser(t, "digest")
	raw, hash := e.login(t, u.ID, time.Hour)

	var stored []byte
	var family, id, ua, ip string
	if err := e.pool.QueryRow(context.Background(),
		`SELECT token_hash, family_id::text, id::text, user_agent, ip FROM refresh_sessions WHERE user_id = $1::uuid`, u.ID).
		Scan(&stored, &family, &id, &ua, &ip); err != nil {
		t.Fatal(err)
	}
	if string(stored) == raw || len(stored) != 32 {
		t.Fatal("raw refresh token must never be stored; expected a 32-byte digest")
	}
	if string(stored) != string(hash) {
		t.Fatal("stored digest mismatch")
	}
	if family != id {
		t.Fatalf("a new login must start its own family (family=%s id=%s)", family, id)
	}
	if ua != "test-agent" || ip != "203.0.113.7" {
		t.Fatalf("metadata not stored: %q %q", ua, ip)
	}
}

func TestSessionDuplicateTokenHashRejected(t *testing.T) {
	e := newSessEnv(t)
	u := e.mkUser(t, "dup")
	_, hash := e.login(t, u.ID, time.Hour)
	err := e.sessions.Create(context.Background(), NewSession{UserID: u.ID, TokenHash: hash, ExpiresAt: time.Now().Add(time.Hour)})
	if err == nil {
		t.Fatal("token_hash must be unique")
	}
}

func TestSessionRotateSuccess(t *testing.T) {
	e := newSessEnv(t)
	ctx := context.Background()
	u := e.mkUser(t, "rotate")
	_, oldHash := e.login(t, u.ID, time.Hour)
	next := nextSession(t, time.Hour)

	res, err := e.sessions.Rotate(ctx, oldHash, next, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != RotateOK || res.UserID != u.ID {
		t.Fatalf("got %+v", res)
	}

	var (
		oldFamily, newFamily, newID, replacedBy string
		reason                                  *string
		lastUsed                                *time.Time
		oldRevoked                              *time.Time
	)
	if err := e.pool.QueryRow(ctx,
		`SELECT family_id::text, revoked_reason, replaced_by::text, last_used_at, revoked_at FROM refresh_sessions WHERE token_hash = $1`, oldHash).
		Scan(&oldFamily, &reason, &replacedBy, &lastUsed, &oldRevoked); err != nil {
		t.Fatal(err)
	}
	if err := e.pool.QueryRow(ctx,
		`SELECT family_id::text, id::text FROM refresh_sessions WHERE token_hash = $1`, next.TokenHash).Scan(&newFamily, &newID); err != nil {
		t.Fatal(err)
	}
	if newFamily != oldFamily {
		t.Fatal("rotated session must stay in the same family")
	}
	if reason == nil || *reason != "rotated" || replacedBy != newID || lastUsed == nil || oldRevoked == nil {
		t.Fatalf("old session not marked rotated correctly: reason=%v replacedBy=%s", reason, replacedBy)
	}
	if e.live(t, u.ID) != 1 {
		t.Fatalf("exactly one live session expected, got %d", e.live(t, u.ID))
	}

	// The new token can itself be rotated (chain continues).
	res, err = e.sessions.Rotate(ctx, next.TokenHash, nextSession(t, time.Hour), time.Now())
	if err != nil || res.Outcome != RotateOK {
		t.Fatalf("second rotation: %+v, %v", res, err)
	}
}

func TestSessionRotateNotFound(t *testing.T) {
	e := newSessEnv(t)
	res, err := e.sessions.Rotate(context.Background(), []byte("does-not-exist-does-not-exist-32"), nextSession(t, time.Hour), time.Now())
	if err != nil || res.Outcome != RotateNotFound {
		t.Fatalf("got %+v, %v", res, err)
	}
}

func TestSessionRotateExpired(t *testing.T) {
	e := newSessEnv(t)
	u := e.mkUser(t, "expired")
	_, hash := e.login(t, u.ID, time.Hour)

	res, err := e.sessions.Rotate(context.Background(), hash, nextSession(t, time.Hour), time.Now().Add(2*time.Hour))
	if err != nil || res.Outcome != RotateExpired {
		t.Fatalf("got %+v, %v", res, err)
	}
	// An expired attempt must not create a replacement session.
	var n int
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM refresh_sessions WHERE user_id = $1::uuid`, u.ID).Scan(&n)
	if n != 1 {
		t.Fatalf("expected 1 row, got %d", n)
	}
}

func TestSessionRotateRevoked(t *testing.T) {
	e := newSessEnv(t)
	ctx := context.Background()
	u := e.mkUser(t, "revoked")
	_, hash := e.login(t, u.ID, time.Hour)

	if err := e.sessions.RevokeFamily(ctx, hash, "logout", time.Now()); err != nil {
		t.Fatal(err)
	}
	res, err := e.sessions.Rotate(ctx, hash, nextSession(t, time.Hour), time.Now())
	if err != nil || res.Outcome != RotateRevoked {
		t.Fatalf("got %+v, %v", res, err)
	}
}

func TestSessionReuseRevokesWholeFamilyAndPersists(t *testing.T) {
	e := newSessEnv(t)
	ctx := context.Background()
	u := e.mkUser(t, "reuse")
	_, first := e.login(t, u.ID, time.Hour)

	second := nextSession(t, time.Hour)
	if res, _ := e.sessions.Rotate(ctx, first, second, time.Now()); res.Outcome != RotateOK {
		t.Fatalf("setup rotation failed: %+v", res)
	}

	// Attacker replays the first (already rotated) token.
	res, err := e.sessions.Rotate(ctx, first, nextSession(t, time.Hour), time.Now())
	if err != nil || res.Outcome != RotateReuseDetected {
		t.Fatalf("expected reuse detection, got %+v, %v", res, err)
	}
	// The revocation must be committed, not rolled back with the "error" path.
	if e.live(t, u.ID) != 0 {
		t.Fatalf("family must be fully revoked, %d live sessions remain", e.live(t, u.ID))
	}
	var reason string
	_ = e.pool.QueryRow(ctx, `SELECT revoked_reason FROM refresh_sessions WHERE token_hash = $1`, second.TokenHash).Scan(&reason)
	if reason != "reuse_detected" {
		t.Fatalf("legit descendant should be revoked with reuse_detected, got %q", reason)
	}
	// The legitimate holder's newest token is now dead too.
	res, _ = e.sessions.Rotate(ctx, second.TokenHash, nextSession(t, time.Hour), time.Now())
	if res.Outcome != RotateRevoked {
		t.Fatalf("descendant should be revoked, got %+v", res)
	}
}

func TestSessionInactiveUserRevokesFamily(t *testing.T) {
	e := newSessEnv(t)
	ctx := context.Background()
	u := e.mkUser(t, "inactive")
	_, hash := e.login(t, u.ID, time.Hour)
	if _, err := e.pool.Exec(ctx, `UPDATE users SET is_active = false WHERE id = $1::uuid`, u.ID); err != nil {
		t.Fatal(err)
	}

	res, err := e.sessions.Rotate(ctx, hash, nextSession(t, time.Hour), time.Now())
	if err != nil || res.Outcome != RotateRevoked {
		t.Fatalf("got %+v, %v", res, err)
	}
	if e.live(t, u.ID) != 0 {
		t.Fatal("sessions of a deactivated user must be revoked")
	}
}

// Concurrent use of one refresh token: FOR UPDATE must serialize the
// attempts so exactly one wins; the rest are treated as reuse.
func TestSessionConcurrentRotationSingleWinner(t *testing.T) {
	e := newSessEnv(t)
	u := e.mkUser(t, "concurrent")
	_, hash := e.login(t, u.ID, time.Hour)

	const n = 20
	results := make([]RotateResult, n)
	errs := make([]error, n)
	nexts := make([]NewSession, n)
	for i := range nexts {
		nexts[i] = nextSession(t, time.Hour)
	}
	start := make(chan struct{}) // release all goroutines at once to maximize overlap
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = e.sessions.Rotate(context.Background(), hash, nexts[i], time.Now())
		}(i)
	}
	close(start)
	wg.Wait()

	ok, reuse := 0, 0
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("rotate error: %v", errs[i])
		}
		switch results[i].Outcome {
		case RotateOK:
			ok++
		case RotateReuseDetected:
			reuse++
		default:
			t.Fatalf("unexpected outcome %v", results[i].Outcome)
		}
	}
	if ok != 1 || reuse != n-1 {
		t.Fatalf("want 1 OK and %d reuse, got ok=%d reuse=%d", n-1, ok, reuse)
	}
	if e.live(t, u.ID) != 0 {
		t.Fatal("reuse must have revoked the family")
	}
}

func TestSessionRevokeFamilyIsScoped(t *testing.T) {
	e := newSessEnv(t)
	ctx := context.Background()
	u := e.mkUser(t, "scoped")
	other := e.mkUser(t, "bystander")

	_, deviceA := e.login(t, u.ID, time.Hour)
	_, deviceB := e.login(t, u.ID, time.Hour) // same user, different login/family
	_, otherHash := e.login(t, other.ID, time.Hour)

	// Rotate device A once so its family has two rows.
	if res, _ := e.sessions.Rotate(ctx, deviceA, nextSession(t, time.Hour), time.Now()); res.Outcome != RotateOK {
		t.Fatal("setup rotation failed")
	}
	if err := e.sessions.RevokeFamily(ctx, deviceA, "logout", time.Now()); err != nil {
		t.Fatal(err)
	}

	if e.live(t, u.ID) != 1 {
		t.Fatalf("only device B should stay live, got %d live", e.live(t, u.ID))
	}
	if res, _ := e.sessions.Rotate(ctx, deviceB, nextSession(t, time.Hour), time.Now()); res.Outcome != RotateOK {
		t.Fatalf("device B must be unaffected, got %v", res.Outcome)
	}
	if e.live(t, other.ID) != 1 {
		t.Fatal("another user's session must be unaffected")
	}
	_ = otherHash

	// Unknown token: no error, nothing revoked.
	if err := e.sessions.RevokeFamily(ctx, []byte("unknown-unknown-unknown-unknown!"), "logout", time.Now()); err != nil {
		t.Fatalf("revoking an unknown token must not error: %v", err)
	}
}

func TestSessionsCascadeOnUserDelete(t *testing.T) {
	e := newSessEnv(t)
	ctx := context.Background()
	u := e.mkUser(t, "cascade")
	_, hash := e.login(t, u.ID, time.Hour)
	if res, _ := e.sessions.Rotate(ctx, hash, nextSession(t, time.Hour), time.Now()); res.Outcome != RotateOK {
		t.Fatal("setup rotation failed")
	}

	if _, err := e.pool.Exec(ctx, `DELETE FROM users WHERE id = $1::uuid`, u.ID); err != nil {
		t.Fatalf("deleting a user with a rotated chain must succeed: %v", err)
	}
	var n int
	_ = e.pool.QueryRow(ctx, `SELECT count(*) FROM refresh_sessions`).Scan(&n)
	if n != 0 {
		t.Fatalf("sessions should cascade-delete, %d remain", n)
	}
}

func TestSessionDoesNotCreateRowForUnknownUser(t *testing.T) {
	e := newSessEnv(t)
	err := e.sessions.Create(context.Background(), NewSession{
		UserID: "00000000-0000-0000-0000-000000000001", TokenHash: []byte(fmt.Sprintf("%032d", 1)), ExpiresAt: time.Now().Add(time.Hour),
	})
	if err == nil {
		t.Fatal("foreign key must reject sessions for non-existent users")
	}
}

// Deterministic proof that Rotate locks the session row (SELECT ... FOR UPDATE).
//
// A competing transaction locks the row and rotates the token itself, but has
// not committed yet. Rotate starts meanwhile. With the lock, Rotate waits, then
// re-reads the committed row, sees it was already rotated and reports reuse.
// Without the lock, Rotate reads the stale un-rotated row, waits only at its
// later UPDATE, and then wrongly succeeds, so the same token would be spent
// twice. (A pure goroutine race test only catches that sporadically.)
func TestSessionRotateSerializesOnRowLock(t *testing.T) {
	e := newSessEnv(t)
	ctx := context.Background()
	u := e.mkUser(t, "locked")
	_, hash := e.login(t, u.ID, time.Hour)

	holder, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }()

	var oldID, familyID string
	if err := holder.QueryRow(ctx,
		`SELECT id::text, family_id::text FROM refresh_sessions WHERE token_hash = $1 FOR UPDATE`, hash).Scan(&oldID, &familyID); err != nil {
		t.Fatal(err)
	}
	winner := nextSession(t, time.Hour)
	var newID string
	if err := holder.QueryRow(ctx,
		`INSERT INTO refresh_sessions (family_id, user_id, token_hash, expires_at)
		 VALUES ($1::uuid, $2::uuid, $3, $4) RETURNING id::text`,
		familyID, u.ID, winner.TokenHash, winner.ExpiresAt).Scan(&newID); err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(ctx,
		`UPDATE refresh_sessions SET revoked_at = now(), revoked_reason = 'rotated', replaced_by = $2::uuid WHERE id = $1::uuid`,
		oldID, newID); err != nil {
		t.Fatal(err)
	}

	done := make(chan RotateResult, 1)
	go func() {
		res, _ := e.sessions.Rotate(ctx, hash, nextSession(t, time.Hour), time.Now())
		done <- res
	}()
	time.Sleep(400 * time.Millisecond) // let Rotate reach its SELECT while the holder is uncommitted

	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case res := <-done:
		if res.Outcome != RotateReuseDetected {
			t.Fatalf("Rotate must observe the competing rotation and report reuse, got outcome %v (token spent twice?)", res.Outcome)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Rotate did not finish after the competing transaction committed")
	}
}
