package auth

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

func (d *resetDB) credRepo() *CredentialRepository { return NewCredentialRepository(d.pool) }

func newChange(userID, expected, newHash string) (PasswordChange, []byte) {
	_, h, _ := NewRefreshToken()
	return PasswordChange{
		UserID: userID, ExpectedHash: expected, NewHash: newHash,
		Session: NewSession{UserID: userID, TokenHash: h, ExpiresAt: time.Now().Add(time.Hour), UserAgent: "ua", IP: "203.0.113.8"},
	}, h
}

func (d *resetDB) hashOf(t *testing.T, userID string) string {
	t.Helper()
	u, err := d.users.GetByID(context.Background(), userID)
	if err != nil {
		t.Fatal(err)
	}
	return *u.PasswordHash
}

func TestChangePasswordAppliesEverythingAtomically(t *testing.T) {
	d := newResetDB(t)
	ctx := context.Background()
	u := d.mkUser(t, "cp") // hash "old-hash-cp"
	bystander := d.mkUser(t, "cpb")
	d.login(t, u.ID)
	d.login(t, u.ID)
	d.login(t, bystander.ID)
	d.token(t, u.ID, time.Hour) // pending reset link for u
	bystanderReset := d.token(t, bystander.ID, time.Hour)

	change, newTok := newChange(u.ID, "old-hash-cp", "new-hash")
	out, err := d.credRepo().ChangePassword(ctx, change, time.Now())
	if err != nil || out != ChangeOK {
		t.Fatalf("%v %v", out, err)
	}

	if d.hashOf(t, u.ID) != "new-hash" {
		t.Fatal("password hash not updated")
	}
	if n := d.count(t, `SELECT count(*) FROM refresh_sessions WHERE user_id = $1::uuid AND revoked_reason = 'password_changed'`, u.ID); n != 2 {
		t.Fatalf("both old sessions must be revoked with reason, got %d", n)
	}
	// Exactly one live session remains: the caller's new one, stored as a digest.
	var live int
	var hash []byte
	var ua, ip, family, id string
	if err := d.pool.QueryRow(ctx,
		`SELECT count(*) OVER (), token_hash, user_agent, ip, family_id::text, id::text
		   FROM refresh_sessions WHERE user_id = $1::uuid AND revoked_at IS NULL`, u.ID).Scan(&live, &hash, &ua, &ip, &family, &id); err != nil {
		t.Fatal(err)
	}
	if live != 1 || string(hash) != string(newTok) || ua != "ua" || ip != "203.0.113.8" || family != id {
		t.Fatalf("new session wrong: live=%d ua=%q ip=%q family==id:%v", live, ua, ip, family == id)
	}
	if n := d.count(t, `SELECT count(*) FROM password_reset_tokens WHERE user_id = $1::uuid`, u.ID); n != 0 {
		t.Fatal("pending reset tokens must be deleted")
	}
	// Bystander untouched.
	if n := d.count(t, `SELECT count(*) FROM refresh_sessions WHERE user_id = $1::uuid AND revoked_at IS NULL`, bystander.ID); n != 1 {
		t.Fatal("another user's session must be untouched")
	}
	if ok, _ := d.resets.Valid(ctx, bystanderReset, time.Now()); !ok {
		t.Fatal("another user's reset token must be untouched")
	}
}

func TestChangePasswordCASConflictChangesNothing(t *testing.T) {
	d := newResetDB(t)
	ctx := context.Background()
	u := d.mkUser(t, "cas")
	d.login(t, u.ID)
	resetTok := d.token(t, u.ID, time.Hour)

	// The caller verified against a hash that is no longer current.
	change, newTok := newChange(u.ID, "stale-hash", "attacker-hash")
	out, err := d.credRepo().ChangePassword(ctx, change, time.Now())
	if err != nil || out != ChangeConflict {
		t.Fatalf("want conflict, got %v %v", out, err)
	}
	if d.hashOf(t, u.ID) != "old-hash-cas" {
		t.Fatal("password must be unchanged")
	}
	if n := d.count(t, `SELECT count(*) FROM refresh_sessions WHERE user_id = $1::uuid AND revoked_at IS NULL`, u.ID); n != 1 {
		t.Fatal("existing session must NOT be revoked by a conflicting change")
	}
	if n := d.count(t, `SELECT count(*) FROM refresh_sessions WHERE token_hash = $1`, newTok); n != 0 {
		t.Fatal("no replacement session may be created")
	}
	if ok, _ := d.resets.Valid(ctx, resetTok, time.Now()); !ok {
		t.Fatal("reset token must be untouched")
	}

	// Inactive users conflict too.
	if _, err := d.pool.Exec(ctx, `UPDATE users SET is_active = false WHERE id = $1::uuid`, u.ID); err != nil {
		t.Fatal(err)
	}
	change, _ = newChange(u.ID, "old-hash-cas", "x")
	if out, _ := d.credRepo().ChangePassword(ctx, change, time.Now()); out != ChangeConflict {
		t.Fatalf("inactive user: %v", out)
	}
}

// If any step after the password update fails, the password update must roll
// back too. Forced here with a duplicate token_hash on the new session insert.
func TestChangePasswordIsAtomicOnFailure(t *testing.T) {
	d := newResetDB(t)
	ctx := context.Background()
	u := d.mkUser(t, "atom")
	_, existing, _ := NewRefreshToken()
	if err := d.sessions.Create(ctx, NewSession{UserID: u.ID, TokenHash: existing, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	resetTok := d.token(t, u.ID, time.Hour)

	change, _ := newChange(u.ID, "old-hash-atom", "new-hash")
	change.Session.TokenHash = existing // violates UNIQUE(token_hash) on the last statement
	if _, err := d.credRepo().ChangePassword(ctx, change, time.Now()); err == nil {
		t.Fatal("expected the forced failure")
	}
	if d.hashOf(t, u.ID) != "old-hash-atom" {
		t.Fatal("password change must roll back with the failed transaction")
	}
	if n := d.count(t, `SELECT count(*) FROM refresh_sessions WHERE user_id = $1::uuid AND revoked_at IS NULL`, u.ID); n != 1 {
		t.Fatal("session revocation must roll back")
	}
	if ok, _ := d.resets.Valid(ctx, resetTok, time.Now()); !ok {
		t.Fatal("reset-token deletion must roll back")
	}
}

// Many requests verified the same current password at once: only one may apply.
func TestChangePasswordConcurrentSingleWinner(t *testing.T) {
	d := newResetDB(t)
	ctx := context.Background()
	u := d.mkUser(t, "cc")
	d.login(t, u.ID)

	const n = 10
	outs := make([]ChangeOutcome, n)
	errs := make([]error, n)
	tokens := make([][]byte, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		c, h := newChange(u.ID, "old-hash-cc", fmt.Sprintf("hash-%d", i))
		tokens[i] = h
		go func(i int, c PasswordChange) {
			defer wg.Done()
			<-start
			outs[i], errs[i] = d.credRepo().ChangePassword(ctx, c, time.Now())
		}(i, c)
	}
	close(start)
	wg.Wait()

	winner := -1
	for i := range outs {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		if outs[i] == ChangeOK {
			if winner != -1 {
				t.Fatal("two concurrent changes both succeeded against the same current password")
			}
			winner = i
		}
	}
	if winner == -1 {
		t.Fatal("no winner")
	}
	if d.hashOf(t, u.ID) != fmt.Sprintf("hash-%d", winner) {
		t.Fatal("stored hash is not the winner's")
	}
	// Exactly one live session, and it is the winner's.
	if c := d.count(t, `SELECT count(*) FROM refresh_sessions WHERE user_id = $1::uuid AND revoked_at IS NULL`, u.ID); c != 1 {
		t.Fatalf("expected 1 live session, got %d", c)
	}
	if c := d.count(t, `SELECT count(*) FROM refresh_sessions WHERE token_hash = $1 AND revoked_at IS NULL`, tokens[winner]); c != 1 {
		t.Fatal("the live session must be the winner's")
	}
}
