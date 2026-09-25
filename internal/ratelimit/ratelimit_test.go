package ratelimit

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTest(l Limit) (*Memory, *clock) {
	c := &clock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	m := NewMemory(l)
	m.now = c.now
	return m, c
}

var ctx = context.Background()

func TestBurstThenDeny(t *testing.T) {
	m, _ := newTest(Limit{Burst: 3, Every: time.Minute})
	for i := 0; i < 3; i++ {
		d, _ := m.Allow(ctx, "k")
		if !d.Allowed || d.Remaining != 2-i {
			t.Fatalf("request %d: %+v", i, d)
		}
	}
	d, _ := m.Allow(ctx, "k")
	if d.Allowed {
		t.Fatal("4th request must be denied")
	}
	if d.RetryAfter <= 0 || d.RetryAfter > time.Minute {
		t.Fatalf("RetryAfter out of range: %v", d.RetryAfter)
	}
}

func TestRefillAndRetryAfter(t *testing.T) {
	m, c := newTest(Limit{Burst: 2, Every: 10 * time.Second})
	m.Allow(ctx, "k")
	m.Allow(ctx, "k")
	d, _ := m.Allow(ctx, "k")
	if d.Allowed || d.RetryAfter != 10*time.Second {
		t.Fatalf("expected deny with 10s retry, got %+v", d)
	}
	c.advance(4 * time.Second)
	d, _ = m.Allow(ctx, "k")
	if d.Allowed || d.RetryAfter != 6*time.Second {
		t.Fatalf("expected deny with 6s retry, got %+v", d)
	}
	c.advance(6 * time.Second)
	if d, _ = m.Allow(ctx, "k"); !d.Allowed {
		t.Fatal("token should have refilled")
	}
	// Refill never exceeds Burst, however long we wait.
	c.advance(24 * time.Hour)
	allowed := 0
	for i := 0; i < 5; i++ {
		if d, _ := m.Allow(ctx, "k"); d.Allowed {
			allowed++
		}
	}
	if allowed != 2 {
		t.Fatalf("burst cap violated: %d allowed", allowed)
	}
}

func TestKeysAreIndependentAndResettable(t *testing.T) {
	m, _ := newTest(Limit{Burst: 1, Every: time.Hour})
	if d, _ := m.Allow(ctx, "a"); !d.Allowed {
		t.Fatal("a first")
	}
	if d, _ := m.Allow(ctx, "a"); d.Allowed {
		t.Fatal("a second should be denied")
	}
	if d, _ := m.Allow(ctx, "b"); !d.Allowed {
		t.Fatal("b must not be affected by a")
	}
	_ = m.Reset(ctx, "a")
	if d, _ := m.Allow(ctx, "a"); !d.Allowed {
		t.Fatal("Reset should restore the budget")
	}
}

func TestConcurrentAllowExactBudget(t *testing.T) {
	m := NewMemory(Limit{Burst: 10, Every: time.Hour})
	var allowed atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if d, _ := m.Allow(ctx, "shared"); d.Allowed {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	if allowed.Load() != 10 {
		t.Fatalf("exactly 10 must pass, got %d", allowed.Load())
	}
}

func TestIdleKeysPurgedAndMemoryBounded(t *testing.T) {
	m, c := newTest(Limit{Burst: 2, Every: time.Minute})
	for i := 0; i < 50; i++ {
		m.Allow(ctx, "k"+strconv.Itoa(i))
	}
	if len(m.entries) != 50 {
		t.Fatalf("expected 50 entries, got %d", len(m.entries))
	}
	c.advance(10 * time.Minute) // all buckets fully refilled -> carry no state
	m.Allow(ctx, "fresh")
	if len(m.entries) != 1 {
		t.Fatalf("idle keys should be purged, %d remain", len(m.entries))
	}

	// Hard cap: past maxKeys new keys fail open instead of growing memory.
	small, _ := newTest(Limit{Burst: 1, Every: time.Hour})
	small.maxKeys = 3
	for i := 0; i < 10; i++ {
		d, _ := small.Allow(ctx, "x"+strconv.Itoa(i))
		if !d.Allowed {
			t.Fatal("new keys over the cap must fail open")
		}
	}
	if len(small.entries) > 3 {
		t.Fatalf("entries exceeded cap: %d", len(small.entries))
	}
}

func TestNewMemoryRejectsInvalidLimit(t *testing.T) {
	for _, l := range []Limit{{0, time.Second}, {1, 0}, {-1, time.Second}} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("expected panic for %+v", l)
				}
			}()
			NewMemory(l)
		}()
	}
}

func TestIPKey(t *testing.T) {
	key := func(remote string) string {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = remote
		return IPKey(r)
	}
	if key("203.0.113.9:5555") != "203.0.113.9" {
		t.Fatal("ipv4 key")
	}
	a, b := key("[2001:db8:1:2:aaaa::1]:1"), key("[2001:db8:1:2:bbbb::9]:1")
	if a != b || a != "2001:db8:1:2::/64" {
		t.Fatalf("ipv6 addresses in one /64 must share a bucket: %q %q", a, b)
	}
	if key("[2001:db8:1:3::1]:1") == a {
		t.Fatal("different /64s must not share a bucket")
	}
}

func TestMiddleware429(t *testing.T) {
	m := NewMemory(Limit{Burst: 2, Every: 90 * time.Second})
	called := 0
	h := Middleware(m, IPKey, slog.New(slog.NewTextHandler(io.Discard, nil)))(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called++ }))

	do := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/", nil)
		r.RemoteAddr = "198.51.100.1:1"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	do()
	do()
	w := do()
	if w.Code != 429 || called != 2 {
		t.Fatalf("status=%d called=%d", w.Code, called)
	}
	if ra := w.Header().Get("Retry-After"); ra != "90" {
		t.Fatalf("Retry-After = %q", ra)
	}
	if w.Header().Get("Content-Type") == "" || !contains(w.Body.String(), `"rate_limited"`) {
		t.Fatalf("unexpected body: %s", w.Body)
	}
}

func TestMiddlewareNilLimiterIsPassthroughAndBackendErrorFailsOpen(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })

	w := httptest.NewRecorder()
	Middleware(nil, IPKey, log)(ok).ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 204 {
		t.Fatalf("nil limiter: %d", w.Code)
	}

	w = httptest.NewRecorder()
	Middleware(failing{}, IPKey, log)(ok).ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 204 {
		t.Fatalf("backend error must fail open, got %d", w.Code)
	}
}

type failing struct{}

func (failing) Allow(context.Context, string) (Decision, error) {
	return Decision{}, context.DeadlineExceeded
}
func (failing) Reset(context.Context, string) error { return nil }

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
