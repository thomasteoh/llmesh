// router/internal/scheduler/scheduler.go
package scheduler

import (
	"log/slog"
	"sync"
	"time"

	"llmesh/pkg/types"
	"llmesh/router/internal/reqopt"
)

// prefixAffinityTTL bounds how long a prefix→client mapping is honoured. After
// this, a conversation's prior client is assumed cold and normal load-spreading
// resumes. prefixAffinityMax caps the map so a flood of unique prefixes cannot
// grow it without bound.
const (
	// prefixAffinityTTL is measured from dispatch, not from completion, so it
	// has to outlast the longest single turn as well as the user's thinking
	// time between turns. At 10 minutes a large-context turn on slow hardware
	// outlived its own affinity entry: the follow-up turn then load-spread to a
	// client without the warm KV cache, and that cold prefill of the whole
	// conversation had to fit inside the TTFT budget or the request failed.
	prefixAffinityTTL = 45 * time.Minute
	prefixAffinityMax = 4096
)

// AliasProvider supplies the current alias maps. Satisfied by *admin.State.
// Both views come from one cached snapshot, so they cannot disagree: AliasMap
// answers "can this client serve the request at all" for the queue, while
// AliasTargets carries the preference tiers the dispatch comparison needs.
type AliasProvider interface {
	AliasMap() map[string][]string
	AliasTargets() map[string][]types.AliasTarget
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
	// resolved is the concrete model name this client would serve for req.Model,
	// computed once so the owner-slot check, context check, and the final
	// dispatch all agree (map iteration for "any"/aliases is otherwise
	// nondeterministic and could disagree across those three sites).
	resolved string
	// tier is the alias preference tier of resolved (0 for a concrete model or
	// "any"). Lower is preferred. Only comparable between candidates holding the
	// same request, since tiers of different aliases are unrelated numbers.
	tier int
}

// JobQueue is satisfied by *queue.Queue. Naming the three methods the dispatch
// loop uses lets a test stand in a queue that misbehaves — notably one whose
// selection and removal disagree, which is what the drain loop's termination
// guard exists to survive.
type JobQueue interface {
	PeekBestForClient(models map[string]bool, aliases map[string][]string, preferOwner string, eligible func(reqOwner string) bool) *types.InferenceRequest
	PopByID(id string) *types.InferenceRequest
	Push(req types.InferenceRequest)
}

// Dispatcher is satisfied by *hub.Hub. It exposes only the methods the scheduler
// needs to dispatch jobs, so the scheduler package does not import hub.
type Dispatcher interface {
	AvailableClientList() []types.ClientSummary
	SendToClient(clientID string, msg any) bool
	IncrInFlight(clientID string)
	DecrInFlight(clientID string)
	TrackJob(clientID string, req types.InferenceRequest) bool
	// UntrackJob reports whether this caller held the record and removed it.
	UntrackJob(clientID, requestID string) bool
}

// Scheduler dispatches queued InferenceRequests to available hub clients.
type Scheduler struct {
	queue    JobQueue
	hub      Dispatcher
	aliases  AliasProvider
	opts     OptProvider
	pairing  PairingPolicy
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
func New(q JobQueue, h Dispatcher, aliases AliasProvider, logger *slog.Logger) *Scheduler {
	s := &Scheduler{
		queue:     q,
		hub:       h,
		aliases:   aliases,
		log:       logger,
		signal:    make(chan struct{}, 1),
		stopCh:    make(chan struct{}),
		prefixAff: make(map[string]prefixEntry),
		pairing:   ownerPairing{},
	}
	return s
}

// SetOptProvider registers the source of request-optimization toggles.
// Must be called before Start. Safe to leave unset (prefix affinity disabled).
func (s *Scheduler) SetOptProvider(p OptProvider) { s.opts = p }

// Pairing is whether a requester may run on a client and on what terms,
// decided by access management (see authz.ClientPairing).
type Pairing struct {
	Allowed bool
	// OwnerSide is the client's owner, or a member of the team that owns it.
	OwnerSide bool
	// IdleOnly serves the requester only while no owner-side work runs.
	IdleOnly bool
	// Reserved is slots held back from the requester per model; "*" covers
	// models not listed.
	Reserved map[string]int
	// PerRequesterMax caps the requester's concurrent jobs on the client.
	PerRequesterMax int
}

func (p Pairing) reservedFor(model string) int {
	if n, ok := p.Reserved[model]; ok {
		return n
	}
	return p.Reserved["*"]
}

// PairingPolicy decides pairings; implementations must be cheap, since the
// scheduler asks once per requester per client per drain.
type PairingPolicy interface {
	Pair(reqOwner string, client types.ClientSummary) Pairing
}

// SetPairingPolicy installs access management's pairing decisions. Until
// one is installed, ownerPairing applies.
func (s *Scheduler) SetPairingPolicy(p PairingPolicy) { s.pairing = p }

// ownerPairing is the pairing without access management: anyone may use any
// client, its owner is the owner side, and the client's reserved slots are
// held back from everyone else.
type ownerPairing struct{}

func (ownerPairing) Pair(reqOwner string, c types.ClientSummary) Pairing {
	return Pairing{Allowed: true, OwnerSide: reqOwner == c.Owner, Reserved: c.OwnerSlots}
}

// jobRefLister is the optional hub method the pairing path uses to see who
// is already running on a client.
type jobRefLister interface {
	JobRefsOn(clientID string) []types.JobRef
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

// retryInterval is the scheduler's safety-net tick. The loop is otherwise
// edge-triggered, and a drain that abandons itself after re-queueing a request
// (client disconnected mid-dispatch, or its send buffer was momentarily full)
// leaves that request with no pending edge to wake it. On a small mesh whose
// only worker is part-way through a long generation, the next natural edge can
// be many minutes away — long enough for the request to expire at TTFT without
// ever having been offered to a client. Re-waking immediately instead would
// spin hot against a persistently full buffer.
const retryInterval = 5 * time.Second

func (s *Scheduler) loop() {
	ticker := time.NewTicker(retryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-s.signal:
			s.drainQueue()
		case <-ticker.C:
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
	aliasTargets := s.aliases.AliasTargets()

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
	// Access-managed pairing. pairMemo caches decisions for this drain;
	// running[clientID] is who is on each client, seeded from the hub and
	// extended as this drain dispatches.
	pairMemo := map[string]Pairing{}
	pair := func(reqOwner string, c types.ClientSummary) Pairing {
		k := reqOwner + "\x00" + c.ID
		p, ok := pairMemo[k]
		if !ok {
			p = s.pairing.Pair(reqOwner, c)
			pairMemo[k] = p
		}
		return p
	}
	running := map[string][]types.JobRef{}
	runningOn := func(clientID string) []types.JobRef {
		refs, ok := running[clientID]
		if !ok {
			if l, can := s.hub.(jobRefLister); can {
				refs = l.JobRefsOn(clientID)
			}
			running[clientID] = refs
		}
		return refs
	}
	// ownerSideBusy reports whether owner-side work is running on c, which
	// is what an idle-only client waits for to end.
	ownerSideBusy := func(c types.ClientSummary) bool {
		// Requests to the worker's local API come from the machine itself,
		// which is the owner's side of it.
		if c.LocalBusy > 0 {
			return true
		}
		for _, j := range runningOn(c.ID) {
			if pair(j.Owner, c).OwnerSide {
				return true
			}
		}
		return false
	}

	// IDs selected for dispatch that the queue then refused to hand over. See
	// the PopByID failure below for why one repeat is fatal to the drain.
	var unpoppable map[string]bool

	for {
		var best *candidate
		for _, c := range clients {
			if inFlight[c.ID] >= c.MaxConcurrent {
				continue
			}
			// Sharing filter: never offer this client a request it may not
			// serve. Applied during selection so a blocked request cannot
			// shadow one the client may run.
			client := c
			eligible := func(reqOwner string) bool {
				p := pair(reqOwner, client)
				if !p.Allowed {
					return false
				}
				if p.OwnerSide {
					return true
				}
				if p.IdleOnly && ownerSideBusy(client) {
					return false
				}
				if p.PerRequesterMax > 0 {
					n := 0
					for _, j := range runningOn(client.ID) {
						if j.Owner == reqOwner {
							n++
						}
					}
					if n >= p.PerRequesterMax {
						return false
					}
				}
				return true
			}
			req := s.queue.PeekBestForClient(c.Models, aliases, c.Owner, eligible)
			if req == nil {
				continue
			}
			// Resolve the concrete model once and reuse it everywhere below so
			// the reserved-slot cap, context check, and dispatch cannot disagree.
			resolved, tier := resolveModel(req, c.Models, aliasTargets)
			// Slots the client keeps back from non-owner-side requesters.
			if p := pair(req.Owner, c); !p.OwnerSide {
				reserved := p.reservedFor(resolved)
				capacity := c.MaxConcurrent - reserved
				if capacity <= 0 {
					continue
				}
				others := 0
				for _, j := range runningOn(c.ID) {
					if j.Model == resolved && !pair(j.Owner, c).OwnerSide {
						others++
					}
				}
				if others >= capacity {
					s.log.Debug("scheduler: reserved slots hold this client back",
						"client_id", c.ID, "model", resolved, "reserved", reserved)
					continue
				}
			}
			// Skip this client if its context window is too small for the estimated token count.
			if req.WordCount > 0 {
				if ctxSize := c.ModelContextSizes[resolved]; ctxSize > 0 {
					if needed := types.EstimateTokens(req.WordCount, req.MaxTokens); needed > ctxSize {
						s.log.Debug("scheduler: skipping client — context too small",
							"client_id", c.ID, "model", resolved,
							"context_size", ctxSize, "estimated_tokens", needed)
						continue
					}
				}
			}
			// Skip this client if the request needs input modalities the client
			// positively does not support. Clients with unknown capabilities
			// (no advertised modalities) are never skipped, preserving
			// pass-through for backends that don't report capability.
			if len(req.Modalities) > 0 {
				if !types.ModalitiesCompatible(c.ModelModalities[resolved], req.Modalities) {
					s.log.Debug("scheduler: skipping client — model lacks required modalities",
						"client_id", c.ID, "model", resolved,
						"required", req.Modalities, "advertised", c.ModelModalities[resolved])
					continue
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
			cand := &candidate{client: cc, req: *req, affinity: affinity, resolved: resolved, tier: tier}
			if best == nil || betterCandidate(cand, best) {
				best = cand
			}
		}
		if best == nil {
			return // no dispatchable request
		}

		req := s.queue.PopByID(best.req.ID)
		if req == nil {
			// Normally this means the request was consumed concurrently (the
			// caller cancelled it, say). It is gone from the queue, so the next
			// iteration selects something else and the drain makes progress.
			//
			// If selection hands back the same ID a second time, though, the
			// queue is returning a request it cannot remove, and continuing
			// would spin this loop at full tilt on the single scheduler
			// goroutine — no request would ever dispatch again, and the burnt
			// CPU would slow the very inference workers we schedule onto.
			// Abandon the drain instead; the next wake starts clean.
			if unpoppable[best.req.ID] {
				s.log.Error("scheduler: queue returned a request it cannot remove, abandoning drain",
					"request_id", best.req.ID)
				return
			}
			if unpoppable == nil {
				unpoppable = make(map[string]bool)
			}
			unpoppable[best.req.ID] = true
			s.log.Debug("scheduler: request already consumed by another client", "request_id", best.req.ID)
			continue
		}

		// Preserve the name the caller asked for before rewriting to the concrete
		// model, so a retry re-resolves the alias instead of being pinned to the
		// model that just failed. Idempotent: a released request arrives with
		// Model already restored to RequestedModel.
		if req.RequestedModel == "" {
			req.RequestedModel = req.Model
		}
		req.Model = best.resolved

		// Count the slot first, then track, then send. A client can answer faster
		// than this goroutine reaches the next line — a cache hit, or a shim fronting
		// an API that already has the response — so every step the completion path
		// undoes must already be in place before the job goes on the wire. Tracking
		// after sending leaves a window where the completion finds no job to untrack,
		// and its slot is never released nor its tokens and timings recorded;
		// incrementing after tracking leaves one where the reply's DecrInFlight
		// lands before this IncrInFlight and the count leaks the other way.
		s.hub.IncrInFlight(best.client.ID)
		if !s.hub.TrackJob(best.client.ID, *req) {
			s.hub.DecrInFlight(best.client.ID)
			s.queue.Push(*req)
			s.log.Warn("scheduler: client disconnected during dispatch, re-queued", "client_id", best.client.ID, "request_id", req.ID)
			return
		}
		job := types.JobMsg{Type: "job", Request: req.ForWorker()}
		if !s.hub.SendToClient(best.client.ID, job) {
			// The job never reached the client, so undo the tracking as well —
			// otherwise it lingers as a phantom in-flight job until its lease expires.
			//
			// Untracking also decides who re-queues. If the client died between
			// TrackJob and here, the hub's disconnect sweep has already taken
			// the record and released the request; untracking then finds
			// nothing, and pushing anyway would put a second copy of the same
			// ID in the queue behind the hub's.
			if !s.hub.UntrackJob(best.client.ID, req.ID) {
				s.log.Warn("scheduler: client unavailable, already re-queued by the hub", "client_id", best.client.ID, "request_id", req.ID)
				return
			}
			s.hub.DecrInFlight(best.client.ID)
			s.queue.Push(*req)
			s.log.Warn("scheduler: client unavailable, re-queued", "client_id", best.client.ID, "request_id", req.ID)
			return
		}

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
		running[best.client.ID] = append(runningOn(best.client.ID), types.JobRef{Owner: req.Owner, Model: req.Model})

		s.log.Info("scheduler: dispatched", "request_id", req.ID, "model", req.Model, "owner", req.Owner, "client_id", best.client.ID, "client_owner", best.client.Owner)
	}
}

// betterCandidate reports whether candidate a should beat b. Ordering:
// alias preference tier > affinity > priority > FIFO > load.
//
// Tier leads because it is an explicit operator statement about which model
// should serve the request, whereas affinity is only a cache-warmth
// optimisation — a conversation that spilled to a fallback tier should return to
// the preferred model as soon as it has capacity, even at the cost of a cold
// prefix. Tier is compared only between candidates holding the same request:
// applying it across requests would let a fallback-tier request jump the queue
// ahead of a higher-priority or older one.
func betterCandidate(a, b *candidate) bool {
	if a.req.ID == b.req.ID && a.tier != b.tier {
		return a.tier < b.tier
	}
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
// clientModels actually serves, plus that model's alias preference tier. Handles
// "any" (pick first available) and aliases (pick the most-preferred matching
// target, since targets are ordered preferred-first). Returns reqModel unchanged
// at tier 0 if it is already a concrete name served by this client.
func resolveModel(req *types.InferenceRequest, clientModels map[string]bool, aliases map[string][]types.AliasTarget) (string, int) {
	reqModel := req.Model
	if reqModel == "any" {
		for m := range clientModels {
			if req.ModelAllowed(m) {
				return m, 0
			}
		}
		return reqModel, 0
	}
	if targets, ok := aliases[reqModel]; ok {
		for _, t := range targets {
			if clientModels[t.Model] && req.ModelAllowed(t.Model) {
				return t.Model, t.Priority
			}
		}
	}
	return reqModel, 0
}
