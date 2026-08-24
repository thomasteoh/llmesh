package correlation

import (
	"log/slog"
	"testing"
	"time"

	"llmesh/pkg/types"
)

// fill loads the channel to capacity so the next Send has to wait.
func fill(t *testing.T, s *Store, id string) {
	t.Helper()
	for i := 0; i < chanBuffer; i++ {
		if got := s.Send(types.ChunkMsg{RequestID: id, Delta: "x"}); got != SendOK {
			t.Fatalf("priming send %d: got %v, want SendOK", i, got)
		}
	}
}

// A handler that falls behind for a moment must not cost the caller its
// generation. SendFull cancels the request, so a full buffer has to mean the
// handler is gone, not that it was busy flushing when a fast worker burst.
func TestSend_WaitsOutTransientBackpressure(t *testing.T) {
	prev := SendGrace
	SendGrace = 2 * time.Second
	defer func() { SendGrace = prev }()

	s := New(slog.Default())
	ch := s.Create("req-1")
	fill(t, s, "req-1")

	// A reader that wakes up after a delay, as a handler blocked on a slow SSE
	// write would.
	go func() {
		time.Sleep(150 * time.Millisecond)
		<-ch
	}()

	if got := s.Send(types.ChunkMsg{RequestID: "req-1", Delta: "y"}); got != SendOK {
		t.Errorf("Send = %v, want SendOK — a brief stall cancelled the request", got)
	}
}

func TestSend_ReportsFullAfterGrace(t *testing.T) {
	prev := SendGrace
	SendGrace = 50 * time.Millisecond
	defer func() { SendGrace = prev }()

	s := New(slog.Default())
	s.Create("req-2")
	fill(t, s, "req-2")

	start := time.Now()
	if got := s.Send(types.ChunkMsg{RequestID: "req-2", Delta: "y"}); got != SendFull {
		t.Errorf("Send = %v, want SendFull for a handler that never drains", got)
	}
	if elapsed := time.Since(start); elapsed < SendGrace {
		t.Errorf("gave up after %v, want at least %v", elapsed, SendGrace)
	}
}

// Deleting the request closes the channel. A Send parked in the grace wait must
// report that as SendNotFound rather than panicking on a closed channel.
func TestSend_ChannelClosedDuringGrace(t *testing.T) {
	prev := SendGrace
	SendGrace = 2 * time.Second
	defer func() { SendGrace = prev }()

	s := New(slog.Default())
	s.Create("req-3")
	fill(t, s, "req-3")

	go func() {
		time.Sleep(100 * time.Millisecond)
		s.Delete("req-3")
	}()

	if got := s.Send(types.ChunkMsg{RequestID: "req-3", Delta: "y"}); got != SendNotFound {
		t.Errorf("Send = %v, want SendNotFound", got)
	}
}
