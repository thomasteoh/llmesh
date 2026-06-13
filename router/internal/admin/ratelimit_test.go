package admin

import (
	"testing"
	"time"
)

// TestRateLimit_BurstAllowsLimit verifies a client can make up to `limit`
// requests immediately (burst), not just one. Regression for the burst=1 bug
// that broke the login page (GET page + POST submit tripped the limit).
func TestRateLimit_BurstAllowsLimit(t *testing.T) {
	rl := newRateLimiter(time.Minute, 5*time.Minute)
	const limit = 5

	for i := 0; i < limit; i++ {
		if !rl.Allow("1.2.3.4", limit) {
			t.Fatalf("request %d/%d should be allowed within burst", i+1, limit)
		}
	}
	// The (limit+1)-th immediate request must be rejected.
	if rl.Allow("1.2.3.4", limit) {
		t.Error("request beyond burst should be rejected")
	}
}

// TestRateLimit_SeparateBucketsPerLimit verifies that endpoints with different
// limits get independent buckets. Regression for the shared-per-IP-bucket bug
// where the first route's limit governed all subsequent routes for that IP.
func TestRateLimit_SeparateBucketsPerLimit(t *testing.T) {
	rl := newRateLimiter(time.Minute, 5*time.Minute)
	ip := "9.9.9.9"

	// Exhaust the 5/min bucket (login).
	for i := 0; i < 5; i++ {
		rl.Allow(ip, 5)
	}
	if rl.Allow(ip, 5) {
		t.Fatal("5/min bucket should be exhausted")
	}

	// The 20/min bucket for the same IP must be unaffected.
	if !rl.Allow(ip, 20) {
		t.Error("20/min bucket should be independent of the exhausted 5/min bucket")
	}
}

// TestRateLimit_PerIPIsolation verifies different IPs do not share a bucket.
func TestRateLimit_PerIPIsolation(t *testing.T) {
	rl := newRateLimiter(time.Minute, 5*time.Minute)
	const limit = 5

	for i := 0; i < limit; i++ {
		rl.Allow("10.0.0.1", limit)
	}
	if rl.Allow("10.0.0.1", limit) {
		t.Fatal("first IP should be exhausted")
	}
	if !rl.Allow("10.0.0.2", limit) {
		t.Error("second IP should have its own fresh bucket")
	}
}
