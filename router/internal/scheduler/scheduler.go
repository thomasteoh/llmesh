// router/internal/scheduler/scheduler.go
package scheduler

import (
	"log/slog"
	"sync"
	"time"

	"llmesh/pkg/types"
	"llmesh/router/internal/queue"
	"llmesh/router/internal/reqopt"
)

// prefixAffinityTTL bounds how long a prefix→client mapping is honoured. After
// this, a conversation's prior client is assumed cold and normal load-spreading
// resumes. prefixAffinityMax caps the map so a flood of unique prefixes cannot
// grow it without bound.
const (
	prefixAffinityTTL = 10 * time.Minute
	prefixAffinityMax = 4096
)

// AliasProvider supplies the current alias→[]models map. Satisfied by *admin.State.
type AliasProvider interface {
	AliasMap() map[string][]string
}

// OptProvider supplies request-optimization toggles. Satisfied by *admin.State.
type OptProvider interface {
	RequestOpts() types.RequestOptimization
}

// prefixEntry records which client last served a given request prefix and when.
type prefixEntry struct {
	clientID string
	at       time.Time
}

// candidate is a (client, request) dispatch pairing under consideration.
type candidate struct {
	client   types.ClientSummary
	req      types.InferenceRequest
	affinity bool
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
	opts     OptProvider
	log      *slog.Logger
	signal   chan struct{}
	stopCh   chan struct{}
	once     sync.Once
	stopOnce sync.Once

	// prefixAff maps a request prefix key to the client that last served it.
	// Accessed only from the single dispatch-loop goroutine (and synchronous
	// drainQueue calls in tests), so it needs no lock.
	prefixAff map[string]prefixEntry
}

// New creates a Scheduler wired to the given queue, hub, and alias provider.
func New(q *queue.Queue, h Dispatcher, aliases AliasProvider, logger *slog.Logger) *Scheduler {
	s := &Scheduler{
		queue:     q,
		hub:       h,
		aliases:   aliases,
		log:       logger,
		signal:    make(chan struct{}, 1),
		stopCh:    make(chan struct{}),
		prefixAff: make(map[string]prefixEntry),
	}
	return s
}

// SetOptProvider registers the source of request-optimization toggles.
// Must be called before Start. Safe to leave unset (prefix affinity disabled).
func (s *Scheduler) SetOptProvider(p OptProvider) { s.opts = p }

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

	var opts types.RequestOptimization
	if s.opts != nil {
		opts = s.opts.RequestOpts()
	}
	// prefixKeyFor memoises the prefix key per request within this drain so we
	// don't re-hash a request that is the best candidate for several clients.
	// Computed from the request as queued (original model name) so lookups and
	// the post-dispatch record use the same key despite the later model rewrite.
	var pkCache map[string]string
	prefixKeyFor := func(req *types.InferenceRequest) string {
		if !opts.PrefixAffinity {
			return ""
		}
		if pk, ok := pkCache[req.ID]; ok {
			return pk
		}
		pk := reqopt.PrefixKey(req)
		pkCache[req.ID] = pk
		return pk
	}
	if opts.PrefixAffinity {
		pkCache = make(map[string]string)
		s.prunePrefixAffinity()
	}

	// Snapshot the available clients once per drain. Copying each client's
	// Models/OwnerSlots/context maps is the expensive part, and none of those
	// change mid-drain — only in-flight counts do, and those we track locally
	// below. A concurrent job completion only frees slots, so a stale snapshot
	// is safe (at worst slightly conservative; the resulting OnAvailable wakes
	// the scheduler again).
	clients := s.hub.AvailableClientList()
	if len(clients) == 0 {
		return
	}

	// Local in-flight accounting layered on the snapshot so we don't re-list
	// (and re-copy maps) on every dispatch iteration.
	inFlight := make(map[string]int, len(clients))
	for _, c := range clients {
		inFlight[c.ID] = c.InFlight
	}
	// nonOwner[clientID][model] tracks non-owner jobs for a model on a client.
	// Seeded lazily from the hub's live count on first access, then incremented
	// locally as we dispatch within this drain.
	nonOwner := make(map[string]map[string]int)
	nonOwnerCount := func(clientID, owner, model string) int {
		m := nonOwner[clientID]
		if m == nil {
			m = make(map[string]int)
			nonOwner[clientID] = m
		}
		if v, ok := m[model]; ok {
			return v
		}
		v := s.hub.NonOwnerInFlight(clientID, owner, model)
		m[model] = v
		return v
	}

	for {
		var best *candidate
		for _, c := range clients {
			if inFlight[c.ID] >= c.MaxConcurrent {
				continue
			}
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
				if nonOwnerCount(c.ID, c.Owner, inFlightModel) >= nonOwnerCap {
					s.log.Debug("scheduler: owner-slot cap reached for model",
						"client_id", c.ID, "model", inFlightModel,
						"non_owner_cap", nonOwnerCap)
					continue
				}
			}
			// Skip this client if its context window is too small for the estimated token count.
			if req.WordCount > 0 {
				resolved := resolveModel(req.Model, c.Models, aliases)
				if ctxSize := c.ModelContextSizes[resolved]; ctxSize > 0 {
					if needed := types.EstimateTokens(req.WordCount, req.MaxTokens); needed > ctxSize {
						s.log.Debug("scheduler: skipping client — context too small",
							"client_id", c.ID, "model", resolved,
							"context_size", ctxSize, "estimated_tokens", needed)
						continue
					}
				}
			}
			// Compare using the locally-tracked in-flight count, not the
			// snapshot's stale value, so load-spreading stays accurate.
			cc := c
			cc.InFlight = inFlight[c.ID]
			// A candidate is affinity-preferred when this client last served the
			// request's conversation prefix (and that mapping is still warm).
			affinity := false
			if opts.PrefixAffinity {
				if pk := prefixKeyFor(req); pk != "" {
					if e, ok := s.prefixAff[pk]; ok && e.clientID == c.ID {
						affinity = true
					}
				}
			}
			cand := &candidate{client: cc, req: *req, affinity: affinity}
			if best == nil || betterCandidate(cand, best) {
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

		req.Model = resolveModel(req.Model, best.client.Models, aliases)

		// Seed the local non-owner base from the hub before tracking this job, so
		// the base excludes the job we are about to dispatch.
		isNonOwner := req.Owner != best.client.Owner
		if isNonOwner {
			if nonOwner[best.client.ID] == nil {
				nonOwner[best.client.ID] = make(map[string]int)
			}
			if _, ok := nonOwner[best.client.ID][req.Model]; !ok {
				nonOwner[best.client.ID][req.Model] = s.hub.NonOwnerInFlight(best.client.ID, best.client.Owner, req.Model)
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

		// Record the prefix→client mapping so the next turn of this conversation
		// prefers the same client (warm KV cache). Guarded by the cap so a flood
		// of unique prefixes cannot grow the map without bound; an already-tracked
		// prefix is always refreshed to the new client/time.
		if opts.PrefixAffinity {
			if pk := pkCache[best.req.ID]; pk != "" {
				if _, exists := s.prefixAff[pk]; exists || len(s.prefixAff) < prefixAffinityMax {
					s.prefixAff[pk] = prefixEntry{clientID: best.client.ID, at: time.Now()}
				}
			}
		}

		// Update local accounting so subsequent iterations see this dispatch.
		inFlight[best.client.ID]++
		if isNonOwner {
			nonOwner[best.client.ID][req.Model]++
		}

		s.log.Info("scheduler: dispatched", "request_id", req.ID, "origin_id", req.OriginID, "model", req.Model, "owner", req.Owner, "client_id", best.client.ID, "client_owner", best.client.Owner)
	}
}

// betterCandidate reports whether candidate a should beat b. An affinity match
// (this client last served the request's prefix) wins outright; otherwise the
// usual affinity > priority > FIFO > load ordering applies.
func betterCandidate(a, b *candidate) bool {
	if a.affinity != b.affinity {
		return a.affinity
	}
	return betterPair(a.client, a.req, b.client, b.req)
}

// prunePrefixAffinity drops prefix→client mappings older than prefixAffinityTTL.
// Called once at the top of each drain so stale entries don't pin cold clients.
func (s *Scheduler) prunePrefixAffinity() {
	cutoff := time.Now().Add(-prefixAffinityTTL)
	for k, e := range s.prefixAff {
		if e.at.Before(cutoff) {
			delete(s.prefixAff, k)
		}
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

// resolveModel maps a request model name to the concrete model name that
// clientModels actually serves. Handles "any" (pick first available) and
// aliases (pick the first matching target). Returns reqModel unchanged if it
// is already a concrete name served by this client.
func resolveModel(reqModel string, clientModels map[string]bool, aliases map[string][]string) string {
	if reqModel == "any" {
		for m := range clientModels {
			return m
		}
		return reqModel
	}
	if targets, ok := aliases[reqModel]; ok {
		for _, t := range targets {
			if clientModels[t] {
				return t
			}
		}
	}
	return reqModel
}
