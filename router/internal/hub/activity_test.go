package hub

import (
	"log/slog"
	"testing"

	"llmesh/pkg/types"
)

// activityHub builds a hub with the given clients already registered.
func activityHub(t *testing.T, clients ...*Client) *Hub {
	t.Helper()
	h := New(slog.Default())
	h.mu.Lock()
	for _, c := range clients {
		h.clients[c.ID] = c
	}
	h.mu.Unlock()
	return h
}

func worker(id string, maxConcurrent int, models ...string) *Client {
	c := &Client{
		ID:                id,
		Owner:             "alice",
		Name:              id,
		MaxConcurrent:     maxConcurrent,
		Models:            make(map[string]bool),
		ModelContextSizes: make(map[string]int),
		ModelModalities:   make(map[string][]string),
	}
	for _, m := range models {
		c.Models[m] = true
	}
	return c
}

// find returns the entry for model, failing if the model is absent.
func find(t *testing.T, got []ModelActivity, model string) ModelActivity {
	t.Helper()
	for _, a := range got {
		if a.Model == model {
			return a
		}
	}
	t.Fatalf("model %q not reported; got %+v", model, got)
	return ModelActivity{}
}

// A job that has produced no output is still evaluating its prompt; one that has
// is generating. Distinguishing them is the whole point of the phase split, and
// a hub with both must not collapse them into a single "busy".
func TestActivityByModel_SplitsPrefillFromDecoding(t *testing.T) {
	c := worker("c1", 4, "llama")
	h := activityHub(t, c)
	h.TrackJob("c1", types.InferenceRequest{ID: "r1", Model: "llama"})
	h.TrackJob("c1", types.InferenceRequest{ID: "r2", Model: "llama"})
	h.TrackJob("c1", types.InferenceRequest{ID: "r3", Model: "llama"})
	// r2 and r3 have started producing output.
	h.dispatch(c, chunkJSON("r2", "hello", false))
	h.dispatch(c, chunkJSON("r3", "hello", false))

	got := find(t, h.ActivityByModel(), "llama")

	if got.PromptProcessing != 1 {
		t.Errorf("PromptProcessing: got %d, want 1", got.PromptProcessing)
	}
	if got.Decoding != 2 {
		t.Errorf("Decoding: got %d, want 2", got.Decoding)
	}
	if got.Busy() != 3 {
		t.Errorf("Busy: got %d, want 3", got.Busy())
	}
}

// A model nobody is asking for is still being served, and has to be reported as
// such rather than omitted for having no jobs.
func TestActivityByModel_ReportsIdleModels(t *testing.T) {
	h := activityHub(t, worker("c1", 2, "llama"))

	got := find(t, h.ActivityByModel(), "llama")

	if got.Busy() != 0 {
		t.Errorf("Busy: got %d, want 0", got.Busy())
	}
	if got.Clients != 1 || got.TotalSlots != 2 {
		t.Errorf("got Clients=%d TotalSlots=%d, want 1 and 2", got.Clients, got.TotalSlots)
	}
}

// Slots are shared across the models a client serves, not partitioned between
// them, so each model's ceiling is the client's whole concurrency.
func TestActivityByModel_SharedSlotsCountForEveryModel(t *testing.T) {
	h := activityHub(t, worker("c1", 4, "llama", "qwen"))

	got := h.ActivityByModel()

	if len(got) != 2 {
		t.Fatalf("got %d models, want 2: %+v", len(got), got)
	}
	for _, a := range got {
		if a.TotalSlots != 4 {
			t.Errorf("%s TotalSlots: got %d, want 4", a.Model, a.TotalSlots)
		}
	}
}

// Two clients serving the same model add their capacity together.
func TestActivityByModel_SumsAcrossClients(t *testing.T) {
	h := activityHub(t, worker("c1", 2, "llama"), worker("c2", 3, "llama"))
	h.TrackJob("c2", types.InferenceRequest{ID: "r1", Model: "llama"})

	got := find(t, h.ActivityByModel(), "llama")

	if got.Clients != 2 {
		t.Errorf("Clients: got %d, want 2", got.Clients)
	}
	if got.TotalSlots != 5 {
		t.Errorf("TotalSlots: got %d, want 5", got.TotalSlots)
	}
	if got.PromptProcessing != 1 {
		t.Errorf("PromptProcessing: got %d, want 1", got.PromptProcessing)
	}
}

// A connected client that has not sent its register message advertises nothing
// yet, and must not appear as a model with no name.
func TestActivityByModel_SkipsUnregisteredClients(t *testing.T) {
	h := activityHub(t, &Client{ID: "c1", Owner: "alice"}) // Models nil

	if got := h.ActivityByModel(); len(got) != 0 {
		t.Errorf("got %+v, want no models", got)
	}
}

// A job outlives its client between disconnect and lease expiry. The work is
// genuinely in flight, so it is reported, with no client behind it.
func TestActivityByModel_ReportsJobWhoseClientHasGone(t *testing.T) {
	h := activityHub(t, worker("c1", 1, "llama"))
	h.TrackJob("c1", types.InferenceRequest{ID: "r1", Model: "llama"})
	h.mu.Lock()
	delete(h.clients, "c1")
	h.mu.Unlock()

	got := find(t, h.ActivityByModel(), "llama")

	if got.Clients != 0 || got.TotalSlots != 0 {
		t.Errorf("got Clients=%d TotalSlots=%d, want 0 and 0", got.Clients, got.TotalSlots)
	}
	if got.PromptProcessing != 1 {
		t.Errorf("PromptProcessing: got %d, want 1", got.PromptProcessing)
	}
}

// Context size is the largest advertised, and modalities the union, matching how
// ActiveModelInfos already reports a model served by differently configured
// clients. Modalities are sorted so the output is stable.
func TestActivityByModel_MergesCapabilitiesAcrossClients(t *testing.T) {
	c1 := worker("c1", 1, "llama")
	c1.ModelContextSizes["llama"] = 8192
	c1.ModelModalities["llama"] = []string{"text", "vision"}
	c2 := worker("c2", 1, "llama")
	c2.ModelContextSizes["llama"] = 32768
	c2.ModelModalities["llama"] = []string{"audio", "text"}
	h := activityHub(t, c1, c2)

	got := find(t, h.ActivityByModel(), "llama")

	if got.ContextSize != 32768 {
		t.Errorf("ContextSize: got %d, want the largest advertised 32768", got.ContextSize)
	}
	want := []string{"audio", "text", "vision"}
	if len(got.Modalities) != len(want) {
		t.Fatalf("Modalities: got %v, want %v", got.Modalities, want)
	}
	for i, m := range want {
		if got.Modalities[i] != m {
			t.Fatalf("Modalities: got %v, want %v", got.Modalities, want)
		}
	}
}

// Output feeds a public endpoint, so the order must not depend on map iteration.
func TestActivityByModel_SortedByModel(t *testing.T) {
	h := activityHub(t, worker("c1", 1, "qwen", "llama", "mistral"))

	got := h.ActivityByModel()

	for i := 1; i < len(got); i++ {
		if got[i-1].Model > got[i].Model {
			t.Fatalf("not sorted: %+v", got)
		}
	}
}
