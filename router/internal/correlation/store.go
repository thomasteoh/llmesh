// router/internal/correlation/store.go
package correlation

import (
	"sync"
	"llmesh/pkg/types"
)

// Store maps requestIDs to channels through which result chunks are delivered to HTTP handlers.
type Store struct {
	mu       sync.Mutex
	channels map[string]chan types.ChunkMsg
}

func New() *Store {
	return &Store{
		channels: make(map[string]chan types.ChunkMsg),
	}
}

// Create registers a new result channel for requestID. The channel is buffered.
// The caller is responsible for calling Delete when done.
func (s *Store) Create(requestID string) <-chan types.ChunkMsg {
	ch := make(chan types.ChunkMsg, 32)
	s.mu.Lock()
	s.channels[requestID] = ch
	s.mu.Unlock()
	return ch
}

// Send delivers a chunk to the waiting HTTP handler for the given requestID.
// Returns false if no handler is registered (request timed out or was cancelled).
func (s *Store) Send(msg types.ChunkMsg) bool {
	s.mu.Lock()
	ch, ok := s.channels[msg.RequestID]
	s.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case ch <- msg:
		return true
	default:
		return false
	}
}

// Delete removes the channel for requestID. Should be called by the HTTP handler on completion or timeout.
func (s *Store) Delete(requestID string) {
	s.mu.Lock()
	delete(s.channels, requestID)
	s.mu.Unlock()
}
