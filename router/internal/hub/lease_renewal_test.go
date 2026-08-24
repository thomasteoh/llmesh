package hub

import (
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"llmesh/pkg/types"
)

// trackExpiring registers a job whose fixed lease ran out five minutes ago, as
// it would for a generation that has been streaming for longer than
// LeaseDuration. Returns the client so callers can assert on its slot count.
func trackExpiring(t *testing.T, h *Hub, clientID, reqID string) *Client {
	t.Helper()
	c := &Client{ID: clientID}
	h.mu.Lock()
	h.clients[clientID] = c
	h.mu.Unlock()
	h.IncrInFlight(clientID)
	h.mu.Lock()
	h.jobs[reqID] = InFlightRecord{
		ClientID:     clientID,
		Req:          types.InferenceRequest{ID: reqID, Model: "llama3", Stream: true},
		DispatchedAt: time.Now().Add(-25 * time.Minute),
		LeaseExpiry:  time.Now().Add(-5 * time.Minute),
		live:         &jobLiveStats{},
	}
	h.mu.Unlock()
	return c
}

func tracked(h *Hub, reqID string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, ok := h.jobs[reqID]
	return ok
}

// A worker still streaming tokens past LeaseDuration must keep its slot. The
// reaper exists to reclaim slots from workers that died silently, and
// cancelling a healthy generation at a fixed deadline truncated exactly the
// long large-context requests this router is for.
func TestLeaseReaper_KeepsJobStreamingPastFixedExpiry(t *testing.T) {
	h := New(slog.Default())
	c := trackExpiring(t, h, "c1", "req-streaming")

	h.dispatch(c, mustJSON(t, map[string]any{
		"type": "chunk", "request_id": "req-streaming", "delta": "token",
	}))

	h.handleExpiredLeases()

	if !tracked(h, "req-streaming") {
		t.Error("a job that just delivered a token was reaped")
	}
	if c.InFlight() != 1 {
		t.Errorf("InFlight = %d, want 1", c.InFlight())
	}
}

// Once output has started, a keep-alive is proof of life like any other chunk:
// it is what a worker sends between tokens when decoding is slow.
func TestLeaseReaper_KeepAliveRenewsAfterOutput(t *testing.T) {
	h := New(slog.Default())
	c := trackExpiring(t, h, "c1", "req-slow")

	h.dispatch(c, mustJSON(t, map[string]any{
		"type": "chunk", "request_id": "req-slow", "delta": "token",
	}))
	// Backdate the token so only the keep-alive that follows can save the job.
	h.mu.RLock()
	rec := h.jobs["req-slow"]
	h.mu.RUnlock()
	rec.live.lastActivity.Store(time.Now().Add(-30 * time.Minute).UnixNano())

	h.dispatch(c, mustJSON(t, map[string]any{
		"type": "chunk", "request_id": "req-slow", "delta": "",
	}))

	h.handleExpiredLeases()

	if !tracked(h, "req-slow") {
		t.Error("keep-alive after first output did not renew the lease")
	}
}

// Before any output, keep-alives must not renew: otherwise a worker that pings
// forever without ever generating holds its slot for good.
func TestLeaseReaper_KeepAliveDoesNotRenewBeforeOutput(t *testing.T) {
	h := New(slog.Default())
	c := trackExpiring(t, h, "c1", "req-stuck")

	h.dispatch(c, mustJSON(t, map[string]any{
		"type": "chunk", "request_id": "req-stuck", "delta": "",
	}))

	h.handleExpiredLeases()

	if tracked(h, "req-stuck") {
		t.Error("a job that never produced output kept its slot on keep-alives alone")
	}
	if c.InFlight() != 0 {
		t.Errorf("InFlight = %d, want 0", c.InFlight())
	}
}

// A worker that has produced output and then genuinely went silent is still
// reclaimed, one LeaseDuration after its last sign of life.
func TestLeaseReaper_ReapsSilentWorkerAfterOutput(t *testing.T) {
	h := New(slog.Default())
	c := trackExpiring(t, h, "c1", "req-died")

	h.dispatch(c, mustJSON(t, map[string]any{
		"type": "chunk", "request_id": "req-died", "delta": "token",
	}))
	h.mu.RLock()
	rec := h.jobs["req-died"]
	h.mu.RUnlock()
	rec.live.lastActivity.Store(time.Now().Add(-LeaseDuration - time.Minute).UnixNano())

	h.handleExpiredLeases()

	if tracked(h, "req-died") {
		t.Error("a worker silent for longer than LeaseDuration should be reaped")
	}
	if c.InFlight() != 0 {
		t.Errorf("InFlight = %d, want 0", c.InFlight())
	}
}

// Reasoning-only chunks are output: a thinking model emits nothing else for
// minutes at a time, and its lease has to survive that.
func TestLeaseReaper_ReasoningCountsAsOutput(t *testing.T) {
	h := New(slog.Default())
	c := trackExpiring(t, h, "c1", "req-thinking")

	h.dispatch(c, mustJSON(t, map[string]any{
		"type": "chunk", "request_id": "req-thinking", "reasoning_delta": "let me think",
	}))

	h.handleExpiredLeases()

	if !tracked(h, "req-thinking") {
		t.Error("a job streaming reasoning tokens was reaped")
	}
}

func TestDispatch_ForwardsReasoningDelta(t *testing.T) {
	h := New(slog.Default())
	c := trackExpiring(t, h, "c1", "req-r")

	var got types.ChunkMsg
	h.OnChunk = func(m types.ChunkMsg) { got = m }

	h.dispatch(c, mustJSON(t, map[string]any{
		"type": "chunk", "request_id": "req-r", "reasoning_delta": "thinking out loud",
	}))

	if got.ReasoningDelta != "thinking out loud" {
		t.Errorf("ReasoningDelta = %q, want %q", got.ReasoningDelta, "thinking out loud")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
