package queue

import (
	"sync"
	"llmesh/pkg/types"
)

// Queue is a thread-safe priority queue.
// Requests are ordered by priority (lower = higher priority), then by EnqueuedAt (FIFO within tier).
type Queue struct {
	mu       sync.Mutex
	cond     *sync.Cond
	items    []types.InferenceRequest
	byID     map[string]int // requestID → index in items; kept in sync via pushLocked/removeAt
	MaxDepth int            // 0 = unlimited
}

// New creates and returns an empty Queue.
func New() *Queue {
	q := &Queue{byID: make(map[string]int)}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// pushLocked appends req and records its index. Caller must hold q.mu.
func (q *Queue) pushLocked(req types.InferenceRequest) {
	q.byID[req.ID] = len(q.items)
	q.items = append(q.items, req)
}

// removeAt removes the item at index i using swap-and-pop (O(1)).
// Caller must hold q.mu. The slice order is not preserved, but all
// selection methods (PeekBestForClient, popBestLocked) scan the whole
// slice and pick by priority+FIFO, so order is irrelevant for correctness.
func (q *Queue) removeAt(i int) types.InferenceRequest {
	req := q.items[i]
	last := len(q.items) - 1
	if i != last {
		q.items[i] = q.items[last]
		q.byID[q.items[i].ID] = i
	}
	q.items = q.items[:last]
	delete(q.byID, req.ID)
	return req
}

// Push adds a request unconditionally (used for internal re-queues).
func (q *Queue) Push(req types.InferenceRequest) {
	q.mu.Lock()
	q.pushLocked(req)
	q.cond.Signal()
	q.mu.Unlock()
}

// TryPush adds a request and returns true, or returns false without adding if
// MaxDepth > 0 and the queue is at capacity. Used for new API requests.
func (q *Queue) TryPush(req types.InferenceRequest) bool {
	q.mu.Lock()
	if q.MaxDepth > 0 && len(q.items) >= q.MaxDepth {
		q.mu.Unlock()
		return false
	}
	q.pushLocked(req)
	q.cond.Signal()
	q.mu.Unlock()
	return true
}

// Len returns the number of items in the queue.
func (q *Queue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// canHandle reports whether a client with the given models and aliases can serve req.
// req.Model may be a canonical model name, an alias pointing to one or more targets,
// or the reserved pseudo-model "any" which matches any client with at least one model.
func canHandle(req types.InferenceRequest, models map[string]bool, aliases map[string][]string) bool {
	if req.Model == "any" {
		return len(models) > 0
	}
	if models[req.Model] {
		return true
	}
	for _, target := range aliases[req.Model] {
		if models[target] {
			return true
		}
	}
	return false
}

// PopBest removes and returns the highest-priority request whose model (or alias) is
// supported by the client described by models+aliases.
// Within the same priority tier, the oldest request (FIFO) is returned.
// Returns nil if no matching request exists.
func (q *Queue) PopBest(models map[string]bool, aliases map[string][]string) *types.InferenceRequest {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.popBestLocked(models, aliases)
}

func (q *Queue) popBestLocked(models map[string]bool, aliases map[string][]string) *types.InferenceRequest {
	bestIdx := -1
	for i, req := range q.items {
		if !canHandle(req, models, aliases) {
			continue
		}
		if bestIdx == -1 {
			bestIdx = i
			continue
		}
		best := q.items[bestIdx]
		if req.Priority < best.Priority ||
			(req.Priority == best.Priority && req.EnqueuedAt.Before(best.EnqueuedAt)) {
			bestIdx = i
		}
	}
	if bestIdx == -1 {
		return nil
	}
	req := q.removeAt(bestIdx)
	return &req
}

// PeekBestForClient returns the best request (without removing it) for a client
// supporting the given models and aliases, preferring requests from preferOwner.
// Comparison order: affinity match > priority tier > FIFO.
// Returns nil if no matching request exists.
func (q *Queue) PeekBestForClient(models map[string]bool, aliases map[string][]string, preferOwner string) *types.InferenceRequest {
	q.mu.Lock()
	defer q.mu.Unlock()
	bestIdx := -1
	for i, req := range q.items {
		if !canHandle(req, models, aliases) {
			continue
		}
		if bestIdx == -1 {
			bestIdx = i
			continue
		}
		if betterForClient(req, q.items[bestIdx], preferOwner) {
			bestIdx = i
		}
	}
	if bestIdx == -1 {
		return nil
	}
	cp := q.items[bestIdx]
	return &cp
}

// PopByID removes and returns the request with the given ID in O(1).
// Returns nil if not found (e.g. already consumed).
func (q *Queue) PopByID(id string) *types.InferenceRequest {
	q.mu.Lock()
	defer q.mu.Unlock()
	i, ok := q.byID[id]
	if !ok {
		return nil
	}
	req := q.removeAt(i)
	return &req
}

// betterForClient reports whether a is a better dispatch choice than b for a client
// whose owner is preferOwner. Delegates to types.BetterRequest.
func betterForClient(a, b types.InferenceRequest, preferOwner string) bool {
	return types.BetterRequest(a, b,
		preferOwner != "" && a.Owner == preferOwner,
		preferOwner != "" && b.Owner == preferOwner)
}

// Snapshot returns a copy of all queued requests in their current order.
func (q *Queue) Snapshot() []types.InferenceRequest {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]types.InferenceRequest, len(q.items))
	copy(out, q.items)
	return out
}

// Drain removes and returns all queued requests, leaving the queue empty.
// Used during graceful shutdown to fail pending requests cleanly instead of
// leaving HTTP handlers hanging until the TTFT timeout fires.
func (q *Queue) Drain() []types.InferenceRequest {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]types.InferenceRequest, len(q.items))
	copy(out, q.items)
	q.items = q.items[:0]
	for k := range q.byID {
		delete(q.byID, k)
	}
	return out
}

// Signal wakes up goroutines waiting on the queue (e.g., after a client becomes available).
func (q *Queue) Signal() {
	q.cond.Signal()
}

