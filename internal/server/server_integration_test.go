package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"migo/internal/auth"
	"migo/internal/mailer"
	"migo/internal/testutil/dbtest"
	"migo/internal/user"
)

const allowedOrigin = "https://app.example.com"

type sink struct {
	mu   sync.Mutex
	msgs []mailer.Message
}

func (s *sink) Send(_ context.Context, m mailer.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msgs = append(s.msgs, m)
	return nil
}

func (s *sink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.msgs)
}

func (s *sink) last() mailer.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.msgs[len(s.msgs)-1]
}

func newStack(t *testing.T, trusted ...string) http.Handler {
	h, _ := newStackWithMail(t, trusted...)
	return h
}

func newStackWithMail(t *testing.T, trusted ...string) (http.Handler, *sink) {
	return newStackWithFeatures(t, false, trusted...)
}

func newStackWithAccountFeatures(t *testing.T) (http.Handler, *sink) {
	return newStackWithFeatures(t, true)
}

func newStackWithFeatures(t *testing.T, accountFeatures bool, trusted ...string) (http.Handler, *sink) {
	t.Helper()
	pool := dbtest.NewMigrated(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	tokens := auth.NewTokenManager([]byte(strings.Repeat("s", 32)), "migo", 15*time.Minute)
	svc, err := auth.NewService(user.NewRepository(pool), auth.NewSessionRepository(pool),
		auth.NewHasher(auth.Params{Memory: 8, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}, 4), tokens, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	mail := &sink{}
	svc.EnablePasswordReset(auth.NewResetRepository(pool), mail, "https://app.example.com/reset-password", 30*time.Minute, log)
	if accountFeatures {
		svc.EnableEmailVerification(auth.NewVerifyRepository(pool), mail, "https://app.example.com/verify-email", 24*time.Hour, log)
		svc.EnableChangePassword(auth.NewCredentialRepository(pool), mail, log)
	}
	h := auth.NewHandler(svc, log)
	lim := DefaultLimiters()
	h.SetLoginLimiter(lim.LoginAccount)
	h.SetForgotEmailLimiter(lim.ForgotEmail)
	if accountFeatures {
		h.SetResendLimiter(lim.ResendUser)
		h.SetChangePasswordLimiter(lim.ChangePasswordUser)
	}

	var nets []*net.IPNet
	for _, c := range trusted {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			t.Fatal(err)
		}
		nets = append(nets, n)
	}
	return New(Deps{Log: log, DB: pool, Auth: h, Tokens: tokens, Limiters: lim,
		AllowedOrigins: []string{allowedOrigin}, TrustedProxies: nets}), mail
}

type req struct {
	method, path, body string
	remote             string
	headers            map[string]string
}

func call(h http.Handler, q req) *httptest.ResponseRecorder {
	r := httptest.NewRequest(q.method, q.path, strings.NewReader(q.body))
	r.RemoteAddr = q.remote
	if q.remote == "" {
		r.RemoteAddr = "203.0.113.50:4000"
	}
	for k, v := range q.headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func register(h http.Handler, i int, remote string, headers map[string]string) *httptest.ResponseRecorder {
	return call(h, req{"POST", "/api/v1/auth/register",
		fmt.Sprintf(`{"full_name":"User %d","email":"u%d@example.com","password":"a-long-password"}`, i, i), remote, headers})
}

func login(h http.Handler, email, pw, remote string) *httptest.ResponseRecorder {
	return call(h, req{"POST", "/api/v1/auth/login",
		fmt.Sprintf(`{"email":%q,"password":%q}`, email, pw), remote, nil})
}

func TestHealth(t *testing.T) {
	h := newStack(t)
	w := call(h, req{method: "GET", path: "/health"})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"database":"ok"`) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

func TestRegisterRateLimitedPerIP(t *testing.T) {
	h := newStack(t)
	hdr := map[string]string{"Origin": allowedOrigin, "Content-Type": "application/json"}

	for i := 0; i < 5; i++ {
		if w := register(h, i, "198.51.100.1:1000", hdr); w.Code != 201 {
			t.Fatalf("request %d: %d %s", i, w.Code, w.Body)
		}
	}
	w := register(h, 99, "198.51.100.1:2000", hdr) // new source port, same IP
	if w.Code != 429 || !strings.Contains(w.Body.String(), `"rate_limited"`) {
		t.Fatalf("6th register must be 429, got %d %s", w.Code, w.Body)
	}
	if ra := w.Header().Get("Retry-After"); ra == "" || ra == "0" {
		t.Fatalf("Retry-After missing: %q", ra)
	}
	// The browser must be able to read the 429 (CORS headers on error responses).
	if w.Header().Get("Access-Control-Allow-Origin") != allowedOrigin {
		t.Fatalf("429 lacks CORS header: %v", w.Header())
	}
	// A different client is unaffected.
	if w := register(h, 100, "198.51.100.2:1000", hdr); w.Code != 201 {
		t.Fatalf("other IP should not be limited: %d", w.Code)
	}
}

func TestSpoofedXFFDoesNotBypassLimit(t *testing.T) {
	h := newStack(t) // no trusted proxies
	for i := 0; i < 5; i++ {
		register(h, i, "198.51.100.1:1", map[string]string{"X-Forwarded-For": fmt.Sprintf("9.9.9.%d", i)})
	}
	w := register(h, 50, "198.51.100.1:1", map[string]string{"X-Forwarded-For": "8.8.8.8"})
	if w.Code != 429 {
		t.Fatalf("rotating X-Forwarded-For must not evade the limit, got %d", w.Code)
	}
}

func TestTrustedProxySeesRealClients(t *testing.T) {
	h := newStack(t, "10.0.0.0/8")
	// Same proxy, five distinct real clients: none should be limited.
	for i := 0; i < 8; i++ {
		w := register(h, i, "10.0.0.1:1", map[string]string{"X-Forwarded-For": fmt.Sprintf("198.51.100.%d", i+1)})
		if w.Code != 201 {
			t.Fatalf("client %d behind proxy: %d %s", i, w.Code, w.Body)
		}
	}
	// One real client hammering through the proxy IS limited (and the forged prefix is ignored).
	var last *httptest.ResponseRecorder
	for i := 0; i < 6; i++ {
		last = register(h, 100+i, "10.0.0.1:1", map[string]string{"X-Forwarded-For": fmt.Sprintf("1.1.1.%d, 203.0.113.77", i)})
	}
	if last.Code != 429 {
		t.Fatalf("expected 429 for a single real client, got %d", last.Code)
	}
}

func TestLoginPerAccountLimitAndReset(t *testing.T) {
	h := newStack(t)
	if w := register(h, 1, "192.0.2.1:1", nil); w.Code != 201 {
		t.Fatal(w.Body)
	}
	const attacker = "192.0.2.66:1"

	// 3 wrong guesses, then the right password: succeeds and clears the account bucket.
	for i := 0; i < 3; i++ {
		if w := login(h, "u1@example.com", "wrong-password", attacker); w.Code != 401 {
			t.Fatalf("guess %d: %d", i, w.Code)
		}
	}
	if w := login(h, "u1@example.com", "a-long-password", attacker); w.Code != 200 {
		t.Fatalf("correct login: %d %s", w.Code, w.Body)
	}
	for i := 0; i < 4; i++ {
		if w := login(h, "u1@example.com", "wrong-password", attacker); w.Code != 401 {
			t.Fatalf("after reset, guess %d: %d (budget should have been restored)", i, w.Code)
		}
	}

	// Fresh IP hammering one account: 5 guesses allowed, the 6th is blocked, even with the right password.
	const bot = "192.0.2.99:1"
	for i := 0; i < 5; i++ {
		login(h, "u1@example.com", "wrong-password", bot)
	}
	if w := login(h, "u1@example.com", "a-long-password", bot); w.Code != 429 {
		t.Fatalf("account+IP budget exhausted must return 429, got %d", w.Code)
	}
	// The victim, from their own IP, is not locked out by the attacker.
	if w := login(h, "u1@example.com", "a-long-password", "192.0.2.1:1"); w.Code != 200 {
		t.Fatalf("victim must still be able to log in from another IP: %d", w.Code)
	}
}

func TestLoginPerIPLimit(t *testing.T) {
	h := newStack(t)
	const ip = "192.0.2.77:1"
	for i := 0; i < 10; i++ { // different emails so only the IP bucket fills
		if w := login(h, fmt.Sprintf("nobody%d@example.com", i), "whatever-pass", ip); w.Code != 401 {
			t.Fatalf("attempt %d: %d", i, w.Code)
		}
	}
	if w := login(h, "nobody99@example.com", "whatever-pass", ip); w.Code != 429 {
		t.Fatalf("11th login from one IP must be 429, got %d", w.Code)
	}
}

func TestCORSThroughFullStack(t *testing.T) {
	h := newStack(t)

	w := call(h, req{method: "OPTIONS", path: "/api/v1/auth/login", headers: map[string]string{
		"Origin": allowedOrigin, "Access-Control-Request-Method": "POST", "Access-Control-Request-Headers": "content-type",
	}})
	if w.Code != 204 || w.Header().Get("Access-Control-Allow-Origin") != allowedOrigin {
		t.Fatalf("preflight: %d %v", w.Code, w.Header())
	}

	w = call(h, req{method: "OPTIONS", path: "/api/v1/auth/login", headers: map[string]string{
		"Origin": "https://evil.example", "Access-Control-Request-Method": "POST",
	}})
	if w.Code != 403 {
		t.Fatalf("disallowed preflight: %d", w.Code)
	}

	// Actual request from the allowed origin exposes headers; /me 401 also carries CORS.
	w = call(h, req{method: "GET", path: "/api/v1/me", headers: map[string]string{"Origin": allowedOrigin}})
	if w.Code != 401 || w.Header().Get("Access-Control-Allow-Origin") != allowedOrigin {
		t.Fatalf("/me: %d %v", w.Code, w.Header())
	}
	// Preflights must not consume the rate-limit budget.
	for i := 0; i < 30; i++ {
		call(h, req{method: "OPTIONS", path: "/api/v1/auth/register", headers: map[string]string{
			"Origin": allowedOrigin, "Access-Control-Request-Method": "POST"}})
	}
	if w := register(h, 1, "", map[string]string{"Origin": allowedOrigin}); w.Code != 201 {
		t.Fatalf("preflights must not exhaust the budget: %d", w.Code)
	}
}

func TestSessionRecordsResolvedClientIP(t *testing.T) {
	pool := dbtest.NewMigrated(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	tokens := auth.NewTokenManager([]byte(strings.Repeat("s", 32)), "migo", time.Minute)
	svc, _ := auth.NewService(user.NewRepository(pool), auth.NewSessionRepository(pool),
		auth.NewHasher(auth.Params{Memory: 8, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}, 2), tokens, time.Hour)
	_, n, _ := net.ParseCIDR("10.0.0.0/8")
	h := New(Deps{Log: log, DB: pool, Auth: auth.NewHandler(svc, log), Tokens: tokens, TrustedProxies: []*net.IPNet{n}})

	if w := register(h, 1, "10.0.0.1:1", map[string]string{"X-Forwarded-For": "198.51.100.42", "User-Agent": "it-agent"}); w.Code != 201 {
		t.Fatal(w.Body)
	}
	var ip, ua string
	if err := pool.QueryRow(context.Background(), `SELECT ip, user_agent FROM refresh_sessions`).Scan(&ip, &ua); err != nil {
		t.Fatal(err)
	}
	if ip != "198.51.100.42" || ua != "it-agent" {
		t.Fatalf("session recorded ip=%q ua=%q", ip, ua)
	}
}

// ---- password reset, end to end ----

func post(h http.Handler, path, body, remote string) *httptest.ResponseRecorder {
	return call(h, req{"POST", path, body, remote, nil})
}

var tokenRe = regexp.MustCompile(`#token=([A-Za-z0-9_-]+)`)

func TestPasswordResetEndToEnd(t *testing.T) {
	h, mail := newStackWithMail(t)
	const ip = "192.0.2.10:1"

	// Account with one live session.
	reg := register(h, 1, ip, nil)
	if reg.Code != 201 {
		t.Fatal(reg.Body)
	}
	oldRefresh := jsonField(t, reg.Body.String(), "refresh_token")

	// Forgot -> 202 and an email with a link.
	w := post(h, "/api/v1/auth/forgot-password", `{"email":"u1@example.com"}`, ip)
	if w.Code != 202 {
		t.Fatalf("forgot: %d %s", w.Code, w.Body)
	}
	if mail.count() != 1 {
		t.Fatalf("expected 1 email, got %d", mail.count())
	}
	m := tokenRe.FindStringSubmatch(mail.last().Body)
	if m == nil {
		t.Fatalf("no link in email: %s", mail.last().Body)
	}
	token := m[1]

	// A weak password is rejected and does not burn the link.
	if w := post(h, "/api/v1/auth/reset-password", `{"token":"`+token+`","new_password":"short"}`, ip); w.Code != 422 {
		t.Fatalf("weak: %d %s", w.Code, w.Body)
	}
	// Reset succeeds.
	if w := post(h, "/api/v1/auth/reset-password", `{"token":"`+token+`","new_password":"my-new-strong-password"}`, ip); w.Code != 204 {
		t.Fatalf("reset: %d %s", w.Code, w.Body)
	}
	if mail.count() != 2 || !strings.Contains(mail.last().Subject, "changed") {
		t.Fatalf("expected a password-changed notice, got %d emails, last %q", mail.count(), mail.last().Subject)
	}

	// Old password dead, new one works.
	if w := login(h, "u1@example.com", "a-long-password", ip); w.Code != 401 {
		t.Fatalf("old password must stop working: %d", w.Code)
	}
	if w := login(h, "u1@example.com", "my-new-strong-password", ip); w.Code != 200 {
		t.Fatalf("new password: %d %s", w.Code, w.Body)
	}
	// The session that existed before the reset is revoked everywhere.
	if w := post(h, "/api/v1/auth/refresh", `{"refresh_token":"`+oldRefresh+`"}`, ip); w.Code != 401 {
		t.Fatalf("pre-reset refresh token must be revoked: %d", w.Code)
	}
	// /me reflects the verified email.
	newAccess := jsonField(t, login(h, "u1@example.com", "my-new-strong-password", ip).Body.String(), "access_token")
	me := call(h, req{method: "GET", path: "/api/v1/me", remote: ip, headers: map[string]string{"Authorization": "Bearer " + newAccess}})
	if !strings.Contains(me.Body.String(), `"email_verified":true`) {
		t.Fatalf("email should be verified after reset: %s", me.Body)
	}
	// The link is single-use.
	if w := post(h, "/api/v1/auth/reset-password", `{"token":"`+token+`","new_password":"another-strong-password"}`, ip); w.Code != 400 || !strings.Contains(w.Body.String(), "invalid_reset_token") {
		t.Fatalf("replay: %d %s", w.Code, w.Body)
	}
}

func jsonField(t *testing.T, body, field string) string {
	t.Helper()
	m := regexp.MustCompile(`"` + field + `":"([^"]+)"`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("field %s not in %s", field, body)
	}
	return m[1]
}

func TestEmailVerificationEndToEnd(t *testing.T) {
	h, mail := newStackWithAccountFeatures(t)
	const ip = "192.0.2.30:1"

	reg := register(h, 1, ip, nil)
	if reg.Code != 201 || mail.count() != 1 {
		t.Fatalf("register: status %d, emails %d, body %s", reg.Code, mail.count(), reg.Body)
	}
	access := jsonField(t, reg.Body.String(), "access_token")
	firstTokenMatch := tokenRe.FindStringSubmatch(mail.last().Body)
	if firstTokenMatch == nil {
		t.Fatalf("verification link missing from email: %s", mail.last().Body)
	}

	// Resending replaces the previous token, and the authenticated route is live.
	resend := call(h, req{"POST", "/api/v1/auth/resend-verification", `{}`, ip,
		map[string]string{"Authorization": "Bearer " + access}})
	if resend.Code != 202 || mail.count() != 2 {
		t.Fatalf("resend: %d %s; emails=%d", resend.Code, resend.Body, mail.count())
	}
	secondTokenMatch := tokenRe.FindStringSubmatch(mail.last().Body)
	if secondTokenMatch == nil {
		t.Fatalf("resent verification link missing: %s", mail.last().Body)
	}

	first := post(h, "/api/v1/auth/verify-email", `{"token":"`+firstTokenMatch[1]+`"}`, ip)
	if first.Code != 400 || !strings.Contains(first.Body.String(), `"invalid_verification_token"`) {
		t.Fatalf("replaced token should be rejected: %d %s", first.Code, first.Body)
	}
	verified := post(h, "/api/v1/auth/verify-email", `{"token":"`+secondTokenMatch[1]+`"}`, ip)
	if verified.Code != 204 {
		t.Fatalf("verification: %d %s", verified.Code, verified.Body)
	}
	me := call(h, req{"GET", "/api/v1/me", "", ip, map[string]string{"Authorization": "Bearer " + access}})
	if me.Code != 200 || !strings.Contains(me.Body.String(), `"email_verified":true`) {
		t.Fatalf("/me should reflect persisted verification: %d %s", me.Code, me.Body)
	}

	resend = call(h, req{"POST", "/api/v1/auth/resend-verification", `{}`, ip,
		map[string]string{"Authorization": "Bearer " + access}})
	if resend.Code != 200 || mail.count() != 2 {
		t.Fatalf("verified account should not receive another email: %d %s; emails=%d", resend.Code, resend.Body, mail.count())
	}
}

func TestChangePasswordEndToEnd(t *testing.T) {
	h, mail := newStackWithAccountFeatures(t)
	const ip = "192.0.2.31:1"

	reg := register(h, 1, ip, nil)
	if reg.Code != 201 {
		t.Fatalf("register: %d %s", reg.Code, reg.Body)
	}
	oldRefresh := jsonField(t, reg.Body.String(), "refresh_token")
	access := jsonField(t, reg.Body.String(), "access_token")
	change := call(h, req{"POST", "/api/v1/auth/change-password",
		`{"current_password":"a-long-password","new_password":"a-new-long-password"}`, ip,
		map[string]string{"Authorization": "Bearer " + access}})
	if change.Code != 200 {
		t.Fatalf("change password: %d %s", change.Code, change.Body)
	}
	newRefresh := jsonField(t, change.Body.String(), "refresh_token")
	newAccess := jsonField(t, change.Body.String(), "access_token")
	if oldRefresh == newRefresh {
		t.Fatal("password change must issue a replacement refresh token")
	}
	if mail.count() != 2 || !strings.Contains(mail.last().Subject, "changed") {
		t.Fatalf("expected verification and password-changed emails, got %d; last=%q", mail.count(), mail.last().Subject)
	}

	if w := post(h, "/api/v1/auth/refresh", `{"refresh_token":"`+oldRefresh+`"}`, ip); w.Code != 401 {
		t.Fatalf("old session must be revoked: %d %s", w.Code, w.Body)
	}
	if w := login(h, "u1@example.com", "a-long-password", ip); w.Code != 401 {
		t.Fatalf("old password must stop working: %d %s", w.Code, w.Body)
	}
	if w := login(h, "u1@example.com", "a-new-long-password", ip); w.Code != 200 {
		t.Fatalf("new password should work: %d %s", w.Code, w.Body)
	}
	if w := call(h, req{"GET", "/api/v1/me", "", ip, map[string]string{"Authorization": "Bearer " + newAccess}}); w.Code != 200 {
		t.Fatalf("fresh access token should remain signed in: %d %s", w.Code, w.Body)
	}
	if w := post(h, "/api/v1/auth/refresh", `{"refresh_token":"`+newRefresh+`"}`, ip); w.Code != 200 {
		t.Fatalf("replacement session should be active: %d %s", w.Code, w.Body)
	}
}

func TestForgotPasswordDoesNotRevealAccounts(t *testing.T) {
	h, mail := newStackWithMail(t)
	register(h, 1, "192.0.2.10:1", nil)

	known := post(h, "/api/v1/auth/forgot-password", `{"email":"u1@example.com"}`, "192.0.2.20:1")
	unknown := post(h, "/api/v1/auth/forgot-password", `{"email":"ghost@example.com"}`, "192.0.2.21:1")
	if known.Code != unknown.Code || known.Body.String() != unknown.Body.String() {
		t.Fatalf("responses differ: %d %q vs %d %q", known.Code, known.Body, unknown.Code, unknown.Body)
	}
	if mail.count() != 1 {
		t.Fatalf("only the real account is emailed, got %d", mail.count())
	}
}

func TestForgotPasswordRateLimits(t *testing.T) {
	h, mail := newStackWithMail(t)
	register(h, 1, "192.0.2.10:1", nil)

	// Per-address budget: attackers rotating IPs can't mail-bomb one victim.
	for i := 0; i < 8; i++ {
		ip := fmt.Sprintf("198.51.100.%d:1", i+1)
		if w := post(h, "/api/v1/auth/forgot-password", `{"email":"u1@example.com"}`, ip); w.Code != 202 {
			t.Fatalf("request %d: %d (throttled requests must still answer 202)", i, w.Code)
		}
	}
	if mail.count() != 3 {
		t.Fatalf("victim must receive at most the burst of 3 emails, got %d", mail.count())
	}

	// Per-IP budget: one client spraying many addresses gets a visible 429.
	var last *httptest.ResponseRecorder
	for i := 0; i < 6; i++ {
		last = post(h, "/api/v1/auth/forgot-password", fmt.Sprintf(`{"email":"spray%d@example.com"}`, i), "203.0.113.200:1")
	}
	if last.Code != 429 || last.Header().Get("Retry-After") == "" {
		t.Fatalf("expected per-IP 429, got %d", last.Code)
	}
}

func TestResetPasswordRateLimitedPerIP(t *testing.T) {
	h := newStack(t)
	var last *httptest.ResponseRecorder
	for i := 0; i < 11; i++ {
		last = post(h, "/api/v1/auth/reset-password", `{"token":"guess","new_password":"a-long-password"}`, "203.0.113.5:1")
		if i < 10 && last.Code != 400 {
			t.Fatalf("attempt %d: %d", i, last.Code)
		}
	}
	if last.Code != 429 {
		t.Fatalf("token guessing must be throttled, got %d", last.Code)
	}
}
