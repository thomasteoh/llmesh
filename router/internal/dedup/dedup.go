// router/internal/dedup/dedup.go
package dedup

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"

	"llmesh/pkg/types"
)

// Entry tracks an in-flight request and any coalesced subscribers.
type Entry struct {
	mu     sync.Mutex
	chunks []types.ChunkMsg      // buffer of all chunks received so far
	subs   []chan types.ChunkMsg // live subscriber channels
	done   bool
}

// Registry deduplicates concurrent requests with identical content.
// When a duplicate arrives while the original is in-flight or queued,
// the duplicate subscribes to the original's response stream instead of
// occupying a new worker slot.
type Registry struct {
	mu      sync.Mutex
	entries map[string]*Entry
}

// New creates a Registry.
func New() *Registry {
	return &Registry{entries: make(map[string]*Entry)}
}

// RegisterOrSubscribe atomically either registers hash as a new in-flight entry
// (returning isOriginal=true) or subscribes to the existing entry (returning
// isOriginal=false with a buffered replay + live channel).
//
// When isOriginal=false and live==nil, the original has already finished;
// buffer contains the complete response. When live!=nil, buffer contains
// chunks emitted so far and live receives future chunks.
func (r *Registry) RegisterOrSubscribe(hash string) (isOriginal bool, buffer []types.ChunkMsg, live <-chan types.ChunkMsg) {
	r.mu.Lock()
	e, exists := r.entries[hash]
	if !exists {
		r.entries[hash] = &Entry{}
		r.mu.Unlock()
		return true, nil, nil
	}

	// Subscribing: hold r.mu while acquiring e.mu to prevent a race where
	// Unregister deletes the entry between our lookup and our subscribe.
	e.mu.Lock()
	buf := make([]types.ChunkMsg, len(e.chunks))
	copy(buf, e.chunks)
	var ch chan types.ChunkMsg
	if !e.done {
		ch = make(chan types.ChunkMsg, 256)
		e.subs = append(e.subs, ch)
	}
	e.mu.Unlock()
	r.mu.Unlock()

	return false, buf, ch
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
	defer e.mu.Unlock()
	e.chunks = append(e.chunks, chunk)
	for _, sub := range e.subs {
		select {
		case sub <- chunk:
		default:
		}
	}
	if chunk.Done {
		e.done = true
		for _, sub := range e.subs {
			close(sub)
		}
		e.subs = nil
	}
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
	defer e.mu.Unlock()
	if !e.done {
		for _, sub := range e.subs {
			close(sub)
		}
		e.subs = nil
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
func MakeSubscriberChan(buffer []types.ChunkMsg, live <-chan types.ChunkMsg) <-chan types.ChunkMsg {
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
			for c := range live {
				ch <- c
			}
			close(ch)
		}()
	} else {
		close(ch)
	}
	return ch
}
