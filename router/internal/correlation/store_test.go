package correlation

import (
	"testing"

	"llmesh/pkg/types"
)

func TestDrainAll_UnblocksReaders(t *testing.T) {
	s := New(nil)

	ch1 := s.Create("req-1")
	ch2 := s.Create("req-2")

	n := s.DrainAll()
	if n != 2 {
		t.Errorf("DrainAll returned %d, want 2", n)
	}

	// Both channels should receive a terminal error chunk and then be closed.
	for _, ch := range []<-chan types.ChunkMsg{ch1, ch2} {
		msg, ok := <-ch
		if !ok {
			t.Error("expected terminal chunk before close")
			continue
		}
		if !msg.Done {
			t.Errorf("expected Done=true, got %v", msg.Done)
		}
		if msg.FinishReason != "error" {
			t.Errorf("expected FinishReason=error, got %q", msg.FinishReason)
		}
		// Channel should now be closed.
		if _, stillOpen := <-ch; stillOpen {
			t.Error("expected channel to be closed after DrainAll")
		}
	}
}

func TestDrainAll_EmptyStore(t *testing.T) {
	s := New(nil)
	if n := s.DrainAll(); n != 0 {
		t.Errorf("DrainAll on empty store returned %d, want 0", n)
	}
}

func TestDrainAll_ClearsStore(t *testing.T) {
	s := New(nil)
	s.Create("req-1")
	s.DrainAll()

	// After DrainAll, Create should return a fresh channel (not the drained one).
	ch := s.Create("req-1")
	s.Delete("req-1")
	if _, ok := <-ch; ok {
		t.Error("expected closed channel from Delete, channel still open")
	}
}
