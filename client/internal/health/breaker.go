// Package health tracks whether each model is fit to advertise to the router:
// a circuit breaker per model, opened by repeated inference failures that a
// health check would not catch — a backend that answers /health but fails
// every request (out of memory, a broken template).
package health

import (
	"sync"
	"time"
)

const (
	// TripAfter is how many failures in a row open a model's breaker.
	TripAfter = 3
	// FirstCooldown is how long a model is withdrawn the first time. Each
	// trip in a row doubles it, up to MaxCooldown.
	FirstCooldown = 30 * time.Second
	MaxCooldown   = 10 * time.Minute
)

type modelState struct {
	failures  int       // in a row, since the last success
	openUntil time.Time // withdrawn until then; zero when closed
	cooldown  time.Duration
	active    int // requests running now
}

// Breaker records inference outcomes per model and withdraws a model that
// keeps failing. After its cooldown the model is offered again; one success
// closes the breaker, one failure reopens it for twice as long.
type Breaker struct {
	mu     sync.Mutex
	models map[string]*modelState
	now    func() time.Time
	// onChange is called, outside the lock, when a model is withdrawn, so the
	// router can be told straight away rather than at the next health check.
	onChange func()
}

// NewBreaker returns a Breaker. onChange may be nil.
func NewBreaker(onChange func()) *Breaker {
	return &Breaker{models: map[string]*modelState{}, now: time.Now, onChange: onChange}
}

// SetOnChange replaces the callback run when a model is withdrawn.
func (b *Breaker) SetOnChange(fn func()) {
	b.mu.Lock()
	b.onChange = fn
	b.mu.Unlock()
}

func (b *Breaker) state(model string) *modelState {
	s, ok := b.models[model]
	if !ok {
		s = &modelState{}
		b.models[model] = s
	}
	return s
}

// Success records a completed request: the model is healthy.
func (b *Breaker) Success(model string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.state(model)
	s.failures = 0
	s.openUntil = time.Time{}
	s.cooldown = 0
}

// Failure records a request the backend failed. It returns true when this
// failure withdrew the model.
func (b *Breaker) Failure(model string) bool {
	b.mu.Lock()
	s := b.state(model)
	s.failures++
	// A failure while being offered again after a cooldown reopens at once;
	// otherwise it takes TripAfter in a row.
	halfOpen := s.cooldown > 0
	if !halfOpen && s.failures < TripAfter {
		b.mu.Unlock()
		return false
	}
	if s.cooldown == 0 {
		s.cooldown = FirstCooldown
	} else if s.cooldown < MaxCooldown {
		s.cooldown *= 2
		if s.cooldown > MaxCooldown {
			s.cooldown = MaxCooldown
		}
	}
	s.openUntil = b.now().Add(s.cooldown)
	fn := b.onChange
	b.mu.Unlock()
	if fn != nil {
		fn()
	}
	return true
}

// Open reports whether the model is withdrawn now.
func (b *Breaker) Open(model string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.models[model]
	return ok && b.now().Before(s.openUntil)
}

// OpenUntil returns when the model's current withdrawal ends, or the zero
// time when it is not withdrawn.
func (b *Breaker) OpenUntil(model string) time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	if s, ok := b.models[model]; ok && b.now().Before(s.openUntil) {
		return s.openUntil
	}
	return time.Time{}
}

// Start records that a request on the model has begun. Pair with Done.
func (b *Breaker) Start(model string) {
	b.mu.Lock()
	b.state(model).active++
	b.mu.Unlock()
}

// Done records that a request on the model has ended, however it ended.
func (b *Breaker) Done(model string) {
	b.mu.Lock()
	if s := b.state(model); s.active > 0 {
		s.active--
	}
	b.mu.Unlock()
}

// Active is how many requests on the model are running now. A backend busy
// with them can be slow to answer a health check without being down.
func (b *Breaker) Active(model string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if s, ok := b.models[model]; ok {
		return s.active
	}
	return 0
}
