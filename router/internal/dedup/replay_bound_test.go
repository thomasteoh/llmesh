package dedup

import (
	"context"
	"testing"
	"time"

	"llmesh/pkg/types"
)

func textChunk(s string) types.ChunkMsg { return types.ChunkMsg{Type: "chunk", Delta: s} }

func readAll(t *testing.T, ch <-chan types.ChunkMsg) (count int, sawDone bool) {
	t.Helper()
	for {
		select {
		case c, open := <-ch:
			if !open {
				return count, sawDone
			}
			count++
			if c.Done {
				sawDone = true
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out after %d chunks", count)
		}
	}
}

// Short requests — the ones coalescing is actually for — must still replay in
// full, so a follower joining mid-response sees the whole answer.
func TestReplayBuffer_ShortResponseStillReplays(t *testing.T) {
	r := New(nil)
	r.RegisterOrSubscribe("h")
	for i := 0; i < 10; i++ {
		r.Forward("h", textChunk("x"))
	}

	role, buf, live := r.RegisterOrSubscribe("h")
	if role != RoleFollower {
		t.Fatalf("role = %v, want RoleFollower", role)
	}
	if len(buf) != 10 {
		t.Errorf("replay buffer has %d chunks, want 10", len(buf))
	}
	r.Forward("h", doneChunk())

	got, sawDone := readAll(t, MakeSubscriberChan(context.Background(), buf, live))
	if got != 11 || !sawDone {
		t.Errorf("follower saw %d chunks (done=%v), want 11 and done", got, sawDone)
	}
}

// A generation long enough to outgrow the cap must stop retaining chunks. The
// buffer is held for every request whether or not a follower ever arrives, so
// on a router running many concurrent long generations it is a per-request
// memory multiplier competing with the inference process.
func TestReplayBuffer_DroppedOnceCapExceeded(t *testing.T) {
	r := New(nil)
	r.RegisterOrSubscribe("h")
	for i := 0; i < maxReplayChunks+10; i++ {
		r.Forward("h", textChunk("x"))
	}

	role, buf, live := r.RegisterOrSubscribe("h")
	if role != RoleIndependent {
		t.Fatalf("role = %v, want RoleIndependent once the replay was dropped", role)
	}
	if buf != nil || live != nil {
		t.Error("an independent caller was handed a buffer or channel it does not own")
	}
	if !r.bufferDropped("h") {
		t.Error("the buffer was not released")
	}
}

// A follower that subscribed before the cap was reached holds a live channel
// and must keep receiving to the end — dropping the replay buffer is about new
// arrivals, not existing ones.
func TestReplayBuffer_ExistingFollowerUnaffectedByDrop(t *testing.T) {
	r := New(nil)
	r.RegisterOrSubscribe("h")
	_, buf, live := r.RegisterOrSubscribe("h")
	sub := MakeSubscriberChan(context.Background(), buf, live)

	// Stay under the subscriber channel's own capacity so this exercises the
	// replay bound rather than follower overflow.
	for i := 0; i < 200; i++ {
		r.Forward("h", textChunk("x"))
	}
	r.forceDropReplay("h")
	r.Forward("h", textChunk("after"))
	r.Forward("h", doneChunk())

	got, sawDone := readAll(t, sub)
	if !sawDone {
		t.Error("existing follower lost its terminal chunk when the replay was dropped")
	}
	if got != 202 {
		t.Errorf("existing follower saw %d chunks, want 202", got)
	}
}

// An independent caller owns nothing. If it were to unregister, it would tear
// down the real original's entry and fail every follower on it.
func TestReplayBuffer_IndependentCallerDoesNotOwnEntry(t *testing.T) {
	r := New(nil)
	r.RegisterOrSubscribe("h")
	_, _, live := r.RegisterOrSubscribe("h")
	if live == nil {
		t.Fatal("follower did not get a live channel")
	}
	r.forceDropReplay("h")

	if role, _, _ := r.RegisterOrSubscribe("h"); role != RoleIndependent {
		t.Fatalf("role = %v, want RoleIndependent", role)
	}
	// The entry and its follower must still be intact.
	if got := r.subscriberCount("h"); got != 1 {
		t.Errorf("subscriber count = %d, want 1 — the original's follower was lost", got)
	}
}
