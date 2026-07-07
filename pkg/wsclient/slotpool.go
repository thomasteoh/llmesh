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
// Capacity is set by the first call to init (called by wsclient on connect).
// Subsequent calls are no-ops, so reconnects do not reset in-flight counts and
// local requests that arrive before the first WS connection block until the
// pool is ready.
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

// init sets the pool capacity on the first call; subsequent calls are no-ops.
func (p *SlotPool) init(capacity int) {
	p.mu.Lock()
	if !p.ready {
		if capacity < 1 {
			capacity = 1
		}
		p.capacity = capacity
		p.ready = true
	}
	p.mu.Unlock()
	p.cond.Broadcast()
}

// AcquireLocal acquires a slot with priority over router jobs. If any slots
// are free it returns immediately; otherwise it blocks until one is released.
// Router jobs waiting for a slot will continue to yield as long as this call
// (or any other local acquire) is outstanding. Returns false if ctx expires.
func (p *SlotPool) AcquireLocal(ctx context.Context) bool {
	stop := context.AfterFunc(ctx, func() { p.cond.Broadcast() })
	defer stop()

	p.mu.Lock()
	defer p.mu.Unlock()
	p.localWaiters++
	defer func() { p.localWaiters-- }()

	for !p.ready || p.inFlight >= p.capacity {
		if ctx.Err() != nil {
			return false
		}
		p.cond.Wait()
	}
	p.inFlight++
	return true
}

// acquireRouter acquires a slot for a router-dispatched job. It yields to any
// waiting local requests: if localWaiters > 0 this call blocks until all local
// waiters have been served first. Returns false if ctx expires.
func (p *SlotPool) acquireRouter(ctx context.Context) bool {
	stop := context.AfterFunc(ctx, func() { p.cond.Broadcast() })
	defer stop()

	p.mu.Lock()
	defer p.mu.Unlock()

	for !p.ready || p.inFlight >= p.capacity || p.localWaiters > 0 {
		if ctx.Err() != nil {
			return false
		}
		p.cond.Wait()
	}
	p.inFlight++
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
