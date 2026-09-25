// Package server wires routes and middleware into an http.Handler.
package server

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"time"

	"migo/internal/auth"
	"migo/internal/httpx"
	"migo/internal/ratelimit"
)

// Pinger is satisfied by *pgxpool.Pool.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Limiters are the per-route auth rate limiters. A nil field disables that limit.
type Limiters struct {
	Register     ratelimit.Limiter // per IP
	Login        ratelimit.Limiter // per IP
	LoginAccount ratelimit.Limiter // per IP + email (installed on the auth handler)
	Refresh      ratelimit.Limiter // per IP; shared by refresh and logout

	Forgot      ratelimit.Limiter // per IP
	ForgotEmail ratelimit.Limiter // per email (installed on the auth handler)
	Reset       ratelimit.Limiter // per IP

	Verify             ratelimit.Limiter // per IP (verify-email)
	Resend             ratelimit.Limiter // per IP (resend-verification)
	ResendUser         ratelimit.Limiter // per signed-in user (installed on the auth handler)
	ChangePassword     ratelimit.Limiter // per IP
	ChangePasswordUser ratelimit.Limiter // per signed-in user (installed on the auth handler)
}

// DefaultLimiters returns in-memory limiters with production-sane budgets.
// A "burst" is how many requests are allowed at once; tokens then refill at
// one per "every". Every field must be set (see TestDefaultLimitersAllSet).
func DefaultLimiters() Limiters {
	mem := func(burst int, every time.Duration) ratelimit.Limiter {
		return ratelimit.NewMemory(ratelimit.Limit{Burst: burst, Every: every})
	}
	return Limiters{
		Register:     mem(5, 2*time.Minute),  // ~30/hour sustained
		Login:        mem(10, 6*time.Second), // ~10/min sustained
		LoginAccount: mem(5, 3*time.Minute),  // ~20/hour per account+IP
		Refresh:      mem(30, 2*time.Second), // ~30/min sustained

		Forgot:      mem(5, 6*time.Minute),   // ~10/hour per IP
		ForgotEmail: mem(3, 20*time.Minute),  // ~3/hour per address
		Reset:       mem(10, 30*time.Second), // ~2/min per IP

		Verify:             mem(10, 30*time.Second), // ~2/min per IP
		Resend:             mem(5, 6*time.Minute),   // ~10/hour per IP
		ResendUser:         mem(3, 20*time.Minute),  // ~3/hour per user
		ChangePassword:     mem(10, time.Minute),    // ~10/min per IP
		ChangePasswordUser: mem(5, 3*time.Minute),   // ~20/hour per user
	}
}

type Deps struct {
	Log            *slog.Logger
	DB             Pinger
	Auth           *auth.Handler
	Tokens         *auth.TokenManager
	Limiters       Limiters
	AllowedOrigins []string
	TrustedProxies []*net.IPNet
}

func New(d Deps) http.Handler {
	mux := http.NewServeMux()
	limit := func(l ratelimit.Limiter) func(http.Handler) http.Handler {
		return ratelimit.Middleware(l, ratelimit.IPKey, d.Log)
	}

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := d.DB.Ping(ctx); err != nil {
			d.Log.Error("health: database ping failed", "error", err)
			httpx.WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "degraded", "database": "unavailable"})
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok", "database": "ok"})
	})

	mux.Handle("POST /api/v1/auth/register", limit(d.Limiters.Register)(http.HandlerFunc(d.Auth.Register)))
	mux.Handle("POST /api/v1/auth/login", limit(d.Limiters.Login)(http.HandlerFunc(d.Auth.Login)))
	mux.Handle("POST /api/v1/auth/refresh", limit(d.Limiters.Refresh)(http.HandlerFunc(d.Auth.Refresh)))
	mux.Handle("POST /api/v1/auth/logout", limit(d.Limiters.Refresh)(http.HandlerFunc(d.Auth.Logout)))

	mux.Handle("POST /api/v1/auth/forgot-password", limit(d.Limiters.Forgot)(http.HandlerFunc(d.Auth.ForgotPassword)))
	mux.Handle("POST /api/v1/auth/reset-password", limit(d.Limiters.Reset)(http.HandlerFunc(d.Auth.ResetPassword)))

	mux.Handle("POST /api/v1/auth/verify-email", limit(d.Limiters.Verify)(http.HandlerFunc(d.Auth.VerifyEmail)))

	// Signed-in routes: IP limit first (cheap, rejects floods before token work), then auth.
	requireAuth := auth.RequireAuth(d.Tokens)
	mux.Handle("GET /api/v1/me", requireAuth(http.HandlerFunc(d.Auth.Me)))
	mux.Handle("POST /api/v1/auth/resend-verification", limit(d.Limiters.Resend)(requireAuth(http.HandlerFunc(d.Auth.ResendVerification))))
	mux.Handle("POST /api/v1/auth/change-password", limit(d.Limiters.ChangePassword)(requireAuth(http.HandlerFunc(d.Auth.ChangePassword))))

	// Outermost first: logging sees everything; CORS wraps the router so
	// preflights and 429s carry the right headers; client-IP resolution runs
	// before any handler that keys on the IP.
	var h http.Handler = mux
	h = httpx.SecurityHeaders(h)
	h = httpx.ClientIPMiddleware(d.TrustedProxies)(h)
	h = httpx.CORS(d.AllowedOrigins)(h)
	h = httpx.Recover(d.Log)(h)
	h = httpx.Logging(d.Log)(h)
	return h
}
