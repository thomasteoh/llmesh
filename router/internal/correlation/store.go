// router/internal/correlation/store.go
package correlation

import (
	"log/slog"
	"sync"

	"llmesh/pkg/types"
)

// SendResult indicates the outcome of a Send call.
type SendResult int

const (
	SendOK       SendResult = iota // chunk delivered to handler
	SendNotFound                   // no handler registered (timed out, cancelled, or completed)
	SendFull                       // handler channel full — caller should cancel the request
)

// Store maps requestIDs to channels through which result chunks are delivered to HTTP handlers.
type Store struct {
	mu       sync.Mutex
	channels map[string]chan types.ChunkMsg
	log      *slog.Logger
}

func New(log *slog.Logger) *Store {
	if log == nil {
		log = slog.Default()
	}
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
// Returns SendOK on success, SendNotFound if no handler is registered, or
// SendFull if the handler's channel is full (caller should cancel the request
// to avoid silently truncating the response stream).
func (s *Store) Send(msg types.ChunkMsg) (result SendResult) {
	s.mu.Lock()
	ch, found := s.channels[msg.RequestID]
	s.mu.Unlock()
	if !found {
		return SendNotFound
	}
	defer func() {
		if recover() != nil {
			result = SendNotFound // channel was closed (request completed)
		}
	}()
	select {
	case ch <- msg:
		return SendOK
	default:
		s.log.Warn("correlation: handler backpressure, cancelling request",
			"request_id", msg.RequestID, "done", msg.Done)
		return SendFull
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
