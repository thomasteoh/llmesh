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

// Percentiles returns p50, p95, p99 and the observation count within the window.
// All values are 0 when there are no observations.
func (h *Histogram) Percentiles() (p50, p95, p99 float64, n int) {
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
		return 0, 0, 0, 0
	}
	sort.Float64s(vals)
	count := len(vals)
	return vals[count*50/100],
		vals[int(math.Ceil(float64(count)*0.95))-1],
		vals[int(math.Ceil(float64(count)*0.99))-1],
		count
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

// Recorder holds latency histograms for the three key router stages.
// Observations are keyed by model name and retained for a 10-minute rolling window.
type Recorder struct {
	window    time.Duration
	mu        sync.Mutex
	queueWait map[string]*Histogram // model → queue wait time histogram
	ttft      map[string]*Histogram // model → time-to-first-token histogram
	duration  map[string]*Histogram // model → total job duration histogram
}

// New creates a Recorder with a 10-minute rolling window.
func New() *Recorder {
	return &Recorder{
		window:    10 * time.Minute,
		queueWait: make(map[string]*Histogram),
		ttft:      make(map[string]*Histogram),
		duration:  make(map[string]*Histogram),
	}
}

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

// WritePrometheus appends all latency metrics to b in Prometheus text format.
func (r *Recorder) WritePrometheus(b *strings.Builder) {
	r.mu.Lock()
	models := make(map[string]struct{})
	for m := range r.queueWait {
		models[m] = struct{}{}
	}
	for m := range r.ttft {
		models[m] = struct{}{}
	}
	for m := range r.duration {
		models[m] = struct{}{}
	}

	// Snapshot histograms under lock, then release before computing percentiles.
	type snap struct {
		model string
		qw    *Histogram
		ttft  *Histogram
		dur   *Histogram
	}
	snaps := make([]snap, 0, len(models))
	for model := range models {
		snaps = append(snaps, snap{
			model: model,
			qw:    r.queueWait[model],
			ttft:  r.ttft[model],
			dur:   r.duration[model],
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
				"Time from job dispatch to first non-empty token received from worker (p50/p95/p99 over 10m window).",
				s.model)
		}
		if s.dur != nil {
			s.dur.WritePrometheus(b,
				"llmrouter_job_duration_seconds",
				"Total job duration from dispatch to completion (p50/p95/p99 over 10m window).",
				s.model)
		}
	}
}
