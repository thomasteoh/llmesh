// router/internal/correlation/store.go
package correlation

import (
	"log/slog"
	"sync"
	"llmesh/pkg/types"
)

// Store maps requestIDs to channels through which result chunks are delivered to HTTP handlers.
type Store struct {
	mu       sync.Mutex
	channels map[string]chan types.ChunkMsg
	log      *slog.Logger
}

func New(log *slog.Logger) *Store {
	return &Store{
		channels: make(map[string]chan types.ChunkMsg),
		log:      log,
	}
}

// Create registers a new result channel for requestID. The channel is buffered.
// The caller is responsible for calling Delete when done.
// If an entry for requestID already exists (e.g. a duplicate job from a misbehaving
// upstream), the existing channel is returned rather than overwriting it, so the
// first goroutine's channel is never orphaned.
func (s *Store) Create(requestID string) <-chan types.ChunkMsg {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ch, exists := s.channels[requestID]; exists {
		return ch
	}
	ch := make(chan types.ChunkMsg, 256)
	s.channels[requestID] = ch
	return ch
}

// Send delivers a chunk to the waiting HTTP handler for the given requestID.
// Returns false if no handler is registered (request timed out, cancelled, or completed).
func (s *Store) Send(msg types.ChunkMsg) (ok bool) {
	s.mu.Lock()
	ch, found := s.channels[msg.RequestID]
	s.mu.Unlock()
	if !found {
		return false
	}
	defer func() {
		if recover() != nil {
			ok = false // channel was closed (request completed)
		}
	}()
	select {
	case ch <- msg:
		return true
	default:
		s.log.Warn("correlation: chunk dropped, handler not reading fast enough",
			"request_id", msg.RequestID, "done", msg.Done)
		return false
	}
}

// Delete removes the channel for requestID and closes it to unblock any reader.
// The HTTP handler's reader loop will receive a zero-value ChunkMsg when the channel closes,
// but it should check Done:true or use a context timeout as its primary termination signal.
func (s *Store) Delete(requestID string) {
	s.mu.Lock()
	ch, ok := s.channels[requestID]
	delete(s.channels, requestID)
	s.mu.Unlock()
	if ok {
		close(ch)
	}
}
