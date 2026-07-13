package admin

import (
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// rateLimiter implements a per-IP token bucket rate limiter for admin endpoints.
type rateLimiter struct {
	mu      sync.Mutex
	clients map[string]*clientEntry
	window  time.Duration
	cleanup time.Duration
}

type clientEntry struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

func newRateLimiter(window time.Duration, cleanupEvery time.Duration) *rateLimiter {
	rl := &rateLimiter{
		clients: make(map[string]*clientEntry),
		window:  window,
		cleanup: cleanupEvery,
	}
	go rl.reap()
	return rl
}

// reap periodically evicts buckets not seen within the cleanup window so the
// map cannot grow without bound (especially important since a spoofed or
// high-cardinality client key set would otherwise accumulate forever).
func (rl *rateLimiter) reap() {
	ticker := time.NewTicker(rl.cleanup)
	defer ticker.Stop()
	for range ticker.C {
		cutoff := time.Now().Add(-rl.cleanup)
		rl.mu.Lock()
		for k, e := range rl.clients {
			if e.lastSeen.Before(cutoff) {
				delete(rl.clients, k)
			}
		}
		rl.mu.Unlock()
	}
}

// Allow reports whether a request from ip is permitted under the given per-window
// limit. Buckets are keyed by (ip, limit) so endpoints with different limits get
// independent budgets — e.g. the login page's 5/min does not share a bucket with
// the 20/min admin mutations. The burst equals the limit, so a client may make
// up to `limit` requests immediately before the bucket refills.
func (rl *rateLimiter) Allow(ip string, limit float64) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	key := ip + ":" + strconv.FormatFloat(limit, 'f', -1, 64)
	entry, ok := rl.clients[key]
	if !ok {
		burst := int(limit)
		if burst < 1 {
			burst = 1
		}
		entry = &clientEntry{
			limiter: rate.NewLimiter(rate.Limit(limit/rl.window.Seconds()), burst),
		}
		rl.clients[key] = entry
	}

	entry.lastSeen = time.Now()
	return entry.limiter.Allow()
}

// clientIP returns the request's client IP. X-Forwarded-For is honoured only
// when trustProxy is set (the router is behind a trusted reverse proxy);
// otherwise the direct peer (RemoteAddr) is used, so a client cannot spoof its
// IP to get a fresh rate-limit bucket per request.
func (a *Admin) clientIP(r *http.Request) string {
	if a.trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			return strings.TrimSpace(strings.Split(xff, ",")[0])
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// isSecure reports whether the request reached the router over TLS, either
// directly or (when trustProxy is set) via a proxy that terminated TLS and set
// X-Forwarded-Proto. Used to decide the session cookie's Secure flag.
func (a *Admin) isSecure(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return a.trustProxy && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// requireRateLimit returns a middleware that enforces per-IP rate limiting.
func (a *Admin) requireRateLimit(next http.HandlerFunc, limit float64) http.HandlerFunc {
	if a.rateLimiter == nil {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		ip := a.clientIP(r)
		if !a.rateLimiter.Allow(ip, limit) {
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		next(w, r)
	}
}
