// router/internal/correlation/store.go
package correlation

import (
	"hash/fnv"
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

// shardCount is the number of independent locks/maps the store is split across.
// Send is called once per streamed token across all in-flight requests, so a
// single global mutex would serialise all token delivery. Sharding by requestID
// spreads that contention. Must be a power of two for the mask in shardFor.
const shardCount = 32

type shard struct {
	mu       sync.Mutex
	channels map[string]chan types.ChunkMsg
}

// Store maps requestIDs to channels through which result chunks are delivered to HTTP handlers.
// The map is sharded by requestID to avoid a single lock on the per-token hot path.
type Store struct {
	shards [shardCount]shard
	log    *slog.Logger
}

func New(log *slog.Logger) *Store {
	if log == nil {
		log = slog.Default()
	}
	s := &Store{log: log}
	for i := range s.shards {
		s.shards[i].channels = make(map[string]chan types.ChunkMsg)
	}
	return s
}

// shardFor returns the shard responsible for requestID.
func (s *Store) shardFor(requestID string) *shard {
	h := fnv.New32a()
	h.Write([]byte(requestID))
	return &s.shards[h.Sum32()&(shardCount-1)]
}

// Create registers a new result channel for requestID. The channel is buffered.
// The caller is responsible for calling Delete when done.
// If an entry for requestID already exists (e.g. a duplicate job from a misbehaving
// upstream), the existing channel is returned rather than overwriting it, so the
// first goroutine's channel is never orphaned.
func (s *Store) Create(requestID string) <-chan types.ChunkMsg {
	sh := s.shardFor(requestID)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if ch, exists := sh.channels[requestID]; exists {
		return ch
	}
	ch := make(chan types.ChunkMsg, 256)
	sh.channels[requestID] = ch
	return ch
}

// Send delivers a chunk to the waiting HTTP handler for the given requestID.
// Returns SendOK on success, SendNotFound if no handler is registered, or
// SendFull if the handler's channel is full (caller should cancel the request
// to avoid silently truncating the response stream).
func (s *Store) Send(msg types.ChunkMsg) (result SendResult) {
	sh := s.shardFor(msg.RequestID)
	sh.mu.Lock()
	ch, found := sh.channels[msg.RequestID]
	sh.mu.Unlock()
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
	sh := s.shardFor(requestID)
	sh.mu.Lock()
	ch, ok := sh.channels[requestID]
	delete(sh.channels, requestID)
	sh.mu.Unlock()
	if ok {
		close(ch)
	}
}

// DrainAll sends a terminal error chunk to every registered handler then closes
// all channels. Calling this during shutdown unblocks all waiting SSE handlers
// so the HTTP server can drain cleanly. Returns the number of entries drained.
func (s *Store) DrainAll() int {
	drained := 0
	for i := range s.shards {
		sh := &s.shards[i]
		sh.mu.Lock()
		snapshot := sh.channels
		sh.channels = make(map[string]chan types.ChunkMsg)
		sh.mu.Unlock()

		for id, ch := range snapshot {
			select {
			case ch <- types.ChunkMsg{
				Type:         "chunk",
				RequestID:    id,
				Done:         true,
				FinishReason: "error",
			}:
			default:
			}
			close(ch)
		}
		drained += len(snapshot)
	}
	return drained
}
