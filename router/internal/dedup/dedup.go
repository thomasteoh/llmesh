// router/internal/dedup/dedup.go
package dedup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"

	"llmesh/pkg/types"
)

// subscriber is one coalesced follower of an in-flight request. overflow is set
// when a chunk had to be dropped because the follower's buffer was full; such a
// follower is closed without ever receiving a Done chunk so its handler reports
// an error rather than a silently truncated success.
type subscriber struct {
	ch       chan types.ChunkMsg
	overflow bool
	// delivered records that at least one chunk has been handed to this
	// subscriber, so Reset can tell who has already seen output of an attempt
	// that is about to be abandoned.
	delivered bool
}

// maxReplayChunks caps the per-entry replay buffer. A follower has to be given
// everything emitted before it arrived, so an entry retains its chunks for as
// long as it is registered — for every request, whether or not a follower ever
// turns up, which for most requests is never. A 20-minute generation is tens of
// thousands of chunks, so on a router built to run many concurrent long
// generations the buffer is a real per-request memory multiplier that competes
// with the inference process for RAM.
//
// Coalescing exists for requests that arrive near-simultaneously, and those
// join within seconds. Past this many chunks the buffer is dropped and later
// arrivals run as ordinary requests instead, which the backend's prompt cache
// makes cheap. Roughly 400 KB per entry at the observed ~200 bytes per chunk.
const maxReplayChunks = 2048

// Role describes how a caller relates to an in-flight entry.
type Role int

const (
	// RoleOriginal means the caller owns the entry: it must Forward its chunks
	// and Unregister when done.
	RoleOriginal Role = iota
	// RoleFollower means the caller is coalesced onto an existing entry and
	// should read the replay buffer followed by the live channel.
	RoleFollower
	// RoleIndependent means an entry exists but can no longer be replayed, so
	// the caller must run as an ordinary uncoalesced request. It owns nothing:
	// it must not Forward and must not Unregister.
	RoleIndependent
)

// Entry tracks an in-flight request and any coalesced subscribers.
type Entry struct {
	mu     sync.Mutex
	chunks []types.ChunkMsg // buffer of chunks received so far, capped
	subs   []*subscriber    // live subscribers
	done   bool
	// replayDropped records that the buffer outgrew maxReplayChunks and was
	// released. Existing followers are unaffected — they hold live channels —
	// but no new one can be given a faithful replay.
	replayDropped bool
}

// Registry deduplicates concurrent requests with identical content.
// When a duplicate arrives while the original is in-flight or queued,
// the duplicate subscribes to the original's response stream instead of
// occupying a new worker slot.
type Registry struct {
	mu      sync.Mutex
	entries map[string]*Entry
	log     *slog.Logger
}

// New creates a Registry. A nil log falls back to slog.Default().
func New(log *slog.Logger) *Registry {
	if log == nil {
		log = slog.Default()
	}
	return &Registry{entries: make(map[string]*Entry), log: log}
}

// shortHash trims a content hash to something readable in a log line while
// staying long enough to correlate entries for the same request.
func shortHash(hash string) string {
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}

// RegisterOrSubscribe atomically registers hash as a new in-flight entry
// (RoleOriginal), subscribes to an existing one (RoleFollower, with a buffered
// replay plus a live channel), or reports that the existing entry can no longer
// be replayed (RoleIndependent, with no buffer and no channel).
//
// For RoleFollower with live==nil, the original has already finished and buffer
// holds the complete response. With live!=nil, buffer holds the chunks emitted
// so far and live carries the rest.
func (r *Registry) RegisterOrSubscribe(hash string) (role Role, buffer []types.ChunkMsg, live <-chan types.ChunkMsg) {
	r.mu.Lock()
	e, exists := r.entries[hash]
	if !exists {
		r.entries[hash] = &Entry{}
		r.mu.Unlock()
		return RoleOriginal, nil, nil
	}

	// Take e.mu before releasing r.mu so Unregister cannot delete the entry
	// between the lookup and the subscribe. r.mu is released immediately after,
	// rather than being held across the buffer copy: Forward takes r.mu for
	// every chunk of every in-flight request, so copying a long backlog under
	// it stalled chunk delivery router-wide, not just for this hash.
	e.mu.Lock()
	r.mu.Unlock()
	defer e.mu.Unlock()

	if e.replayDropped {
		// The earlier part of the answer is gone, so this caller cannot be
		// served from the entry. It runs on its own.
		return RoleIndependent, nil, nil
	}

	buf := make([]types.ChunkMsg, len(e.chunks))
	copy(buf, e.chunks)
	var ch chan types.ChunkMsg
	if !e.done {
		ch = make(chan types.ChunkMsg, 256)
		e.subs = append(e.subs, &subscriber{ch: ch})
	}

	return RoleFollower, buf, ch
}

// Forward buffers chunk and delivers it to all current subscribers.
// Called by the original request's handler for every chunk it receives.
func (r *Registry) Forward(hash string, chunk types.ChunkMsg) {
	r.mu.Lock()
	e, ok := r.entries[hash]
	r.mu.Unlock()
	if !ok {
		return
	}

	e.mu.Lock()
	// Counted under the lock and reported after it, so a log write never sits
	// on the path every chunk of every coalesced request takes.
	justDroppedReplay := false
	overflowed := 0
	switch {
	case e.replayDropped:
		// Buffer already released; live followers are served below.
	case len(e.chunks) >= maxReplayChunks:
		e.replayDropped = true
		e.chunks = nil
		justDroppedReplay = true
	default:
		e.chunks = append(e.chunks, chunk)
	}
	for _, sub := range e.subs {
		if sub.overflow {
			continue // already lost data; will be closed without Done
		}
		select {
		case sub.ch <- chunk:
			sub.delivered = true
		default:
			// Follower is too slow; mark it so it is closed without a Done
			// chunk, turning a silent gap into a signalled error downstream.
			sub.overflow = true
			overflowed++
		}
	}
	subs := len(e.subs)
	if chunk.Done {
		e.done = true
		for _, sub := range e.subs {
			close(sub.ch)
		}
		e.subs = nil
	}
	e.mu.Unlock()

	if justDroppedReplay {
		r.log.Info("dedup: replay buffer released, later duplicates will run on their own",
			"hash", shortHash(hash), "chunks", maxReplayChunks, "subscribers", subs)
	}
	if overflowed > 0 {
		// The follower is now guaranteed to fail. Without this the only trace
		// was the follower's own truncated-stream error, which reads as a
		// worker problem and gives no hint that its own read rate caused it.
		r.log.Warn("dedup: coalesced follower fell behind and will be failed",
			"hash", shortHash(hash), "followers_failed", overflowed, "subscribers", subs)
	}
}

// Reset discards the chunks buffered for hash, and marks every live subscriber
// as having lost data so none of them is handed a stitched-together answer.
//
// The original's handler calls this when it abandons an attempt and re-queues:
// its own accumulator is reset at the same moment, but the chunks it already
// forwarded are still in the entry, so without this a follower would receive
// the abandoned attempt's partial output followed by the retry's full output —
// as a well-formed 200, which is worse than an error.
func (r *Registry) Reset(hash string) {
	r.mu.Lock()
	e, ok := r.entries[hash]
	r.mu.Unlock()
	if !ok {
		return
	}
	e.mu.Lock()
	if e.done {
		e.mu.Unlock()
		return
	}
	e.chunks = nil
	// A subscriber that already saw the abandoned attempt's output cannot be
	// rewound, so it is failed rather than silently corrupted. One that has not
	// yet been sent anything is untouched and rides the retry normally.
	failed := 0
	for _, sub := range e.subs {
		if sub.overflow {
			continue
		}
		if len(sub.ch) > 0 || sub.delivered {
			sub.overflow = true
			failed++
		}
	}
	subs := len(e.subs)
	e.mu.Unlock()

	if failed > 0 {
		r.log.Warn("dedup: retry cost coalesced followers their partial answer",
			"hash", shortHash(hash), "followers_failed", failed, "subscribers", subs)
	}
}

// HasSubscribers reports whether hash still has coalesced followers waiting on
// a response. Used to decide whether the original leaving means the work should
// stop, or whether someone is still owed the answer.
func (r *Registry) HasSubscribers(hash string) bool {
	r.mu.Lock()
	e, ok := r.entries[hash]
	r.mu.Unlock()
	if !ok {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return !e.done && len(e.subs) > 0
}

// Unregister removes hash from the registry and closes any remaining subscriber
// channels. Called when the original request finishes (normally or via cancel/timeout).
func (r *Registry) Unregister(hash string) {
	r.mu.Lock()
	e, ok := r.entries[hash]
	delete(r.entries, hash)
	r.mu.Unlock()
	if !ok {
		return
	}
	e.mu.Lock()
	abandoned := 0
	if !e.done {
		// Original ended without a Done chunk (cancel/timeout/error). Closing
		// the subscriber channels without a Done makes each follower's handler
		// report an error instead of a truncated success.
		for _, sub := range e.subs {
			close(sub.ch)
		}
		abandoned = len(e.subs)
		e.subs = nil
	}
	e.mu.Unlock()

	if abandoned > 0 {
		// Says how many callers one failed inference took down with it, which
		// the failing callers' own logs cannot show.
		r.log.Warn("dedup: entry ended without completion, failing coalesced followers",
			"hash", shortHash(hash), "followers_failed", abandoned)
	}
}

// ContentHash returns a stable SHA-256 hash of the request fields that
// determine the response: model, messages, and generation parameters.
// Fields that do not affect the output (ID, owner, priority, timestamps) are excluded.
func ContentHash(req *types.InferenceRequest) string {
	return ContentHashOpts(req, false)
}

// ContentHashOpts is ContentHash with optional content normalisation. When
// normalize is true, each message's content is canonicalised (JSON object keys
// sorted, insignificant whitespace removed, string content trimmed) before
// hashing, so two requests that are semantically identical but differ only in
// JSON byte layout produce the same hash and therefore coalesce. Normalisation
// affects the hash only — the request dispatched to the model is unchanged.
func ContentHashOpts(req *types.InferenceRequest, normalize bool) string {
	type hashInput struct {
		Model       string          `json:"model"`
		Messages    []types.Message `json:"messages"`
		MaxTokens   int             `json:"max_tokens,omitempty"`
		Temperature *float64        `json:"temperature,omitempty"`
		TopP        *float64        `json:"top_p,omitempty"`
		Stop        []string        `json:"stop,omitempty"`
		Tools       json.RawMessage `json:"tools,omitempty"`
		ToolChoice  json.RawMessage `json:"tool_choice,omitempty"`
	}
	messages := req.Messages
	tools := req.Tools
	toolChoice := req.ToolChoice
	if normalize {
		messages = make([]types.Message, len(req.Messages))
		for i, m := range req.Messages {
			m.Content = canonicalJSON(m.Content)
			m.ToolCalls = canonicalJSON(m.ToolCalls)
			messages[i] = m
		}
		tools = canonicalJSON(tools)
		toolChoice = canonicalJSON(toolChoice)
	}
	data, _ := json.Marshal(hashInput{
		Model:       req.Model,
		Messages:    messages,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stop:        req.Stop,
		Tools:       tools,
		ToolChoice:  toolChoice,
	})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// canonicalJSON re-encodes raw so that object keys are sorted and insignificant
// whitespace is dropped (both guaranteed by encoding/json). String values are
// additionally trimmed of surrounding whitespace. Returns raw unchanged if it
// is empty or not valid JSON.
func canonicalJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return raw
	}
	if s, ok := v.(string); ok {
		v = strings.TrimSpace(s)
	}
	out, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return out
}

// MakeSubscriberChan returns a single channel pre-loaded with buffered chunks
// followed by live chunks. When live is nil, the channel is closed after the buffer.
// The caller reads this channel exactly like a correlation channel.
//
// ctx must be the subscriber request's context: when it is cancelled (the
// follower disconnected or returned early) the forwarding goroutine exits
// instead of blocking forever on a send to a channel nobody is reading.
func MakeSubscriberChan(ctx context.Context, buffer []types.ChunkMsg, live <-chan types.ChunkMsg) <-chan types.ChunkMsg {
	size := len(buffer) + 256
	if live == nil {
		size = len(buffer)
	}
	ch := make(chan types.ChunkMsg, size)
	for _, c := range buffer {
		ch <- c
	}
	if live != nil {
		go func() {
			defer close(ch)
			for {
				select {
				case c, ok := <-live:
					if !ok {
						return
					}
					select {
					case ch <- c:
					case <-ctx.Done():
						return
					}
				case <-ctx.Done():
					return
				}
			}
		}()
	} else {
		close(ch)
	}
	return ch
}
