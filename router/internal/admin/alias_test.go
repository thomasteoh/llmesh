package admin

import (
	"path/filepath"
	"testing"

	"llmesh/pkg/types"
)

func aliasTestState(t *testing.T) *State {
	t.Helper()
	s, err := LoadState(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	return s
}

// models flattens an alias's targets to names for terse assertions.
func aliasModels(targets []types.AliasTarget) []string {
	out := make([]string, 0, len(targets))
	for _, t := range targets {
		out = append(out, t.Model)
	}
	return out
}

func assertOrder(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("chain length: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("chain order: got %v, want %v", got, want)
		}
	}
}

// AddAlias must keep putting targets in tier 0. Before preference existed every
// target of an alias was interchangeable, and an upgrade that silently demoted
// the second target would change routing on live deployments.
func TestAddAlias_DefaultsToTopTierSoUpgradesDoNotReorder(t *testing.T) {
	s := aliasTestState(t)
	if err := s.AddAlias("chat", "gpt-4o"); err != nil {
		t.Fatalf("AddAlias: %v", err)
	}
	if err := s.AddAlias("chat", "local-llama"); err != nil {
		t.Fatalf("AddAlias: %v", err)
	}
	for _, tg := range s.AliasTargets()["chat"] {
		if tg.Priority != 0 {
			t.Errorf("target %s: got tier %d, want 0", tg.Model, tg.Priority)
		}
	}
}

// Within a tier, ordering must be by model name so the target list — and the
// resolution that reads it — is stable across restarts and cache rebuilds.
func TestAliasTargets_OrderedByTierThenName(t *testing.T) {
	s := aliasTestState(t)
	if err := s.AddAliasWithPriority("chat", "zeta", 0); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := s.AddAliasWithPriority("chat", "alpha", 0); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := s.AddAliasWithPriority("chat", "beta", 1); err != nil {
		t.Fatalf("add: %v", err)
	}
	// alpha and zeta share tier 0 so they sort by name; beta is tier 1 and comes
	// last despite sorting before zeta alphabetically.
	assertOrder(t, aliasModels(s.AliasTargets()["chat"]), "alpha", "zeta", "beta")
}

func TestAddAliasWithPriority_RejectsNegativeTier(t *testing.T) {
	s := aliasTestState(t)
	if err := s.AddAliasWithPriority("chat", "gpt-4o", -1); err == nil {
		t.Fatal("expected negative tier to be rejected")
	}
}

// The two alias views are read on the hot path from separate caches. If a
// mutation invalidated only one, the scheduler could see a tier for a target the
// queue no longer believes the alias reaches (or vice versa).
func TestAliasViewsStayConsistentAcrossMutations(t *testing.T) {
	s := aliasTestState(t)
	check := func(step string, want ...string) {
		t.Helper()
		names := s.AliasMap()["chat"]
		targets := aliasModels(s.AliasTargets()["chat"])
		assertOrder(t, names, want...)
		assertOrder(t, targets, want...)
	}

	if err := s.AddAliasWithPriority("chat", "local-llama", 0); err != nil {
		t.Fatalf("add: %v", err)
	}
	check("after first add", "local-llama")

	// Populate both caches, then mutate: a stale AliasMap would still say one target.
	_ = s.AliasMap()
	_ = s.AliasTargets()
	if err := s.AddAliasWithPriority("chat", "gpt-4o", 1); err != nil {
		t.Fatalf("add: %v", err)
	}
	check("after second add", "local-llama", "gpt-4o")

	_ = s.AliasMap()
	if err := s.MoveAliasTarget("chat", "gpt-4o", -1); err != nil {
		t.Fatalf("move: %v", err)
	}
	check("after promote", "gpt-4o", "local-llama")

	_ = s.AliasMap()
	if err := s.DeleteAlias("chat", "gpt-4o"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	check("after delete", "local-llama")

	_ = s.AliasMap()
	if err := s.DeleteAliasGroup("chat"); err != nil {
		t.Fatalf("delete group: %v", err)
	}
	if got := s.AliasMap()["chat"]; len(got) != 0 {
		t.Errorf("AliasMap after group delete: got %v, want empty", got)
	}
	if got := s.AliasTargets()["chat"]; len(got) != 0 {
		t.Errorf("AliasTargets after group delete: got %v, want empty", got)
	}
}

// A group of tied targets has no unambiguous "one place up", so the first move
// must resolve the whole group into an explicit 0..n-1 chain. Without the
// renumber, promoting within a tie would write a duplicate tier and the arrow
// would appear to do nothing.
func TestMoveAliasTarget_RenumbersTiedGroupIntoStrictChain(t *testing.T) {
	s := aliasTestState(t)
	for _, m := range []string{"alpha", "beta", "gamma"} {
		if err := s.AddAlias("chat", m); err != nil { // all tier 0
			t.Fatalf("add %s: %v", m, err)
		}
	}
	if err := s.MoveAliasTarget("chat", "gamma", -1); err != nil {
		t.Fatalf("move: %v", err)
	}
	targets := s.AliasTargets()["chat"]
	assertOrder(t, aliasModels(targets), "alpha", "gamma", "beta")
	for i, tg := range targets {
		if tg.Priority != i {
			t.Errorf("target %s: got tier %d, want %d (strict chain)", tg.Model, tg.Priority, i)
		}
	}
}

func TestMoveAliasTarget_AtEndsIsNoOp(t *testing.T) {
	s := aliasTestState(t)
	if err := s.AddAliasWithPriority("chat", "first", 0); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := s.AddAliasWithPriority("chat", "second", 1); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := s.MoveAliasTarget("chat", "first", -1); err != nil {
		t.Fatalf("promote first: %v", err)
	}
	if err := s.MoveAliasTarget("chat", "second", 1); err != nil {
		t.Fatalf("demote last: %v", err)
	}
	assertOrder(t, aliasModels(s.AliasTargets()["chat"]), "first", "second")
}

func TestMoveAliasTarget_UnknownAliasOrModel(t *testing.T) {
	s := aliasTestState(t)
	if err := s.AddAlias("chat", "gpt-4o"); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := s.MoveAliasTarget("nope", "gpt-4o", 1); err == nil {
		t.Error("expected error for unknown alias")
	}
	if err := s.MoveAliasTarget("chat", "nope", 1); err == nil {
		t.Error("expected error for unknown model")
	}
}

func TestAliasChainRows_MarksReachabilityTiesAndBounds(t *testing.T) {
	targets := map[string][]types.AliasTarget{
		"chat": {
			{Model: "local-a", Priority: 0},
			{Model: "local-b", Priority: 0},
			{Model: "gpt-4o", Priority: 1},
		},
		"solo": {{Model: "only", Priority: 0}},
	}
	rows := aliasChainRows(targets, []string{"local-a", "gpt-4o"})

	if len(rows) != 2 || rows[0].Alias != "chat" || rows[1].Alias != "solo" {
		t.Fatalf("rows should be sorted by alias, got %+v", rows)
	}

	chat := rows[0].Targets
	if len(chat) != 3 {
		t.Fatalf("chat targets: got %d, want 3", len(chat))
	}
	// local-a and local-b share tier 0, so both are flagged; gpt-4o is alone in 1.
	for i, want := range []bool{true, true, false} {
		if chat[i].Shared != want {
			t.Errorf("%s Shared: got %v, want %v", chat[i].Model, chat[i].Shared, want)
		}
	}
	for i, want := range []bool{true, false, true} {
		if chat[i].Live != want {
			t.Errorf("%s Live: got %v, want %v", chat[i].Model, chat[i].Live, want)
		}
	}
	if chat[0].CanUp || !chat[0].CanDown {
		t.Error("first target should be demotable only")
	}
	if !chat[2].CanUp || chat[2].CanDown {
		t.Error("last target should be promotable only")
	}

	solo := rows[1].Targets[0]
	if solo.CanUp || solo.CanDown || solo.Shared {
		t.Error("a lone target has nowhere to move and shares its tier with nothing")
	}
}
