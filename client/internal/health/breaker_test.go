package health

import (
	"testing"
	"time"
)

func TestBreaker(t *testing.T) {
	now := time.Unix(1000, 0)
	changes := 0
	b := NewBreaker(func() { changes++ })
	b.now = func() time.Time { return now }

	// Failures short of TripAfter, or broken by a success, leave it offered.
	b.Failure("m")
	b.Failure("m")
	b.Success("m")
	b.Failure("m")
	b.Failure("m")
	if b.Open("m") || changes != 0 {
		t.Fatal("withdrawn before TripAfter failures in a row")
	}
	if !b.Failure("m") || !b.Open("m") || changes != 1 {
		t.Fatal("not withdrawn after TripAfter failures in a row")
	}
	if b.Open("other") {
		t.Fatal("one model's failures withdrew another")
	}

	// After the cooldown it is offered again; one failure reopens it for
	// twice as long.
	now = now.Add(FirstCooldown)
	if b.Open("m") {
		t.Fatal("still withdrawn after the cooldown")
	}
	if !b.Failure("m") {
		t.Fatal("a failure after the cooldown did not reopen at once")
	}
	if got := b.OpenUntil("m").Sub(now); got != 2*FirstCooldown {
		t.Fatalf("second cooldown %v, want %v", got, 2*FirstCooldown)
	}

	// The cooldown is capped.
	for i := 0; i < 10; i++ {
		now = b.OpenUntil("m")
		b.Failure("m")
	}
	if got := b.OpenUntil("m").Sub(now); got != MaxCooldown {
		t.Fatalf("cooldown %v, want the cap %v", got, MaxCooldown)
	}

	// A success after a cooldown closes it and resets the backoff.
	now = b.OpenUntil("m")
	b.Success("m")
	for i := 0; i < TripAfter; i++ {
		b.Failure("m")
	}
	if got := b.OpenUntil("m").Sub(now); got != FirstCooldown {
		t.Fatalf("cooldown after recovery %v, want %v", got, FirstCooldown)
	}
}
