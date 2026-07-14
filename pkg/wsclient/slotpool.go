package wsclient

import (
	"context"
	"sync"
)

// SlotPool is a shared concurrency limiter for inference requests. It enforces
// a capacity ceiling shared across both router-dispatched jobs and local API
// requests, giving local requests priority: router jobs yield when local
// requests are waiting for a slot.
//
// Capacity is (re)set by Init, called by wsclient on every connect and,
// optionally, eagerly at startup so local requests work before the first WS
// connection. Init preserves in-flight counts, so a reconnect that re-advertises
// a different max_concurrent adjusts capacity without losing accounting.
type SlotPool struct {
	mu           sync.Mutex
	cond         *sync.Cond
	capacity     int
	ready        bool
	inFlight     int
	localWaiters int
}

func newSlotPool() *SlotPool {
	p := &SlotPool{}
	p.cond = sync.NewCond(&p.mu)
	return p
}

// Init sets (or updates) the pool capacity and marks it ready. It is safe to
// call repeatedly: in-flight counts are preserved, so a reconnect that detects
// a different slot count adjusts the ceiling without resetting accounting. If
// capacity is lowered below the current in-flight count, new acquisitions wait
// until releases bring in-flight back under the new ceiling.
func (p *SlotPool) Init(capacity int) {
	if capacity < 1 {
		capacity = 1
	}
	p.mu.Lock()
	p.capacity = capacity
	p.ready = true
	p.mu.Unlock()
	p.cond.Broadcast()
}

// AcquireLocal acquires a slot with priority over router jobs. If any slots
// are free it returns immediately; otherwise it blocks until one is released.
// Router jobs waiting for a slot will continue to yield as long as this call
// (or any other local acquire) is outstanding. Returns false if ctx expires.
func (p *SlotPool) AcquireLocal(ctx context.Context) bool {
	// Take and release the lock before broadcasting so the wakeup is ordered
	// after the waiter has entered cond.Wait() (which atomically releases the
	// lock). Broadcasting without the lock can race ahead of the waiter and be
	// lost, parking it until the next Release.
	stop := context.AfterFunc(ctx, func() {
		p.mu.Lock()
		p.mu.Unlock() //nolint:staticcheck // ordering barrier, not a guarded section
		p.cond.Broadcast()
	})
	defer stop()

	p.mu.Lock()
	p.localWaiters++

	for !p.ready || p.inFlight >= p.capacity {
		if ctx.Err() != nil {
			p.localWaiters--
			p.mu.Unlock()
			// Broadcast so router goroutines that yielded to us wake up and
			// can now compete for slots — without this they stay asleep until
			// the next Release, since the AfterFunc broadcast fired before
			// localWaiters was decremented.
			p.cond.Broadcast()
			return false
		}
		p.cond.Wait()
	}
	p.inFlight++
	p.localWaiters--
	p.mu.Unlock()
	return true
}

// acquireRouter acquires a slot for a router-dispatched job. It yields to any
// waiting local requests: if localWaiters > 0 this call blocks until all local
// waiters have been served first. Returns false if ctx expires.
func (p *SlotPool) acquireRouter(ctx context.Context) bool {
	// See AcquireLocal for why the broadcast is fenced by the lock.
	stop := context.AfterFunc(ctx, func() {
		p.mu.Lock()
		p.mu.Unlock() //nolint:staticcheck // ordering barrier, not a guarded section
		p.cond.Broadcast()
	})
	defer stop()

	p.mu.Lock()

	for !p.ready || p.inFlight >= p.capacity || p.localWaiters > 0 {
		if ctx.Err() != nil {
			p.mu.Unlock()
			return false
		}
		p.cond.Wait()
	}
	p.inFlight++
	p.mu.Unlock()
	return true
}

// Release returns a slot to the pool and wakes waiting goroutines.
func (p *SlotPool) Release() {
	p.mu.Lock()
	if p.inFlight > 0 {
		p.inFlight--
	}
	p.mu.Unlock()
	p.cond.Broadcast()
}

// Capacity returns the pool capacity, or 0 if not yet initialised.
func (p *SlotPool) Capacity() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.capacity
}
