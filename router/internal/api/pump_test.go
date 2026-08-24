package api

import (
	"context"
	"sync"
	"testing"
	"time"

	"llmesh/pkg/types"
	"llmesh/router/internal/dedup"
)

type fakeCorrelation struct {
	mu      sync.Mutex
	deleted []string
}

func (f *fakeCorrelation) Create(string) <-chan types.ChunkMsg { return nil }

func (f *fakeCorrelation) Delete(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, id)
}

func (f *fakeCorrelation) deletedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deleted...)
}

type fakeCanceller struct {
	mu        sync.Mutex
	cancelled []string
}

func (f *fakeCanceller) CancelRequest(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelled = append(f.cancelled, id)
}

func (f *fakeCanceller) cancelledIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.cancelled...)
}

// fakeQueue satisfies Enqueuer; cancelRequest calls PopByID on it.
type fakeQueue struct{}

func (fakeQueue) TryPush(types.InferenceRequest) bool    { return true }
func (fakeQueue) Push(types.InferenceRequest)            {}
func (fakeQueue) PopByID(string) *types.InferenceRequest { return nil }
func (fakeQueue) Len() int                               { return 0 }

func pumpHandler(activity time.Duration) (*Handler, *dedup.Registry, *fakeCorrelation, *fakeCanceller) {
	reg := dedup.New()
	corr := &fakeCorrelation{}
	canc := &fakeCanceller{}
	h := &Handler{
		Dedup:           reg,
		Correlation:     corr,
		Canceller:       canc,
		Queue:           fakeQueue{},
		ActivityTimeout: activity,
	}
	return h, reg, corr, canc
}

// subscribe registers the original and one follower, returning the follower's
// merged channel.
func subscribe(t *testing.T, reg *dedup.Registry, ctx context.Context, hash string) <-chan types.ChunkMsg {
	t.Helper()
	if role, _, _ := reg.RegisterOrSubscribe(hash); role != dedup.RoleOriginal {
		t.Fatalf("first caller role = %v, want RoleOriginal", role)
	}
	_, buf, live := reg.RegisterOrSubscribe(hash)
	if live == nil {
		t.Fatal("follower did not get a live channel")
	}
	return dedup.MakeSubscriberChan(ctx, buf, live)
}

func collect(t *testing.T, ch <-chan types.ChunkMsg) (string, bool) {
	t.Helper()
	var text string
	for {
		select {
		case c, open := <-ch:
			if !open {
				return text, false
			}
			text += c.Delta
			if c.Done {
				return text, true
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out; text so far %q", text)
		}
	}
}

// The original caller hanging up must not abort an answer other callers were
// coalesced onto. Coalescing is always on and the content hash ignores who
// asked, so those followers can be unrelated clients.
func TestPumpForFollowers_FinishesAfterOriginalDisconnects(t *testing.T) {
	h, reg, corr, canc := pumpHandler(time.Minute)
	sub := subscribe(t, reg, context.Background(), "h")

	ch := make(chan types.ChunkMsg, 4)
	done := make(chan struct{})
	go func() { h.pumpForFollowers("req-1", "h", ch); close(done) }()

	ch <- types.ChunkMsg{Delta: "still "}
	ch <- types.ChunkMsg{Delta: "working"}
	ch <- types.ChunkMsg{Done: true, FinishReason: "stop"}

	text, sawDone := collect(t, sub)
	if text != "still working" {
		t.Errorf("follower text = %q, want %q", text, "still working")
	}
	if !sawDone {
		t.Error("follower never received a terminal chunk")
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("pump did not exit after the terminal chunk")
	}
	if got := canc.cancelledIDs(); len(got) != 0 {
		t.Errorf("pump cancelled a healthy request: %v", got)
	}
	if got := corr.deletedIDs(); len(got) != 1 || got[0] != "req-1" {
		t.Errorf("correlation cleanup = %v, want [req-1]", got)
	}
}

// With every follower gone there is nobody left to finish for, so the worker
// should be released rather than left generating into nothing.
func TestPumpForFollowers_CancelsWhenAllFollowersLeave(t *testing.T) {
	h, reg, _, canc := pumpHandler(time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	sub := subscribe(t, reg, ctx, "h")

	ch := make(chan types.ChunkMsg, 4)
	done := make(chan struct{})
	go func() { h.pumpForFollowers("req-1", "h", ch); close(done) }()

	ch <- types.ChunkMsg{Delta: "first"}
	if _, open := <-sub; !open {
		t.Fatal("follower channel closed early")
	}

	// The follower goes away. Its subscription is dropped on the next Forward.
	cancel()
	reg.Unregister("h")
	ch <- types.ChunkMsg{Delta: "second"}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("pump kept running with no followers left")
	}
	if got := canc.cancelledIDs(); len(got) != 1 || got[0] != "req-1" {
		t.Errorf("cancelled = %v, want [req-1]", got)
	}
}

// A pump must not outlive a worker that has gone silent.
func TestPumpForFollowers_CancelsOnSilentWorker(t *testing.T) {
	h, reg, _, canc := pumpHandler(50 * time.Millisecond)
	subscribe(t, reg, context.Background(), "h")

	done := make(chan struct{})
	go func() { h.pumpForFollowers("req-1", "h", make(chan types.ChunkMsg)); close(done) }()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("pump did not give up on a silent worker")
	}
	if got := canc.cancelledIDs(); len(got) != 1 || got[0] != "req-1" {
		t.Errorf("cancelled = %v, want [req-1]", got)
	}
}

// A stream that dies without a terminal chunk must leave followers reporting
// the truncation, not a short success.
func TestPumpForFollowers_TruncatedStreamFailsFollowers(t *testing.T) {
	h, reg, _, _ := pumpHandler(time.Minute)
	sub := subscribe(t, reg, context.Background(), "h")

	ch := make(chan types.ChunkMsg, 2)
	ch <- types.ChunkMsg{Delta: "half an ans"}
	close(ch)

	h.pumpForFollowers("req-1", "h", ch)

	text, sawDone := collect(t, sub)
	if text != "half an ans" {
		t.Errorf("text = %q, want %q", text, "half an ans")
	}
	if sawDone {
		t.Error("a truncated response was reported as complete")
	}
}
