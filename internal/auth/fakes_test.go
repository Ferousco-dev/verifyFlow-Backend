package auth

import (
	"bytes"
	"context"
	"strconv"
	"sync"
	"time"

	"migo/internal/user"
)

// fakeUsers mimics the DB unique constraints.
type fakeUsers struct {
	mu   sync.Mutex
	n    int
	byID map[string]*user.User
}

func newFakeUsers() *fakeUsers { return &fakeUsers{byID: map[string]*user.User{}} }

func (f *fakeUsers) Create(_ context.Context, u user.User) (user.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range f.byID {
		if e.Email == u.Email {
			return user.User{}, user.ErrEmailTaken
		}
		if e.Username == u.Username {
			return user.User{}, user.ErrUsernameTaken
		}
	}
	f.n++
	u.ID = "00000000-0000-0000-0000-" + pad(f.n)
	u.IsActive = true
	u.CreatedAt = time.Now()
	f.byID[u.ID] = &u
	return u, nil
}

func pad(n int) string {
	s := strconv.Itoa(n)
	for len(s) < 12 {
		s = "0" + s
	}
	return s
}

func (f *fakeUsers) GetByEmail(_ context.Context, email string) (user.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range f.byID {
		if e.Email == email {
			return *e, nil
		}
	}
	return user.User{}, user.ErrNotFound
}

func (f *fakeUsers) GetByID(_ context.Context, id string) (user.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if e, ok := f.byID[id]; ok {
		return *e, nil
	}
	return user.User{}, user.ErrNotFound
}

func (f *fakeUsers) UpdatePasswordHash(_ context.Context, id, hash string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byID[id].PasswordHash = &hash
	return nil
}

func (f *fakeUsers) setActive(id string, v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byID[id].IsActive = v
}

type fakeSession struct {
	userID     string
	family     int
	hash       []byte
	expiresAt  time.Time
	revoked    bool
	replacedBy bool
}

// fakeSessions mirrors SessionRepository.Rotate semantics.
type fakeSessions struct {
	mu       sync.Mutex
	users    *fakeUsers
	rows     []*fakeSession
	families int
}

func (f *fakeSessions) find(h []byte) *fakeSession {
	for _, s := range f.rows {
		if bytes.Equal(s.hash, h) {
			return s
		}
	}
	return nil
}

func (f *fakeSessions) Create(_ context.Context, s NewSession) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.families++
	f.rows = append(f.rows, &fakeSession{userID: s.UserID, family: f.families, hash: s.TokenHash, expiresAt: s.ExpiresAt})
	return nil
}

func (f *fakeSessions) revokeFamily(fam int) {
	for _, s := range f.rows {
		if s.family == fam {
			s.revoked = true
		}
	}
}

func (f *fakeSessions) Rotate(_ context.Context, old []byte, next NewSession, now time.Time) (RotateResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.find(old)
	if s == nil {
		return RotateResult{Outcome: RotateNotFound}, nil
	}
	switch {
	case s.replacedBy:
		f.revokeFamily(s.family)
		return RotateResult{Outcome: RotateReuseDetected, UserID: s.userID}, nil
	case s.revoked:
		return RotateResult{Outcome: RotateRevoked, UserID: s.userID}, nil
	case !s.expiresAt.After(now):
		return RotateResult{Outcome: RotateExpired, UserID: s.userID}, nil
	}
	if u, err := f.users.GetByID(context.Background(), s.userID); err == nil && !u.IsActive {
		f.revokeFamily(s.family)
		return RotateResult{Outcome: RotateRevoked, UserID: s.userID}, nil
	}
	s.revoked, s.replacedBy = true, true
	f.rows = append(f.rows, &fakeSession{userID: s.userID, family: s.family, hash: next.TokenHash, expiresAt: next.ExpiresAt})
	return RotateResult{Outcome: RotateOK, UserID: s.userID}, nil
}

func (f *fakeSessions) RevokeFamily(_ context.Context, h []byte, _ string, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s := f.find(h); s != nil {
		f.revokeFamily(s.family)
	}
	return nil
}

// rawHashes lets tests assert that raw tokens are never stored.
func (f *fakeSessions) rawHashes() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][]byte
	for _, s := range f.rows {
		out = append(out, s.hash)
	}
	return out
}

// ---- password reset fakes ----

func (f *fakeUsers) setPasswordVerified(id, hash string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byID[id].PasswordHash = &hash
	f.byID[id].EmailVerified = true
}

func (f *fakeSessions) revokeUser(userID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.rows {
		if s.userID == userID {
			s.revoked = true
		}
	}
}

type fakeReset struct {
	userID  string
	hash    []byte
	expires time.Time
	used    bool
}

// fakeResets mirrors ResetRepository semantics.
type fakeResets struct {
	mu       sync.Mutex
	users    *fakeUsers
	sessions *fakeSessions
	rows     []*fakeReset
}

func (f *fakeResets) find(h []byte) *fakeReset {
	for _, r := range f.rows {
		if bytes.Equal(r.hash, h) {
			return r
		}
	}
	return nil
}

func (f *fakeResets) Create(_ context.Context, userID string, hash []byte, exp time.Time, _ string, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	kept := f.rows[:0]
	for _, r := range f.rows {
		if !(r.userID == userID && !r.used) {
			kept = append(kept, r)
		}
	}
	f.rows = append(kept, &fakeReset{userID: userID, hash: hash, expires: exp})
	return nil
}

func (f *fakeResets) active(userID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.rows {
		if r.userID == userID && !r.used {
			n++
		}
	}
	return n
}

func (f *fakeResets) Valid(_ context.Context, h []byte, now time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.find(h)
	if r == nil || r.used || !r.expires.After(now) {
		return false, nil
	}
	u, err := f.users.GetByID(context.Background(), r.userID)
	return err == nil && u.IsActive, nil
}

func (f *fakeResets) Consume(_ context.Context, h []byte, newHash string, now time.Time) (ConsumeResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.find(h)
	if r == nil {
		return ConsumeResult{Outcome: ConsumeNotFound}, nil
	}
	u, _ := f.users.GetByID(context.Background(), r.userID)
	switch {
	case r.used:
		return ConsumeResult{Outcome: ConsumeUsed, UserID: r.userID}, nil
	case !r.expires.After(now):
		return ConsumeResult{Outcome: ConsumeExpired, UserID: r.userID}, nil
	case !u.IsActive:
		return ConsumeResult{Outcome: ConsumeInactive, UserID: r.userID}, nil
	}
	f.users.setPasswordVerified(r.userID, newHash)
	f.sessions.revokeUser(r.userID)
	r.used = true
	kept := f.rows[:0]
	for _, o := range f.rows {
		if o.userID != r.userID || o == r {
			kept = append(kept, o)
		}
	}
	f.rows = kept
	return ConsumeResult{Outcome: ConsumeOK, UserID: r.userID}, nil
}

// ---- verification + credential fakes ----

func (f *fakeUsers) setEmail(id, email string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byID[id].Email = email
}

func (f *fakeUsers) setHash(id string, hash *string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byID[id].PasswordHash = hash
}

func (f *fakeResets) deleteForUser(userID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	kept := f.rows[:0]
	for _, r := range f.rows {
		if r.userID != userID {
			kept = append(kept, r)
		}
	}
	f.rows = kept
}

type fakeVerify struct {
	userID, email string
	hash          []byte
	expires       time.Time
	used          bool
}

// fakeVerifies mirrors VerifyRepository semantics.
type fakeVerifies struct {
	mu    sync.Mutex
	users *fakeUsers
	rows  []*fakeVerify
}

func (f *fakeVerifies) Create(_ context.Context, userID, email string, hash []byte, exp, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	kept := f.rows[:0]
	for _, r := range f.rows {
		if !(r.userID == userID && !r.used) {
			kept = append(kept, r)
		}
	}
	f.rows = append(kept, &fakeVerify{userID: userID, email: email, hash: hash, expires: exp})
	return nil
}

func (f *fakeVerifies) Consume(_ context.Context, h []byte, now time.Time) (VerifyResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var r *fakeVerify
	for _, x := range f.rows {
		if bytes.Equal(x.hash, h) {
			r = x
		}
	}
	if r == nil {
		return VerifyResult{Outcome: VerifyNotFound}, nil
	}
	u, _ := f.users.GetByID(context.Background(), r.userID)
	switch {
	case r.used:
		return VerifyResult{Outcome: VerifyUsed, UserID: r.userID}, nil
	case !r.expires.After(now):
		return VerifyResult{Outcome: VerifyExpired, UserID: r.userID}, nil
	case !u.IsActive:
		return VerifyResult{Outcome: VerifyInactive, UserID: r.userID}, nil
	case u.Email != r.email:
		return VerifyResult{Outcome: VerifyEmailChanged, UserID: r.userID}, nil
	}
	f.users.mu.Lock()
	f.users.byID[r.userID].EmailVerified = true
	f.users.mu.Unlock()
	r.used = true
	kept := f.rows[:0]
	for _, o := range f.rows {
		if o.userID != r.userID || o == r {
			kept = append(kept, o)
		}
	}
	f.rows = kept
	return VerifyResult{Outcome: VerifyOK, UserID: r.userID}, nil
}

func (f *fakeVerifies) live(userID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.rows {
		if r.userID == userID && !r.used {
			n++
		}
	}
	return n
}

// fakeCreds mirrors CredentialRepository semantics, including the CAS.
type fakeCreds struct {
	users       *fakeUsers
	sessions    *fakeSessions
	resets      *fakeResets
	beforeApply func() // test hook: simulate a competing change landing first
}

func (f *fakeCreds) ChangePassword(ctx context.Context, c PasswordChange, _ time.Time) (ChangeOutcome, error) {
	if f.beforeApply != nil {
		f.beforeApply()
	}
	u, err := f.users.GetByID(ctx, c.UserID)
	if err != nil || !u.IsActive || u.PasswordHash == nil || *u.PasswordHash != c.ExpectedHash {
		return ChangeConflict, nil
	}
	f.users.setHash(c.UserID, &c.NewHash)
	f.sessions.revokeUser(c.UserID)
	f.resets.deleteForUser(c.UserID)
	_ = f.sessions.Create(ctx, c.Session)
	return ChangeOK, nil
}
