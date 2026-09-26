package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"migo/internal/user"
)

func TestSetUserRolePromotesAndDemotes(t *testing.T) {
	svc, _, _ := newTestService(t)
	a, _ := register(t, svc, "Admin", "admin@example.com")
	b, _ := register(t, svc, "Second Admin", "second-admin@example.com")

	if _, err := svc.SetUserRole(context.Background(), a.ID, user.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetUserRole(context.Background(), b.ID, user.RoleAdmin); err != nil {
		t.Fatal(err)
	}

	got, err := svc.SetUserRole(context.Background(), a.ID, user.RoleUser)
	if err != nil {
		t.Fatalf("demoting one of two admins: %v", err)
	}
	if got.Role != user.RoleUser {
		t.Fatalf("role = %q", got.Role)
	}
}

func TestSetUserRoleRefusesToDemoteLastAdmin(t *testing.T) {
	svc, _, _ := newTestService(t)
	a, _ := register(t, svc, "Only Admin", "only-admin@example.com")
	if _, err := svc.SetUserRole(context.Background(), a.ID, user.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetUserRole(context.Background(), a.ID, user.RoleUser); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("demoting the last admin error = %v", err)
	}
}

func TestSetUserRoleRejectsUnknownRole(t *testing.T) {
	svc, _, _ := newTestService(t)
	u, _ := register(t, svc, "User", "unknown-role@example.com")
	if _, err := svc.SetUserRole(context.Background(), u.ID, "superuser"); !errors.Is(err, ErrInvalidRole) {
		t.Fatalf("unknown role error = %v", err)
	}
}

func TestSetUserRoleUnknownUser(t *testing.T) {
	svc, _, _ := newTestService(t)
	if _, err := svc.SetUserRole(context.Background(), "does-not-exist", user.RoleAdmin); !errors.Is(err, user.ErrNotFound) {
		t.Fatalf("unknown user error = %v", err)
	}
}

func TestSetUserRoleIsNoopWhenUnchanged(t *testing.T) {
	svc, _, _ := newTestService(t)
	u, _ := register(t, svc, "User", "noop-role@example.com")
	got, err := svc.SetUserRole(context.Background(), u.ID, user.RoleUser)
	if err != nil || got.Role != user.RoleUser {
		t.Fatalf("noop role change: %+v, %v", got, err)
	}
}

func TestRequireRoleMiddleware(t *testing.T) {
	svc, _, _ := newTestService(t)
	admin, adminTok := register(t, svc, "Admin", "require-role-admin@example.com")
	_, regularTok := register(t, svc, "Regular", "require-role-user@example.com")
	if _, err := svc.SetUserRole(context.Background(), admin.ID, user.RoleAdmin); err != nil {
		t.Fatal(err)
	}

	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	chain := RequireAuth(svc.tokens)(RequireRole(svc, user.RoleAdmin)(ok))

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
	if w := call(regularTok.AccessToken); w.Code != 403 || !strings.Contains(w.Body.String(), `"forbidden"`) {
		t.Fatalf("non-admin: %d %s", w.Code, w.Body)
	}
	if w := call(adminTok.AccessToken); w.Code != 200 {
		t.Fatalf("admin: %d", w.Code)
	}
	// Used without RequireAuth in front it fails closed.
	w := httptest.NewRecorder()
	RequireRole(svc, user.RoleAdmin)(ok).ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 401 {
		t.Fatalf("misconfigured chain must fail closed: %d", w.Code)
	}
}
