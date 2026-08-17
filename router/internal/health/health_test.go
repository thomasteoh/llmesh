package health

import (
	"encoding/json"
	"testing"
	"time"

	"llmesh/router/internal/hub"
	"llmesh/router/internal/latency"
	"llmesh/router/internal/stats"
)

func baseInputs() inputs {
	return inputs{
		version: "1.2.3",
		window:  10 * time.Minute,
		recent:  map[string]latency.ModelSnapshot{},
		totals:  map[string]stats.Summary{},
	}
}

// only returns the single model in the document, failing if there is not exactly one.
func only(t *testing.T, got Response) ModelHealth {
	t.Helper()
	if len(got.Models) != 1 {
		t.Fatalf("got %d models, want 1: %+v", len(got.Models), got.Models)
	}
	return got.Models[0]
}

// The state is what a dashboard reads at a glance, so each phase has to win over
// the ones below it: a model with one job decoding and another still on its
// prompt is decoding.
func TestBuild_State(t *testing.T) {
	for _, tc := range []struct {
		name     string
		activity hub.ModelActivity
		want     string
	}{
		{"idle when online with no work", hub.ModelActivity{Clients: 1}, stateIdle},
		{"processing before any output", hub.ModelActivity{Clients: 1, PromptProcessing: 2}, stateProcessing},
		{"decoding once output starts", hub.ModelActivity{Clients: 1, Decoding: 1}, stateDecoding},
		{"decoding wins over processing", hub.ModelActivity{Clients: 1, PromptProcessing: 3, Decoding: 1}, stateDecoding},
		{"offline with no clients", hub.ModelActivity{}, stateOffline},
		{"work outliving its client still counts", hub.ModelActivity{Decoding: 1}, stateDecoding},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := baseInputs()
			tc.activity.Model = "llama"
			in.activity = []hub.ModelActivity{tc.activity}

			if got := only(t, build(in)).State; got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBuild_SlotsAndActivity(t *testing.T) {
	in := baseInputs()
	in.activity = []hub.ModelActivity{{
		Model: "llama", Clients: 2, TotalSlots: 6, PromptProcessing: 1, Decoding: 2,
		ContextSize: 32768, Modalities: []string{"text", "vision"},
	}}

	got := only(t, build(in))

	if got.Slots != (Slots{Total: 6, Busy: 3, Free: 3}) {
		t.Errorf("Slots: got %+v, want {6 3 3}", got.Slots)
	}
	if got.Activity != (Activity{PromptProcessing: 1, Decoding: 2}) {
		t.Errorf("Activity: got %+v", got.Activity)
	}
	if got.ContextWindow != 32768 {
		t.Errorf("ContextWindow: got %d", got.ContextWindow)
	}
}

// Slots are shared across a client's models, so a model can be busier than the
// capacity attributed to it. Free must not go negative.
func TestBuild_FreeSlotsNeverNegative(t *testing.T) {
	in := baseInputs()
	in.activity = []hub.ModelActivity{{Model: "llama", Clients: 1, TotalSlots: 1, Decoding: 3}}

	if got := only(t, build(in)).Slots.Free; got != 0 {
		t.Errorf("Free: got %d, want 0", got)
	}
}

// A model that has been served but never measured must not report a p50 of zero:
// read as milliseconds that means instant, when it means nothing was observed.
func TestBuild_OmitsPercentilesWithNoObservations(t *testing.T) {
	in := baseInputs()
	in.activity = []hub.ModelActivity{{Model: "llama", Clients: 1}}
	in.recent["llama"] = latency.ModelSnapshot{
		PromptTokens: latency.Snapshot{Count: 3, Sum: 300},
		GenTokens:    latency.Snapshot{Count: 3, Sum: 90},
	}

	got := only(t, build(in))

	if got.Recent.TTFTMS != nil || got.Recent.GenTokensPerSec != nil {
		t.Errorf("unobserved stages should be omitted, got %+v", got.Recent)
	}
	if got.Recent.Requests != 3 {
		t.Errorf("Requests: got %d, want 3", got.Recent.Requests)
	}
	if got.Recent.PromptTokens != 300 || got.Recent.CompletionTokens != 90 {
		t.Errorf("tokens: got %d/%d, want 300/90", got.Recent.PromptTokens, got.Recent.CompletionTokens)
	}
}

// The recorder holds durations in seconds; the document reports milliseconds,
// matching every other latency figure the router publishes.
func TestBuild_ConvertsDurationsToMilliseconds(t *testing.T) {
	in := baseInputs()
	in.activity = []hub.ModelActivity{{Model: "llama", Clients: 1}}
	in.recent["llama"] = latency.ModelSnapshot{
		TTFT:      latency.Snapshot{Count: 1, P50: 0.42, P95: 1.8, P99: 2.0},
		GenTPS:    latency.Snapshot{Count: 1, P50: 41.257, P95: 55.5, P99: 60},
		QueueWait: latency.Snapshot{Count: 1, P50: 0.001},
	}

	got := only(t, build(in)).Recent

	if got.TTFTMS == nil || got.TTFTMS.P50 != 420 || got.TTFTMS.P95 != 1800 {
		t.Errorf("TTFTMS: got %+v, want p50 420 p95 1800", got.TTFTMS)
	}
	if got.QueueMS == nil || got.QueueMS.P50 != 1 {
		t.Errorf("QueueMS: got %+v, want p50 1", got.QueueMS)
	}
	// Throughput is already per-second and only gets rounded.
	if got.GenTokensPerSec == nil || got.GenTokensPerSec.P50 != 41.26 {
		t.Errorf("GenTokensPerSec: got %+v, want p50 41.26", got.GenTokensPerSec)
	}
}

func TestBuild_CumulativeTotals(t *testing.T) {
	in := baseInputs()
	in.activity = []hub.ModelActivity{{Model: "llama", Clients: 1}}
	in.totals["llama"] = stats.Summary{Requests: 1201, PromptTokens: 50000, CompletionTokens: 9000}

	got := only(t, build(in)).Totals

	if got != (Totals{Requests: 1201, PromptTokens: 50000, CompletionTokens: 9000}) {
		t.Errorf("got %+v", got)
	}
}

// Counters can hold names the fleet is not serving: a model whose clients have
// all disconnected, or an alias recorded when no worker reported a model.
// /health describes what is being served, so those must not appear.
func TestBuild_ListsOnlyServedModels(t *testing.T) {
	in := baseInputs()
	in.activity = []hub.ModelActivity{{Model: "llama", Clients: 1}}
	in.totals["chat"] = stats.Summary{Requests: 5}
	in.totals["retired-model"] = stats.Summary{Requests: 99}
	in.recent["chat"] = latency.ModelSnapshot{PromptTokens: latency.Snapshot{Count: 5}}

	got := build(in)

	if len(got.Models) != 1 || got.Models[0].Model != "llama" {
		t.Errorf("got %+v, want only llama", got.Models)
	}
}

// The six original keys are what existing consumers read, so they have to keep
// their names and their meanings. models is purely additive.
func TestBuild_PreservesTheOriginalDocument(t *testing.T) {
	in := baseInputs()
	in.clients, in.queueDepth, in.activeJobs = 3, 7, 2
	in.upstreams = []UpstreamStatus{{URL: "https://up", Name: "up", Connected: true}}

	raw, err := json.Marshal(build(in))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	for key, want := range map[string]any{
		"status": "ok", "version": "1.2.3",
		"clients": float64(3), "queue_depth": float64(7), "active_jobs": float64(2),
	} {
		if got[key] != want {
			t.Errorf("%s: got %v, want %v", key, got[key], want)
		}
	}
	if _, ok := got["upstreams"]; !ok {
		t.Error("upstreams missing")
	}
	if _, ok := got["models"]; !ok {
		t.Error("models missing")
	}
}

// Nothing in the document may identify a client, an owner, or a key: it is
// served without authentication.
func TestBuild_CarriesNothingIdentifying(t *testing.T) {
	in := baseInputs()
	in.activity = []hub.ModelActivity{{Model: "llama", Clients: 1, TotalSlots: 2, Decoding: 1}}
	in.totals["llama"] = stats.Summary{Requests: 1}

	raw, err := json.Marshal(build(in))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	for _, forbidden := range []string{"owner", "key_label", "client_name", "api_key", "token", "cost"} {
		if containsKey(raw, forbidden) {
			t.Errorf("document exposes %q: %s", forbidden, raw)
		}
	}
}

func containsKey(doc []byte, key string) bool {
	var walk func(any) bool
	walk = func(v any) bool {
		switch t := v.(type) {
		case map[string]any:
			for k, child := range t {
				if k == key || walk(child) {
					return true
				}
			}
		case []any:
			for _, child := range t {
				if walk(child) {
					return true
				}
			}
		}
		return false
	}
	var parsed any
	if err := json.Unmarshal(doc, &parsed); err != nil {
		return false
	}
	return walk(parsed)
}
