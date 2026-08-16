package api

import (
	"testing"

	"llmesh/pkg/types"
)

type fakeAliases map[string][]string

func (f fakeAliases) AliasMap() map[string][]string { return f }

type recordedStat struct {
	model, user        string
	prompt, completion int
}

type fakeStats struct{ got []recordedStat }

func (f *fakeStats) Record(model, user string, prompt, completion int) {
	f.got = append(f.got, recordedStat{model, user, prompt, completion})
}

type recordedUsage struct {
	model, owner, keyLabel string
}

type fakeUsage struct{ got []recordedUsage }

func (f *fakeUsage) RecordUsage(model, owner, keyLabel string, prompt, completion int) {
	f.got = append(f.got, recordedUsage{model, owner, keyLabel})
}

// aliasHandler wires a handler with a two-target alias: "chat" prefers the free
// local model and falls back to a paid API, which is the shape that made the
// old first-target guess expensive to get wrong.
func aliasHandler() (*Handler, *fakeStats, *fakeUsage) {
	stats := &fakeStats{}
	usage := &fakeUsage{}
	h := &Handler{
		Aliases: fakeAliases{"chat": {"local-llama", "gpt-4o"}},
		Stats:   stats,
		Usage:   usage,
	}
	return h, stats, usage
}

func aliasReq() *types.InferenceRequest {
	return &types.InferenceRequest{ID: "r1", Model: "chat", Owner: "alice", APIKeyLabel: "alice/prod"}
}

// The scheduler dispatches by tier and availability, so an alias request that
// spilled over to the second target must be billed to that target. Recording the
// head of the alias list instead charged paid-API tokens to the free local model.
func TestRecordStats_AttributesToTheModelThatServed(t *testing.T) {
	h, stats, usage := aliasHandler()

	h.recordStats(aliasReq(), &types.UsageInfo{PromptTokens: 10, CompletionTokens: 20}, "gpt-4o")

	if len(stats.got) != 1 || stats.got[0].model != "gpt-4o" {
		t.Errorf("stats: got %+v, want model gpt-4o", stats.got)
	}
	if len(usage.got) != 1 || usage.got[0].model != "gpt-4o" {
		t.Errorf("usage: got %+v, want model gpt-4o", usage.got)
	}
}

// "any" resolves to whatever a client happens to advertise. It is not in the
// alias table, so the old path recorded the literal string "any" and the usage
// rows could not be joined against the perf rows the hub wrote.
func TestRecordStats_ResolvesAnyToTheConcreteModel(t *testing.T) {
	h, stats, _ := aliasHandler()
	req := aliasReq()
	req.Model = "any"

	h.recordStats(req, &types.UsageInfo{PromptTokens: 1, CompletionTokens: 2}, "local-llama")

	if len(stats.got) != 1 || stats.got[0].model != "local-llama" {
		t.Errorf("stats: got %+v, want model local-llama", stats.got)
	}
}

// No chunk carried a model — a timeout or a shutdown drain. Accounting still has
// to land somewhere, so it keeps the old first-target guess rather than dropping
// the tokens or filing them under an alias no model answers to.
func TestRecordStats_FallsBackToFirstAliasTargetWhenUnknown(t *testing.T) {
	h, stats, _ := aliasHandler()

	h.recordStats(aliasReq(), &types.UsageInfo{PromptTokens: 1, CompletionTokens: 2}, "")

	if len(stats.got) != 1 || stats.got[0].model != "local-llama" {
		t.Errorf("stats: got %+v, want the local-llama fallback", stats.got)
	}
}

// A concrete request is unaffected either way, but the served name still wins:
// it is the one the hub observed.
func TestAttributedModel_ConcreteRequestPassesThrough(t *testing.T) {
	h, _, _ := aliasHandler()
	req := aliasReq()
	req.Model = "local-llama"

	if got := h.attributedModel(req, ""); got != "local-llama" {
		t.Errorf("without a served name: got %q, want local-llama", got)
	}
	if got := h.attributedModel(req, "local-llama"); got != "local-llama" {
		t.Errorf("with a served name: got %q, want local-llama", got)
	}
}

// Owner and key label ride along unchanged — only the model attribution moved.
func TestRecordStats_PreservesOwnerAndKeyLabel(t *testing.T) {
	h, stats, usage := aliasHandler()

	h.recordStats(aliasReq(), &types.UsageInfo{PromptTokens: 10, CompletionTokens: 20}, "gpt-4o")

	if stats.got[0].user != "alice" || stats.got[0].prompt != 10 || stats.got[0].completion != 20 {
		t.Errorf("stats: got %+v", stats.got[0])
	}
	if usage.got[0].owner != "alice" || usage.got[0].keyLabel != "alice/prod" {
		t.Errorf("usage: got %+v", usage.got[0])
	}
}

// Nothing to account for, and nothing should be invented.
func TestRecordStats_NilUsageRecordsNothing(t *testing.T) {
	h, stats, usage := aliasHandler()

	h.recordStats(aliasReq(), nil, "gpt-4o")

	if len(stats.got) != 0 || len(usage.got) != 0 {
		t.Errorf("recorded despite nil usage: stats=%+v usage=%+v", stats.got, usage.got)
	}
}
