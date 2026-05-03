// router/internal/scheduler/scheduler.go
package scheduler

import (
	"log/slog"
	"sync"

	"llmesh/pkg/types"
	"llmesh/router/internal/hub"
	"llmesh/router/internal/queue"
)

// AliasProvider supplies the current alias→[]models map. Satisfied by *admin.State.
type AliasProvider interface {
	AliasMap() map[string][]string
}

// Scheduler dispatches queued InferenceRequests to available hub clients.
type Scheduler struct {
	queue    *queue.Queue
	hub      *hub.Hub
	aliases  AliasProvider
	log      *slog.Logger
	signal   chan struct{}
	stopCh   chan struct{}
	once     sync.Once
	stopOnce sync.Once
}

// New creates a Scheduler wired to the given queue, hub, and alias provider.
// It registers itself as the hub's OnAvailable callback.
func New(q *queue.Queue, h *hub.Hub, aliases AliasProvider, logger *slog.Logger) *Scheduler {
	s := &Scheduler{
		queue:   q,
		hub:     h,
		aliases: aliases,
		log:     logger,
		signal:  make(chan struct{}, 1),
		stopCh:  make(chan struct{}),
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

// drainQueue dispatches all currently dispatchable requests using client-centric
// affinity scheduling: for each available client, find the best request for that
// client (affinity > priority > FIFO), then pick the globally best (client, request)
// pair and dispatch.
// req.Model may be an alias; it is rewritten to the canonical model name before
// the job is sent to the client.
func (s *Scheduler) drainQueue() {
	for {
		clients := s.hub.AvailableClientList()
		if len(clients) == 0 {
			return
		}

		aliases := s.aliases.AliasMap()

		type candidate struct {
			clientID       string
			clientOwner    string
			clientModels   map[string]bool
			req            types.InferenceRequest
		}

		var best *candidate
		for _, c := range clients {
			req := s.queue.PeekBestForClient(c.Models, aliases, c.Owner)
			if req == nil {
				continue
			}
			// Exclusive clients only serve their owner's requests.
			if c.ExclusiveOwner && req.Owner != c.Owner {
				s.log.Debug("scheduler: skipping exclusive client for non-owner request",
					"client_id", c.ID, "client_owner", c.Owner, "request_owner", req.Owner)
				continue
			}
			cand := &candidate{
				clientID:     c.ID,
				clientOwner:  c.Owner,
				clientModels: c.Models,
				req:          *req,
			}
			if best == nil || betterPair(cand.clientOwner, cand.req, best.clientOwner, best.req) {
				best = cand
			}
		}
		if best == nil {
			return // no dispatchable request
		}

		req := s.queue.PopByID(best.req.ID)
		if req == nil {
			return // race: request already consumed; scheduler will be re-woken
		}

		// Rewrite alias → the specific model name the selected client serves.
		if targets, ok := aliases[req.Model]; ok {
			for _, t := range targets {
				if best.clientModels[t] {
					req.Model = t
					break
				}
			}
		}

		s.hub.IncrInFlight(best.clientID)
		job := types.JobMsg{Type: "job", Request: *req}
		if !s.hub.SendToClient(best.clientID, job) {
			s.hub.DecrInFlight(best.clientID)
			s.queue.Push(*req)
			s.log.Warn("scheduler: client unavailable, re-queued", "client_id", best.clientID, "request_id", req.ID)
			return
		}
		s.hub.TrackJob(best.clientID, *req)
		s.log.Info("scheduler: dispatched", "request_id", req.ID, "model", req.Model, "owner", req.Owner, "client_id", best.clientID, "client_owner", best.clientOwner)
	}
}

// betterPair reports whether (ownerA, reqA) is a better dispatch pair than (ownerB, reqB).
// A pair with affinity (request owner matches client owner) beats a non-affinity pair.
// Among equal affinity: lower priority tier wins, then earlier enqueue time.
func betterPair(ownerA string, reqA types.InferenceRequest, ownerB string, reqB types.InferenceRequest) bool {
	aAffinity := ownerA != "" && reqA.Owner == ownerA
	bAffinity := ownerB != "" && reqB.Owner == ownerB
	if aAffinity != bAffinity {
		return aAffinity
	}
	if reqA.Priority != reqB.Priority {
		return reqA.Priority < reqB.Priority
	}
	return reqA.EnqueuedAt.Before(reqB.EnqueuedAt)
}
