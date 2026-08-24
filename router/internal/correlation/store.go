// router/internal/correlation/store.go
package correlation

import (
	"hash/fnv"
	"log/slog"
	"sync"
	"time"

	"llmesh/pkg/types"
)

// chanBuffer is the per-request chunk buffer. It absorbs the gap between a
// worker that emits tokens in bursts and a handler that does a write plus a
// flush per chunk. Sized for roughly a minute of a fast worker's output so a
// routine stall never reaches SendGrace.
const chanBuffer = 2048

// SendGrace is how long Send waits for a full buffer to drain before giving up
// and reporting SendFull, which cancels the generation. Bounded because the
// waiter is the client's WebSocket read goroutine, shared with that client's
// other jobs.
var SendGrace = 15 * time.Second

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

// entry is one registered handler's delivery channel plus the bookkeeping that
// lets Send wait on a full buffer without racing the close.
//
// Send can park on ch for up to SendGrace, so closing ch out from under it
// would be a data race on the channel itself — not something recover() makes
// safe. Instead a closing caller closes done first, which releases any parked
// sender, waits for senders to leave, and only then closes ch.
type entry struct {
	ch   chan types.ChunkMsg
	done chan struct{}
	// senders counts Sends between their lookup and their return. Incremented
	// under the shard lock, so once a closer has removed the entry from the map
	// no further sender can join.
	senders sync.WaitGroup
}

// close releases the entry: parked senders wake, and ch is closed once they
// have all left. Callers must have already removed e from the shard map.
func (e *entry) close() {
	close(e.done)
	e.senders.Wait()
	close(e.ch)
}

type shard struct {
	mu      sync.Mutex
	entries map[string]*entry
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
		s.shards[i].entries = make(map[string]*entry)
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
	if e, exists := sh.entries[requestID]; exists {
		return e.ch
	}
	e := &entry{
		ch:   make(chan types.ChunkMsg, chanBuffer),
		done: make(chan struct{}),
	}
	sh.entries[requestID] = e
	return e.ch
}

// Send delivers a chunk to the waiting HTTP handler for the given requestID.
// Returns SendOK on success, SendNotFound if no handler is registered, or
// SendFull if the handler's channel is full (caller should cancel the request
// to avoid silently truncating the response stream).
func (s *Store) Send(msg types.ChunkMsg) SendResult {
	sh := s.shardFor(msg.RequestID)
	sh.mu.Lock()
	e, found := sh.entries[msg.RequestID]
	if found {
		// Joining the wait group under the shard lock is what makes the close
		// safe: a closer removes the entry under this same lock before waiting,
		// so it can never miss a sender.
		e.senders.Add(1)
	}
	sh.mu.Unlock()
	if !found {
		return SendNotFound
	}
	defer e.senders.Done()

	select {
	case e.ch <- msg:
		return SendOK
	case <-e.done:
		return SendNotFound
	default:
	}
	// The handler is behind. Giving up here cancels the generation, so spend a
	// bounded wait first: the handler writes and flushes one SSE frame per chunk,
	// so a stalled TCP window, a proxy hiccup or a GC pause on a loaded box backs
	// it up for seconds at a time, and a fast worker fills the buffer in less than
	// that. Waiting costs this client's read goroutine some latency; not waiting
	// destroys a healthy response, and the longer the generation the likelier it
	// is to meet one such stall.
	t := time.NewTimer(SendGrace)
	defer t.Stop()
	select {
	case e.ch <- msg:
		return SendOK
	case <-e.done:
		return SendNotFound
	case <-t.C:
		s.log.Warn("correlation: handler backpressure, cancelling request",
			"request_id", msg.RequestID, "done", msg.Done, "waited", SendGrace.String())
		return SendFull
	}
}

// Delete removes the channel for requestID and closes it to unblock any reader.
// The HTTP handler's reader loop will receive a zero-value ChunkMsg when the channel closes,
// but it should check Done:true or use a context timeout as its primary termination signal.
func (s *Store) Delete(requestID string) {
	sh := s.shardFor(requestID)
	sh.mu.Lock()
	e, ok := sh.entries[requestID]
	delete(sh.entries, requestID)
	sh.mu.Unlock()
	if ok {
		e.close()
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
		snapshot := sh.entries
		sh.entries = make(map[string]*entry)
		sh.mu.Unlock()

		for id, e := range snapshot {
			select {
			case e.ch <- types.ChunkMsg{
				Type:         "chunk",
				RequestID:    id,
				Done:         true,
				FinishReason: "error",
			}:
			default:
			}
			e.close()
		}
		drained += len(snapshot)
	}
	return drained
}
