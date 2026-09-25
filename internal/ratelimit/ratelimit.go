// Package ratelimit provides request rate limiting behind a small interface.
//
// The in-memory implementation is per-process. If Migo runs several API
// instances, each enforces its own budget; swap in a Redis-backed Limiter
// (same interface) at that point. Redis is intentionally not used yet.
package ratelimit

import (
	"context"
	"log/slog"
	"math"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"migo/internal/httpx"
)

// Limit is a token bucket: up to Burst requests at once, and one more token
// regained every Every.
type Limit struct {
	Burst int
	Every time.Duration
}

type Decision struct {
	Allowed    bool
	Remaining  int
	RetryAfter time.Duration
}

type Limiter interface {
	// Allow consumes one token for key.
	Allow(ctx context.Context, key string) (Decision, error)
	// Reset forgets key (e.g. after a successful login).
	Reset(ctx context.Context, key string) error
}

const (
	defaultMaxKeys       = 100_000
	defaultPurgeInterval = time.Minute
)

type entry struct {
	tokens float64
	last   time.Time
}

// Memory is a concurrency-safe in-memory token-bucket limiter. Idle keys are
// purged lazily (no goroutine to leak) and the key set is bounded.
type Memory struct {
	mu        sync.Mutex
	limit     Limit
	now       func() time.Time
	entries   map[string]*entry
	maxKeys   int
	lastPurge time.Time
}

// NewMemory panics on an invalid Limit: that is a programming error.
func NewMemory(l Limit) *Memory {
	if l.Burst < 1 || l.Every <= 0 {
		panic("ratelimit: Burst must be >= 1 and Every > 0")
	}
	return &Memory{limit: l, now: time.Now, entries: make(map[string]*entry), maxKeys: defaultMaxKeys}
}

func (m *Memory) Allow(_ context.Context, key string) (Decision, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	m.purgeLocked(now)

	e, ok := m.entries[key]
	if !ok {
		if len(m.entries) >= m.maxKeys {
			// Still full after purging idle keys (extreme, many-source flood).
			// Fail open rather than lock out legitimate users; bounded memory wins.
			return Decision{Allowed: true, Remaining: 0}, nil
		}
		e = &entry{tokens: float64(m.limit.Burst), last: now}
		m.entries[key] = e
	}

	if elapsed := now.Sub(e.last); elapsed > 0 {
		e.tokens = math.Min(float64(m.limit.Burst), e.tokens+float64(elapsed)/float64(m.limit.Every))
		e.last = now
	}
	if e.tokens >= 1 {
		e.tokens--
		return Decision{Allowed: true, Remaining: int(e.tokens)}, nil
	}
	wait := time.Duration((1 - e.tokens) * float64(m.limit.Every))
	return Decision{Allowed: false, RetryAfter: wait}, nil
}

func (m *Memory) Reset(_ context.Context, key string) error {
	m.mu.Lock()
	delete(m.entries, key)
	m.mu.Unlock()
	return nil
}

// purgeLocked drops keys whose bucket has fully refilled (they carry no state).
func (m *Memory) purgeLocked(now time.Time) {
	if now.Sub(m.lastPurge) < defaultPurgeInterval && len(m.entries) < m.maxKeys {
		return
	}
	m.lastPurge = now
	full := time.Duration(m.limit.Burst) * m.limit.Every
	for k, e := range m.entries {
		if now.Sub(e.last) >= full {
			delete(m.entries, k)
		}
	}
}

// IPKey keys a request by client IP. IPv6 addresses are collapsed to their /64
// so an attacker with a whole /64 can't get a fresh bucket per address.
func IPKey(r *http.Request) string {
	ip := net.ParseIP(httpx.ClientIP(r))
	if ip == nil {
		return httpx.ClientIP(r)
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

// Deny writes the standard 429 response.
func Deny(w http.ResponseWriter, d Decision) {
	secs := int(math.Ceil(d.RetryAfter.Seconds()))
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	httpx.WriteError(w, http.StatusTooManyRequests, "rate_limited", "Too many requests. Please try again later.", nil)
}

// Middleware limits by key(r). A nil limiter disables limiting. If the
// limiter backend errors, requests are allowed (fail open) and the error logged.
func Middleware(l Limiter, key func(*http.Request) string, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if l == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			d, err := l.Allow(r.Context(), key(r))
			if err != nil {
				log.Error("rate limiter failed; allowing request", "error", err)
				next.ServeHTTP(w, r)
				return
			}
			if !d.Allowed {
				Deny(w, d)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
