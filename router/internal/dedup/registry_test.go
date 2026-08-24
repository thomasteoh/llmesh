package dedup

import (
	"context"
	"testing"
	"time"

	"llmesh/pkg/types"
)

func chunk(delta string) types.ChunkMsg {
	return types.ChunkMsg{Type: "chunk", Delta: delta}
}

func doneChunk() types.ChunkMsg {
	return types.ChunkMsg{Type: "chunk", Done: true, FinishReason: "stop"}
}

// drain reads everything available from ch until it closes or stalls, and
// reports whether a terminal chunk arrived.
func drain(t *testing.T, ch <-chan types.ChunkMsg) (text string, sawDone bool) {
	t.Helper()
	for {
		select {
		case c, open := <-ch:
			if !open {
				return text, sawDone
			}
			text += c.Delta
			if c.Done {
				sawDone = true
			}
		case <-time.After(2 * time.Second):
			t.Fatal("timed out reading subscriber channel")
		}
	}
}

func TestHasSubscribers(t *testing.T) {
	r := New(nil)
	if role, _, _ := r.RegisterOrSubscribe("h"); role != RoleOriginal {
		t.Fatalf("first caller role = %v, want RoleOriginal", role)
	}
	if r.HasSubscribers("h") {
		t.Error("a fresh entry reports subscribers")
	}
	if _, _, live := r.RegisterOrSubscribe("h"); live == nil {
		t.Fatal("second caller should get a live channel")
	}
	if !r.HasSubscribers("h") {
		t.Error("entry with a follower reports none")
	}
	r.Forward("h", doneChunk())
	if r.HasSubscribers("h") {
		t.Error("entry reports subscribers after completing")
	}
	if r.HasSubscribers("nope") {
		t.Error("unknown hash reports subscribers")
	}
}

// A follower must receive the whole response when the original runs to
// completion — the baseline the rest of these tests deviate from.
func TestForward_FollowerGetsFullResponse(t *testing.T) {
	r := New(nil)
	r.RegisterOrSubscribe("h")
	r.Forward("h", chunk("one "))
	_, buf, live := r.RegisterOrSubscribe("h")
	sub := MakeSubscriberChan(context.Background(), buf, live)

	r.Forward("h", chunk("two"))
	r.Forward("h", doneChunk())

	text, sawDone := drain(t, sub)
	if text != "one two" {
		t.Errorf("text = %q, want %q", text, "one two")
	}
	if !sawDone {
		t.Error("follower never received a terminal chunk")
	}
}

// Reset drops the abandoned attempt's output so a retry's response is not
// appended to it. A follower that already saw some of the abandoned attempt
// cannot be rewound, so it is failed rather than handed a stitched answer.
func TestReset_FailsFollowersThatSawTheAbandonedAttempt(t *testing.T) {
	r := New(nil)
	r.RegisterOrSubscribe("h")
	_, buf, live := r.RegisterOrSubscribe("h")
	sub := MakeSubscriberChan(context.Background(), buf, live)

	r.Forward("h", chunk("partial from attempt one"))
	r.Reset("h")
	r.Forward("h", chunk("the real answer"))
	r.Forward("h", doneChunk())

	text, sawDone := drain(t, sub)
	if sawDone {
		t.Errorf("follower was told the stitched response completed normally: %q", text)
	}
	if text == "partial from attempt onethe real answer" {
		t.Error("follower received the abandoned attempt concatenated with the retry")
	}
}

// Reset must clear the replay buffer, so a follower arriving after the retry
// gets only the retry's output.
func TestReset_ClearsBufferForLaterFollowers(t *testing.T) {
	r := New(nil)
	r.RegisterOrSubscribe("h")
	r.Forward("h", chunk("partial from attempt one"))
	r.Reset("h")
	r.Forward("h", chunk("the real "))

	_, buf, live := r.RegisterOrSubscribe("h")
	sub := MakeSubscriberChan(context.Background(), buf, live)
	r.Forward("h", chunk("answer"))
	r.Forward("h", doneChunk())

	text, sawDone := drain(t, sub)
	if text != "the real answer" {
		t.Errorf("text = %q, want %q — the abandoned attempt leaked into the replay", text, "the real answer")
	}
	if !sawDone {
		t.Error("follower never received a terminal chunk")
	}
}

// A completed entry must not be disturbed by a late Reset.
func TestReset_IgnoresCompletedEntry(t *testing.T) {
	r := New(nil)
	r.RegisterOrSubscribe("h")
	r.Forward("h", chunk("all done"))
	r.Forward("h", doneChunk())
	r.Reset("h")

	_, buf, live := r.RegisterOrSubscribe("h")
	if live != nil {
		t.Error("a completed entry handed out a live channel")
	}
	text, _ := drain(t, MakeSubscriberChan(context.Background(), buf, live))
	if text != "all done" {
		t.Errorf("replay = %q, want %q", text, "all done")
	}
}

// Unregister before completion closes followers without a terminal chunk, so
// their handlers report the truncation instead of a short success.
func TestUnregister_ClosesFollowersWithoutDone(t *testing.T) {
	r := New(nil)
	r.RegisterOrSubscribe("h")
	_, buf, live := r.RegisterOrSubscribe("h")
	sub := MakeSubscriberChan(context.Background(), buf, live)

	r.Forward("h", chunk("partial"))
	r.Unregister("h")

	text, sawDone := drain(t, sub)
	if text != "partial" {
		t.Errorf("text = %q, want %q", text, "partial")
	}
	if sawDone {
		t.Error("a truncated response was reported as complete")
	}
}
