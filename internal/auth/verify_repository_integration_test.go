package auth

import (
	"context"
	"sync"
	"testing"
	"time"
)

func (d *resetDB) verifyRepo() *VerifyRepository { return NewVerifyRepository(d.pool) }

func (d *resetDB) vtoken(t *testing.T, userID, email string, ttl time.Duration) []byte {
	t.Helper()
	_, h, _ := NewRefreshToken()
	now := time.Now()
	if err := d.verifyRepo().Create(context.Background(), userID, email, h, now.Add(ttl), now); err != nil {
		t.Fatal(err)
	}
	return h
}

func (d *resetDB) verified(t *testing.T, userID string) bool {
	t.Helper()
	u, err := d.users.GetByID(context.Background(), userID)
	if err != nil {
		t.Fatal(err)
	}
	return u.EmailVerified
}

func TestVerifyCreateStoresDigestBoundToEmailAndReplacesUnused(t *testing.T) {
	d := newResetDB(t)
	ctx := context.Background()
	u := d.mkUser(t, "vc")
	first := d.vtoken(t, u.ID, u.Email, time.Hour)
	second := d.vtoken(t, u.ID, u.Email, time.Hour)

	if n := d.count(t, `SELECT count(*) FROM email_verification_tokens WHERE user_id = $1::uuid`, u.ID); n != 1 {
		t.Fatalf("one live token per user, got %d", n)
	}
	if res, _ := d.verifyRepo().Consume(ctx, first, time.Now()); res.Outcome != VerifyNotFound {
		t.Fatalf("older token must be gone: %+v", res)
	}
	var stored []byte
	var email string
	_ = d.pool.QueryRow(ctx, `SELECT token_hash, email FROM email_verification_tokens`).Scan(&stored, &email)
	if len(stored) != 32 || email != u.Email || string(stored) != string(second) {
		t.Fatalf("digest len=%d email=%q", len(stored), email)
	}
	// Another user's token is untouched.
	other := d.mkUser(t, "vc2")
	otherTok := d.vtoken(t, other.ID, other.Email, time.Hour)
	d.vtoken(t, u.ID, u.Email, time.Hour)
	if res, _ := d.verifyRepo().Consume(ctx, otherTok, time.Now()); res.Outcome != VerifyOK {
		t.Fatalf("other user's token must survive: %+v", res)
	}
}

func TestVerifyConsumeEffects(t *testing.T) {
	d := newResetDB(t)
	ctx := context.Background()
	u := d.mkUser(t, "ve")
	h := d.vtoken(t, u.ID, u.Email, time.Hour)
	// A sibling row (e.g. from a race) must be cleaned up too.
	_, sib, _ := NewRefreshToken()
	if _, err := d.pool.Exec(ctx, `INSERT INTO email_verification_tokens (user_id, email, token_hash, expires_at) VALUES ($1::uuid, $2, $3, now() + interval '1 hour')`, u.ID, u.Email, sib); err != nil {
		t.Fatal(err)
	}

	res, err := d.verifyRepo().Consume(ctx, h, time.Now())
	if err != nil || res.Outcome != VerifyOK || res.UserID != u.ID {
		t.Fatalf("%+v %v", res, err)
	}
	if !d.verified(t, u.ID) {
		t.Fatal("email_verified must be set")
	}
	if n := d.count(t, `SELECT count(*) FROM email_verification_tokens WHERE user_id = $1::uuid`, u.ID); n != 1 {
		t.Fatalf("only the spent token should remain, got %d", n)
	}
	if n := d.count(t, `SELECT count(*) FROM email_verification_tokens WHERE used_at IS NOT NULL`); n != 1 {
		t.Fatal("token must be marked used")
	}
	if res, _ := d.verifyRepo().Consume(ctx, h, time.Now()); res.Outcome != VerifyUsed {
		t.Fatalf("replay must be rejected: %+v", res)
	}
}

func TestVerifyConsumeFailuresHaveNoSideEffects(t *testing.T) {
	d := newResetDB(t)
	ctx := context.Background()
	repo := d.verifyRepo()
	u := d.mkUser(t, "vf")

	if res, _ := repo.Consume(ctx, []byte("unknown-unknown-unknown-unknown!"), time.Now()); res.Outcome != VerifyNotFound {
		t.Fatalf("not found: %+v", res)
	}
	h := d.vtoken(t, u.ID, u.Email, time.Hour)
	if res, _ := repo.Consume(ctx, h, time.Now().Add(2*time.Hour)); res.Outcome != VerifyExpired {
		t.Fatalf("expired: %+v", res)
	}
	// Email changed after the token was issued: the token must not verify the NEW address.
	if _, err := d.pool.Exec(ctx, `UPDATE users SET email = 'new-address@example.com' WHERE id = $1::uuid`, u.ID); err != nil {
		t.Fatal(err)
	}
	if res, _ := repo.Consume(ctx, h, time.Now()); res.Outcome != VerifyEmailChanged {
		t.Fatalf("email changed: %+v", res)
	}
	if _, err := d.pool.Exec(ctx, `UPDATE users SET email = $2, is_active = false WHERE id = $1::uuid`, u.ID, u.Email); err != nil {
		t.Fatal(err)
	}
	if res, _ := repo.Consume(ctx, h, time.Now()); res.Outcome != VerifyInactive {
		t.Fatalf("inactive: %+v", res)
	}
	if d.verified(t, u.ID) {
		t.Fatal("no failed attempt may verify the email")
	}
}

func TestVerifyConcurrentConsumeSingleWinner(t *testing.T) {
	d := newResetDB(t)
	ctx := context.Background()
	u := d.mkUser(t, "vr")
	h := d.vtoken(t, u.ID, u.Email, time.Hour)

	const n = 10
	res := make([]VerifyResult, n)
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			res[i], errs[i] = d.verifyRepo().Consume(ctx, h, time.Now())
		}(i)
	}
	close(start)
	wg.Wait()
	ok := 0
	for i := range res {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		switch res[i].Outcome {
		case VerifyOK:
			ok++
		case VerifyUsed:
		default:
			t.Fatalf("unexpected outcome %v", res[i].Outcome)
		}
	}
	if ok != 1 {
		t.Fatalf("exactly one consumer may win, got %d", ok)
	}
}

// Deterministic proof that Consume locks the token row (see the same test for
// refresh/reset tokens). Without FOR UPDATE the stale unused row is read and a
// second spend succeeds.
func TestVerifyConsumeSerializesOnRowLock(t *testing.T) {
	d := newResetDB(t)
	ctx := context.Background()
	u := d.mkUser(t, "vl")
	h := d.vtoken(t, u.ID, u.Email, time.Hour)

	holder, err := d.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	var id string
	if err := holder.QueryRow(ctx, `SELECT id::text FROM email_verification_tokens WHERE token_hash = $1 FOR UPDATE`, h).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(ctx, `UPDATE email_verification_tokens SET used_at = now() WHERE id = $1::uuid`, id); err != nil {
		t.Fatal(err)
	}
	done := make(chan VerifyResult, 1)
	go func() {
		r, _ := d.verifyRepo().Consume(ctx, h, time.Now())
		done <- r
	}()
	time.Sleep(400 * time.Millisecond)
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.Outcome != VerifyUsed {
			t.Fatalf("Consume must observe the competing spend, got %v", r.Outcome)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Consume did not finish")
	}
	if d.verified(t, u.ID) {
		t.Fatal("email must not be verified by a token that was already spent")
	}
}

func TestVerifyTokensCascadeAndSweep(t *testing.T) {
	d := newResetDB(t)
	ctx := context.Background()
	u := d.mkUser(t, "vcas")
	d.vtoken(t, u.ID, u.Email, time.Hour)
	if _, err := d.pool.Exec(ctx, `DELETE FROM users WHERE id = $1::uuid`, u.ID); err != nil {
		t.Fatal(err)
	}
	if n := d.count(t, `SELECT count(*) FROM email_verification_tokens`); n != 0 {
		t.Fatalf("cascade: %d remain", n)
	}
	a, b := d.mkUser(t, "vsa"), d.mkUser(t, "vsb")
	_, old, _ := NewRefreshToken()
	if _, err := d.pool.Exec(ctx, `INSERT INTO email_verification_tokens (user_id, email, token_hash, expires_at) VALUES ($1::uuid, $2, $3, now() - interval '30 days')`, a.ID, a.Email, old); err != nil {
		t.Fatal(err)
	}
	d.vtoken(t, b.ID, b.Email, time.Hour)
	if n := d.count(t, `SELECT count(*) FROM email_verification_tokens WHERE user_id = $1::uuid`, a.ID); n != 0 {
		t.Fatal("long-expired rows should be swept")
	}
}
