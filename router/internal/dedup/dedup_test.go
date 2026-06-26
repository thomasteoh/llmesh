package dedup

import (
	"encoding/json"
	"testing"

	"llmesh/pkg/types"
)

func TestContentHash_NormalizeCollapsesKeyOrderAndWhitespace(t *testing.T) {
	a := &types.InferenceRequest{
		Model: "llama3",
		Messages: []types.Message{
			{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"hi"}]`)},
		},
	}
	b := &types.InferenceRequest{
		Model: "llama3",
		Messages: []types.Message{
			{Role: "user", Content: json.RawMessage(`[ { "text" : "hi" , "type" : "text" } ]`)},
		},
	}

	if ContentHash(a) == ContentHash(b) {
		t.Fatal("unnormalised hashes unexpectedly equal; test would not prove anything")
	}
	if ContentHashOpts(a, true) != ContentHashOpts(b, true) {
		t.Error("normalised hashes should match across key order / whitespace differences")
	}
}

func TestContentHash_NormalizeTrimsStringContent(t *testing.T) {
	a := &types.InferenceRequest{
		Model:    "llama3",
		Messages: []types.Message{{Role: "user", Content: json.RawMessage(`"hello"`)}},
	}
	b := &types.InferenceRequest{
		Model:    "llama3",
		Messages: []types.Message{{Role: "user", Content: json.RawMessage(`"  hello  "`)}},
	}
	if ContentHashOpts(a, true) != ContentHashOpts(b, true) {
		t.Error("normalised hashes should match after trimming string content")
	}
}

func TestContentHash_DistinctRequestsDiffer(t *testing.T) {
	a := &types.InferenceRequest{Model: "llama3", Messages: []types.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}}}
	b := &types.InferenceRequest{Model: "llama3", Messages: []types.Message{{Role: "user", Content: json.RawMessage(`"bye"`)}}}
	if ContentHashOpts(a, true) == ContentHashOpts(b, true) {
		t.Error("different content must not collide under normalisation")
	}
}
