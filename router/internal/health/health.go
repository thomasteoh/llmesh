// router/internal/health/health.go
// The public /health document. Unauthenticated, like /metrics, so it reports
// models and fleet-wide activity and nothing that identifies a client, an
// owner, or an API key.
package health

import (
	"encoding/json"
	"math"
	"net/http"
	"time"

	"llmesh/router/internal/hub"
	"llmesh/router/internal/latency"
	"llmesh/router/internal/stats"
)

// UpstreamStatus is one configured upstream router and whether we hold a
// connection to it.
type UpstreamStatus struct {
	URL       string `json:"url"`
	Name      string `json:"name,omitempty"`
	Connected bool   `json:"connected"`
}

// Response is the /health document. The first six fields are the original
// shape and are unchanged; models is additive.
type Response struct {
	Status     string           `json:"status"`
	Version    string           `json:"version"`
	Clients    int              `json:"clients"`
	QueueDepth int              `json:"queue_depth"`
	ActiveJobs int              `json:"active_jobs"`
	Upstreams  []UpstreamStatus `json:"upstreams"`
	Models     []ModelHealth    `json:"models"`
}

// Model states, in precedence order: a model with any job generating reads as
// decoding even if others are still on their prompts.
const (
	stateOffline    = "offline"           // no client advertises it; only seen while a job outlives its client
	stateIdle       = "idle"              // online with nothing in flight
	stateProcessing = "prompt_processing" // in flight, none producing output yet
	stateDecoding   = "decoding"          // at least one job generating
)

type ModelHealth struct {
	Model         string   `json:"model"`
	State         string   `json:"state"`
	Clients       int      `json:"clients"`
	Slots         Slots    `json:"slots"`
	Activity      Activity `json:"activity"`
	ContextWindow int      `json:"context_window,omitempty"`
	Modalities    []string `json:"modalities,omitempty"`
	Totals        Totals   `json:"totals"`
	Recent        Recent   `json:"recent"`
}

// Slots is capacity. Free can read lower than Total minus Busy when a
// client serves several models, since they share one pool of slots.
type Slots struct {
	Total int `json:"total"`
	Busy  int `json:"busy"`
	Free  int `json:"free"`
}

type Activity struct {
	PromptProcessing int `json:"prompt_processing"`
	Decoding         int `json:"decoding"`
}

// Totals is cumulative since the router started.
type Totals struct {
	Requests         int64 `json:"requests"`
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
}

// Recent covers the rolling window only. The percentile blocks are
// omitted rather than zeroed when nothing was observed, because a p50 of 0 for
// TTFT reads as "instant" when it means "not measured": batch requests never
// observe TTFT, and throughput needs a backend that reports timings.
type Recent struct {
	WindowSeconds      int          `json:"window_seconds"`
	Requests           int          `json:"requests"`
	PromptTokens       int64        `json:"prompt_tokens"`
	CompletionTokens   int64        `json:"completion_tokens"`
	PromptTokensPerSec *Percentiles `json:"prompt_tokens_per_sec,omitempty"`
	GenTokensPerSec    *Percentiles `json:"gen_tokens_per_sec,omitempty"`
	TTFTMS             *Percentiles `json:"ttft_ms,omitempty"`
	QueueMS            *Percentiles `json:"queue_ms,omitempty"`
	DurationMS         *Percentiles `json:"duration_ms,omitempty"`
}

type Percentiles struct {
	P50 float64 `json:"p50"`
	P95 float64 `json:"p95"`
	P99 float64 `json:"p99"`
}

// inputs is everything build needs, already snapshotted. Taking
// values rather than the live components keeps the assembly testable without a
// hub, a queue, or a recorder.
type inputs struct {
	version    string
	clients    int
	queueDepth int
	activeJobs int
	upstreams  []UpstreamStatus
	activity   []hub.ModelActivity
	window     time.Duration
	recent     map[string]latency.ModelSnapshot
	totals     map[string]stats.Summary
}

// build assembles the document. The model list is driven by what the hub
// is serving, not by what has statistics: a name that only appears in the
// counters is either a model whose clients have all gone or an alias recorded
// when no worker reported one, and neither is something /health should present
// as a served model.
func build(in inputs) Response {
	models := make([]ModelHealth, 0, len(in.activity))
	for _, a := range in.activity {
		m := ModelHealth{
			Model:         a.Model,
			State:         modelState(a),
			Clients:       a.Clients,
			Slots:         Slots{Total: a.TotalSlots, Busy: a.Busy(), Free: max(0, a.TotalSlots-a.Busy())},
			Activity:      Activity{PromptProcessing: a.PromptProcessing, Decoding: a.Decoding},
			ContextWindow: a.ContextSize,
			Modalities:    a.Modalities,
			Recent:        Recent{WindowSeconds: int(in.window.Seconds())},
		}
		if t, ok := in.totals[a.Model]; ok {
			m.Totals = Totals{
				Requests:         t.Requests,
				PromptTokens:     t.PromptTokens,
				CompletionTokens: t.CompletionTokens,
			}
		}
		if s, ok := in.recent[a.Model]; ok {
			// Requests comes from the token histogram rather than duration:
			// both observe once per completed request, but tokens are recorded
			// for every one of them whether or not its timings were usable.
			m.Recent.Requests = s.PromptTokens.Count
			m.Recent.PromptTokens = int64(s.PromptTokens.Sum)
			m.Recent.CompletionTokens = int64(s.GenTokens.Sum)
			m.Recent.PromptTokensPerSec = rate(s.PromptTPS)
			m.Recent.GenTokensPerSec = rate(s.GenTPS)
			m.Recent.TTFTMS = millis(s.TTFT)
			m.Recent.QueueMS = millis(s.QueueWait)
			m.Recent.DurationMS = millis(s.Duration)
		}
		models = append(models, m)
	}
	// Both lists are always lists. A consumer that indexes into them should not
	// have to special-case a null for "none configured" or "none connected".
	upstreams := in.upstreams
	if upstreams == nil {
		upstreams = []UpstreamStatus{}
	}
	return Response{
		Status:     "ok",
		Version:    in.version,
		Clients:    in.clients,
		QueueDepth: in.queueDepth,
		ActiveJobs: in.activeJobs,
		Upstreams:  upstreams,
		Models:     models,
	}
}

func modelState(a hub.ModelActivity) string {
	switch {
	case a.Decoding > 0:
		return stateDecoding
	case a.PromptProcessing > 0:
		return stateProcessing
	case a.Clients == 0:
		return stateOffline
	default:
		return stateIdle
	}
}

// rate renders a throughput snapshot, which is already in tokens per second.
func rate(s latency.Snapshot) *Percentiles {
	if s.Count == 0 {
		return nil
	}
	return &Percentiles{P50: round2(s.P50), P95: round2(s.P95), P99: round2(s.P99)}
}

// millis renders a duration snapshot, which the recorder holds in seconds.
func millis(s latency.Snapshot) *Percentiles {
	if s.Count == 0 {
		return nil
	}
	return &Percentiles{P50: round2(s.P50 * 1000), P95: round2(s.P95 * 1000), P99: round2(s.P99 * 1000)}
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// Handler serves the document. upstreams is a callback because upstream
// state lives behind the admin state and the connector, neither of which this
// file should reach into.
func Handler(
	version string,
	h *hub.Hub,
	queueDepth func() int,
	reqStats *stats.Stats,
	upstreams func() []UpstreamStatus,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		in := inputs{
			version:    version,
			clients:    h.ActiveClientCount(),
			queueDepth: queueDepth(),
			activeJobs: len(h.AllInFlightJobs()),
			upstreams:  upstreams(),
			activity:   h.ActivityByModel(),
			totals:     make(map[string]stats.Summary),
		}
		if h.Latency != nil {
			in.window = h.Latency.Window()
			in.recent = h.Latency.SnapshotByModel()
		}
		if reqStats != nil {
			for _, row := range reqStats.ByModel() {
				in.totals[row.Name] = row.Summary
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(build(in))
	}
}
