package httpx

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func cidrs(t *testing.T, list ...string) []*net.IPNet {
	t.Helper()
	var out []*net.IPNet
	for _, c := range list {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	return out
}

func ipFor(t *testing.T, trusted []*net.IPNet, remote string, xff ...string) string {
	t.Helper()
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = remote
	for _, v := range xff {
		r.Header.Add("X-Forwarded-For", v)
	}
	var got string
	h := ClientIPMiddleware(trusted)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = ClientIP(r) }))
	h.ServeHTTP(httptest.NewRecorder(), r)
	return got
}

func TestClientIPIgnoresXFFWithoutTrustedProxy(t *testing.T) {
	if got := ipFor(t, nil, "203.0.113.9:4000", "1.2.3.4"); got != "203.0.113.9" {
		t.Fatalf("spoofed XFF honoured: %s", got)
	}
	// Peer is not in the trusted set -> header is attacker-controlled.
	if got := ipFor(t, cidrs(t, "10.0.0.0/8"), "203.0.113.9:4000", "1.2.3.4"); got != "203.0.113.9" {
		t.Fatalf("untrusted peer's XFF honoured: %s", got)
	}
}

func TestClientIPWithTrustedProxy(t *testing.T) {
	tr := cidrs(t, "10.0.0.0/8")
	cases := []struct {
		name, remote string
		xff          []string
		want         string
	}{
		{"single hop", "10.0.0.1:1", []string{"198.51.100.7"}, "198.51.100.7"},
		{"client-forged prefix is ignored", "10.0.0.1:1", []string{"6.6.6.6, 198.51.100.7"}, "198.51.100.7"},
		{"chain of our proxies", "10.0.0.1:1", []string{"198.51.100.7, 10.0.0.5"}, "198.51.100.7"},
		{"multiple header lines", "10.0.0.1:1", []string{"6.6.6.6", "198.51.100.7"}, "198.51.100.7"},
		{"ipv6 client", "10.0.0.1:1", []string{"2001:db8::1"}, "2001:db8::1"},
		{"no header falls back to peer", "10.0.0.1:1", nil, "10.0.0.1"},
		{"malformed entry falls back to peer", "10.0.0.1:1", []string{"198.51.100.7, garbage"}, "10.0.0.1"},
		{"all hops trusted -> leftmost", "10.0.0.1:1", []string{"10.1.1.1, 10.2.2.2"}, "10.1.1.1"},
	}
	for _, c := range cases {
		if got := ipFor(t, tr, c.remote, c.xff...); got != c.want {
			t.Errorf("%s: got %s want %s", c.name, got, c.want)
		}
	}
}

func TestClientIPFallbackWithoutMiddleware(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "192.0.2.10:99"
	if got := ClientIP(r); got != "192.0.2.10" {
		t.Fatal(got)
	}
}

func corsCall(allowed []string, method, origin string, preflight bool) *httptest.ResponseRecorder {
	h := CORS(allowed)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	r := httptest.NewRequest(method, "/api/x", nil)
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	if preflight {
		r.Header.Set("Access-Control-Request-Method", "POST")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestCORSAllowedOrigin(t *testing.T) {
	allowed := []string{"https://app.example.com"}
	w := corsCall(allowed, "POST", "https://app.example.com", false)
	if w.Code != 200 || w.Header().Get("Access-Control-Allow-Origin") != "https://app.example.com" {
		t.Fatalf("headers: %v", w.Header())
	}
	if w.Header().Get("Access-Control-Allow-Credentials") != "" {
		t.Fatal("credentialed CORS must stay off")
	}
	if w.Header().Get("Access-Control-Allow-Origin") == "*" {
		t.Fatal("wildcard must never be emitted")
	}
	if got := w.Header().Values("Vary"); len(got) == 0 || got[0] != "Origin" {
		t.Fatalf("Vary: %v", got)
	}
}

func TestCORSPreflight(t *testing.T) {
	allowed := []string{"https://app.example.com"}
	w := corsCall(allowed, "OPTIONS", "https://app.example.com", true)
	if w.Code != 204 || w.Header().Get("Access-Control-Allow-Headers") != "Authorization, Content-Type" ||
		w.Header().Get("Access-Control-Allow-Methods") == "" || w.Header().Get("Access-Control-Max-Age") == "" {
		t.Fatalf("bad preflight: %d %v", w.Code, w.Header())
	}
	w = corsCall(allowed, "OPTIONS", "https://evil.example", true)
	if w.Code != 403 || w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("disallowed preflight: %d %v", w.Code, w.Header())
	}
}

func TestCORSDisallowedAndAbsentOrigin(t *testing.T) {
	allowed := []string{"https://app.example.com"}
	w := corsCall(allowed, "POST", "https://evil.example", false)
	if w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("disallowed origin got CORS headers")
	}
	// A subdomain or scheme variation is a different origin.
	for _, o := range []string{"https://app.example.com.evil.io", "http://app.example.com", "https://sub.app.example.com"} {
		if corsCall(allowed, "POST", o, false).Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatalf("origin %s must not match", o)
		}
	}
	w = corsCall(allowed, "POST", "", false)
	if w.Code != 200 || w.Header().Get("Vary") != "" {
		t.Fatalf("no Origin header: %d %v", w.Code, w.Header())
	}
	// Empty allow-list disables CORS entirely.
	if corsCall(nil, "POST", "https://app.example.com", false).Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("empty allow-list must emit nothing")
	}
}
