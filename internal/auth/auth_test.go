package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"migo/internal/user"
)

var testParams = Params{Memory: 8, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}

func testSecret() []byte { return []byte(strings.Repeat("s", 32)) }

// ---- password ----

func TestPasswordHashVerify(t *testing.T) {
	h := NewHasher(testParams, 2)
	ctx := context.Background()
	hash, err := h.Hash(ctx, "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$m=8,t=1,p=1$") {
		t.Fatalf("unexpected format: %s", hash)
	}
	if strings.Contains(hash, "correct horse") {
		t.Fatal("hash leaks plaintext")
	}
	ok, rehash, err := h.Verify(ctx, "correct horse battery", hash)
	if err != nil || !ok || rehash {
		t.Fatalf("verify good: ok=%v rehash=%v err=%v", ok, rehash, err)
	}
	if ok, _, _ := h.Verify(ctx, "wrong password!", hash); ok {
		t.Fatal("wrong password verified")
	}
	hash2, _ := h.Hash(ctx, "correct horse battery")
	if hash == hash2 {
		t.Fatal("salt must make hashes unique")
	}
}

func TestPasswordNeedsRehash(t *testing.T) {
	ctx := context.Background()
	oldH := NewHasher(testParams, 1)
	hash, _ := oldH.Hash(ctx, "pw-pw-pw-pw-pw")
	stronger := testParams
	stronger.Time = 2
	ok, rehash, err := NewHasher(stronger, 1).Verify(ctx, "pw-pw-pw-pw-pw", hash)
	if err != nil || !ok || !rehash {
		t.Fatalf("expected ok+rehash, got ok=%v rehash=%v err=%v", ok, rehash, err)
	}
}

func TestPasswordInvalidHash(t *testing.T) {
	h := NewHasher(testParams, 1)
	for _, bad := range []string{"", "plaintext", "$argon2i$v=19$m=8,t=1,p=1$YQ$YQ", "$argon2id$v=19$m=8,t=1,p=0$YQ$YQ"} {
		if _, _, err := h.Verify(context.Background(), "x", bad); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
}

// ---- JWT ----

func TestJWTRoundTrip(t *testing.T) {
	m := NewTokenManager(testSecret(), "migo", time.Minute)
	tok, _, err := m.Issue("user-1")
	if err != nil {
		t.Fatal(err)
	}
	sub, err := m.Parse(tok)
	if err != nil || sub != "user-1" {
		t.Fatalf("got %q, %v", sub, err)
	}
}

func TestJWTRejects(t *testing.T) {
	m := NewTokenManager(testSecret(), "migo", time.Minute)
	tok, _, _ := m.Issue("user-1")
	parts := strings.Split(tok, ".")

	// expired
	past := NewTokenManager(testSecret(), "migo", time.Minute)
	past.now = func() time.Time { return time.Now().Add(-time.Hour) }
	expired, _, _ := past.Issue("user-1")

	// wrong secret / wrong issuer
	other := NewTokenManager([]byte(strings.Repeat("x", 32)), "migo", time.Minute)
	otherTok, _, _ := other.Issue("user-1")
	wrongIss := NewTokenManager(testSecret(), "evil", time.Minute)
	issTok, _, _ := wrongIss.Issue("user-1")

	// alg=none and tampered payload
	noneHeader := b64.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	forgedPayload := b64.EncodeToString([]byte(`{"iss":"migo","sub":"admin","iat":1,"exp":9999999999}`))

	cases := map[string]string{
		"empty":       "",
		"garbage":     "a.b.c",
		"expired":     expired,
		"wrong key":   otherTok,
		"wrong iss":   issTok,
		"alg none":    noneHeader + "." + parts[1] + ".",
		"tampered":    parts[0] + "." + forgedPayload + "." + parts[2],
		"two parts":   parts[0] + "." + parts[1],
		"bad sig b64": parts[0] + "." + parts[1] + ".!!!",
	}
	for name, tk := range cases {
		if _, err := m.Parse(tk); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("%s: expected ErrInvalidToken, got %v", name, err)
		}
	}
}

// ---- refresh tokens ----

func TestRefreshTokenHashing(t *testing.T) {
	raw, hash, err := NewRefreshToken()
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) < 40 || len(hash) != 32 {
		t.Fatalf("unexpected sizes: %d %d", len(raw), len(hash))
	}
	if !bytes.Equal(hash, HashRefreshToken(raw)) {
		t.Fatal("hash mismatch")
	}
	raw2, _, _ := NewRefreshToken()
	if raw == raw2 {
		t.Fatal("tokens must be unique")
	}
}

// ---- service ----

func newTestService(t *testing.T) (*Service, *fakeUsers, *fakeSessions) {
	t.Helper()
	users := newFakeUsers()
	sessions := &fakeSessions{users: users}
	svc, err := NewService(users, sessions, NewHasher(testParams, 2),
		NewTokenManager(testSecret(), "migo", 15*time.Minute), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return svc, users, sessions
}

func register(t *testing.T, svc *Service, name, email string) (user.User, Tokens) {
	t.Helper()
	u, tok, err := svc.Register(context.Background(), RegisterInput{FullName: name, Email: email, Password: "supersecret-pw"}, Meta{})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	return u, tok
}

func TestRegisterNormalizesAndGeneratesUsername(t *testing.T) {
	svc, _, _ := newTestService(t)
	u, tok := register(t, svc, "Feranmi Oresajo", "  Fer@Example.COM ")
	if u.Email != "fer@example.com" || u.Username != "feranmi-oresajo" {
		t.Fatalf("got %q %q", u.Email, u.Username)
	}
	if tok.AccessToken == "" || tok.RefreshToken == "" || tok.ExpiresIn != 900 {
		t.Fatalf("bad tokens: %+v", tok)
	}
	if u.PasswordHash == nil || strings.Contains(*u.PasswordHash, "supersecret") || !strings.HasPrefix(*u.PasswordHash, "$argon2id$") {
		t.Fatal("password not stored as argon2id hash")
	}
}

func TestRegisterUsernameCollisionGetsUniqueVariant(t *testing.T) {
	svc, _, _ := newTestService(t)
	a, _ := register(t, svc, "Ada Lovelace", "a1@example.com")
	b, _ := register(t, svc, "Ada Lovelace", "a2@example.com")
	c, _ := register(t, svc, "Ada Lovelace", "a3@example.com")
	if a.Username != "ada-lovelace" {
		t.Fatalf("first username %q", a.Username)
	}
	seen := map[string]bool{a.Username: true}
	for _, u := range []user.User{b, c} {
		if seen[u.Username] || !strings.HasPrefix(u.Username, "ada-lovelace-") {
			t.Fatalf("bad variant %q", u.Username)
		}
		seen[u.Username] = true
	}
}

func TestRegisterDuplicateEmail(t *testing.T) {
	svc, _, _ := newTestService(t)
	register(t, svc, "One", "dup@example.com")
	_, _, err := svc.Register(context.Background(), RegisterInput{FullName: "Two", Email: "DUP@example.com", Password: "supersecret-pw"}, Meta{})
	if !errors.Is(err, user.ErrEmailTaken) {
		t.Fatalf("expected ErrEmailTaken, got %v", err)
	}
}

func TestRegisterValidation(t *testing.T) {
	svc, _, _ := newTestService(t)
	_, _, err := svc.Register(context.Background(), RegisterInput{FullName: "", Email: "nope", Password: "short"}, Meta{})
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected ValidationError, got %v", err)
	}
	for _, f := range []string{"full_name", "email", "password"} {
		if ve.Fields[f] == "" {
			t.Errorf("missing field error for %s", f)
		}
	}
}

func TestLogin(t *testing.T) {
	svc, users, _ := newTestService(t)
	u, _ := register(t, svc, "Login User", "login@example.com")
	ctx := context.Background()

	if _, _, err := svc.Login(ctx, LoginInput{Email: " LOGIN@example.com", Password: "supersecret-pw"}, Meta{}); err != nil {
		t.Fatalf("good login: %v", err)
	}
	for name, in := range map[string]LoginInput{
		"wrong pw":     {Email: "login@example.com", Password: "wrong-password"},
		"unknown user": {Email: "ghost@example.com", Password: "supersecret-pw"},
		"empty":        {},
	} {
		if _, _, err := svc.Login(ctx, in, Meta{}); !errors.Is(err, ErrInvalidCredentials) {
			t.Errorf("%s: expected ErrInvalidCredentials, got %v", name, err)
		}
	}
	users.setActive(u.ID, false)
	if _, _, err := svc.Login(ctx, LoginInput{Email: "login@example.com", Password: "supersecret-pw"}, Meta{}); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("inactive user should look like invalid credentials, got %v", err)
	}
}

func TestLoginUpgradesOutdatedHash(t *testing.T) {
	svc, users, _ := newTestService(t)
	u, _ := register(t, svc, "Upgrade", "up@example.com")
	stronger := testParams
	stronger.Time = 2
	svc.hasher = NewHasher(stronger, 2)
	if _, _, err := svc.Login(context.Background(), LoginInput{Email: "up@example.com", Password: "supersecret-pw"}, Meta{}); err != nil {
		t.Fatal(err)
	}
	got, _ := users.GetByID(context.Background(), u.ID)
	if !strings.Contains(*got.PasswordHash, "t=2") {
		t.Fatalf("hash not upgraded: %s", *got.PasswordHash)
	}
}

func TestRefreshRotationAndReuseDetection(t *testing.T) {
	svc, _, sessions := newTestService(t)
	_, first := register(t, svc, "Rotate", "rot@example.com")
	ctx := context.Background()

	second, err := svc.Refresh(ctx, first.RefreshToken, Meta{})
	if err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	if second.RefreshToken == first.RefreshToken || second.AccessToken == "" {
		t.Fatal("refresh token must rotate")
	}

	// Replaying the old token is theft evidence: rejected AND family revoked.
	if _, err := svc.Refresh(ctx, first.RefreshToken, Meta{}); !errors.Is(err, ErrInvalidRefreshToken) {
		t.Fatalf("reuse should fail, got %v", err)
	}
	if _, err := svc.Refresh(ctx, second.RefreshToken, Meta{}); !errors.Is(err, ErrInvalidRefreshToken) {
		t.Fatalf("legit descendant must be revoked after reuse, got %v", err)
	}

	// Only digests are stored.
	for _, h := range sessions.rawHashes() {
		if bytes.Equal(h, []byte(first.RefreshToken)) || bytes.Equal(h, []byte(second.RefreshToken)) {
			t.Fatal("raw refresh token stored")
		}
	}
}

func TestRefreshRejectsUnknownExpiredAndInactive(t *testing.T) {
	svc, users, _ := newTestService(t)
	ctx := context.Background()
	if _, err := svc.Refresh(ctx, "", Meta{}); !errors.Is(err, ErrInvalidRefreshToken) {
		t.Fatal("empty token must fail")
	}
	if _, err := svc.Refresh(ctx, "not-a-real-token", Meta{}); !errors.Is(err, ErrInvalidRefreshToken) {
		t.Fatal("unknown token must fail")
	}

	u, tok := register(t, svc, "Expiry", "exp@example.com")
	svc.now = func() time.Time { return time.Now().Add(2 * time.Hour) } // past 1h TTL
	if _, err := svc.Refresh(ctx, tok.RefreshToken, Meta{}); !errors.Is(err, ErrInvalidRefreshToken) {
		t.Fatalf("expired token must fail, got %v", err)
	}

	svc.now = time.Now
	_, tok2 := register(t, svc, "Inactive", "inactive@example.com")
	uu, _ := users.GetByEmail(ctx, "inactive@example.com")
	users.setActive(uu.ID, false)
	if _, err := svc.Refresh(ctx, tok2.RefreshToken, Meta{}); !errors.Is(err, ErrInvalidRefreshToken) {
		t.Fatalf("inactive user must not refresh, got %v", err)
	}
	_ = u
}

func TestLogoutRevokesSession(t *testing.T) {
	svc, _, _ := newTestService(t)
	_, tok := register(t, svc, "Logout", "out@example.com")
	ctx := context.Background()
	if err := svc.Logout(ctx, tok.RefreshToken); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Refresh(ctx, tok.RefreshToken, Meta{}); !errors.Is(err, ErrInvalidRefreshToken) {
		t.Fatalf("refresh after logout must fail, got %v", err)
	}
	// idempotent and non-revealing
	if err := svc.Logout(ctx, tok.RefreshToken); err != nil {
		t.Fatal(err)
	}
	if err := svc.Logout(ctx, "unknown"); err != nil {
		t.Fatal(err)
	}
}

// ---- HTTP layer ----

func newTestMux(t *testing.T) http.Handler {
	t.Helper()
	svc, _, _ := newTestService(t)
	h := NewHandler(svc, slog.New(slog.NewTextHandler(io.Discard, nil)))
	mux := http.NewServeMux()
	mux.HandleFunc("POST /register", h.Register)
	mux.HandleFunc("POST /login", h.Login)
	mux.HandleFunc("POST /refresh", h.Refresh)
	mux.HandleFunc("POST /logout", h.Logout)
	mux.Handle("GET /me", RequireAuth(svc.tokens)(http.HandlerFunc(h.Me)))
	return mux
}

func do(t *testing.T, mux http.Handler, method, path, body, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestHTTPFullFlow(t *testing.T) {
	mux := newTestMux(t)

	rec := do(t, mux, "POST", "/register", `{"full_name":"Flow User","email":"flow@example.com","password":"supersecret-pw"}`, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "password") {
		t.Fatal("response must not contain password material")
	}
	var reg struct {
		User         map[string]any `json:"user"`
		AccessToken  string         `json:"access_token"`
		RefreshToken string         `json:"refresh_token"`
		TokenType    string         `json:"token_type"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &reg)
	if reg.TokenType != "Bearer" || reg.User["username"] != "flow-user" {
		t.Fatalf("bad register body: %s", rec.Body)
	}

	if rec := do(t, mux, "GET", "/me", "", reg.AccessToken); rec.Code != 200 || !strings.Contains(rec.Body.String(), "flow@example.com") {
		t.Fatalf("me: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, mux, "GET", "/me", "", ""); rec.Code != 401 {
		t.Fatalf("me without token: %d", rec.Code)
	}
	if rec := do(t, mux, "GET", "/me", "", "garbage.token.here"); rec.Code != 401 {
		t.Fatalf("me with bad token: %d", rec.Code)
	}

	rec = do(t, mux, "POST", "/refresh", `{"refresh_token":"`+reg.RefreshToken+`"}`, "")
	if rec.Code != 200 {
		t.Fatalf("refresh: %d %s", rec.Code, rec.Body)
	}
	var ref struct {
		RefreshToken string `json:"refresh_token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &ref)

	if rec := do(t, mux, "POST", "/logout", `{"refresh_token":"`+ref.RefreshToken+`"}`, ""); rec.Code != 204 {
		t.Fatalf("logout: %d", rec.Code)
	}
	if rec := do(t, mux, "POST", "/refresh", `{"refresh_token":"`+ref.RefreshToken+`"}`, ""); rec.Code != 401 {
		t.Fatalf("refresh after logout: %d", rec.Code)
	}
}

func TestHTTPErrorsAndLimits(t *testing.T) {
	mux := newTestMux(t)

	rec := do(t, mux, "POST", "/register", `{"full_name":"","email":"bad","password":"x"}`, "")
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), `"validation_failed"`) {
		t.Fatalf("validation: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, mux, "POST", "/register", `{"nope":1}`, ""); rec.Code != 400 {
		t.Fatalf("unknown field: %d", rec.Code)
	}
	if rec := do(t, mux, "POST", "/register", `not json`, ""); rec.Code != 400 {
		t.Fatalf("bad json: %d", rec.Code)
	}
	if rec := do(t, mux, "POST", "/login", `{"email":"a@b.co","password":"x"}{"extra":1}`, ""); rec.Code != 400 {
		t.Fatalf("trailing data: %d", rec.Code)
	}
	big := `{"full_name":"` + strings.Repeat("a", 20000) + `","email":"a@b.co","password":"x"}`
	if rec := do(t, mux, "POST", "/register", big, ""); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize: %d", rec.Code)
	}

	do(t, mux, "POST", "/register", `{"full_name":"Dup","email":"dup@example.com","password":"supersecret-pw"}`, "")
	rec = do(t, mux, "POST", "/register", `{"full_name":"Dup","email":"dup@example.com","password":"supersecret-pw"}`, "")
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), `"email_taken"`) {
		t.Fatalf("duplicate: %d %s", rec.Code, rec.Body)
	}

	rec = do(t, mux, "POST", "/login", `{"email":"dup@example.com","password":"wrong-password"}`, "")
	rec2 := do(t, mux, "POST", "/login", `{"email":"ghost@example.com","password":"wrong-password"}`, "")
	if rec.Code != 401 || rec2.Code != 401 || rec.Body.String() != rec2.Body.String() {
		t.Fatalf("login failures must be identical: %d/%d", rec.Code, rec2.Code)
	}
}
