package hub

import (
	"encoding/json"
	"log/slog"
	"testing"

	"llmesh/pkg/types"
)

// chunkHub returns a hub with one connected client and a capture for the chunks
// it forwards. The client is inserted directly rather than driven through a real
// WebSocket handshake: dispatch only reads ID, Owner, Token, and Name off it.
func chunkHub(t *testing.T) (*Hub, *Client, *[]types.ChunkMsg) {
	t.Helper()
	h := New(slog.Default())
	c := &Client{ID: "c1", Owner: "alice", Name: "mac"}
	h.mu.Lock()
	h.clients[c.ID] = c
	h.mu.Unlock()

	var got []types.ChunkMsg
	h.OnChunk = func(msg types.ChunkMsg) { got = append(got, msg) }
	return h, c, &got
}

// chunkJSON is what a client puts on the wire. Deliberately built as raw JSON,
// not a marshalled ChunkMsg, to pin down that Model is not something a client
// can send: it must be filled in on this side.
func chunkJSON(requestID, delta string, done bool) []byte {
	b, _ := json.Marshal(map[string]any{
		"type":       "chunk",
		"request_id": requestID,
		"delta":      delta,
		"done":       done,
		"model":      "spoofed-by-client",
	})
	return b
}

// The scheduler rewrites Model from the caller's alias to a concrete name at
// dispatch, and the HTTP handler never sees that rewrite — it holds its own copy
// of the request. Stamping the chunk is the only thing that tells the handler
// which model actually ran, so stats and usage stop being attributed to a guess.
func TestDispatchChunk_StampsResolvedModel(t *testing.T) {
	h, c, got := chunkHub(t)
	// Dispatched as alias "chat", resolved to a second-tier model under load.
	h.TrackJob(c.ID, types.InferenceRequest{ID: "r1", Model: "gpt-4o", RequestedModel: "chat"})

	h.dispatch(c, chunkJSON("r1", "hello", false))
	h.dispatch(c, chunkJSON("r1", "", true))

	if len(*got) != 2 {
		t.Fatalf("got %d chunks, want 2", len(*got))
	}
	for i, msg := range *got {
		if msg.Model != "gpt-4o" {
			t.Errorf("chunk %d: Model got %q, want the resolved gpt-4o", i, msg.Model)
		}
	}
}

// The done chunk is stamped too. It is the one the handler accounts on, so a
// lookup that happened after untracking would leave usage unattributed.
func TestDispatchChunk_StampsDoneChunkBeforeUntracking(t *testing.T) {
	h, c, got := chunkHub(t)
	h.TrackJob(c.ID, types.InferenceRequest{ID: "r1", Model: "local-llama", RequestedModel: "any"})

	h.dispatch(c, chunkJSON("r1", "", true))

	if len(*got) != 1 {
		t.Fatalf("got %d chunks, want 1", len(*got))
	}
	if (*got)[0].Model != "local-llama" {
		t.Errorf("Model: got %q, want local-llama", (*got)[0].Model)
	}
}

// Model is router-internal. A client that sets it on the wire must not be able
// to name the model its tokens are billed to.
func TestDispatchChunk_IgnoresClientSuppliedModel(t *testing.T) {
	h, c, got := chunkHub(t)
	h.TrackJob(c.ID, types.InferenceRequest{ID: "r1", Model: "local-llama"})

	h.dispatch(c, chunkJSON("r1", "hi", false))

	if (*got)[0].Model != "local-llama" {
		t.Errorf("Model: got %q, want the tracked local-llama, not the client's value", (*got)[0].Model)
	}
}

// A chunk from a client that no longer holds the job is stale — the request was
// re-dispatched elsewhere. Stamping it would relabel the live attempt with a
// model the superseded one was running.
func TestDispatchChunk_DoesNotStampForSupersededClient(t *testing.T) {
	h, _, got := chunkHub(t)
	stale := &Client{ID: "c2", Owner: "alice"}
	h.mu.Lock()
	h.clients[stale.ID] = stale
	h.mu.Unlock()
	// c1 holds r1; c2 is a previous attempt still talking.
	h.TrackJob("c1", types.InferenceRequest{ID: "r1", Model: "local-llama"})

	h.dispatch(stale, chunkJSON("r1", "hi", false))

	if (*got)[0].Model != "" {
		t.Errorf("Model: got %q, want empty so the handler falls back rather than trusting a stale attempt", (*got)[0].Model)
	}
}

// Terminal chunks the router synthesises for itself (shutdown drains, error
// paths) have no job record left. They must pass through rather than panic.
func TestDispatchChunk_UntrackedRequestLeavesModelEmpty(t *testing.T) {
	h, c, got := chunkHub(t)

	h.dispatch(c, chunkJSON("unknown", "", true))

	if len(*got) != 1 {
		t.Fatalf("got %d chunks, want 1", len(*got))
	}
	if (*got)[0].Model != "" {
		t.Errorf("Model: got %q, want empty", (*got)[0].Model)
	}
}
