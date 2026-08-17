// router/internal/latency/latency.go
// Package latency provides rolling-window latency histograms for the router's
// /metrics endpoint. No external dependencies — histograms are output in
// Prometheus text format (summary-style p50/p95/p99) consistent with the
// existing custom metrics handler.
package latency

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// observation is a single timed measurement.
type observation struct {
	value float64
	at    time.Time
}

// Histogram tracks observations in a rolling time window and computes percentiles on demand.
type Histogram struct {
	mu     sync.Mutex
	window time.Duration
	obs    []observation
}

// newHistogram creates a Histogram retaining observations within the given window.
func newHistogram(window time.Duration) *Histogram {
	return &Histogram{window: window}
}

// Observe records a new value.
func (h *Histogram) Observe(v float64) {
	h.mu.Lock()
	h.obs = append(h.obs, observation{v, time.Now()})
	h.mu.Unlock()
}

// ObserveDuration records a duration in seconds.
func (h *Histogram) ObserveDuration(d time.Duration) {
	h.Observe(d.Seconds())
}

// Snapshot is one histogram's state over the current window. Values carry the
// unit that was observed: seconds for durations, tokens/sec for throughput,
// tokens for counts. Sum is meaningful only where adding observations is, which
// is the token histograms rather than the latency ones.
type Snapshot struct {
	P50, P95, P99 float64
	Sum           float64
	Count         int
}

// Snapshot prunes observations that have aged out of the window and returns what
// remains. All fields are 0 when the window is empty.
func (h *Histogram) Snapshot() Snapshot {
	h.mu.Lock()
	cutoff := time.Now().Add(-h.window)
	fresh := h.obs[:0]
	for _, o := range h.obs {
		if o.at.After(cutoff) {
			fresh = append(fresh, o)
		}
	}
	h.obs = fresh
	vals := make([]float64, len(fresh))
	for i, o := range fresh {
		vals[i] = o.value
	}
	h.mu.Unlock()

	if len(vals) == 0 {
		return Snapshot{}
	}
	s := Snapshot{Count: len(vals)}
	for _, v := range vals {
		s.Sum += v
	}
	sort.Float64s(vals)
	s.P50 = vals[s.Count*50/100]
	s.P95 = vals[int(math.Ceil(float64(s.Count)*0.95))-1]
	s.P99 = vals[int(math.Ceil(float64(s.Count)*0.99))-1]
	return s
}

// Percentiles returns p50, p95, p99 and the observation count within the window.
// All values are 0 when there are no observations.
func (h *Histogram) Percentiles() (p50, p95, p99 float64, n int) {
	s := h.Snapshot()
	return s.P50, s.P95, s.P99, s.Count
}

// WritePrometheus appends a Prometheus summary block (p50/p95/p99 + count) to b.
// model is included as a label; pass "" to omit the label.
func (h *Histogram) WritePrometheus(b *strings.Builder, name, help, model string) {
	p50, p95, p99, count := h.Percentiles()
	label := ""
	if model != "" {
		label = fmt.Sprintf("{model=%q}", model)
	}
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s summary\n", name, help, name)
	if count > 0 {
		fmt.Fprintf(b, "%s{quantile=\"0.5\"%s} %g\n", name, labelSuffix(model), p50)
		fmt.Fprintf(b, "%s{quantile=\"0.95\"%s} %g\n", name, labelSuffix(model), p95)
		fmt.Fprintf(b, "%s{quantile=\"0.99\"%s} %g\n", name, labelSuffix(model), p99)
	}
	fmt.Fprintf(b, "%s_count%s %d\n", name, label, count)
}

func labelSuffix(model string) string {
	if model == "" {
		return ""
	}
	return fmt.Sprintf(",model=%q", model)
}

// Recorder holds latency and throughput histograms for the key router stages.
// Observations are keyed by model name and retained for a 10-minute rolling window.
type Recorder struct {
	window    time.Duration
	mu        sync.Mutex
	queueWait map[string]*Histogram // model → queue wait time histogram
	ttft      map[string]*Histogram // model → time-to-first-token histogram
	duration  map[string]*Histogram // model → total job duration histogram
	promptTPS map[string]*Histogram // model → prompt-evaluation tokens/sec histogram
	genTPS    map[string]*Histogram // model → token-generation tokens/sec histogram
	// promptTokens and genTokens observe one completed request's token counts,
	// so a snapshot's Sum is how many tokens the window processed and its Count
	// is how many requests did so. Kept apart from the TPS histograms because
	// those drop unmeasured requests and these must not.
	promptTokens map[string]*Histogram
	genTokens    map[string]*Histogram
}

// New creates a Recorder with a 10-minute rolling window.
func New() *Recorder {
	return &Recorder{
		window:       10 * time.Minute,
		queueWait:    make(map[string]*Histogram),
		ttft:         make(map[string]*Histogram),
		duration:     make(map[string]*Histogram),
		promptTPS:    make(map[string]*Histogram),
		genTPS:       make(map[string]*Histogram),
		promptTokens: make(map[string]*Histogram),
		genTokens:    make(map[string]*Histogram),
	}
}

// Window returns the rolling window every snapshot covers. Fixed at construction.
func (r *Recorder) Window() time.Duration { return r.window }

func (r *Recorder) histogram(m map[string]*Histogram, model string) *Histogram {
	h, ok := m[model]
	if !ok {
		h = newHistogram(r.window)
		m[model] = h
	}
	return h
}

// RecordQueueWait records the time from request enqueue to dispatch for a model.
func (r *Recorder) RecordQueueWait(model string, d time.Duration) {
	r.mu.Lock()
	h := r.histogram(r.queueWait, model)
	r.mu.Unlock()
	h.ObserveDuration(d)
}

// RecordTTFT records the time from dispatch to first token for a model.
func (r *Recorder) RecordTTFT(model string, d time.Duration) {
	r.mu.Lock()
	h := r.histogram(r.ttft, model)
	r.mu.Unlock()
	h.ObserveDuration(d)
}

// RecordDuration records the total job duration (dispatch to done) for a model.
func (r *Recorder) RecordDuration(model string, d time.Duration) {
	r.mu.Lock()
	h := r.histogram(r.duration, model)
	r.mu.Unlock()
	h.ObserveDuration(d)
}

// RecordPromptThroughput records prompt-evaluation speed in tokens per second.
// Values <= 0 are ignored so an unmeasured request cannot drag the window down.
func (r *Recorder) RecordPromptThroughput(model string, tokensPerSec float64) {
	if tokensPerSec <= 0 {
		return
	}
	r.mu.Lock()
	h := r.histogram(r.promptTPS, model)
	r.mu.Unlock()
	h.Observe(tokensPerSec)
}

// RecordGenThroughput records token-generation speed in tokens per second.
// Values <= 0 are ignored, as for RecordPromptThroughput.
func (r *Recorder) RecordGenThroughput(model string, tokensPerSec float64) {
	if tokensPerSec <= 0 {
		return
	}
	r.mu.Lock()
	h := r.histogram(r.genTPS, model)
	r.mu.Unlock()
	h.Observe(tokensPerSec)
}

// RecordTokens records one completed request's token counts for a model. Unlike
// the throughput histograms this observes zero counts too: a request that
// generated nothing is still a request the window should account for.
func (r *Recorder) RecordTokens(model string, promptTokens, completionTokens int) {
	r.mu.Lock()
	p := r.histogram(r.promptTokens, model)
	g := r.histogram(r.genTokens, model)
	r.mu.Unlock()
	p.Observe(float64(promptTokens))
	g.Observe(float64(completionTokens))
}

// ModelSnapshot is one model's observations across every stage, over the window.
type ModelSnapshot struct {
	QueueWait Snapshot // seconds
	TTFT      Snapshot // seconds; streaming requests only
	Duration  Snapshot // seconds
	PromptTPS Snapshot // tokens/sec
	GenTPS    Snapshot // tokens/sec
	// PromptTokens and GenTokens observe per-request token counts, so Sum is the
	// tokens processed over the window and Count the requests that reported any.
	PromptTokens Snapshot
	GenTokens    Snapshot
}

// SnapshotByModel returns the current window for every model with observations.
//
// Follows the same discipline as WritePrometheus: histogram pointers are
// collected under the recorder's lock and read only after releasing it, because
// reading one takes its own lock and prunes as it goes.
func (r *Recorder) SnapshotByModel() map[string]ModelSnapshot {
	r.mu.Lock()
	all := []map[string]*Histogram{
		r.queueWait, r.ttft, r.duration, r.promptTPS, r.genTPS, r.promptTokens, r.genTokens,
	}
	models := make(map[string]struct{})
	for _, m := range all {
		for model := range m {
			models[model] = struct{}{}
		}
	}
	type held struct {
		qw, ttft, dur, ptps, gtps, ptok, gtok *Histogram
	}
	pending := make(map[string]held, len(models))
	for model := range models {
		pending[model] = held{
			qw:   r.queueWait[model],
			ttft: r.ttft[model],
			dur:  r.duration[model],
			ptps: r.promptTPS[model],
			gtps: r.genTPS[model],
			ptok: r.promptTokens[model],
			gtok: r.genTokens[model],
		}
	}
	r.mu.Unlock()

	out := make(map[string]ModelSnapshot, len(pending))
	for model, h := range pending {
		out[model] = ModelSnapshot{
			QueueWait:    snapshotOf(h.qw),
			TTFT:         snapshotOf(h.ttft),
			Duration:     snapshotOf(h.dur),
			PromptTPS:    snapshotOf(h.ptps),
			GenTPS:       snapshotOf(h.gtps),
			PromptTokens: snapshotOf(h.ptok),
			GenTokens:    snapshotOf(h.gtok),
		}
	}
	return out
}

// snapshotOf tolerates a model that has observations for some stages but not
// others, which is the norm: batch requests never observe TTFT, and a backend
// that reports no timings never observes throughput.
func snapshotOf(h *Histogram) Snapshot {
	if h == nil {
		return Snapshot{}
	}
	return h.Snapshot()
}

// WritePrometheus appends all latency and throughput metrics to b in Prometheus
// text format. The token histograms are deliberately not included: /metrics
// already publishes cumulative per-model token counters, and a windowed
// duplicate under a similar name would invite the two to be confused.
func (r *Recorder) WritePrometheus(b *strings.Builder) {
	r.mu.Lock()
	models := make(map[string]struct{})
	for _, m := range []map[string]*Histogram{r.queueWait, r.ttft, r.duration, r.promptTPS, r.genTPS} {
		for model := range m {
			models[model] = struct{}{}
		}
	}

	// Snapshot histograms under lock, then release before computing percentiles.
	type snap struct {
		model     string
		qw        *Histogram
		ttft      *Histogram
		dur       *Histogram
		promptTPS *Histogram
		genTPS    *Histogram
	}
	snaps := make([]snap, 0, len(models))
	for model := range models {
		snaps = append(snaps, snap{
			model:     model,
			qw:        r.queueWait[model],
			ttft:      r.ttft[model],
			dur:       r.duration[model],
			promptTPS: r.promptTPS[model],
			genTPS:    r.genTPS[model],
		})
	}
	r.mu.Unlock()

	// Sort for deterministic output.
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].model < snaps[j].model })

	for _, s := range snaps {
		if s.qw != nil {
			s.qw.WritePrometheus(b,
				"llmrouter_queue_wait_seconds",
				"Time from request enqueue to dispatch to a worker (p50/p95/p99 over 10m window).",
				s.model)
		}
		if s.ttft != nil {
			s.ttft.WritePrometheus(b,
				"llmrouter_ttft_seconds",
				"Time from job dispatch to first non-empty token received from worker, over streaming requests only (p50/p95/p99 over 10m window). Batch responses arrive as one chunk at completion, so they have no first-token signal and are not observed here.",
				s.model)
		}
		if s.dur != nil {
			s.dur.WritePrometheus(b,
				"llmrouter_job_duration_seconds",
				"Total job duration from dispatch to completion (p50/p95/p99 over 10m window).",
				s.model)
		}
		if s.promptTPS != nil {
			s.promptTPS.WritePrometheus(b,
				"llmrouter_prompt_tokens_per_second",
				"Prompt evaluation (prefill) speed in tokens/sec (p50/p95/p99 over 10m window).",
				s.model)
		}
		if s.genTPS != nil {
			s.genTPS.WritePrometheus(b,
				"llmrouter_generated_tokens_per_second",
				"Token generation (decode) speed in tokens/sec (p50/p95/p99 over 10m window).",
				s.model)
		}
	}
}
