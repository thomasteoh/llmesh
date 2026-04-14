// router/internal/scheduler/scheduler.go
package scheduler

import (
	"log"
	"sync"

	"llmesh/pkg/types"
	"llmesh/router/internal/hub"
	"llmesh/router/internal/queue"
)

// Scheduler dispatches queued InferenceRequests to available hub clients.
type Scheduler struct {
	queue    *queue.Queue
	hub      *hub.Hub
	signal   chan struct{}
	stopCh   chan struct{}
	once     sync.Once
	stopOnce sync.Once
}

// New creates a Scheduler wired to the given queue and hub.
// It registers itself as the hub's OnAvailable callback.
func New(q *queue.Queue, h *hub.Hub) *Scheduler {
	s := &Scheduler{
		queue:  q,
		hub:    h,
		signal: make(chan struct{}, 1),
		stopCh: make(chan struct{}),
	}
	h.OnAvailable = func() { s.Wake() }
	return s
}

// Wake signals the scheduler to attempt dispatch. Safe to call from any goroutine.
func (s *Scheduler) Wake() {
	select {
	case s.signal <- struct{}{}:
	default:
	}
}

// Start runs the dispatch loop in a background goroutine. Idempotent.
func (s *Scheduler) Start() {
	s.once.Do(func() {
		go s.loop()
	})
}

// Stop shuts down the scheduler. Safe to call multiple times.
func (s *Scheduler) Stop() {
	s.stopOnce.Do(func() { close(s.stopCh) })
}

func (s *Scheduler) loop() {
	for {
		select {
		case <-s.stopCh:
			return
		case <-s.signal:
			s.drainQueue()
		}
	}
}

// drainQueue dispatches all currently dispatchable requests.
func (s *Scheduler) drainQueue() {
	for {
		available := s.hub.AvailableModels()
		if len(available) == 0 {
			return
		}
		req := s.queue.PopBest(available)
		if req == nil {
			return
		}
		clientID := s.hub.FindAvailable(req.Model)
		if clientID == "" {
			// Race: model became unavailable between AvailableModels and FindAvailable.
			// Re-queue and stop; scheduler will be woken when a client is free again.
			s.queue.Push(*req)
			return
		}
		s.hub.IncrInFlight(clientID)
		job := types.JobMsg{Type: "job", Request: *req}
		if !s.hub.SendToClient(clientID, job) {
			// Client gone or send buffer full; undo the in-flight increment and re-queue.
			// DecrInFlight is a no-op if the client disconnected and was already removed.
			s.hub.DecrInFlight(clientID)
			s.queue.Push(*req)
			log.Printf("scheduler: client %s unavailable, re-queued %s", clientID, req.ID)
			return
		}
		log.Printf("scheduler: dispatched %s (model=%s) to client %s", req.ID, req.Model, clientID)
	}
}
