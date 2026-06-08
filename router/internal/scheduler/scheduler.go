// router/internal/scheduler/scheduler.go
package scheduler

import (
	"log/slog"
	"sync"

	"llmesh/pkg/types"
	"llmesh/router/internal/queue"
)

// AliasProvider supplies the current alias→[]models map. Satisfied by *admin.State.
type AliasProvider interface {
	AliasMap() map[string][]string
}

// Dispatcher is satisfied by *hub.Hub. It exposes only the methods the scheduler
// needs to dispatch jobs, so the scheduler package does not import hub.
type Dispatcher interface {
	AvailableClientList() []types.ClientSummary
	SendToClient(clientID string, msg any) bool
	IncrInFlight(clientID string)
	DecrInFlight(clientID string)
	TrackJob(clientID string, req types.InferenceRequest)
	NonOwnerInFlight(clientID, owner, model string) int
}

// Scheduler dispatches queued InferenceRequests to available hub clients.
type Scheduler struct {
	queue    *queue.Queue
	hub      Dispatcher
	aliases  AliasProvider
	log      *slog.Logger
	signal   chan struct{}
	stopCh   chan struct{}
	once     sync.Once
	stopOnce sync.Once
}

// New creates a Scheduler wired to the given queue, hub, and alias provider.
func New(q *queue.Queue, h Dispatcher, aliases AliasProvider, logger *slog.Logger) *Scheduler {
	s := &Scheduler{
		queue:   q,
		hub:     h,
		aliases: aliases,
		log:     logger,
		signal:  make(chan struct{}, 1),
		stopCh:  make(chan struct{}),
	}
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
	aliases := s.aliases.AliasMap()
	for {
		clients := s.hub.AvailableClientList()
		if len(clients) == 0 {
			return
		}

		type candidate struct {
			client types.ClientSummary
			req    types.InferenceRequest
		}

		var best *candidate
		for _, c := range clients {
			req := s.queue.PeekBestForClient(c.Models, aliases, c.Owner)
			if req == nil {
				continue
			}
			// Enforce per-model owner-slot constraints for non-owner requests.
			if req.Owner != c.Owner {
				// Resolve model names for OwnerSlots and NonOwnerInFlight separately:
				//
				//   ownerSlotsKey — the key into c.OwnerSlots. For a direct model name
				//     or "any", this is req.Model unchanged (users set OwnerSlots["any"]
				//     for "any" requests; there is no single concrete name to use).
				//     For an alias it is resolved to the canonical model name the client
				//     actually serves (users set OwnerSlots by canonical name).
				//
				//   inFlightModel — the model name used in NonOwnerInFlight's live scan.
				//     Dispatched jobs always store the rewritten (concrete) model name,
				//     so this must be the resolved name in all cases — including "any",
				//     which is rewritten before TrackJob is called.
				ownerSlotsKey := req.Model // default: same as request
				inFlightModel := req.Model // will be resolved below
				if req.Model == "any" {
					// OwnerSlots key stays "any" (user-visible).
					// Resolve inFlightModel to the concrete model for the live scan.
					for m := range c.Models {
						inFlightModel = m
						break
					}
				} else if targets, ok := aliases[req.Model]; ok {
					for _, t := range targets {
						if c.Models[t] {
							ownerSlotsKey = t
							inFlightModel = t
							break
						}
					}
				}
				ownerReserved := c.OwnerSlots[ownerSlotsKey] // 0 if unset = fully shared
				nonOwnerCap := c.MaxConcurrent - ownerReserved
				if nonOwnerCap <= 0 {
					s.log.Debug("scheduler: skipping exclusive client for non-owner request",
						"client_id", c.ID, "client_owner", c.Owner, "request_owner", req.Owner,
						"model", ownerSlotsKey, "owner_reserved", ownerReserved)
					continue
				}
				if s.hub.NonOwnerInFlight(c.ID, c.Owner, inFlightModel) >= nonOwnerCap {
					s.log.Debug("scheduler: owner-slot cap reached for model",
						"client_id", c.ID, "model", inFlightModel,
						"non_owner_cap", nonOwnerCap)
					continue
				}
			}
			cand := &candidate{client: c, req: *req}
			if best == nil || betterPair(cand.client, cand.req, best.client, best.req) {
				best = cand
			}
		}
		if best == nil {
			return // no dispatchable request
		}

		req := s.queue.PopByID(best.req.ID)
		if req == nil {
			// Request was already consumed by another iteration.
			// Continue to check remaining clients — they may have their own best request.
			s.log.Debug("scheduler: request already consumed by another client", "request_id", best.req.ID)
			continue
		}

		// "any" is a system pseudo-model: dispatch to whichever model the selected client serves.
		if req.Model == "any" {
			for m := range best.client.Models {
				req.Model = m
				break
			}
		} else if targets, ok := aliases[req.Model]; ok {
			// Rewrite alias → the specific model name the selected client serves.
			for _, t := range targets {
				if best.client.Models[t] {
					req.Model = t
					break
				}
			}
		}

		s.hub.IncrInFlight(best.client.ID)
		job := types.JobMsg{Type: "job", Request: *req}
		if !s.hub.SendToClient(best.client.ID, job) {
			s.hub.DecrInFlight(best.client.ID)
			s.queue.Push(*req)
			s.log.Warn("scheduler: client unavailable, re-queued", "client_id", best.client.ID, "request_id", req.ID)
			return
		}
		s.hub.TrackJob(best.client.ID, *req)
		s.log.Info("scheduler: dispatched", "request_id", req.ID, "origin_id", req.OriginID, "model", req.Model, "owner", req.Owner, "client_id", best.client.ID, "client_owner", best.client.Owner)
	}
}

// betterPair reports whether (cA, reqA) is a better dispatch pair than (cB, reqB).
// Ordering: affinity > priority tier > FIFO > client load (betterClient).
// When both candidates hold the same request, client load decides immediately.
func betterPair(cA types.ClientSummary, reqA types.InferenceRequest, cB types.ClientSummary, reqB types.InferenceRequest) bool {
	// Same request competing across multiple clients: pure client quality comparison.
	if reqA.ID == reqB.ID {
		return betterClient(cA, cB)
	}
	aMatch := cA.Owner != "" && reqA.Owner == cA.Owner
	bMatch := cB.Owner != "" && reqB.Owner == cB.Owner
	if aMatch != bMatch || reqA.Priority != reqB.Priority {
		return types.BetterRequest(reqA, reqB, aMatch, bMatch)
	}
	if !reqA.EnqueuedAt.Equal(reqB.EnqueuedAt) {
		return reqA.EnqueuedAt.Before(reqB.EnqueuedAt)
	}
	return betterClient(cA, cB)
}

// betterClient reports whether client a is a better dispatch target than b.
// Ordering:
//  1. Unloaded (0 in-flight) before any loaded client — spreads work across machines.
//  2. Among equally unloaded: higher MaxConcurrent first (0/4 before 0/2).
//  3. Once all clients are loaded: more free slots first (2/4 before 1/2).
func betterClient(a, b types.ClientSummary) bool {
	aUnloaded := a.InFlight == 0
	bUnloaded := b.InFlight == 0
	if aUnloaded != bUnloaded {
		return aUnloaded
	}
	if a.InFlight == 0 {
		return a.MaxConcurrent > b.MaxConcurrent
	}
	return (a.MaxConcurrent - a.InFlight) > (b.MaxConcurrent - b.InFlight)
}
