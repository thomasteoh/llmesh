package admin

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// rateLimiter implements a per-IP token bucket rate limiter for admin endpoints.
type rateLimiter struct {
	mu       sync.Mutex
	clients  map[string]*clientEntry
	limiter  *rate.Limiter
	window   time.Duration
	cleanup  time.Duration
}

type clientEntry struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

func newRateLimiter(window time.Duration, cleanupEvery time.Duration) *rateLimiter {
	return &rateLimiter{
		clients: make(map[string]*clientEntry),
		window:  window,
		cleanup: cleanupEvery,
	}
}

// Allow checks if the given IP is allowed to make a request at the specified limit.
// Returns false if the rate limit is exceeded.
func (rl *rateLimiter) Allow(ip string, limit float64) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	entry, ok := rl.clients[ip]
	if !ok {
		rl.clients[ip] = &clientEntry{
			limiter:  rate.NewLimiter(rate.Limit(limit/rl.window.Seconds()), 1),
			lastSeen: time.Now(),
		}
		return true
	}

	entry.lastSeen = time.Now()
	return entry.limiter.Allow()
}

// extractIP extracts the client IP from the request, checking X-Forwarded-For
// first (behind reverse proxy) then falling back to RemoteAddr.
func extractIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// Take the first IP in the chain
		parts := strings.Split(xff, ",")
		return strings.TrimSpace(parts[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// requireRateLimit returns a middleware that enforces per-IP rate limiting.
func (a *Admin) requireRateLimit(next http.HandlerFunc, limit float64) http.HandlerFunc {
	if a.rateLimiter == nil {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		ip := extractIP(r)
		if !a.rateLimiter.Allow(ip, limit) {
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		next(w, r)
	}
}
