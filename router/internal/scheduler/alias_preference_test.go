package scheduler

import (
	"log/slog"
	"testing"
	"time"

	"llmesh/pkg/types"
	"llmesh/router/internal/hub"
	"llmesh/router/internal/queue"
)

// chatChain is an alias with a preferred local model and a paid fallback, the
// shape this feature exists to serve.
func chatChain() tieredAlias {
	return tieredAlias{"chat": []types.AliasTarget{
		{Model: "local-llama", Priority: 0},
		{Model: "gpt-4o", Priority: 1},
	}}
}

func pushChat(q *queue.Queue, ids ...string) {
	base := time.Now()
	for i, id := range ids {
		q.Push(types.InferenceRequest{
			ID:         id,
			Model:      "chat",
			Owner:      "alice",
			EnqueuedAt: base.Add(time.Duration(i) * time.Millisecond),
		})
	}
}

// The headline behaviour: a saturated preferred tier spills to the next one,
// and until it is saturated it wins even against a completely idle fallback
// client — which is the opposite of what plain load-spreading would do.
func TestDrainQueue_PreferredTierFillsFirstThenSpills(t *testing.T) {
	h := hub.New(slog.Default())
	q := queue.New()
	s := New(q, h, chatChain(), slog.Default())

	// Both clients advertise max_concurrent=2 (registerModels), so the local box
	// can take two jobs before the third has to go somewhere else.
	local := dialClient(t, h, "alice", "ct-local", nil)
	registerModels(t, local, "local-llama")
	cloud := dialClient(t, h, "alice", "ct-cloud", nil)
	registerModels(t, cloud, "gpt-4o")

	pushChat(q, "r1", "r2", "r3")
	s.drainQueue()

	// Two jobs land on the preferred tier. Without tier comparison the second
	// would have gone to the idle cloud client, since betterClient prefers an
	// unloaded client over a loaded one.
	for i := 0; i < 2; i++ {
		job := readJob(t, local, 300*time.Millisecond)
		if job == nil {
			t.Fatalf("preferred client should receive job %d of 2", i+1)
		}
		if job.Request.Model != "local-llama" {
			t.Errorf("job %d: got model %q, want local-llama", i+1, job.Request.Model)
		}
	}
	// The third spills, because tier 0 has no free slot at this moment.
	job := readJob(t, cloud, 300*time.Millisecond)
	if job == nil {
		t.Fatal("third job should spill to the fallback tier once the preferred tier is full")
	}
	if job.Request.Model != "gpt-4o" {
		t.Errorf("spilled job: got model %q, want gpt-4o", job.Request.Model)
	}
	if q.Len() != 0 {
		t.Errorf("queue should be drained, %d left", q.Len())
	}
}

// Targets sharing a tier must keep load-spreading. Every alias target in a
// pre-upgrade database sits in tier 0, so this is the regression guard that
// upgrading changes no routing decision.
func TestDrainQueue_EqualTiersStillSpreadLoad(t *testing.T) {
	h := hub.New(slog.Default())
	q := queue.New()
	s := New(q, h, tieredAlias{"chat": []types.AliasTarget{
		{Model: "local-llama", Priority: 0},
		{Model: "gpt-4o", Priority: 0},
	}}, slog.Default())

	local := dialClient(t, h, "alice", "ct-local-eq", nil)
	registerModels(t, local, "local-llama")
	cloud := dialClient(t, h, "alice", "ct-cloud-eq", nil)
	registerModels(t, cloud, "gpt-4o")

	pushChat(q, "r1", "r2")
	s.drainQueue()

	if readJob(t, local, 300*time.Millisecond) == nil {
		t.Error("tied tiers should spread: local client got nothing")
	}
	if readJob(t, cloud, 300*time.Millisecond) == nil {
		t.Error("tied tiers should spread: cloud client got nothing")
	}
}

// A retry must be able to reach a different tier, so the alias the caller named
// has to survive the rewrite to a concrete model.
func TestDrainQueue_DispatchPreservesRequestedAlias(t *testing.T) {
	h := hub.New(slog.Default())
	q := queue.New()
	s := New(q, h, chatChain(), slog.Default())

	local := dialClient(t, h, "alice", "ct-local-req", nil)
	registerModels(t, local, "local-llama")

	pushChat(q, "r1")
	s.drainQueue()

	job := readJob(t, local, 300*time.Millisecond)
	if job == nil {
		t.Fatal("expected dispatch")
	}
	if job.Request.Model != "local-llama" {
		t.Errorf("Model: got %q, want the concrete local-llama", job.Request.Model)
	}
	if job.Request.RequestedModel != "chat" {
		t.Errorf("RequestedModel: got %q, want the alias chat", job.Request.RequestedModel)
	}
}

// "any" is rewritten at dispatch just like an alias, so it needs the same
// preservation or a retried "any" request would be pinned to one model.
func TestDrainQueue_DispatchPreservesAnyPseudoModel(t *testing.T) {
	h := hub.New(slog.Default())
	q := queue.New()
	s := New(q, h, noAlias{}, slog.Default())

	conn := dialClient(t, h, "alice", "ct-any-req", nil)
	registerModels(t, conn, "llama3")

	q.Push(types.InferenceRequest{ID: "r-any", Model: "any", Owner: "alice", EnqueuedAt: time.Now()})
	s.drainQueue()

	job := readJob(t, conn, 300*time.Millisecond)
	if job == nil {
		t.Fatal("expected dispatch")
	}
	if job.Request.RequestedModel != "any" {
		t.Errorf("RequestedModel: got %q, want any", job.Request.RequestedModel)
	}
}

func TestResolveModel_ReturnsMatchedTargetTier(t *testing.T) {
	aliases := map[string][]types.AliasTarget{
		"chat": {
			{Model: "local-llama", Priority: 0},
			{Model: "gpt-4o", Priority: 2},
		},
	}
	cases := []struct {
		name      string
		reqModel  string
		models    map[string]bool
		wantModel string
		wantTier  int
	}{
		{"preferred target served", "chat", map[string]bool{"local-llama": true}, "local-llama", 0},
		{"only fallback served", "chat", map[string]bool{"gpt-4o": true}, "gpt-4o", 2},
		{
			// A client serving both must resolve to the preferred one, since the
			// target list is ordered preferred-first.
			"both served picks preferred", "chat",
			map[string]bool{"local-llama": true, "gpt-4o": true}, "local-llama", 0,
		},
		{"concrete model is tier 0", "local-llama", map[string]bool{"local-llama": true}, "local-llama", 0},
		{"any is tier 0", "any", map[string]bool{"solo": true}, "solo", 0},
		{"unservable alias falls through unchanged", "chat", map[string]bool{"other": true}, "chat", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotModel, gotTier := resolveModel(c.reqModel, c.models, aliases)
			if gotModel != c.wantModel || gotTier != c.wantTier {
				t.Errorf("got (%q, %d), want (%q, %d)", gotModel, gotTier, c.wantModel, c.wantTier)
			}
		})
	}
}

// Tier must not be compared across different requests. If it were, a
// fallback-tier candidate for a high-priority request would lose to a
// preferred-tier candidate for a low-priority one, letting preference silently
// reorder the queue.
func TestBetterCandidate_TierDoesNotOutrankQueueOrder(t *testing.T) {
	idle := types.ClientSummary{ID: "idle", MaxConcurrent: 2}
	now := time.Now()

	highOnFallback := &candidate{
		client: idle,
		req:    types.InferenceRequest{ID: "high", Priority: types.PriorityHigh, EnqueuedAt: now},
		tier:   1,
	}
	normalOnPreferred := &candidate{
		client: idle,
		req:    types.InferenceRequest{ID: "normal", Priority: types.PriorityNormal, EnqueuedAt: now},
		tier:   0,
	}
	if betterCandidate(normalOnPreferred, highOnFallback) {
		t.Error("a preferred-tier candidate must not jump ahead of a higher-priority request")
	}
	if !betterCandidate(highOnFallback, normalOnPreferred) {
		t.Error("the higher-priority request should win regardless of tier")
	}
}

// Preference is an explicit operator statement about which model should answer;
// affinity is only a cache-warmth optimisation. A conversation that spilled to a
// fallback tier should come back to the preferred model once it has capacity.
func TestBetterCandidate_TierOutranksPrefixAffinity(t *testing.T) {
	req := types.InferenceRequest{ID: "same", EnqueuedAt: time.Now()}
	preferredCold := &candidate{
		client:   types.ClientSummary{ID: "local", MaxConcurrent: 2},
		req:      req,
		tier:     0,
		affinity: false,
	}
	fallbackWarm := &candidate{
		client:   types.ClientSummary{ID: "cloud", MaxConcurrent: 2},
		req:      req,
		tier:     1,
		affinity: true,
	}
	if !betterCandidate(preferredCold, fallbackWarm) {
		t.Error("preferred tier should win over an affinity match on a fallback tier")
	}
	if betterCandidate(fallbackWarm, preferredCold) {
		t.Error("affinity must not pin a conversation to a fallback tier")
	}
}

// Within one tier, affinity still decides — tier only breaks ties it is
// meaningful for.
func TestBetterCandidate_AffinityStillDecidesWithinATier(t *testing.T) {
	req := types.InferenceRequest{ID: "same", EnqueuedAt: time.Now()}
	warm := &candidate{client: types.ClientSummary{ID: "warm", MaxConcurrent: 2}, req: req, tier: 0, affinity: true}
	cold := &candidate{client: types.ClientSummary{ID: "cold", MaxConcurrent: 2}, req: req, tier: 0, affinity: false}
	if !betterCandidate(warm, cold) {
		t.Error("affinity should win between candidates in the same tier")
	}
}
