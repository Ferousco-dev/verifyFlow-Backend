package httpx

import (
	"context"
	"net"
	"net/http"
	"strings"
)

type clientIPKey struct{}

// ClientIPMiddleware resolves the caller's IP once and stores it in the
// request context; read it with ClientIP.
//
// X-Forwarded-For is only honoured when the direct peer (RemoteAddr) is one
// of the trusted proxies. Otherwise any client could spoof the header to dodge
// rate limits or forge the IP recorded on its session.
func ClientIPMiddleware(trusted []*net.IPNet) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := resolveClientIP(r, trusted)
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), clientIPKey{}, ip)))
		})
	}
}

// ClientIP returns the resolved client IP, falling back to the peer address
// when the middleware isn't installed (e.g. in unit tests).
func ClientIP(r *http.Request) string {
	if ip, ok := r.Context().Value(clientIPKey{}).(string); ok && ip != "" {
		return ip
	}
	return peerIP(r)
}

func peerIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.String()
	}
	return host
}

func isTrusted(ip net.IP, trusted []*net.IPNet) bool {
	for _, n := range trusted {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func resolveClientIP(r *http.Request, trusted []*net.IPNet) string {
	peer := peerIP(r)
	peerParsed := net.ParseIP(peer)
	if len(trusted) == 0 || peerParsed == nil || !isTrusted(peerParsed, trusted) {
		return peer
	}

	// Walk X-Forwarded-For right-to-left, skipping our own proxies. The first
	// untrusted address is the real client; entries further left are
	// client-controlled and ignored.
	var hops []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				hops = append(hops, p)
			}
		}
	}
	for i := len(hops) - 1; i >= 0; i-- {
		ip := net.ParseIP(hops[i])
		if ip == nil {
			return peer // malformed header: don't trust any of it
		}
		if !isTrusted(ip, trusted) {
			return ip.String()
		}
	}
	if len(hops) > 0 {
		if ip := net.ParseIP(hops[0]); ip != nil {
			return ip.String()
		}
	}
	return peer
}
