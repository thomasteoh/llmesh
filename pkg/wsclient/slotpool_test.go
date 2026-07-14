package wsclient

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestSlotPool_AcquireRelease(t *testing.T) {
	p := newSlotPool()
	p.Init(2)

	if !p.AcquireLocal(context.Background()) {
		t.Fatal("first local acquire should succeed")
	}
	if !p.acquireRouter(context.Background()) {
		t.Fatal("router acquire should succeed with a free slot")
	}
	// Pool is now full (capacity 2). A router acquire must block; a cancelled
	// context makes it return false rather than hang.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if p.acquireRouter(ctx) {
		t.Fatal("router acquire should fail when the pool is full and ctx expires")
	}
	p.Release()
	if !p.acquireRouter(context.Background()) {
		t.Fatal("router acquire should succeed after a release")
	}
}

func TestSlotPool_InitAdjustsCapacity(t *testing.T) {
	p := newSlotPool()
	p.Init(1)
	if !p.AcquireLocal(context.Background()) {
		t.Fatal("acquire should succeed at capacity 1")
	}
	// Re-Init to a larger capacity (as a reconnect re-advertising more slots
	// would). The in-flight acquire is preserved, and a new slot is available.
	p.Init(2)
	if p.Capacity() != 2 {
		t.Errorf("capacity = %d, want 2", p.Capacity())
	}
	if !p.acquireRouter(context.Background()) {
		t.Fatal("second slot should be available after capacity raised to 2")
	}
}

func TestSlotPool_LocalPriority(t *testing.T) {
	p := newSlotPool()
	p.Init(1)
	// Fill the only slot.
	if !p.AcquireLocal(context.Background()) {
		t.Fatal("initial acquire should succeed")
	}

	// A local waiter and a router waiter both block on the full pool.
	localGot := make(chan bool, 1)
	routerGot := make(chan bool, 1)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); localGot <- p.AcquireLocal(context.Background()) }()
	// Give the local waiter time to register itself before the router waiter.
	time.Sleep(20 * time.Millisecond)
	go func() { defer wg.Done(); routerGot <- p.acquireRouter(context.Background()) }()
	time.Sleep(20 * time.Millisecond)

	// Release one slot. The local waiter must win it; the router waiter keeps
	// waiting because a local request is still outstanding.
	p.Release()

	select {
	case ok := <-localGot:
		if !ok {
			t.Fatal("local waiter should have acquired the freed slot")
		}
	case <-time.After(time.Second):
		t.Fatal("local waiter did not wake")
	}
	select {
	case <-routerGot:
		t.Fatal("router waiter should not have acquired while a local request held the slot")
	case <-time.After(50 * time.Millisecond):
		// expected: router still waiting
	}

	// Release the local slot; now the router waiter can proceed.
	p.Release()
	select {
	case ok := <-routerGot:
		if !ok {
			t.Fatal("router waiter should acquire after local releases")
		}
	case <-time.After(time.Second):
		t.Fatal("router waiter did not wake after local release")
	}
	wg.Wait()
}

func TestSlotPool_CancelWakesWaiter(t *testing.T) {
	p := newSlotPool()
	p.Init(1)
	if !p.AcquireLocal(context.Background()) {
		t.Fatal("initial acquire should succeed")
	}
	// A cancelled context must wake the blocked waiter promptly (regression for
	// the lost-wakeup race where the AfterFunc broadcast could be missed).
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() { done <- p.AcquireLocal(ctx) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("acquire should return false when ctx is cancelled")
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled waiter was not woken (lost wakeup)")
	}
}
