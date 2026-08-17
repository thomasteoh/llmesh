package latency

import (
	"testing"
	"time"
)

func TestSnapshot_EmptyWindowIsAllZero(t *testing.T) {
	got := newHistogram(time.Minute).Snapshot()

	if got != (Snapshot{}) {
		t.Errorf("got %+v, want the zero snapshot", got)
	}
}

// Sum is what makes "tokens processed in the window" answerable, and Count the
// requests behind it. Percentiles alone cannot give either.
func TestSnapshot_SumsAndCountsObservations(t *testing.T) {
	h := newHistogram(time.Minute)
	for _, v := range []float64{10, 20, 30, 40} {
		h.Observe(v)
	}

	got := h.Snapshot()

	if got.Sum != 100 {
		t.Errorf("Sum: got %v, want 100", got.Sum)
	}
	if got.Count != 4 {
		t.Errorf("Count: got %d, want 4", got.Count)
	}
}

// Observations older than the window are pruned, so both the sum and the count
// describe the recent period rather than all time.
func TestSnapshot_ExcludesObservationsOlderThanTheWindow(t *testing.T) {
	h := newHistogram(50 * time.Millisecond)
	h.Observe(100)
	time.Sleep(80 * time.Millisecond)
	h.Observe(7)

	got := h.Snapshot()

	if got.Count != 1 || got.Sum != 7 {
		t.Errorf("got Count=%d Sum=%v, want 1 and 7", got.Count, got.Sum)
	}
}

// Percentiles is now a view over Snapshot; the values it reported before must be
// unchanged, since /metrics is built on it.
func TestPercentiles_AgreesWithSnapshot(t *testing.T) {
	h := newHistogram(time.Minute)
	for i := 1; i <= 100; i++ {
		h.Observe(float64(i))
	}

	p50, p95, p99, n := h.Percentiles()
	s := h.Snapshot()

	if p50 != s.P50 || p95 != s.P95 || p99 != s.P99 || n != s.Count {
		t.Errorf("Percentiles gave (%v %v %v %d), Snapshot gave (%v %v %v %d)",
			p50, p95, p99, n, s.P50, s.P95, s.P99, s.Count)
	}
	if p50 != 51 || p95 != 95 || p99 != 99 {
		t.Errorf("got p50=%v p95=%v p99=%v", p50, p95, p99)
	}
}

// A request that generated nothing still happened. Dropping it, as the
// throughput histograms do, would make the window's request count disagree with
// what the router actually served.
func TestRecordTokens_CountsZeroTokenRequests(t *testing.T) {
	r := New()
	r.RecordTokens("llama", 100, 50)
	r.RecordTokens("llama", 0, 0)

	got := r.SnapshotByModel()["llama"]

	if got.PromptTokens.Count != 2 {
		t.Errorf("request count: got %d, want 2", got.PromptTokens.Count)
	}
	if got.PromptTokens.Sum != 100 || got.GenTokens.Sum != 50 {
		t.Errorf("got prompt=%v gen=%v, want 100 and 50", got.PromptTokens.Sum, got.GenTokens.Sum)
	}
}

// Throughput, by contrast, drops unmeasured requests so an unreported one cannot
// drag the window's rate toward zero.
func TestRecordThroughput_IgnoresNonPositiveRates(t *testing.T) {
	r := New()
	r.RecordGenThroughput("llama", 40)
	r.RecordGenThroughput("llama", 0)
	r.RecordPromptThroughput("llama", -1)

	got := r.SnapshotByModel()["llama"]

	if got.GenTPS.Count != 1 {
		t.Errorf("GenTPS count: got %d, want 1", got.GenTPS.Count)
	}
	if got.PromptTPS.Count != 0 {
		t.Errorf("PromptTPS count: got %d, want 0", got.PromptTPS.Count)
	}
}

// Models observe different stages: a batch request never reports TTFT, and a
// backend without timings never reports throughput. Missing stages read as zero
// rather than tripping over a nil histogram.
func TestSnapshotByModel_ToleratesPartiallyObservedModels(t *testing.T) {
	r := New()
	r.RecordQueueWait("llama", 250*time.Millisecond)

	all := r.SnapshotByModel()

	got, ok := all["llama"]
	if !ok {
		t.Fatalf("llama absent; got %+v", all)
	}
	if got.QueueWait.Count != 1 {
		t.Errorf("QueueWait count: got %d, want 1", got.QueueWait.Count)
	}
	if got.QueueWait.P50 != 0.25 {
		t.Errorf("QueueWait p50: got %v seconds, want 0.25", got.QueueWait.P50)
	}
	if got.TTFT.Count != 0 || got.GenTPS.Count != 0 || got.GenTokens.Count != 0 {
		t.Errorf("unobserved stages should be zero, got %+v", got)
	}
}

func TestSnapshotByModel_SeparatesModels(t *testing.T) {
	r := New()
	r.RecordTokens("llama", 10, 1)
	r.RecordTokens("qwen", 20, 2)

	all := r.SnapshotByModel()

	if len(all) != 2 {
		t.Fatalf("got %d models, want 2: %+v", len(all), all)
	}
	if all["llama"].PromptTokens.Sum != 10 || all["qwen"].PromptTokens.Sum != 20 {
		t.Errorf("got llama=%v qwen=%v", all["llama"].PromptTokens.Sum, all["qwen"].PromptTokens.Sum)
	}
}

func TestWindow_IsTheRecorderWindow(t *testing.T) {
	if got := New().Window(); got != 10*time.Minute {
		t.Errorf("got %v, want 10m", got)
	}
}
