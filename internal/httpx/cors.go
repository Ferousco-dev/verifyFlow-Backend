package httpx

import (
	"net/http"
)

// CORS allows cross-origin browser calls from an explicit allow-list of
// origins. The API authenticates with Bearer tokens (no cookies), so
// credentialed CORS is deliberately NOT enabled and "*" is never emitted.
//
// It must wrap the router so preflights are answered before route matching
// and so error responses (e.g. 429) still carry CORS headers; otherwise the
// browser reports an opaque network error instead of the API error.
//
// An empty allow-list disables CORS entirely (same-origin only).
func CORS(allowed []string) func(http.Handler) http.Handler {
	set := make(map[string]struct{}, len(allowed))
	for _, o := range allowed {
		set[o] = struct{}{}
	}
	return func(next http.Handler) http.Handler {
		if len(set) == 0 {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin == "" {
				next.ServeHTTP(w, r)
				return
			}
			w.Header().Add("Vary", "Origin")
			_, ok := set[origin]
			preflight := r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != ""

			switch {
			case ok && preflight:
				h := w.Header()
				h.Set("Access-Control-Allow-Origin", origin)
				h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
				h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
				h.Set("Access-Control-Max-Age", "600")
				w.WriteHeader(http.StatusNoContent)
			case !ok && preflight:
				WriteError(w, http.StatusForbidden, "cors_origin_not_allowed", "This origin is not allowed.", nil)
			case ok:
				h := w.Header()
				h.Set("Access-Control-Allow-Origin", origin)
				h.Set("Access-Control-Expose-Headers", "X-Request-ID, Retry-After")
				next.ServeHTTP(w, r)
			default:
				// Disallowed origin on a normal request: no CORS headers, so the
				// browser blocks the response. (CORS is not an authorization layer.)
				next.ServeHTTP(w, r)
			}
		})
	}
}
