package admin

import (
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// sample builds a PerfSample with the fields most tests care about.
func sample(model, client string) PerfSample {
	return PerfSample{Owner: "alice", KeyLabel: "alice/prod", Model: model, Client: client}
}

func nearly(t *testing.T, label string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 0.01 {
		t.Fatalf("%s: got %v, want %v", label, got, want)
	}
}

func TestPerf_AddDeltaUpsertAccumulatesAndTakesMax(t *testing.T) {
	s, _ := LoadState(filepath.Join(t.TempDir(), "state.json"))
	bucket := time.Date(2026, 7, 14, 9, 30, 0, 0, time.UTC) // truncates to 09:00

	first := PerfDelta{
		Bucket: bucket, Owner: "alice", KeyLabel: "alice/prod", Model: "llama", Client: "alice/mac",
		Samples: 1, QueueMSSum: 10, QueueMSMax: 10, TotalMSSum: 1000, TotalMSMax: 1000,
		TTFTSamples: 1, TTFTMSSum: 200, TTFTMSMax: 200,
		PrefillSamples: 1, PrefillMSSum: 100, PrefillTokens: 500,
		DecodeSamples: 1, DecodeMSSum: 800, DecodeTokens: 40,
		BackendSamples: 1,
	}
	// Second delta has a smaller max, so the stored maxima must not regress.
	second := first
	second.QueueMSMax, second.TotalMSMax, second.TTFTMSMax = 5, 500, 90

	if err := s.AddPerfDelta(first); err != nil {
		t.Fatal(err)
	}
	if err := s.AddPerfDelta(second); err != nil {
		t.Fatal(err)
	}

	rows, err := s.QueryPerf(bucket.Add(-time.Hour), bucket.Add(time.Hour), "model", false, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	r := rows[0]
	if r.Bucket != "2026-07-14T09:00:00Z" {
		t.Fatalf("bucket not truncated to hour: %s", r.Bucket)
	}
	if r.Samples != 2 {
		t.Fatalf("samples: got %d, want 2", r.Samples)
	}
	nearly(t, "queue sum", r.QueueMSSum, 20)
	nearly(t, "prefill tokens", float64(r.PrefillTokens), 1000)
	// Maxima keep the larger of the two, not the most recent.
	nearly(t, "queue max", r.QueueMSMax, 10)
	nearly(t, "total max", r.TotalMSMax, 1000)
	nearly(t, "ttft max", r.TTFTMSMax, 200)
}

func TestPerf_RatesAreWeightedByTokensNotAveragedPerRequest(t *testing.T) {
	// One long request at 50 tok/s and one tiny request at 5 tok/s. Averaging the
	// two rates would give 27.5 tok/s, which describes neither; weighting by tokens
	// gives the throughput actually observed.
	var p PerfStats
	p.Add(PerfStats{Samples: 1, DecodeSamples: 1, DecodeMSSum: 20000, DecodeTokens: 1000}) // 50 tok/s
	p.Add(PerfStats{Samples: 1, DecodeSamples: 1, DecodeMSSum: 1000, DecodeTokens: 5})     // 5 tok/s

	// 1005 tokens over 21s.
	nearly(t, "weighted gen rate", p.GenTokensPerSec(), 1005.0/21.0)
	if avgOfRates := (50.0 + 5.0) / 2; math.Abs(p.GenTokensPerSec()-avgOfRates) < 1 {
		t.Fatal("rate looks like a mean of per-request rates, not a token-weighted throughput")
	}
}

func TestPerf_DerivedStatsUseTheirOwnSampleCounts(t *testing.T) {
	// Three requests, only one of which was streaming and produced a TTFT. The
	// TTFT average must divide by 1, not by 3.
	p := PerfStats{
		Samples: 3, QueueMSSum: 300, TotalMSSum: 9000,
		TTFTSamples: 1, TTFTMSSum: 400,
	}
	nearly(t, "avg ttft", p.AvgTTFTMS(), 400)
	nearly(t, "avg queue", p.AvgQueueMS(), 100)
	nearly(t, "avg total", p.AvgTotalMS(), 3000)

	// With nothing measured, every derived value is zero rather than NaN.
	var empty PerfStats
	for label, got := range map[string]float64{
		"ttft": empty.AvgTTFTMS(), "queue": empty.AvgQueueMS(), "total": empty.AvgTotalMS(),
		"gen": empty.GenTokensPerSec(), "prompt": empty.PromptTokensPerSec(),
		"frac": empty.BackendMeasuredFrac(),
	} {
		if got != 0 {
			t.Fatalf("empty stats %s: got %v, want 0", label, got)
		}
	}
}

func TestPerf_DeltaAddSkipsUnmeasuredValues(t *testing.T) {
	var d PerfDelta
	// A batch request: no TTFT, and no prefill/decode split observable.
	d.add(PerfSample{Model: "llama", QueueMS: 5, TotalMS: 900})
	// A streaming request with a full set of measurements.
	d.add(PerfSample{
		Model: "llama", QueueMS: 5, TotalMS: 1000, TTFTMS: 300,
		PrefillMS: 100, PrefillTokens: 400, DecodeMS: 700, DecodeTokens: 35,
		FromBackend: true,
	})

	if d.Samples != 2 {
		t.Fatalf("samples: got %d, want 2", d.Samples)
	}
	// Only the streaming request contributed a TTFT, so a zero must not be averaged in.
	if d.TTFTSamples != 1 {
		t.Fatalf("ttft samples: got %d, want 1", d.TTFTSamples)
	}
	if d.PrefillSamples != 1 || d.DecodeSamples != 1 {
		t.Fatalf("throughput samples: prefill %d decode %d, want 1 each", d.PrefillSamples, d.DecodeSamples)
	}
	if d.BackendSamples != 1 {
		t.Fatalf("backend samples: got %d, want 1", d.BackendSamples)
	}

	stats := PerfStats{
		Samples: d.Samples, TTFTSamples: d.TTFTSamples, TTFTMSSum: d.TTFTMSSum,
		BackendSamples: d.BackendSamples,
	}
	nearly(t, "avg ttft", stats.AvgTTFTMS(), 300)
	nearly(t, "backend fraction", stats.BackendMeasuredFrac(), 0.5)
}

func TestPerf_PartialThroughputPairsAreIgnored(t *testing.T) {
	var d PerfDelta
	// Tokens with no duration, and a duration with no tokens: neither yields a rate.
	d.add(PerfSample{Model: "llama", PrefillTokens: 500, DecodeMS: 900})
	if d.PrefillSamples != 0 || d.DecodeSamples != 0 {
		t.Fatalf("half-measured pairs recorded: prefill %d decode %d", d.PrefillSamples, d.DecodeSamples)
	}
	if d.PrefillTokens != 0 || d.DecodeMSSum != 0 {
		t.Fatalf("half-measured pairs contributed counters: tokens %d ms %v", d.PrefillTokens, d.DecodeMSSum)
	}
}

func TestPerf_QueryGroupingAndOwnerFilter(t *testing.T) {
	s, _ := LoadState(filepath.Join(t.TempDir(), "state.json"))
	b := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	mk := func(owner, key, model, client string, samples int64) PerfDelta {
		return PerfDelta{
			Bucket: b, Owner: owner, KeyLabel: key, Model: model, Client: client,
			Samples: samples, TotalMSSum: float64(samples) * 1000,
			DecodeSamples: samples, DecodeMSSum: float64(samples) * 1000, DecodeTokens: samples * 40,
		}
	}
	s.AddPerfDelta(mk("alice", "alice/prod", "llama", "alice/mac", 1))
	s.AddPerfDelta(mk("bob", "bob/dev", "llama", "bob/box", 2))
	s.AddPerfDelta(mk("alice", "alice/prod", "qwen", "alice/mac", 4))

	since, until := b.Add(-time.Hour), b.Add(time.Hour)

	for _, tc := range []struct {
		group string
		want  map[string]int64
	}{
		{"model", map[string]int64{"llama": 3, "qwen": 4}},
		{"owner", map[string]int64{"alice": 5, "bob": 2}},
		{"key", map[string]int64{"alice/prod": 5, "bob/dev": 2}},
		{"client", map[string]int64{"alice/mac": 5, "bob/box": 2}},
	} {
		rows, err := s.QueryPerf(since, until, tc.group, false, "")
		if err != nil {
			t.Fatalf("group %s: %v", tc.group, err)
		}
		got := make(map[string]int64)
		for _, r := range rows {
			got[r.Name] += r.Samples
		}
		if len(got) != len(tc.want) {
			t.Fatalf("group %s: got %v, want %v", tc.group, got, tc.want)
		}
		for name, want := range tc.want {
			if got[name] != want {
				t.Fatalf("group %s name %s: got %d, want %d", tc.group, name, got[name], want)
			}
		}
	}

	// Restricting to one owner hides the other's requests entirely.
	rows, err := s.QueryPerf(since, until, "model", false, "alice")
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, r := range rows {
		total += r.Samples
		if r.Name == "llama" && r.Samples != 1 {
			t.Fatalf("owner filter leaked bob's llama requests: %d", r.Samples)
		}
	}
	if total != 5 {
		t.Fatalf("owner-filtered total: got %d, want 5", total)
	}

	if _, err := s.QueryPerf(since, until, "nonsense", false, ""); err == nil {
		t.Fatal("expected an error for an unknown grouping")
	}
}

func TestPerf_QueryDailyRollup(t *testing.T) {
	s, _ := LoadState(filepath.Join(t.TempDir(), "state.json"))
	day := time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC)
	for _, hour := range []int{1, 5, 20} {
		s.AddPerfDelta(PerfDelta{
			Bucket: day.Add(time.Duration(hour) * time.Hour), Model: "llama",
			Samples: 1, DecodeSamples: 1, DecodeMSSum: 1000, DecodeTokens: 30,
		})
	}
	rows, err := s.QueryPerf(day, day.AddDate(0, 0, 1), "model", true, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 daily row, got %d", len(rows))
	}
	if rows[0].Bucket != "2026-07-14" {
		t.Fatalf("daily bucket format: got %q", rows[0].Bucket)
	}
	if rows[0].Samples != 3 {
		t.Fatalf("daily rollup samples: got %d, want 3", rows[0].Samples)
	}
	nearly(t, "daily rate", rows[0].GenTokensPerSec(), 30)
}

func TestPerf_TotalsOverEmptyRangeIsZeroNotError(t *testing.T) {
	s, _ := LoadState(filepath.Join(t.TempDir(), "state.json"))
	now := time.Now()
	// An ungrouped SUM over zero rows returns a row of NULLs; the query has to
	// coalesce those or scanning fails.
	got, err := s.PerfTotals(now.Add(-time.Hour), now, "")
	if err != nil {
		t.Fatalf("totals over an empty table: %v", err)
	}
	if got.Samples != 0 {
		t.Fatalf("samples: got %d, want 0", got.Samples)
	}
}

func TestPerf_Totals(t *testing.T) {
	s, _ := LoadState(filepath.Join(t.TempDir(), "state.json"))
	b := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	s.AddPerfDelta(PerfDelta{
		Bucket: b, Owner: "alice", Model: "llama", Samples: 2,
		TotalMSSum: 4000, TotalMSMax: 3000,
		TTFTSamples: 2, TTFTMSSum: 600, TTFTMSMax: 400,
		DecodeSamples: 2, DecodeMSSum: 2000, DecodeTokens: 100,
		PrefillSamples: 2, PrefillMSSum: 500, PrefillTokens: 1000,
		BackendSamples: 2,
	})
	s.AddPerfDelta(PerfDelta{Bucket: b, Owner: "bob", Model: "llama", Samples: 1, TotalMSSum: 500, TotalMSMax: 500})

	all, err := s.PerfTotals(b.Add(-time.Hour), b.Add(time.Hour), "")
	if err != nil {
		t.Fatal(err)
	}
	if all.Samples != 3 {
		t.Fatalf("samples: got %d, want 3", all.Samples)
	}
	nearly(t, "gen rate", all.GenTokensPerSec(), 50)
	nearly(t, "prompt rate", all.PromptTokensPerSec(), 2000)
	nearly(t, "avg ttft", all.AvgTTFTMS(), 300)
	nearly(t, "max total", all.TotalMSMax, 3000)
	// Only alice's two requests had backend-reported timings.
	nearly(t, "backend fraction", all.BackendMeasuredFrac(), 2.0/3.0)

	mine, err := s.PerfTotals(b.Add(-time.Hour), b.Add(time.Hour), "bob")
	if err != nil {
		t.Fatal(err)
	}
	if mine.Samples != 1 {
		t.Fatalf("owner-filtered samples: got %d, want 1", mine.Samples)
	}
}

func TestPerf_ByClientAndByClientModel(t *testing.T) {
	s, _ := LoadState(filepath.Join(t.TempDir(), "state.json"))
	b := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	add := func(client, model string, samples int64, decodeMS float64, tokens int64) {
		s.AddPerfDelta(PerfDelta{
			Bucket: b, Owner: "alice", Model: model, Client: client, Samples: samples,
			DecodeSamples: samples, DecodeMSSum: decodeMS, DecodeTokens: tokens,
		})
	}
	add("alice/fast", "llama", 1, 1000, 60) // 60 tok/s
	add("alice/slow", "llama", 1, 1000, 10) // 10 tok/s
	add("alice/fast", "qwen", 1, 1000, 20)  // 20 tok/s
	// A row with no client attribution must not appear as a machine named "".
	s.AddPerfDelta(PerfDelta{Bucket: b, Owner: "alice", Model: "llama", Client: "", Samples: 5})

	since, until := b.Add(-time.Hour), b.Add(time.Hour)

	byClient, err := s.PerfByClient(since, until, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(byClient) != 2 {
		t.Fatalf("want 2 clients, got %d: %v", len(byClient), byClient)
	}
	if _, ok := byClient[""]; ok {
		t.Fatal("unattributed rows leaked in as an empty-named client")
	}
	// alice/fast served two models: 80 tokens over 2s.
	nearly(t, "fast machine rate", byClient["alice/fast"].GenTokensPerSec(), 40)
	nearly(t, "slow machine rate", byClient["alice/slow"].GenTokensPerSec(), 10)

	byClientModel, err := s.PerfByClientModel(since, until, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(byClientModel["alice/fast"]) != 2 {
		t.Fatalf("want 2 models for alice/fast, got %d", len(byClientModel["alice/fast"]))
	}
	nearly(t, "fast/llama rate", byClientModel["alice/fast"]["llama"].GenTokensPerSec(), 60)
	nearly(t, "fast/qwen rate", byClientModel["alice/fast"]["qwen"].GenTokensPerSec(), 20)
}

func TestPerf_PruneBefore(t *testing.T) {
	s, _ := LoadState(filepath.Join(t.TempDir(), "state.json"))
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	recent := time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC)
	s.AddPerfDelta(PerfDelta{Bucket: old, Model: "llama", Samples: 1})
	s.AddPerfDelta(PerfDelta{Bucket: recent, Model: "llama", Samples: 1})

	if err := s.PrunePerfBefore(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	got, err := s.PerfTotals(old.Add(-time.Hour), recent.Add(time.Hour), "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Samples != 1 {
		t.Fatalf("after prune: got %d samples, want 1", got.Samples)
	}
}

func TestPerfRecorder_BuffersAndFlushes(t *testing.T) {
	s, _ := LoadState(filepath.Join(t.TempDir(), "state.json"))
	r := NewPerfRecorder(s, slog.Default())
	defer r.Close()

	for i := 0; i < 3; i++ {
		r.RecordPerf(PerfSample{
			Owner: "alice", KeyLabel: "alice/prod", Model: "llama", Client: "alice/mac",
			QueueMS: 10, TotalMS: 1000, TTFTMS: 200,
			PrefillMS: 100, PrefillTokens: 400, DecodeMS: 800, DecodeTokens: 40,
			FromBackend: true,
		})
	}
	// A sample with no model cannot be attributed and is dropped.
	r.RecordPerf(PerfSample{Owner: "alice", TotalMS: 5000})

	now := time.Now()
	before, err := s.PerfTotals(now.Add(-time.Hour), now.Add(time.Hour), "")
	if err != nil {
		t.Fatal(err)
	}
	if before.Samples != 0 {
		t.Fatalf("samples reached the database before a flush: %d", before.Samples)
	}

	r.Flush()

	after, err := s.PerfTotals(now.Add(-time.Hour), now.Add(time.Hour), "")
	if err != nil {
		t.Fatal(err)
	}
	if after.Samples != 3 {
		t.Fatalf("after flush: got %d samples, want 3 (the model-less sample must be dropped)", after.Samples)
	}
	nearly(t, "gen rate", after.GenTokensPerSec(), 50)
	nearly(t, "avg ttft", after.AvgTTFTMS(), 200)
}

func TestPerfRecorder_MergesSameBucketAcrossFlushes(t *testing.T) {
	s, _ := LoadState(filepath.Join(t.TempDir(), "state.json"))
	r := NewPerfRecorder(s, slog.Default())
	defer r.Close()

	sm := sample("llama", "alice/mac")
	sm.TotalMS, sm.DecodeMS, sm.DecodeTokens = 1000, 1000, 40
	r.RecordPerf(sm)
	r.Flush()
	r.RecordPerf(sm)
	r.Flush()

	now := time.Now()
	got, err := s.PerfTotals(now.Add(-time.Hour), now.Add(time.Hour), "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Samples != 2 {
		t.Fatalf("two flushes into one bucket: got %d samples, want 2", got.Samples)
	}
	nearly(t, "rate is unchanged by bucketing", got.GenTokensPerSec(), 40)
}

func TestPerfDelta_Merge(t *testing.T) {
	a := PerfDelta{
		Samples: 1, QueueMSSum: 10, QueueMSMax: 10, TotalMSSum: 100, TotalMSMax: 100,
		TTFTSamples: 1, TTFTMSSum: 50, TTFTMSMax: 50,
		PrefillSamples: 1, PrefillMSSum: 20, PrefillTokens: 100,
		DecodeSamples: 1, DecodeMSSum: 80, DecodeTokens: 8, BackendSamples: 1,
	}
	b := a
	b.QueueMSMax, b.TotalMSMax, b.TTFTMSMax = 99, 5, 5

	a.merge(b)
	if a.Samples != 2 {
		t.Fatalf("samples: got %d, want 2", a.Samples)
	}
	nearly(t, "queue sum", a.QueueMSSum, 20)
	// Maxima take the larger from either side, in both directions.
	nearly(t, "queue max", a.QueueMSMax, 99)
	nearly(t, "total max", a.TotalMSMax, 100)
	nearly(t, "ttft max", a.TTFTMSMax, 50)
	if a.BackendSamples != 2 {
		t.Fatalf("backend samples: got %d, want 2", a.BackendSamples)
	}
}

// --- JSON API ---

// perfRequest exercises handlePerfJSON against a state pre-loaded by seed, as the
// given user, and returns the decoded response.
func perfRequest(t *testing.T, u User, query string, seed func(*State)) perfResponseJSON {
	t.Helper()
	s, err := LoadState(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	seed(s)
	a := &Admin{state: s, log: slog.Default()}

	r := httptest.NewRequest(http.MethodGet, "/portal/api/perf?"+query, nil)
	r = r.WithContext(context.WithValue(r.Context(), ctxUser, u))
	w := httptest.NewRecorder()
	a.handlePerfJSON(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var got perfResponseJSON
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, w.Body.String())
	}
	return got
}

func TestPerfJSON_GapsAreNullNotZero(t *testing.T) {
	// Two hours of traffic separated by an idle hour. The idle bucket must come
	// back as null: rendering it as 0 tok/s would draw a dip that never happened.
	now := time.Now().UTC().Truncate(time.Hour)
	got := perfRequest(t, User{Username: "alice", Role: "admin"},
		"range=24h&group=model&metric=gen_tps", func(s *State) {
			for _, hoursAgo := range []int{3, 1} {
				s.AddPerfDelta(PerfDelta{
					Bucket: now.Add(-time.Duration(hoursAgo) * time.Hour), Model: "llama",
					Samples: 1, DecodeSamples: 1, DecodeMSSum: 1000, DecodeTokens: 30,
				})
			}
		})

	if len(got.Series) != 1 {
		t.Fatalf("want 1 series, got %d", len(got.Series))
	}
	vals := got.Series[0].Values
	if len(vals) != len(got.Buckets) {
		t.Fatalf("values (%d) do not line up with buckets (%d)", len(vals), len(got.Buckets))
	}
	var populated, nulls int
	for _, v := range vals {
		if v == nil {
			nulls++
			continue
		}
		populated++
		nearly(t, "bucket rate", *v, 30)
	}
	if populated != 2 {
		t.Fatalf("want 2 populated buckets, got %d", populated)
	}
	if nulls != len(vals)-2 {
		t.Fatalf("idle buckets not null: %d nulls of %d", nulls, len(vals))
	}
	if got.Unit != "tok/s" {
		t.Fatalf("unit: got %q, want tok/s", got.Unit)
	}
}

func TestPerfJSON_BucketRateDerivedFromSumsNotAveragedRates(t *testing.T) {
	// Two requests land in the same hour: 1000 tokens in 20s and 5 tokens in 1s.
	// The bucket must report 1005/21 ≈ 47.9 tok/s, not the 27.5 tok/s that
	// averaging the two per-request rates would give.
	now := time.Now().UTC().Truncate(time.Hour)
	got := perfRequest(t, User{Username: "alice", Role: "admin"},
		"range=24h&group=model&metric=gen_tps", func(s *State) {
			s.AddPerfDelta(PerfDelta{Bucket: now, Model: "llama", Samples: 1,
				DecodeSamples: 1, DecodeMSSum: 20000, DecodeTokens: 1000})
			s.AddPerfDelta(PerfDelta{Bucket: now, Owner: "bob", Model: "llama", Samples: 1,
				DecodeSamples: 1, DecodeMSSum: 1000, DecodeTokens: 5})
		})

	var found *float64
	for _, v := range got.Series[0].Values {
		if v != nil {
			found = v
		}
	}
	if found == nil {
		t.Fatal("no populated bucket")
	}
	nearly(t, "bucket rate", *found, 1005.0/21.0)
}

func TestPerfJSON_MemberSeesOnlyOwnRequests(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Hour)
	seed := func(s *State) {
		s.AddPerfDelta(PerfDelta{Bucket: now, Owner: "alice", Model: "llama", Samples: 3, TotalMSSum: 3000})
		s.AddPerfDelta(PerfDelta{Bucket: now, Owner: "bob", Model: "llama", Samples: 7, TotalMSSum: 7000})
	}
	q := "range=24h&group=model&metric=total"

	asMember := perfRequest(t, User{Username: "alice", Role: "member"}, q, seed)
	if asMember.Totals.Samples != 3 {
		t.Fatalf("member totals leaked other users: got %d, want 3", asMember.Totals.Samples)
	}

	asAdmin := perfRequest(t, User{Username: "root", Role: "admin"}, q, seed)
	if asAdmin.Totals.Samples != 10 {
		t.Fatalf("admin totals: got %d, want 10", asAdmin.Totals.Samples)
	}
}

func TestPerfJSON_GroupUserMapsToOwnerColumn(t *testing.T) {
	// The chart's grouping control says "user"; the table column is "owner".
	now := time.Now().UTC().Truncate(time.Hour)
	got := perfRequest(t, User{Username: "root", Role: "admin"},
		"range=24h&group=user&metric=total", func(s *State) {
			s.AddPerfDelta(PerfDelta{Bucket: now, Owner: "alice", Model: "llama", Samples: 1, TotalMSSum: 1000})
			s.AddPerfDelta(PerfDelta{Bucket: now, Owner: "bob", Model: "llama", Samples: 1, TotalMSSum: 2000})
		})
	names := map[string]bool{}
	for _, s := range got.Series {
		names[s.Name] = true
	}
	if !names["alice"] || !names["bob"] {
		t.Fatalf("group=user did not resolve to owners: %v", names)
	}
	if got.Group != "user" {
		t.Fatalf("response should echo the requested grouping, got %q", got.Group)
	}
}

func TestPerfJSON_TotalsExposeTailsAndProvenance(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Hour)
	got := perfRequest(t, User{Username: "root", Role: "admin"},
		"range=24h&group=model&metric=ttft", func(s *State) {
			s.AddPerfDelta(PerfDelta{
				Bucket: now, Model: "llama", Samples: 4,
				TotalMSSum: 8000, TotalMSMax: 5000,
				QueueMSSum: 400, QueueMSMax: 300,
				TTFTSamples: 4, TTFTMSSum: 2000, TTFTMSMax: 1500,
				BackendSamples: 1,
			})
		})
	tot := got.Totals
	nearly(t, "avg ttft", tot.AvgTTFTMS, 500)
	nearly(t, "max ttft", tot.MaxTTFTMS, 1500)
	nearly(t, "avg total", tot.AvgTotalMS, 2000)
	nearly(t, "max total", tot.MaxTotalMS, 5000)
	nearly(t, "avg queue", tot.AvgQueueMS, 100)
	// Only one of four samples was backend-reported, so the portal can flag the
	// remaining speeds as approximate.
	nearly(t, "backend fraction", tot.BackendMeasuredFrac, 0.25)
}

func TestPerfJSON_EmptyRangeReturnsEmptySeriesNotNull(t *testing.T) {
	got := perfRequest(t, User{Username: "root", Role: "admin"},
		"range=7d&group=model&metric=gen_tps", func(*State) {})
	if got.Series == nil {
		t.Fatal("series encoded as null; the chart expects an array")
	}
	if len(got.Series) != 0 || got.Totals.Samples != 0 {
		t.Fatalf("unexpected data: %+v", got)
	}
	if len(got.Buckets) == 0 {
		t.Fatal("bucket grid should still be dense over an empty range")
	}
}

func TestPerfJSON_RejectsBadInput(t *testing.T) {
	for _, q := range []string{"metric=nonsense", "range=nonsense", "group=nonsense"} {
		s, _ := LoadState(filepath.Join(t.TempDir(), "state.json"))
		a := &Admin{state: s, log: slog.Default()}
		r := httptest.NewRequest(http.MethodGet, "/portal/api/perf?"+q, nil)
		r = r.WithContext(context.WithValue(r.Context(), ctxUser, User{Username: "root", Role: "admin"}))
		w := httptest.NewRecorder()
		a.handlePerfJSON(w, r)
		if w.Code == http.StatusOK {
			t.Fatalf("query %q was accepted", q)
		}
	}
}

func TestPerfJSON_DefaultsToGenerationSpeedOverSevenDays(t *testing.T) {
	got := perfRequest(t, User{Username: "root", Role: "admin"}, "", func(*State) {})
	if got.Metric != "gen_tps" || got.Range != "7d" || got.Group != "model" {
		t.Fatalf("defaults: metric=%q range=%q group=%q", got.Metric, got.Range, got.Group)
	}
}

// --- Display formatting ---

func TestFormatTPSAndMS(t *testing.T) {
	for _, tc := range []struct {
		in   float64
		want string
	}{
		{0, ""}, {-1, ""},
		{38.44, "38.4 tok/s"},
		{240.6, "241 tok/s"},
		{1240, "1.2k tok/s"},
	} {
		if got := formatTPS(tc.in); got != tc.want {
			t.Errorf("formatTPS(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
	for _, tc := range []struct {
		in   float64
		want string
	}{
		{0, ""}, {-1, ""},
		{412, "412 ms"},
		{8300, "8.3 s"},
		{61000, "1.0 min"},
	} {
		if got := formatMS(tc.in); got != tc.want {
			t.Errorf("formatMS(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNewClientPerfRow(t *testing.T) {
	// A machine that served nothing gets no perf block at all.
	if got := newClientPerfRow(PerfStats{}, nil); got != nil {
		t.Fatalf("empty stats produced a row: %+v", got)
	}

	p := PerfStats{
		Samples: 4, TotalMSSum: 8000, TotalMSMax: 5000, QueueMSSum: 400,
		TTFTSamples: 4, TTFTMSSum: 2000, TTFTMSMax: 1500,
		DecodeSamples: 4, DecodeMSSum: 4000, DecodeTokens: 200,
		BackendSamples: 4,
	}
	byModel := map[string]PerfStats{
		"qwen":  {Samples: 1, DecodeSamples: 1, DecodeMSSum: 1000, DecodeTokens: 20},
		"llama": {Samples: 3, DecodeSamples: 3, DecodeMSSum: 3000, DecodeTokens: 180},
		"idle":  {}, // never used on this machine; must not appear
	}
	row := newClientPerfRow(p, byModel)
	if row == nil {
		t.Fatal("nil row")
	}
	if row.Requests != 4 {
		t.Fatalf("requests: got %d, want 4", row.Requests)
	}
	if row.GenTPS != "50.0 tok/s" {
		t.Fatalf("gen tps: got %q", row.GenTPS)
	}
	// Prompt throughput was never measured, so it renders as absent rather than 0.
	if row.PromptTPS != "" {
		t.Fatalf("prompt tps invented: %q", row.PromptTPS)
	}
	if row.AvgTTFT != "500 ms" || row.MaxTTFT != "1.5 s" {
		t.Fatalf("ttft: avg %q max %q", row.AvgTTFT, row.MaxTTFT)
	}
	// Every sample was backend-reported, so nothing is flagged as approximate.
	if row.Est {
		t.Fatal("row flagged approximate when all samples were backend-reported")
	}
	if len(row.ByModel) != 2 {
		t.Fatalf("want 2 models (the unused one dropped), got %d: %+v", len(row.ByModel), row.ByModel)
	}
	// Sorted by name for a stable render.
	if row.ByModel[0].Name != "llama" || row.ByModel[1].Name != "qwen" {
		t.Fatalf("models not sorted: %+v", row.ByModel)
	}
	if row.ByModel[0].GenTPS != "60.0 tok/s" {
		t.Fatalf("per-model rate: got %q", row.ByModel[0].GenTPS)
	}
}

func TestNewClientPerfRow_FlagsApproximateSamples(t *testing.T) {
	// Three of four samples were timed by the router from the outside.
	row := newClientPerfRow(PerfStats{Samples: 4, BackendSamples: 1}, nil)
	if row == nil || !row.Est {
		t.Fatalf("row not flagged approximate: %+v", row)
	}
}

func TestPerfJSON_SeriesOrderIsDeterministic(t *testing.T) {
	// Three models with identical sample counts. The chart colours series by
	// position, so an unstable order would reshuffle the legend on every poll.
	now := time.Now().UTC().Truncate(time.Hour)
	seed := func(s *State) {
		for _, m := range []string{"zeta", "alpha", "mid"} {
			s.AddPerfDelta(PerfDelta{Bucket: now, Model: m, Samples: 5, TotalMSSum: 5000})
		}
	}
	want := []string{"alpha", "mid", "zeta"}
	for i := 0; i < 5; i++ {
		got := perfRequest(t, User{Username: "root", Role: "admin"},
			"range=24h&group=model&metric=total", seed)
		for j, s := range got.Series {
			if s.Name != want[j] {
				t.Fatalf("run %d: series order %v, want %v", i, names(got.Series), want)
			}
		}
	}
}

func names(series []perfSeriesJSON) []string {
	out := make([]string, len(series))
	for i, s := range series {
		out[i] = s.Name
	}
	return out
}

func TestPerfJSON_GroupByClientIsAdminOnly(t *testing.T) {
	// Grouping by client names other people's machines. A member's rows are scoped
	// to their own requests, but those may have been served by anyone's hardware,
	// so the series names would disclose a fleet the Clients page hides from them.
	now := time.Now().UTC().Truncate(time.Hour)
	seed := func(s *State) {
		s.AddPerfDelta(PerfDelta{
			Bucket: now, Owner: "alice", Model: "llama", Client: "bob/secret-gpu-rig",
			Samples: 1, TotalMSSum: 1000,
		})
	}

	// Admins may use it.
	got := perfRequest(t, User{Username: "root", Role: "admin"},
		"range=24h&group=client&metric=total", seed)
	if len(got.Series) != 1 || got.Series[0].Name != "bob/secret-gpu-rig" {
		t.Fatalf("admin should see the client grouping: %+v", got.Series)
	}

	// Members may not, even for requests that are genuinely theirs.
	s, _ := LoadState(filepath.Join(t.TempDir(), "state.json"))
	seed(s)
	a := &Admin{state: s, log: slog.Default()}
	r := httptest.NewRequest(http.MethodGet, "/portal/api/perf?range=24h&group=client&metric=total", nil)
	r = r.WithContext(context.WithValue(r.Context(), ctxUser, User{Username: "alice", Role: "member"}))
	w := httptest.NewRecorder()
	a.handlePerfJSON(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("member got status %d, want 403", w.Code)
	}
	if strings.Contains(w.Body.String(), "secret-gpu-rig") {
		t.Fatalf("machine name leaked in the rejection body: %s", w.Body.String())
	}
}
