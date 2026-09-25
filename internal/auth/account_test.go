package auth

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"migo/internal/mailer"
	"migo/internal/ratelimit"
)

const verifyBase = "https://app.example.com/verify-email"

type acctEnv struct {
	*resetEnv
	verifies *fakeVerifies
	creds    *fakeCreds
}

func newAcctEnv(t *testing.T) *acctEnv {
	t.Helper()
	e := newResetEnv(t) // reset already enabled; add verification + change-password
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	verifies := &fakeVerifies{users: e.users}
	creds := &fakeCreds{users: e.users, sessions: e.sessions, resets: e.resets}
	e.svc.EnableEmailVerification(verifies, e.mail, verifyBase, 24*time.Hour, log)
	e.svc.EnableChangePassword(creds, e.mail, log)
	return &acctEnv{resetEnv: e, verifies: verifies, creds: creds}
}

func (e *acctEnv) mailsWithSubject(sub string) []mailer.Message {
	e.mail.mu.Lock()
	defer e.mail.mu.Unlock()
	var out []mailer.Message
	for _, m := range e.mail.msgs {
		if strings.Contains(m.Subject, sub) {
			out = append(out, m)
		}
	}
	return out
}

// ---------------- email verification ----------------

func TestRegisterSendsVerificationEmail(t *testing.T) {
	e := newAcctEnv(t)
	u, _ := register(t, e.svc, "Ada Lovelace", "ada@example.com")

	mails := e.mailsWithSubject("Confirm")
	if len(mails) != 1 {
		t.Fatalf("want 1 verification email, got %d", len(mails))
	}
	m := mails[0]
	if m.To != "ada@example.com" || !strings.Contains(m.Body, verifyBase+"#token=") || !strings.Contains(m.Body, "24 hours") || !strings.Contains(m.Body, "Hi Ada") {
		t.Fatalf("bad email: %+v", m)
	}
	if strings.Contains(m.Body, "?token=") {
		t.Fatal("token must be in the fragment")
	}
	raw := tokenFrom(t, m)
	row := e.verifies.rows[0]
	if row.userID != u.ID || row.email != "ada@example.com" || string(row.hash) == raw || !bytesEqual(row.hash, HashRefreshToken(raw)) {
		t.Fatal("token must be bound to the address and stored only as a digest")
	}
	if u.EmailVerified {
		t.Fatal("new accounts start unverified")
	}
}

func TestRegisterSurvivesVerificationMailFailure(t *testing.T) {
	e := newAcctEnv(t)
	e.mail.err = mailer.ErrQueueFull
	u, tok, err := e.svc.Register(context.Background(), RegisterInput{FullName: "No Mail", Email: "nomail@example.com", Password: "supersecret-pw"}, Meta{})
	if err != nil || u.ID == "" || tok.AccessToken == "" {
		t.Fatalf("registration must not fail because of email trouble: %v", err)
	}
}

func TestVerifyEmail(t *testing.T) {
	e := newAcctEnv(t)
	ctx := context.Background()
	u, _ := register(t, e.svc, "Verify Me", "verify@example.com")
	raw := tokenFrom(t, e.mailsWithSubject("Confirm")[0])

	if err := e.svc.VerifyEmail(ctx, raw); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.users.GetByID(ctx, u.ID); !got.EmailVerified {
		t.Fatal("email should be verified")
	}
	if err := e.svc.VerifyEmail(ctx, raw); !errors.Is(err, ErrInvalidVerificationToken) {
		t.Fatalf("token must be single-use, got %v", err)
	}
}

func TestVerifyEmailRejections(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]func(e *acctEnv, userID string, raw string) string{
		"empty":     func(*acctEnv, string, string) string { return "" },
		"garbage":   func(*acctEnv, string, string) string { return "nope" },
		"oversized": func(*acctEnv, string, string) string { return strings.Repeat("a", 500) },
		"expired": func(e *acctEnv, _ string, raw string) string {
			e.svc.now = func() time.Time { return time.Now().Add(25 * time.Hour) }
			return raw
		},
		"inactive user": func(e *acctEnv, id string, raw string) string { e.users.setActive(id, false); return raw },
		"email changed since issue": func(e *acctEnv, id string, raw string) string {
			e.users.setEmail(id, "different@example.com")
			return raw
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newAcctEnv(t)
			u, _ := register(t, e.svc, "Rej", "rej@example.com")
			raw := tokenFrom(t, e.mailsWithSubject("Confirm")[0])
			tok := tc(e, u.ID, raw)
			if err := e.svc.VerifyEmail(ctx, tok); !errors.Is(err, ErrInvalidVerificationToken) {
				t.Fatalf("got %v", err)
			}
			if got, _ := e.users.GetByID(ctx, u.ID); got.EmailVerified {
				t.Fatal("a rejected token must not verify the email")
			}
		})
	}
}

func TestResendVerification(t *testing.T) {
	e := newAcctEnv(t)
	ctx := context.Background()
	u, _ := register(t, e.svc, "Resend", "resend@example.com")
	first := tokenFrom(t, e.mailsWithSubject("Confirm")[0])

	sent, err := e.svc.ResendVerification(ctx, u.ID)
	if err != nil || !sent {
		t.Fatalf("sent=%v err=%v", sent, err)
	}
	second := tokenFrom(t, e.mailsWithSubject("Confirm")[1])
	if e.verifies.live(u.ID) != 1 {
		t.Fatal("only one live token per user")
	}
	if err := e.svc.VerifyEmail(ctx, first); !errors.Is(err, ErrInvalidVerificationToken) {
		t.Fatalf("older link must be dead: %v", err)
	}
	if err := e.svc.VerifyEmail(ctx, second); err != nil {
		t.Fatalf("newest link must work: %v", err)
	}

	before := e.mail.count()
	if sent, err := e.svc.ResendVerification(ctx, u.ID); err != nil || sent || e.mail.count() != before {
		t.Fatalf("already verified: sent=%v err=%v", sent, err)
	}
	if _, err := e.svc.ResendVerification(ctx, "00000000-0000-0000-0000-999999999999"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("unknown user: %v", err)
	}
}

func TestPasswordResetAlsoVerifiesSoVerifyLinkStaysHarmless(t *testing.T) {
	e := newAcctEnv(t)
	ctx := context.Background()
	u, _ := register(t, e.svc, "Both", "both@example.com")
	verifyTok := tokenFrom(t, e.mailsWithSubject("Confirm")[0])
	_ = e.svc.RequestPasswordReset(ctx, "both@example.com", Meta{})
	resetTok := tokenFrom(t, e.mailsWithSubject("Reset")[0])
	if err := e.svc.ResetPassword(ctx, resetTok, "a-brand-new-password"); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.users.GetByID(ctx, u.ID); !got.EmailVerified {
		t.Fatal("reset verifies the email")
	}
	if err := e.svc.VerifyEmail(ctx, verifyTok); err != nil {
		t.Fatalf("a still-valid verify link should remain harmless/idempotent: %v", err)
	}
}

// ---------------- RequireVerifiedEmail ----------------

func TestRequireVerifiedEmailMiddleware(t *testing.T) {
	e := newAcctEnv(t)
	ctx := context.Background()
	_, tok := register(t, e.svc, "Gate", "gate@example.com")
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	chain := RequireAuth(e.svc.tokens)(RequireVerifiedEmail(e.svc)(ok))

	call := func(bearer string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/", nil)
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		w := httptest.NewRecorder()
		chain.ServeHTTP(w, r)
		return w
	}
	if w := call(""); w.Code != 401 {
		t.Fatalf("no token: %d", w.Code)
	}
	if w := call(tok.AccessToken); w.Code != 403 || !strings.Contains(w.Body.String(), `"email_not_verified"`) {
		t.Fatalf("unverified: %d %s", w.Code, w.Body)
	}
	if err := e.svc.VerifyEmail(ctx, tokenFrom(t, e.mailsWithSubject("Confirm")[0])); err != nil {
		t.Fatal(err)
	}
	if w := call(tok.AccessToken); w.Code != 200 {
		t.Fatalf("verified: %d", w.Code)
	}
	// Used without RequireAuth in front it fails closed.
	w := httptest.NewRecorder()
	RequireVerifiedEmail(e.svc)(ok).ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 401 {
		t.Fatalf("misconfigured chain must fail closed: %d", w.Code)
	}
}

// ---------------- change password ----------------

func TestChangePasswordSuccess(t *testing.T) {
	e := newAcctEnv(t)
	ctx := context.Background()
	u, dev1 := register(t, e.svc, "Change Me", "change@example.com")
	_, dev2, _ := e.svc.Login(ctx, LoginInput{Email: "change@example.com", Password: "supersecret-pw"}, Meta{})
	_ = e.svc.RequestPasswordReset(ctx, "change@example.com", Meta{})
	pendingReset := tokenFrom(t, e.mailsWithSubject("Reset")[0])

	tok, err := e.svc.ChangePassword(ctx, u.ID, ChangePasswordInput{CurrentPassword: "supersecret-pw", NewPassword: "a-much-better-password"}, Meta{IP: "203.0.113.4"})
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken == "" || tok.RefreshToken == "" || tok.ExpiresIn != 900 {
		t.Fatalf("caller must get fresh tokens: %+v", tok)
	}
	// Every earlier session is revoked...
	for i, rt := range []string{dev1.RefreshToken, dev2.RefreshToken} {
		if _, err := e.svc.Refresh(ctx, rt, Meta{}); !errors.Is(err, ErrInvalidRefreshToken) {
			t.Fatalf("old session %d must be revoked: %v", i+1, err)
		}
	}
	// ...but the caller's new one works.
	if _, err := e.svc.Refresh(ctx, tok.RefreshToken, Meta{}); err != nil {
		t.Fatalf("caller's new session must work: %v", err)
	}
	if _, _, err := e.svc.Login(ctx, LoginInput{Email: "change@example.com", Password: "supersecret-pw"}, Meta{}); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("old password must stop working: %v", err)
	}
	if _, _, err := e.svc.Login(ctx, LoginInput{Email: "change@example.com", Password: "a-much-better-password"}, Meta{}); err != nil {
		t.Fatalf("new password must work: %v", err)
	}
	// A reset link requested before the change must not survive it.
	if err := e.svc.ResetPassword(ctx, pendingReset, "attacker-chosen-pw-1"); !errors.Is(err, ErrInvalidResetToken) {
		t.Fatalf("pending reset link must die on password change: %v", err)
	}
	if n := len(e.mailsWithSubject("changed")); n != 1 {
		t.Fatalf("want 1 change notice, got %d", n)
	}
}

func TestChangePasswordRejections(t *testing.T) {
	e := newAcctEnv(t)
	ctx := context.Background()
	u, tok := register(t, e.svc, "Rej", "rej@example.com")

	cases := map[string]struct {
		in    ChangePasswordInput
		field string
	}{
		"wrong current":   {ChangePasswordInput{"not-my-password", "a-much-better-password"}, "current_password"},
		"missing current": {ChangePasswordInput{"", "a-much-better-password"}, "current_password"},
		"weak new":        {ChangePasswordInput{"supersecret-pw", "short"}, "new_password"},
		"same password":   {ChangePasswordInput{"supersecret-pw", "supersecret-pw"}, "new_password"},
	}
	for name, c := range cases {
		var ve *ValidationError
		_, err := e.svc.ChangePassword(ctx, u.ID, c.in, Meta{})
		if !errors.As(err, &ve) || ve.Fields[c.field] == "" {
			t.Errorf("%s: want validation error on %s, got %v", name, c.field, err)
		}
	}
	// Nothing changed: old password works, original session alive, no notice sent.
	if _, _, err := e.svc.Login(ctx, LoginInput{Email: "rej@example.com", Password: "supersecret-pw"}, Meta{}); err != nil {
		t.Fatalf("password must be unchanged: %v", err)
	}
	if _, err := e.svc.Refresh(ctx, tok.RefreshToken, Meta{}); err != nil {
		t.Fatalf("session must be unaffected by rejected changes: %v", err)
	}
	if len(e.mailsWithSubject("changed")) != 0 {
		t.Fatal("no notice for failed attempts")
	}
}

func TestChangePasswordAccountStates(t *testing.T) {
	e := newAcctEnv(t)
	ctx := context.Background()
	in := ChangePasswordInput{CurrentPassword: "supersecret-pw", NewPassword: "a-much-better-password"}

	u, _ := register(t, e.svc, "OAuth", "oauth@example.com")
	e.users.setHash(u.ID, nil)
	if _, err := e.svc.ChangePassword(ctx, u.ID, in, Meta{}); !errors.Is(err, ErrPasswordNotSet) {
		t.Fatalf("no password: %v", err)
	}
	u2, _ := register(t, e.svc, "Off", "off@example.com")
	e.users.setActive(u2.ID, false)
	if _, err := e.svc.ChangePassword(ctx, u2.ID, in, Meta{}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("inactive: %v", err)
	}
	if _, err := e.svc.ChangePassword(ctx, "00000000-0000-0000-0000-999999999999", in, Meta{}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("unknown: %v", err)
	}
}

// A stolen session + old password must not overwrite a password the real owner
// just reset: the compare-and-swap detects the hash changed underneath it.
func TestChangePasswordCannotOverwriteConcurrentReset(t *testing.T) {
	e := newAcctEnv(t)
	ctx := context.Background()
	u, _ := register(t, e.svc, "Race", "race@example.com")

	ownerHash := "hash-from-owners-reset"
	e.creds.beforeApply = func() { e.users.setHash(u.ID, &ownerHash) } // reset lands mid-request

	_, err := e.svc.ChangePassword(ctx, u.ID, ChangePasswordInput{CurrentPassword: "supersecret-pw", NewPassword: "attacker-new-password"}, Meta{})
	if !errors.Is(err, ErrConcurrentChange) {
		t.Fatalf("expected ErrConcurrentChange, got %v", err)
	}
	if got, _ := e.users.GetByID(ctx, u.ID); *got.PasswordHash != ownerHash {
		t.Fatal("the stale change overwrote the newer password")
	}
	if len(e.mailsWithSubject("changed")) != 0 {
		t.Fatal("no notice for a change that didn't happen")
	}
}

// ---------------- HTTP ----------------

type acctHTTP struct {
	*acctEnv
	mux http.Handler
}

func newAcctHTTP(t *testing.T, limiters bool) *acctHTTP {
	t.Helper()
	e := newAcctEnv(t)
	h := NewHandler(e.svc, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if limiters {
		h.SetResendLimiter(ratelimit.NewMemory(ratelimit.Limit{Burst: 3, Every: time.Hour}))
		h.SetChangePasswordLimiter(ratelimit.NewMemory(ratelimit.Limit{Burst: 3, Every: time.Hour}))
	}
	auth := RequireAuth(e.svc.tokens)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /verify", h.VerifyEmail)
	mux.Handle("POST /resend", auth(http.HandlerFunc(h.ResendVerification)))
	mux.Handle("POST /change", auth(http.HandlerFunc(h.ChangePassword)))
	return &acctHTTP{acctEnv: e, mux: mux}
}

func (h *acctHTTP) do(path, body, bearer string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", path, strings.NewReader(body))
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	h.mux.ServeHTTP(w, r)
	return w
}

func TestHTTPVerifyEmail(t *testing.T) {
	h := newAcctHTTP(t, false)
	register(t, h.svc, "HTTP V", "httpv@example.com")
	raw := tokenFrom(t, h.mailsWithSubject("Confirm")[0])

	if w := h.do("/verify", `{"token":"bogus"}`, ""); w.Code != 400 || !strings.Contains(w.Body.String(), `"invalid_verification_token"`) {
		t.Fatalf("bogus: %d %s", w.Code, w.Body)
	}
	if w := h.do("/verify", `{"token":"`+raw+`"}`, ""); w.Code != 204 || w.Body.Len() != 0 {
		t.Fatalf("ok: %d %s", w.Code, w.Body)
	}
	if w := h.do("/verify", `{"token":"`+raw+`"}`, ""); w.Code != 400 {
		t.Fatalf("replay: %d", w.Code)
	}
	if w := h.do("/verify", `{"token":"x","extra":1}`, ""); w.Code != 400 {
		t.Fatalf("unknown field: %d", w.Code)
	}
}

func TestHTTPResendVerification(t *testing.T) {
	h := newAcctHTTP(t, true)
	_, tok := register(t, h.svc, "HTTP R", "httpr@example.com")

	if w := h.do("/resend", "", ""); w.Code != 401 {
		t.Fatalf("needs auth: %d", w.Code)
	}
	for i := 0; i < 3; i++ {
		if w := h.do("/resend", "", tok.AccessToken); w.Code != 202 || !strings.Contains(w.Body.String(), "Verification email sent") {
			t.Fatalf("resend %d: %d %s", i, w.Code, w.Body)
		}
	}
	if w := h.do("/resend", "", tok.AccessToken); w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Fatalf("4th resend must be throttled: %d", w.Code)
	}
	if n := len(h.mailsWithSubject("Confirm")); n != 4 { // 1 at sign-up + 3 resends
		t.Fatalf("emails: %d", n)
	}
}

func TestHTTPResendWhenAlreadyVerified(t *testing.T) {
	h := newAcctHTTP(t, false)
	_, tok := register(t, h.svc, "Done", "done@example.com")
	_ = h.svc.VerifyEmail(context.Background(), tokenFrom(t, h.mailsWithSubject("Confirm")[0]))
	before := h.mail.count()
	if w := h.do("/resend", "", tok.AccessToken); w.Code != 200 || !strings.Contains(w.Body.String(), "already verified") || h.mail.count() != before {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

func TestHTTPChangePassword(t *testing.T) {
	h := newAcctHTTP(t, true)
	_, tok := register(t, h.svc, "HTTP C", "httpc@example.com")
	access := tok.AccessToken

	if w := h.do("/change", `{"current_password":"supersecret-pw","new_password":"a-much-better-password"}`, ""); w.Code != 401 {
		t.Fatalf("needs auth: %d", w.Code)
	}
	w := h.do("/change", `{"current_password":"wrong-password!","new_password":"a-much-better-password"}`, access)
	if w.Code != 422 || !strings.Contains(w.Body.String(), `"current_password"`) {
		t.Fatalf("wrong current: %d %s", w.Code, w.Body)
	}
	w = h.do("/change", `{"current_password":"supersecret-pw","new_password":"a-much-better-password"}`, access)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"token_type":"Bearer"`) || !strings.Contains(w.Body.String(), `"refresh_token"`) {
		t.Fatalf("success: %d %s", w.Code, w.Body)
	}
	if w := h.do("/change", `{"current_password":"a","new_password":"b","extra":1}`, access); w.Code != 400 {
		t.Fatalf("unknown field: %d", w.Code)
	}
}

func TestHTTPChangePasswordGuessingIsThrottledPerUser(t *testing.T) {
	h := newAcctHTTP(t, true) // per-user burst of 3
	_, tok := register(t, h.svc, "Guess", "guess@example.com")
	for i := 0; i < 3; i++ {
		if w := h.do("/change", `{"current_password":"wrong-guess-pw","new_password":"a-much-better-password"}`, tok.AccessToken); w.Code != 422 {
			t.Fatalf("guess %d: %d", i, w.Code)
		}
	}
	// Even the RIGHT password is refused once the budget is spent (a thief can't win by luck).
	if w := h.do("/change", `{"current_password":"supersecret-pw","new_password":"a-much-better-password"}`, tok.AccessToken); w.Code != 429 {
		t.Fatalf("expected 429 after 3 failures, got %d", w.Code)
	}
}

func TestHTTPChangePasswordNotSetAndDisabled(t *testing.T) {
	h := newAcctHTTP(t, false)
	u, tok := register(t, h.svc, "NoPw", "nopw@example.com")
	h.users.setHash(u.ID, nil)
	if w := h.do("/change", `{"current_password":"supersecret-pw","new_password":"a-much-better-password"}`, tok.AccessToken); w.Code != 400 || !strings.Contains(w.Body.String(), `"password_not_set"`) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}

	// Features not enabled -> 404 (verify, resend, change).
	svc, _, _ := newTestService(t)
	dh := NewHandler(svc, slog.New(slog.NewTextHandler(io.Discard, nil)))
	_, dtok := register(t, svc, "Dis", "dis@example.com")
	mux := http.NewServeMux()
	a := RequireAuth(svc.tokens)
	mux.HandleFunc("POST /verify", dh.VerifyEmail)
	mux.Handle("POST /resend", a(http.HandlerFunc(dh.ResendVerification)))
	mux.Handle("POST /change", a(http.HandlerFunc(dh.ChangePassword)))
	for _, p := range []string{"/verify", "/resend", "/change"} {
		r := httptest.NewRequest("POST", p, strings.NewReader(`{}`))
		r.Header.Set("Authorization", "Bearer "+dtok.AccessToken)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != 404 {
			t.Fatalf("%s: %d", p, w.Code)
		}
	}
}

var _ = regexp.MustCompile
