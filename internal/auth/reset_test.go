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
	"sync"
	"testing"
	"time"

	"migo/internal/mailer"
	"migo/internal/ratelimit"
)

const resetBase = "https://app.example.com/reset-password"

type recordingMailer struct {
	mu   sync.Mutex
	msgs []mailer.Message
	err  error
}

func (m *recordingMailer) Send(_ context.Context, msg mailer.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.msgs = append(m.msgs, msg)
	return nil
}

func (m *recordingMailer) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.msgs)
}

func (m *recordingMailer) last() mailer.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.msgs[len(m.msgs)-1]
}

var tokenRe = regexp.MustCompile(`#token=([A-Za-z0-9_-]+)`)

func tokenFrom(t *testing.T, msg mailer.Message) string {
	t.Helper()
	m := tokenRe.FindStringSubmatch(msg.Body)
	if m == nil {
		t.Fatalf("no reset link in email:\n%s", msg.Body)
	}
	return m[1]
}

type resetEnv struct {
	svc      *Service
	users    *fakeUsers
	sessions *fakeSessions
	resets   *fakeResets
	mail     *recordingMailer
}

func newResetEnv(t *testing.T) *resetEnv {
	t.Helper()
	svc, users, sessions := newTestService(t)
	resets := &fakeResets{users: users, sessions: sessions}
	mail := &recordingMailer{}
	svc.EnablePasswordReset(resets, mail, resetBase, 30*time.Minute, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return &resetEnv{svc: svc, users: users, sessions: sessions, resets: resets, mail: mail}
}

func TestForgotUnknownEmailIsSilent(t *testing.T) {
	e := newResetEnv(t)
	if err := e.svc.RequestPasswordReset(context.Background(), "ghost@example.com", Meta{}); err != nil {
		t.Fatalf("must not reveal absence: %v", err)
	}
	if e.mail.count() != 0 || len(e.resets.rows) != 0 {
		t.Fatal("nothing may be created or sent for an unknown address")
	}
}

func TestForgotSendsLinkAndStoresOnlyDigest(t *testing.T) {
	e := newResetEnv(t)
	u, _ := register(t, e.svc, "Ada Lovelace", "ada@example.com")
	if err := e.svc.RequestPasswordReset(context.Background(), " ADA@example.com ", Meta{IP: "203.0.113.1"}); err != nil {
		t.Fatal(err)
	}
	if e.mail.count() != 1 {
		t.Fatalf("want 1 email, got %d", e.mail.count())
	}
	msg := e.mail.last()
	if msg.To != "ada@example.com" || !strings.HasPrefix(msg.Subject, "Reset") {
		t.Fatalf("bad envelope: %+v", msg)
	}
	if !strings.Contains(msg.Body, resetBase+"#token=") || !strings.Contains(msg.Body, "30 minutes") || !strings.Contains(msg.Body, "Hi Ada") {
		t.Fatalf("bad body:\n%s", msg.Body)
	}
	if strings.Contains(msg.Body, "?token=") {
		t.Fatal("token must be in the URL fragment, not the query string")
	}
	raw := tokenFrom(t, msg)
	if len(raw) < 40 {
		t.Fatal("token too short")
	}
	row := e.resets.rows[0]
	if row.userID != u.ID || string(row.hash) == raw || !bytesEqual(row.hash, HashRefreshToken(raw)) {
		t.Fatal("only the SHA-256 digest of the token may be stored")
	}
}

func bytesEqual(a, b []byte) bool { return string(a) == string(b) }

func TestForgotInactiveUserSilentAndMalformedRejected(t *testing.T) {
	e := newResetEnv(t)
	u, _ := register(t, e.svc, "Off User", "off@example.com")
	e.users.setActive(u.ID, false)
	if err := e.svc.RequestPasswordReset(context.Background(), "off@example.com", Meta{}); err != nil || e.mail.count() != 0 {
		t.Fatalf("inactive: err=%v mails=%d", err, e.mail.count())
	}
	var ve *ValidationError
	if err := e.svc.RequestPasswordReset(context.Background(), "not-an-email", Meta{}); !errors.As(err, &ve) {
		t.Fatalf("malformed email should be a validation error, got %v", err)
	}
}

func TestForgotMailFailureNeverSurfaces(t *testing.T) {
	e := newResetEnv(t)
	register(t, e.svc, "Mail Fail", "mf@example.com")
	e.mail.err = mailer.ErrQueueFull
	if err := e.svc.RequestPasswordReset(context.Background(), "mf@example.com", Meta{}); err != nil {
		t.Fatalf("queue failure must not change the response: %v", err)
	}
}

func TestNewRequestInvalidatesOlderLink(t *testing.T) {
	e := newResetEnv(t)
	u, _ := register(t, e.svc, "Two Links", "two@example.com")
	ctx := context.Background()
	_ = e.svc.RequestPasswordReset(ctx, "two@example.com", Meta{})
	first := tokenFrom(t, e.mail.last())
	_ = e.svc.RequestPasswordReset(ctx, "two@example.com", Meta{})
	second := tokenFrom(t, e.mail.last())

	if e.resets.active(u.ID) != 1 {
		t.Fatalf("exactly one live token expected, got %d", e.resets.active(u.ID))
	}
	if err := e.svc.ResetPassword(ctx, first, "brand-new-password"); !errors.Is(err, ErrInvalidResetToken) {
		t.Fatalf("older link must be dead, got %v", err)
	}
	if err := e.svc.ResetPassword(ctx, second, "brand-new-password"); err != nil {
		t.Fatalf("newest link must work: %v", err)
	}
}

func TestResetPasswordFullEffects(t *testing.T) {
	e := newResetEnv(t)
	ctx := context.Background()
	u, tok := register(t, e.svc, "Reset Me", "reset@example.com")
	_, other, _ := e.svc.Login(ctx, LoginInput{Email: "reset@example.com", Password: "supersecret-pw"}, Meta{})
	_ = other

	_ = e.svc.RequestPasswordReset(ctx, "reset@example.com", Meta{})
	raw := tokenFrom(t, e.mail.last())
	before := e.mail.count()

	if err := e.svc.ResetPassword(ctx, raw, "a-brand-new-password"); err != nil {
		t.Fatal(err)
	}

	// New password works, old doesn't.
	if _, _, err := e.svc.Login(ctx, LoginInput{Email: "reset@example.com", Password: "a-brand-new-password"}, Meta{}); err != nil {
		t.Fatalf("new password should log in: %v", err)
	}
	if _, _, err := e.svc.Login(ctx, LoginInput{Email: "reset@example.com", Password: "supersecret-pw"}, Meta{}); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("old password must stop working: %v", err)
	}
	// Every pre-existing session (both devices) was revoked.
	if _, err := e.svc.Refresh(ctx, tok.RefreshToken, Meta{}); !errors.Is(err, ErrInvalidRefreshToken) {
		t.Fatalf("session 1 must be revoked: %v", err)
	}
	if _, err := e.svc.Refresh(ctx, other.RefreshToken, Meta{}); !errors.Is(err, ErrInvalidRefreshToken) {
		t.Fatalf("session 2 must be revoked: %v", err)
	}
	// Email is now verified (proved control of the address).
	got, _ := e.users.GetByID(ctx, u.ID)
	if !got.EmailVerified {
		t.Fatal("email should be marked verified after a successful reset")
	}
	// A security notification was sent.
	if e.mail.count() != before+1 || !strings.Contains(e.mail.last().Subject, "changed") || strings.Contains(e.mail.last().Body, "#token=") {
		t.Fatalf("expected a password-changed notice without a link, got: %+v", e.mail.last())
	}
	// Single use.
	if err := e.svc.ResetPassword(ctx, raw, "yet-another-password"); !errors.Is(err, ErrInvalidResetToken) {
		t.Fatalf("token must be single-use, got %v", err)
	}
}

func TestResetPasswordRejectsBadTokens(t *testing.T) {
	e := newResetEnv(t)
	ctx := context.Background()
	register(t, e.svc, "Bad Tok", "bad@example.com")
	for name, tok := range map[string]string{"empty": "", "spaces": "   ", "garbage": "nope", "oversized": strings.Repeat("a", 500)} {
		if err := e.svc.ResetPassword(ctx, tok, "a-brand-new-password"); !errors.Is(err, ErrInvalidResetToken) {
			t.Errorf("%s: got %v", name, err)
		}
	}
}

func TestResetPasswordExpired(t *testing.T) {
	e := newResetEnv(t)
	ctx := context.Background()
	register(t, e.svc, "Late", "late@example.com")
	_ = e.svc.RequestPasswordReset(ctx, "late@example.com", Meta{})
	raw := tokenFrom(t, e.mail.last())

	e.svc.now = func() time.Time { return time.Now().Add(31 * time.Minute) }
	if err := e.svc.ResetPassword(ctx, raw, "a-brand-new-password"); !errors.Is(err, ErrInvalidResetToken) {
		t.Fatalf("expired token must fail, got %v", err)
	}
	e.svc.now = func() time.Time { return time.Now().Add(29 * time.Minute) }
	if err := e.svc.ResetPassword(ctx, raw, "a-brand-new-password"); err != nil {
		t.Fatalf("token should still be valid inside its TTL: %v", err)
	}
}

func TestWeakPasswordDoesNotBurnTheLink(t *testing.T) {
	e := newResetEnv(t)
	ctx := context.Background()
	register(t, e.svc, "Weak", "weak@example.com")
	_ = e.svc.RequestPasswordReset(ctx, "weak@example.com", Meta{})
	raw := tokenFrom(t, e.mail.last())

	var ve *ValidationError
	if err := e.svc.ResetPassword(ctx, raw, "short"); !errors.As(err, &ve) || ve.Fields["new_password"] == "" {
		t.Fatalf("expected validation error, got %v", err)
	}
	if err := e.svc.ResetPassword(ctx, raw, "a-brand-new-password"); err != nil {
		t.Fatalf("link should survive a rejected password: %v", err)
	}
}

func TestResetDisabledWhenNotConfigured(t *testing.T) {
	svc, _, _ := newTestService(t)
	if svc.PasswordResetEnabled() {
		t.Fatal("should be off by default")
	}
	if err := svc.RequestPasswordReset(context.Background(), "a@example.com", Meta{}); err == nil {
		t.Fatal("expected error when unconfigured")
	}
}

// ---- HTTP ----

type resetHTTP struct {
	*resetEnv
	mux http.Handler
}

func newResetHTTP(t *testing.T, withEmailLimiter bool) *resetHTTP {
	t.Helper()
	e := newResetEnv(t)
	h := NewHandler(e.svc, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if withEmailLimiter {
		h.SetForgotEmailLimiter(ratelimit.NewMemory(ratelimit.Limit{Burst: 3, Every: time.Hour}))
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /forgot", h.ForgotPassword)
	mux.HandleFunc("POST /reset", h.ResetPassword)
	return &resetHTTP{resetEnv: e, mux: mux}
}

func (h *resetHTTP) post(path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", path, strings.NewReader(body))
	w := httptest.NewRecorder()
	h.mux.ServeHTTP(w, r)
	return w
}

func TestHTTPForgotIsIndistinguishable(t *testing.T) {
	h := newResetHTTP(t, false)
	register(t, h.svc, "Known", "known@example.com")

	known := h.post("/forgot", `{"email":"known@example.com"}`)
	unknown := h.post("/forgot", `{"email":"nobody@example.com"}`)
	if known.Code != 202 || unknown.Code != 202 || known.Body.String() != unknown.Body.String() {
		t.Fatalf("responses must be identical: %d %q vs %d %q", known.Code, known.Body, unknown.Code, unknown.Body)
	}
	if h.mail.count() != 1 {
		t.Fatalf("only the registered address gets mail, got %d", h.mail.count())
	}
	if w := h.post("/forgot", `{"email":"nope"}`); w.Code != 422 {
		t.Fatalf("malformed: %d", w.Code)
	}
	if w := h.post("/forgot", `{"email":"a@b.co","extra":1}`); w.Code != 400 {
		t.Fatalf("unknown field: %d", w.Code)
	}
}

func TestHTTPForgotPerEmailLimitIsSilent(t *testing.T) {
	h := newResetHTTP(t, true)
	register(t, h.svc, "Bombed", "bombed@example.com")
	var last *httptest.ResponseRecorder
	for i := 0; i < 6; i++ {
		last = h.post("/forgot", `{"email":"Bombed@Example.com"}`)
		if last.Code != 202 {
			t.Fatalf("request %d: %d (must stay 202 so limits reveal nothing)", i, last.Code)
		}
	}
	if h.mail.count() != 3 {
		t.Fatalf("mail-bombing must be capped at the burst of 3, got %d emails", h.mail.count())
	}
	// Same response body whether throttled or not.
	if !strings.Contains(last.Body.String(), "If an account exists") {
		t.Fatal(last.Body)
	}
	// An address that doesn't exist consumes budget the same way (no oracle).
	for i := 0; i < 5; i++ {
		if w := h.post("/forgot", `{"email":"ghost@example.com"}`); w.Code != 202 {
			t.Fatal(w.Code)
		}
	}
}

func TestHTTPResetEndpoint(t *testing.T) {
	h := newResetHTTP(t, false)
	register(t, h.svc, "HTTP Reset", "http@example.com")
	h.post("/forgot", `{"email":"http@example.com"}`)
	raw := tokenFrom(t, h.mail.last())

	if w := h.post("/reset", `{"token":"`+raw+`","new_password":"tiny"}`); w.Code != 422 || !strings.Contains(w.Body.String(), "new_password") {
		t.Fatalf("weak: %d %s", w.Code, w.Body)
	}
	if w := h.post("/reset", `{"token":"bogus","new_password":"a-brand-new-password"}`); w.Code != 400 || !strings.Contains(w.Body.String(), `"invalid_reset_token"`) {
		t.Fatalf("bogus: %d %s", w.Code, w.Body)
	}
	if w := h.post("/reset", `{"token":"`+raw+`","new_password":"a-brand-new-password"}`); w.Code != 204 || w.Body.Len() != 0 {
		t.Fatalf("ok: %d %s", w.Code, w.Body)
	}
	if w := h.post("/reset", `{"token":"`+raw+`","new_password":"a-brand-new-password"}`); w.Code != 400 {
		t.Fatalf("replay: %d", w.Code)
	}
	if w := h.post("/reset", `{"token":"x"}garbage`); w.Code != 400 {
		t.Fatalf("bad json: %d", w.Code)
	}
}

func TestHTTPDisabledIs404(t *testing.T) {
	svc, _, _ := newTestService(t)
	h := NewHandler(svc, slog.New(slog.NewTextHandler(io.Discard, nil)))
	mux := http.NewServeMux()
	mux.HandleFunc("POST /forgot", h.ForgotPassword)
	mux.HandleFunc("POST /reset", h.ResetPassword)
	for _, p := range []string{"/forgot", "/reset"} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("POST", p, strings.NewReader(`{}`)))
		if w.Code != 404 {
			t.Fatalf("%s: %d", p, w.Code)
		}
	}
}
