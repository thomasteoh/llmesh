package hub

import (
	"log/slog"
	"math"
	"strings"
	"testing"
	"time"

	"llmesh/pkg/types"
	"llmesh/router/internal/latency"
)

// capturePerf collects the samples a hub records, for assertion.
type capturePerf struct{ got []PerfSample }

func (c *capturePerf) RecordPerf(s PerfSample) { c.got = append(c.got, s) }

// finishedJob builds an in-flight record as it would look at completion, with
// enqueue, dispatch, and (optionally) first-token timestamps at fixed offsets so
// the derived intervals are exact.
//
// enqueue → dispatch is 100ms (queue wait), dispatch → first token is 400ms
// (prefill), first token → done is 2000ms (decode). Total end-to-end is 2500ms.
func finishedJob(stream bool, sawFirstToken bool) (InFlightRecord, time.Time) {
	enqueued := time.Now().Add(-2500 * time.Millisecond)
	dispatched := enqueued.Add(100 * time.Millisecond)
	doneAt := enqueued.Add(2500 * time.Millisecond)

	rec := InFlightRecord{
		ClientID:    "c1",
		ClientOwner: "alice",
		ClientName:  "mac",
		Req: types.InferenceRequest{
			ID: "r1", Model: "llama", Owner: "alice", APIKeyLabel: "alice/prod",
			Stream: stream, EnqueuedAt: enqueued,
		},
		DispatchedAt: dispatched,
		live:         &jobLiveStats{},
	}
	if sawFirstToken {
		first := dispatched.Add(400 * time.Millisecond)
		rec.live.firstChunkAt.Store(&first)
	}
	return rec, doneAt
}

func closeTo(t *testing.T, label string, got, want float64) {
	t.Helper()
	// Timestamps are built from a real clock reading, so allow a small tolerance.
	if math.Abs(got-want) > 5 {
		t.Fatalf("%s: got %v, want ~%v", label, got, want)
	}
}

func TestRecordPerf_AttributesOwnerModelAndClient(t *testing.T) {
	cap := &capturePerf{}
	h := New(slog.Default())
	h.Perf = cap

	rec, doneAt := finishedJob(true, true)
	h.recordPerf(rec, &types.UsageInfo{PromptTokens: 800, CompletionTokens: 100}, doneAt)

	if len(cap.got) != 1 {
		t.Fatalf("want 1 sample, got %d", len(cap.got))
	}
	s := cap.got[0]
	if s.Owner != "alice" || s.KeyLabel != "alice/prod" || s.Model != "llama" {
		t.Fatalf("attribution: %+v", s)
	}
	// The client is identified the same way the portal labels machines.
	if s.Client != "alice/mac" {
		t.Fatalf("client label: got %q, want %q", s.Client, "alice/mac")
	}
	closeTo(t, "queue wait", s.QueueMS, 100)
	closeTo(t, "end-to-end", s.TotalMS, 2500)
}

func TestRecordPerf_StreamingFallbackSplitsPrefillAndDecode(t *testing.T) {
	cap := &capturePerf{}
	h := New(slog.Default())
	h.Perf = cap

	rec, doneAt := finishedJob(true, true)
	// No backend timings, so the router falls back to its own observations.
	h.recordPerf(rec, &types.UsageInfo{PromptTokens: 800, CompletionTokens: 100}, doneAt)

	s := cap.got[0]
	if s.FromBackend {
		t.Fatal("sample claims backend-reported timings when none were sent")
	}
	// TTFT runs from dispatch, so the 100ms queue wait is excluded — it is
	// reported separately and would otherwise be counted twice.
	closeTo(t, "ttft", s.TTFTMS, 400)
	closeTo(t, "prefill window", s.PrefillMS, 400)
	if s.PrefillTokens != 800 {
		t.Fatalf("prefill tokens: got %d, want 800", s.PrefillTokens)
	}
	closeTo(t, "decode window", s.DecodeMS, 2000)
	if s.DecodeTokens != 100 {
		t.Fatalf("decode tokens: got %d, want 100", s.DecodeTokens)
	}
}

func TestRecordPerf_FallbackExcludesCachedPromptTokens(t *testing.T) {
	cap := &capturePerf{}
	h := New(slog.Default())
	h.Perf = cap

	rec, doneAt := finishedJob(true, true)
	// 750 of the 800 prompt tokens came from cache and were never evaluated.
	// Charging them to the 400ms prefill window would report ~2000 tok/s instead
	// of the ~125 tok/s actually achieved.
	h.recordPerf(rec, &types.UsageInfo{
		PromptTokens: 800, CacheReadTokens: 750, CompletionTokens: 100,
	}, doneAt)

	s := cap.got[0]
	if s.PrefillTokens != 50 {
		t.Fatalf("prefill tokens: got %d, want 50 (800 total less 750 cached)", s.PrefillTokens)
	}
}

func TestRecordPerf_FullCacheHitRecordsNoPrefillRate(t *testing.T) {
	cap := &capturePerf{}
	h := New(slog.Default())
	h.Perf = cap

	rec, doneAt := finishedJob(true, true)
	// Every prompt token was cached, so there is no prefill throughput to report.
	h.recordPerf(rec, &types.UsageInfo{
		PromptTokens: 800, CacheReadTokens: 800, CompletionTokens: 100,
	}, doneAt)

	s := cap.got[0]
	if s.PrefillTokens != 0 || s.PrefillMS != 0 {
		t.Fatalf("full cache hit produced a prefill measurement: %d tokens over %vms",
			s.PrefillTokens, s.PrefillMS)
	}
	// Decode is unaffected.
	if s.DecodeTokens != 100 {
		t.Fatalf("decode tokens: got %d, want 100", s.DecodeTokens)
	}
}

func TestRecordPerf_BackendTimingsPreferredOverObservation(t *testing.T) {
	cap := &capturePerf{}
	h := New(slog.Default())
	h.Perf = cap

	rec, doneAt := finishedJob(true, true)
	// The backend's own numbers differ from what the router observed (they exclude
	// queueing and network transit) and must win.
	h.recordPerf(rec, &types.UsageInfo{
		PromptTokens: 800, CompletionTokens: 100,
		Timings: &types.Timings{
			PromptN: 780, PromptMS: 250, PredictedN: 100, PredictedMS: 1800,
		},
	}, doneAt)

	s := cap.got[0]
	if !s.FromBackend {
		t.Fatal("sample does not record that timings came from the backend")
	}
	closeTo(t, "prefill window", s.PrefillMS, 250)
	if s.PrefillTokens != 780 {
		t.Fatalf("prefill tokens: got %d, want the backend's 780", s.PrefillTokens)
	}
	closeTo(t, "decode window", s.DecodeMS, 1800)
	if s.DecodeTokens != 100 {
		t.Fatalf("decode tokens: got %d, want 100", s.DecodeTokens)
	}
	// TTFT stays a router-side observation; the backend does not report it.
	closeTo(t, "ttft", s.TTFTMS, 400)
}

func TestRecordPerf_BatchRequestRecordsNoTTFT(t *testing.T) {
	cap := &capturePerf{}
	h := New(slog.Default())
	h.Perf = cap

	// A non-streaming response arrives as a single chunk carrying the whole body,
	// which sets firstChunkAt at what is really the completion moment. Treating
	// that as a first token would report a TTFT equal to the total duration.
	rec, doneAt := finishedJob(false, true)
	h.recordPerf(rec, &types.UsageInfo{PromptTokens: 800, CompletionTokens: 100}, doneAt)

	s := cap.got[0]
	if s.TTFTMS != 0 {
		t.Fatalf("batch request reported a TTFT of %vms", s.TTFTMS)
	}
	// Nor can prefill and decode be separated without backend help.
	if s.PrefillMS != 0 || s.DecodeMS != 0 {
		t.Fatalf("batch request produced a prefill/decode split: %vms / %vms", s.PrefillMS, s.DecodeMS)
	}
	// Queue wait and end-to-end duration are still measurable.
	closeTo(t, "queue wait", s.QueueMS, 100)
	closeTo(t, "end-to-end", s.TotalMS, 2500)
}

func TestRecordPerf_BatchRequestUsesBackendTimings(t *testing.T) {
	cap := &capturePerf{}
	h := New(slog.Default())
	h.Perf = cap

	rec, doneAt := finishedJob(false, true)
	h.recordPerf(rec, &types.UsageInfo{
		PromptTokens: 800, CompletionTokens: 100,
		Timings: &types.Timings{PromptN: 800, PromptMS: 300, PredictedN: 100, PredictedMS: 1900},
	}, doneAt)

	s := cap.got[0]
	// Backend timings are the only way a batch request contributes throughput.
	closeTo(t, "prefill window", s.PrefillMS, 300)
	closeTo(t, "decode window", s.DecodeMS, 1900)
	if s.TTFTMS != 0 {
		t.Fatalf("batch request reported a TTFT of %vms", s.TTFTMS)
	}
}

func TestRecordPerf_NoUsageStillRecordsDurations(t *testing.T) {
	cap := &capturePerf{}
	h := New(slog.Default())
	h.Perf = cap

	rec, doneAt := finishedJob(true, true)
	// A backend that reports no usage at all still yields timing data.
	h.recordPerf(rec, nil, doneAt)

	if len(cap.got) != 1 {
		t.Fatalf("want 1 sample, got %d", len(cap.got))
	}
	s := cap.got[0]
	closeTo(t, "queue wait", s.QueueMS, 100)
	closeTo(t, "end-to-end", s.TotalMS, 2500)
	closeTo(t, "ttft", s.TTFTMS, 400)
	if s.PrefillTokens != 0 || s.DecodeTokens != 0 {
		t.Fatalf("token counts invented without usage: %d / %d", s.PrefillTokens, s.DecodeTokens)
	}
}

func TestRecordPerf_UnusableBackendTimingsFallBack(t *testing.T) {
	cap := &capturePerf{}
	h := New(slog.Default())
	h.Perf = cap

	rec, doneAt := finishedJob(true, true)
	// A backend that sends the field but populates it with zeros must not be
	// trusted over the router's own observation.
	h.recordPerf(rec, &types.UsageInfo{
		PromptTokens: 800, CompletionTokens: 100,
		Timings: &types.Timings{},
	}, doneAt)

	s := cap.got[0]
	if s.FromBackend {
		t.Fatal("all-zero timings treated as backend-reported")
	}
	closeTo(t, "prefill window falls back to observation", s.PrefillMS, 400)
}

func TestRecordPerf_NoFirstTokenRecordsOnlyDurations(t *testing.T) {
	cap := &capturePerf{}
	h := New(slog.Default())
	h.Perf = cap

	// A streaming request that completed without ever emitting a content token
	// (e.g. a pure tool call, or an immediate stop).
	rec, doneAt := finishedJob(true, false)
	h.recordPerf(rec, &types.UsageInfo{PromptTokens: 800, CompletionTokens: 0}, doneAt)

	s := cap.got[0]
	if s.TTFTMS != 0 || s.PrefillMS != 0 || s.DecodeMS != 0 {
		t.Fatalf("measurements invented without a first token: %+v", s)
	}
	closeTo(t, "end-to-end", s.TotalMS, 2500)
}

func TestRecordPerf_NoRecorderIsNoOp(t *testing.T) {
	h := New(slog.Default())
	rec, doneAt := finishedJob(true, true)
	// Must not panic when performance tracking is disabled.
	h.recordPerf(rec, &types.UsageInfo{PromptTokens: 10, CompletionTokens: 5}, doneAt)
}

func TestRecordPerf_FeedsLatencyHistograms(t *testing.T) {
	h := New(slog.Default())
	h.Latency = latency.New()

	rec, doneAt := finishedJob(true, true)
	h.recordPerf(rec, &types.UsageInfo{
		PromptTokens: 800, CompletionTokens: 100,
		Timings: &types.Timings{PromptN: 800, PromptMS: 1000, PredictedN: 100, PredictedMS: 1000},
	}, doneAt)

	var b strings.Builder
	h.Latency.WritePrometheus(&b)
	out := b.String()

	// 800 tokens in 1s and 100 tokens in 1s, both scraped as p50 values.
	for _, want := range []string{
		`llmrouter_prompt_tokens_per_second{quantile="0.5",model="llama"} 800`,
		`llmrouter_generated_tokens_per_second{quantile="0.5",model="llama"} 100`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("metrics output missing %q:\n%s", want, out)
		}
	}
}

func TestRecordPerf_UnmeasuredThroughputIsNotScraped(t *testing.T) {
	h := New(slog.Default())
	h.Latency = latency.New()

	// A batch request with no backend timings yields no throughput measurement, so
	// nothing should be observed — a zero would drag the reported percentiles down.
	rec, doneAt := finishedJob(false, true)
	h.recordPerf(rec, &types.UsageInfo{PromptTokens: 800, CompletionTokens: 100}, doneAt)

	var b strings.Builder
	h.Latency.WritePrometheus(&b)
	if strings.Contains(b.String(), `llmrouter_generated_tokens_per_second{quantile=`) {
		t.Fatalf("an unmeasured request was recorded as a throughput observation:\n%s", b.String())
	}
}

func TestClientLabel(t *testing.T) {
	for _, tc := range []struct {
		owner, name, want string
	}{
		{"alice", "mac", "alice/mac"},
		{"", "mac", ""},   // an unidentified client is left unattributed
		{"alice", "", ""}, // rather than recorded under a half-formed label
	} {
		rec := InFlightRecord{ClientOwner: tc.owner, ClientName: tc.name}
		if got := rec.ClientLabel(); got != tc.want {
			t.Fatalf("owner %q name %q: got %q, want %q", tc.owner, tc.name, got, tc.want)
		}
	}
}

func TestRecordPerf_TTFTExcludesQueueWaitSoTheyCompose(t *testing.T) {
	cap := &capturePerf{}
	h := New(slog.Default())
	h.Perf = cap

	rec, doneAt := finishedJob(true, true)
	h.recordPerf(rec, &types.UsageInfo{PromptTokens: 800, CompletionTokens: 100}, doneAt)

	s := cap.got[0]
	// The portal shows queue wait and TTFT side by side, and /metrics reports TTFT
	// from dispatch. If TTFT started at enqueue instead, it would already contain
	// the queue wait and the two tiles could not be read together.
	closeTo(t, "queue + ttft = enqueue to first token", s.QueueMS+s.TTFTMS, 500)
	if s.TTFTMS >= s.TotalMS {
		t.Fatalf("ttft (%v) should be a fraction of end-to-end (%v)", s.TTFTMS, s.TotalMS)
	}
}
