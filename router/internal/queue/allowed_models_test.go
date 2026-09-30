package queue

import (
	"testing"

	"llmesh/pkg/types"
)

// A request restricted to some models is never matched to a client that only
// serves others, whether it named the model, an alias, or "any".
func TestAllowedModelsRestrictMatching(t *testing.T) {
	aliases := map[string][]string{"smart": {"gpt-4o", "qwen3"}}
	for _, tc := range []struct {
		name   string
		model  string
		client map[string]bool
		want   bool
	}{
		{"alias, permitted target served", "smart", map[string]bool{"qwen3": true}, true},
		{"alias, only forbidden target served", "smart", map[string]bool{"gpt-4o": true}, false},
		{"any, only forbidden model served", "any", map[string]bool{"gpt-4o": true}, false},
		{"any, permitted model served", "any", map[string]bool{"gpt-4o": true, "qwen3": true}, true},
		{"concrete forbidden model", "gpt-4o", map[string]bool{"gpt-4o": true}, false},
	} {
		req := types.InferenceRequest{Model: tc.model, AllowedModels: []string{"qwen3"}}
		if got := canHandle(req, tc.client, aliases); got != tc.want {
			t.Errorf("%s: canHandle = %v, want %v", tc.name, got, tc.want)
		}
	}
	// nil means unrestricted, as every request was before.
	if !canHandle(types.InferenceRequest{Model: "any"}, map[string]bool{"gpt-4o": true}, nil) {
		t.Error("an unrestricted request was refused")
	}
}
