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

// PopBest removes and returns the highest-priority request whose model is in availableModels.
// Within the same priority tier, the oldest request (FIFO) is returned.
// Returns nil if no matching request exists.
func (q *Queue) PopBest(availableModels map[string]bool) *types.InferenceRequest {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.popBestLocked(availableModels)
}

func (q *Queue) popBestLocked(availableModels map[string]bool) *types.InferenceRequest {
	bestIdx := -1
	for i, req := range q.items {
		if !availableModels[req.Model] {
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

// Signal wakes up goroutines waiting on the queue (e.g., after a client becomes available).
func (q *Queue) Signal() {
	q.cond.Signal()
}

// WaitAndPopBest blocks until a request matching availableModels is available, then returns it.
// The caller must NOT hold q.mu. This is used by the scheduler.
func (q *Queue) WaitAndPopBest(availableModels map[string]bool, stop <-chan struct{}) *types.InferenceRequest {
	q.mu.Lock()
	defer q.mu.Unlock()
	for {
		select {
		case <-stop:
			return nil
		default:
		}
		req := q.popBestLocked(availableModels)
		if req != nil {
			return req
		}
		q.cond.Wait()
	}
}
