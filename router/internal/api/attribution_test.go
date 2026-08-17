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

// No chunk carried a model, so nothing observed which one ran. Naming one of the
// alias targets anyway would be a guess, and a guess on a path this rare is one
// nobody would catch: the tokens are recorded under the alias, where a zero
// pricing match puts them in unpriced_requests instead of inflating a model.
func TestRecordStats_DoesNotGuessAModelWhenNoneWasReported(t *testing.T) {
	h, stats, usage := aliasHandler()

	h.recordStats(aliasReq(), &types.UsageInfo{PromptTokens: 1, CompletionTokens: 2}, "")

	if len(stats.got) != 1 || stats.got[0].model != "chat" {
		t.Errorf("stats: got %+v, want the alias chat rather than a guessed target", stats.got)
	}
	if len(usage.got) != 1 || usage.got[0].model != "chat" {
		t.Errorf("usage: got %+v, want the alias chat rather than a guessed target", usage.got)
	}
}

// What the hub reported wins when there is one; the caller's own name stands in
// when there is not, whether that name is an alias or already concrete.
func TestAttributedModel(t *testing.T) {
	for _, tc := range []struct {
		name, reqModel, served, want string
	}{
		{"served name wins over the alias", "chat", "gpt-4o", "gpt-4o"},
		{"alias stands for itself when unreported", "chat", "", "chat"},
		{"concrete request unaffected", "local-llama", "local-llama", "local-llama"},
		{"concrete request unreported", "local-llama", "", "local-llama"},
		{"any stands for itself when unreported", "any", "", "any"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := aliasReq()
			req.Model = tc.reqModel
			if got := attributedModel(req, tc.served); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
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
