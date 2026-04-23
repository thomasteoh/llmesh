package queue

import (
	"sync"
	"llmesh/pkg/types"
)

// Queue is a thread-safe priority queue.
// Requests are ordered by priority (lower = higher priority), then by EnqueuedAt (FIFO within tier).
type Queue struct {
	mu    sync.Mutex
	cond  *sync.Cond
	items []types.InferenceRequest
}

// New creates and returns an empty Queue.
func New() *Queue {
	q := &Queue{}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// Push adds a request to the queue and signals waiting goroutines.
func (q *Queue) Push(req types.InferenceRequest) {
	q.mu.Lock()
	q.items = append(q.items, req)
	q.cond.Signal()
	q.mu.Unlock()
}

// Len returns the number of items in the queue.
func (q *Queue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// canHandle reports whether a client with the given models and aliases can serve req.
// req.Model may be a canonical model name or an alias pointing to one or more targets.
func canHandle(req types.InferenceRequest, models map[string]bool, aliases map[string][]string) bool {
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
	req := q.items[bestIdx]
	q.items = append(q.items[:bestIdx], q.items[bestIdx+1:]...)
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
	copy := q.items[bestIdx]
	return &copy
}

// PopByID removes and returns the request with the given ID.
// Returns nil if not found (e.g. already consumed).
func (q *Queue) PopByID(id string) *types.InferenceRequest {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, req := range q.items {
		if req.ID == id {
			q.items = append(q.items[:i], q.items[i+1:]...)
			return &req
		}
	}
	return nil
}

// betterForClient reports whether a is a better dispatch choice than b for a client
// whose owner is preferOwner. Affinity beats priority; priority beats FIFO.
func betterForClient(a, b types.InferenceRequest, preferOwner string) bool {
	aMatch := preferOwner != "" && a.Owner == preferOwner
	bMatch := preferOwner != "" && b.Owner == preferOwner
	if aMatch != bMatch {
		return aMatch
	}
	if a.Priority != b.Priority {
		return a.Priority < b.Priority
	}
	return a.EnqueuedAt.Before(b.EnqueuedAt)
}

// Signal wakes up goroutines waiting on the queue (e.g., after a client becomes available).
func (q *Queue) Signal() {
	q.cond.Signal()
}

// WaitAndPopBest blocks until a request matching models/aliases is available, then returns it.
// Returns nil if stop is closed. The caller must NOT hold q.mu.
// IMPORTANT: After closing stop, the caller must call Signal() to unblock any goroutine
// that may be blocked inside cond.Wait, otherwise shutdown will hang.
func (q *Queue) WaitAndPopBest(models map[string]bool, aliases map[string][]string, stop <-chan struct{}) *types.InferenceRequest {
	q.mu.Lock()
	defer q.mu.Unlock()
	for {
		select {
		case <-stop:
			return nil
		default:
		}
		req := q.popBestLocked(models, aliases)
		if req != nil {
			return req
		}
		q.cond.Wait()
	}
}
