package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"migo/internal/httpx"
)

type ctxKey struct{}

// UserIDFromContext returns the authenticated user's ID set by RequireAuth.
func UserIDFromContext(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(ctxKey{}).(string)
	return id, ok
}

// RequireAuth verifies the Bearer access token and stores the user ID in the
// request context. All failures return the same 401.
func RequireAuth(tokens *TokenManager) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := r.Header.Get("Authorization")
			scheme, tok, ok := strings.Cut(h, " ")
			if !ok || !strings.EqualFold(scheme, "Bearer") || tok == "" {
				unauthorized(w)
				return
			}
			userID, err := tokens.Parse(strings.TrimSpace(tok))
			if err != nil {
				unauthorized(w)
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, userID)))
		})
	}
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="migo"`)
	httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "Authentication required.", nil)
}

// RequireVerifiedEmail rejects users whose email isn't verified with
// 403 email_not_verified. It must run AFTER RequireAuth. Use it on routes
// that need a confirmed address (e.g. renting numbers, payments).
func RequireVerifiedEmail(svc *Service) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			userID, ok := UserIDFromContext(r.Context())
			if !ok {
				unauthorized(w)
				return
			}
			u, err := svc.Me(r.Context(), userID)
			if err != nil {
				if errors.Is(err, ErrUnauthorized) {
					unauthorized(w)
					return
				}
				httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "Something went wrong.", nil)
				return
			}
			if !u.EmailVerified {
				httpx.WriteError(w, http.StatusForbidden, "email_not_verified", "Please verify your email address to use this feature.", nil)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireRole rejects users whose role does not match. It must run AFTER
// RequireAuth. Use it on operator/admin-only routes (e.g. changing another
// user's role, managing provider configs).
func RequireRole(svc *Service, role string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			userID, ok := UserIDFromContext(r.Context())
			if !ok {
				unauthorized(w)
				return
			}
			u, err := svc.Me(r.Context(), userID)
			if err != nil {
				if errors.Is(err, ErrUnauthorized) {
					unauthorized(w)
					return
				}
				httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "Something went wrong.", nil)
				return
			}
			if u.Role != role {
				httpx.WriteError(w, http.StatusForbidden, "forbidden", "You do not have permission to perform this action.", nil)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
