package admin

import (
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Inference performance tracking. Every completed request contributes one sample
// of timing and throughput data, accumulated into an hourly bucket keyed by
// (owner, api-key label, model, client) and persisted in the state database so
// the portal can chart speed over time and compare client machines.
//
// Buckets hold sums and counts rather than individual samples. That keeps the
// table small — a handful of rows per hour regardless of request volume — and
// still yields the statistic that matters for throughput: total tokens divided
// by total time, which weights each request by its size instead of letting one
// three-token reply count as much as a 4k-token one. Maxima are carried
// alongside so a slow outlier stays visible after averaging.
//
// Exact percentiles are deliberately not stored here; the Prometheus /metrics
// endpoint reports p50/p95/p99 over its own rolling window for that.

// perfRetention is how long hourly performance buckets are kept before pruning.
// Matches usageRetention so both charts cover the same span.
const perfRetention = 90 * 24 * time.Hour

// perfFlushInterval is how often buffered performance deltas are written out.
const perfFlushInterval = 30 * time.Second

// PerfSample is one completed request's measurements, as handed to the recorder.
// Durations are milliseconds. Zero means "not measured for this request" and is
// skipped rather than averaged in as a genuine zero.
type PerfSample struct {
	Owner    string // username of the API-key holder
	KeyLabel string // "owner/label" of the API key used
	Model    string
	Client   string // "owner/name" of the client that served it; "" if unknown

	QueueMS float64 // enqueue → dispatch: time spent waiting for a free worker
	TotalMS float64 // enqueue → done: end-to-end, what the caller experienced
	// TTFTMS is dispatch → first token, so it measures the model rather than the
	// backlog; add QueueMS for the delay the caller actually saw. Matches the
	// /metrics histogram and the in-flight jobs view. 0 for non-streaming requests,
	// whose single terminal chunk gives no first-token signal.
	TTFTMS float64

	// PrefillMS/PrefillTokens and DecodeMS/DecodeTokens are throughput pairs.
	// Each is recorded only when both halves are known, so a rate can always be
	// computed from the sums.
	PrefillMS     float64
	PrefillTokens int
	DecodeMS      float64
	DecodeTokens  int

	// FromBackend marks samples whose prefill/decode split came from the
	// backend's own timings rather than the router's outside observation. Tracked
	// so the portal can say how much of a figure is measured versus inferred.
	FromBackend bool
}

// PerfDelta is one accumulated bucket of performance counters.
type PerfDelta struct {
	Bucket   time.Time // UTC, truncated to the hour
	Owner    string
	KeyLabel string
	Model    string
	Client   string

	Samples int64

	QueueMSSum float64
	QueueMSMax float64
	TotalMSSum float64
	TotalMSMax float64

	TTFTSamples int64
	TTFTMSSum   float64
	TTFTMSMax   float64

	PrefillSamples int64
	PrefillMSSum   float64
	PrefillTokens  int64

	DecodeSamples int64
	DecodeMSSum   float64
	DecodeTokens  int64

	BackendSamples int64
}

// add merges a sample into the delta, tracking each measure's own sample count so
// requests that only report some measures don't skew the others' averages.
func (d *PerfDelta) add(s PerfSample) {
	d.Samples++
	d.QueueMSSum += s.QueueMS
	if s.QueueMS > d.QueueMSMax {
		d.QueueMSMax = s.QueueMS
	}
	d.TotalMSSum += s.TotalMS
	if s.TotalMS > d.TotalMSMax {
		d.TotalMSMax = s.TotalMS
	}
	if s.TTFTMS > 0 {
		d.TTFTSamples++
		d.TTFTMSSum += s.TTFTMS
		if s.TTFTMS > d.TTFTMSMax {
			d.TTFTMSMax = s.TTFTMS
		}
	}
	if s.PrefillMS > 0 && s.PrefillTokens > 0 {
		d.PrefillSamples++
		d.PrefillMSSum += s.PrefillMS
		d.PrefillTokens += int64(s.PrefillTokens)
	}
	if s.DecodeMS > 0 && s.DecodeTokens > 0 {
		d.DecodeSamples++
		d.DecodeMSSum += s.DecodeMS
		d.DecodeTokens += int64(s.DecodeTokens)
	}
	if s.FromBackend {
		d.BackendSamples++
	}
}

// merge folds other into d. Used to re-buffer a delta after a failed flush.
func (d *PerfDelta) merge(o PerfDelta) {
	d.Samples += o.Samples
	d.QueueMSSum += o.QueueMSSum
	if o.QueueMSMax > d.QueueMSMax {
		d.QueueMSMax = o.QueueMSMax
	}
	d.TotalMSSum += o.TotalMSSum
	if o.TotalMSMax > d.TotalMSMax {
		d.TotalMSMax = o.TotalMSMax
	}
	d.TTFTSamples += o.TTFTSamples
	d.TTFTMSSum += o.TTFTMSSum
	if o.TTFTMSMax > d.TTFTMSMax {
		d.TTFTMSMax = o.TTFTMSMax
	}
	d.PrefillSamples += o.PrefillSamples
	d.PrefillMSSum += o.PrefillMSSum
	d.PrefillTokens += o.PrefillTokens
	d.DecodeSamples += o.DecodeSamples
	d.DecodeMSSum += o.DecodeMSSum
	d.DecodeTokens += o.DecodeTokens
	d.BackendSamples += o.BackendSamples
}

// AddPerfDelta merges a delta into its hourly bucket (upsert-add). Sums and
// counts accumulate; maxima take whichever value is larger.
func (s *State) AddPerfDelta(d PerfDelta) error {
	_, err := s.db.Exec(`
		INSERT INTO perf_hourly (
			bucket, owner, key_label, model, client, samples,
			queue_ms_sum, queue_ms_max, total_ms_sum, total_ms_max,
			ttft_samples, ttft_ms_sum, ttft_ms_max,
			prefill_samples, prefill_ms_sum, prefill_tokens,
			decode_samples, decode_ms_sum, decode_tokens,
			backend_samples
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(bucket, owner, key_label, model, client) DO UPDATE SET
			samples         = samples + excluded.samples,
			queue_ms_sum    = queue_ms_sum + excluded.queue_ms_sum,
			queue_ms_max    = MAX(queue_ms_max, excluded.queue_ms_max),
			total_ms_sum    = total_ms_sum + excluded.total_ms_sum,
			total_ms_max    = MAX(total_ms_max, excluded.total_ms_max),
			ttft_samples    = ttft_samples + excluded.ttft_samples,
			ttft_ms_sum     = ttft_ms_sum + excluded.ttft_ms_sum,
			ttft_ms_max     = MAX(ttft_ms_max, excluded.ttft_ms_max),
			prefill_samples = prefill_samples + excluded.prefill_samples,
			prefill_ms_sum  = prefill_ms_sum + excluded.prefill_ms_sum,
			prefill_tokens  = prefill_tokens + excluded.prefill_tokens,
			decode_samples  = decode_samples + excluded.decode_samples,
			decode_ms_sum   = decode_ms_sum + excluded.decode_ms_sum,
			decode_tokens   = decode_tokens + excluded.decode_tokens,
			backend_samples = backend_samples + excluded.backend_samples`,
		d.Bucket.UTC().Truncate(time.Hour).Format(time.RFC3339),
		d.Owner, d.KeyLabel, d.Model, d.Client, d.Samples,
		d.QueueMSSum, d.QueueMSMax, d.TotalMSSum, d.TotalMSMax,
		d.TTFTSamples, d.TTFTMSSum, d.TTFTMSMax,
		d.PrefillSamples, d.PrefillMSSum, d.PrefillTokens,
		d.DecodeSamples, d.DecodeMSSum, d.DecodeTokens,
		d.BackendSamples,
	)
	return err
}

// PrunePerfBefore deletes performance buckets older than cutoff.
func (s *State) PrunePerfBefore(cutoff time.Time) error {
	_, err := s.db.Exec(`DELETE FROM perf_hourly WHERE bucket < ?`,
		cutoff.UTC().Format(time.RFC3339))
	return err
}

// PerfRow is one (name, bucket) cell of aggregated performance data. It carries
// raw sums so the caller can derive rates and averages; see PerfStats.
type PerfRow struct {
	Bucket string // RFC3339 hour or YYYY-MM-DD day, per query granularity
	Name   string // model, owner, key label, or client depending on grouping
	PerfStats
}

// PerfStats holds the summable performance counters for a set of requests, plus
// the derived accessors the portal renders. Keeping the sums (rather than
// pre-averaged values) means rows can be added together at any granularity.
type PerfStats struct {
	Samples        int64   `json:"samples"`
	QueueMSSum     float64 `json:"queue_ms_sum"`
	QueueMSMax     float64 `json:"queue_ms_max"`
	TotalMSSum     float64 `json:"total_ms_sum"`
	TotalMSMax     float64 `json:"total_ms_max"`
	TTFTSamples    int64   `json:"ttft_samples"`
	TTFTMSSum      float64 `json:"ttft_ms_sum"`
	TTFTMSMax      float64 `json:"ttft_ms_max"`
	PrefillSamples int64   `json:"prefill_samples"`
	PrefillMSSum   float64 `json:"prefill_ms_sum"`
	PrefillTokens  int64   `json:"prefill_tokens"`
	DecodeSamples  int64   `json:"decode_samples"`
	DecodeMSSum    float64 `json:"decode_ms_sum"`
	DecodeTokens   int64   `json:"decode_tokens"`
	BackendSamples int64   `json:"backend_samples"`
}

// Add folds another set of counters into p.
func (p *PerfStats) Add(o PerfStats) {
	p.Samples += o.Samples
	p.QueueMSSum += o.QueueMSSum
	if o.QueueMSMax > p.QueueMSMax {
		p.QueueMSMax = o.QueueMSMax
	}
	p.TotalMSSum += o.TotalMSSum
	if o.TotalMSMax > p.TotalMSMax {
		p.TotalMSMax = o.TotalMSMax
	}
	p.TTFTSamples += o.TTFTSamples
	p.TTFTMSSum += o.TTFTMSSum
	if o.TTFTMSMax > p.TTFTMSMax {
		p.TTFTMSMax = o.TTFTMSMax
	}
	p.PrefillSamples += o.PrefillSamples
	p.PrefillMSSum += o.PrefillMSSum
	p.PrefillTokens += o.PrefillTokens
	p.DecodeSamples += o.DecodeSamples
	p.DecodeMSSum += o.DecodeMSSum
	p.DecodeTokens += o.DecodeTokens
	p.BackendSamples += o.BackendSamples
}

// AvgTTFTMS is the mean time from dispatch to first token, over the streaming
// requests that produced one. Excludes queue wait; see AvgQueueMS. 0 when none did.
func (p PerfStats) AvgTTFTMS() float64 {
	if p.TTFTSamples == 0 {
		return 0
	}
	return p.TTFTMSSum / float64(p.TTFTSamples)
}

// AvgQueueMS is the mean time a request waited for a free worker.
func (p PerfStats) AvgQueueMS() float64 {
	if p.Samples == 0 {
		return 0
	}
	return p.QueueMSSum / float64(p.Samples)
}

// AvgTotalMS is the mean end-to-end request duration.
func (p PerfStats) AvgTotalMS() float64 {
	if p.Samples == 0 {
		return 0
	}
	return p.TotalMSSum / float64(p.Samples)
}

// PromptTokensPerSec is prompt-processing (prefill) throughput: all prompt
// tokens evaluated divided by all the time spent evaluating them. Weighting by
// total tokens rather than averaging per-request rates keeps a one-token prompt
// from counting as heavily as a 4k-token one.
func (p PerfStats) PromptTokensPerSec() float64 {
	if p.PrefillMSSum <= 0 {
		return 0
	}
	return float64(p.PrefillTokens) / (p.PrefillMSSum / 1000)
}

// GenTokensPerSec is token-generation (decode) throughput, weighted the same way
// as PromptTokensPerSec.
func (p PerfStats) GenTokensPerSec() float64 {
	if p.DecodeMSSum <= 0 {
		return 0
	}
	return float64(p.DecodeTokens) / (p.DecodeMSSum / 1000)
}

// BackendMeasuredFrac is the fraction of samples whose prefill/decode split came
// from the backend's own timings rather than router-side observation, in [0,1].
func (p PerfStats) BackendMeasuredFrac() float64 {
	if p.Samples == 0 {
		return 0
	}
	return float64(p.BackendSamples) / float64(p.Samples)
}

// perfGroupColumn maps a grouping name onto its column, rejecting anything else
// so the value can be interpolated into the query.
func perfGroupColumn(groupBy string) (string, error) {
	switch groupBy {
	case "model":
		return "model", nil
	case "owner":
		return "owner", nil
	case "key":
		return "key_label", nil
	case "client":
		return "client", nil
	}
	return "", fmt.Errorf("invalid group %q", groupBy)
}

// perfSumColumns is the SELECT list of aggregated counters, in the order
// perfStatsTargets reads them. Every aggregate is wrapped in COALESCE because an
// ungrouped SUM/MAX over zero rows yields one row of NULLs, which would fail to
// scan into the numeric fields.
const perfSumColumns = `COALESCE(SUM(samples),0), COALESCE(SUM(queue_ms_sum),0), COALESCE(MAX(queue_ms_max),0),
	COALESCE(SUM(total_ms_sum),0), COALESCE(MAX(total_ms_max),0),
	COALESCE(SUM(ttft_samples),0), COALESCE(SUM(ttft_ms_sum),0), COALESCE(MAX(ttft_ms_max),0),
	COALESCE(SUM(prefill_samples),0), COALESCE(SUM(prefill_ms_sum),0), COALESCE(SUM(prefill_tokens),0),
	COALESCE(SUM(decode_samples),0), COALESCE(SUM(decode_ms_sum),0), COALESCE(SUM(decode_tokens),0),
	COALESCE(SUM(backend_samples),0)`

// perfStatsTargets returns scan destinations for perfSumColumns.
func perfStatsTargets(p *PerfStats) []any {
	return []any{
		&p.Samples, &p.QueueMSSum, &p.QueueMSMax,
		&p.TotalMSSum, &p.TotalMSMax,
		&p.TTFTSamples, &p.TTFTMSSum, &p.TTFTMSMax,
		&p.PrefillSamples, &p.PrefillMSSum, &p.PrefillTokens,
		&p.DecodeSamples, &p.DecodeMSSum, &p.DecodeTokens,
		&p.BackendSamples,
	}
}

// QueryPerf returns performance counters between since and until (exclusive),
// grouped by groupBy ("model", "owner", "key", or "client") per bucket.
// daily=true aggregates the hourly buckets into days (bucket format YYYY-MM-DD);
// otherwise buckets are RFC3339 hours. If owner is non-empty, only that owner's
// requests are included.
func (s *State) QueryPerf(since, until time.Time, groupBy string, daily bool, owner string) ([]PerfRow, error) {
	nameCol, err := perfGroupColumn(groupBy)
	if err != nil {
		return nil, err
	}
	bucketExpr := "bucket"
	if daily {
		bucketExpr = "substr(bucket, 1, 10)"
	}
	q := `SELECT ` + bucketExpr + ` AS b, ` + nameCol + `, ` + perfSumColumns + `
		FROM perf_hourly WHERE bucket >= ? AND bucket < ?`
	args := []any{since.UTC().Format(time.RFC3339), until.UTC().Format(time.RFC3339)}
	if owner != "" {
		q += ` AND owner = ?`
		args = append(args, owner)
	}
	q += ` GROUP BY b, ` + nameCol + ` ORDER BY b, ` + nameCol
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PerfRow
	for rows.Next() {
		var r PerfRow
		dest := append([]any{&r.Bucket, &r.Name}, perfStatsTargets(&r.PerfStats)...)
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PerfTotals sums performance counters between since and until for one owner
// ("" = all), without bucketing.
func (s *State) PerfTotals(since, until time.Time, owner string) (PerfStats, error) {
	q := `SELECT ` + perfSumColumns + ` FROM perf_hourly WHERE bucket >= ? AND bucket < ?`
	args := []any{since.UTC().Format(time.RFC3339), until.UTC().Format(time.RFC3339)}
	if owner != "" {
		q += ` AND owner = ?`
		args = append(args, owner)
	}
	var p PerfStats
	err := s.db.QueryRow(q, args...).Scan(perfStatsTargets(&p)...)
	return p, err
}

// PerfByClient returns per-client performance counters between since and until,
// keyed by the client's "owner/name". Restricted to one requesting owner when
// owner is non-empty. Used by the Clients page to show each machine's speed.
func (s *State) PerfByClient(since, until time.Time, owner string) (map[string]PerfStats, error) {
	q := `SELECT client, ` + perfSumColumns + ` FROM perf_hourly
		WHERE bucket >= ? AND bucket < ? AND client <> ''`
	args := []any{since.UTC().Format(time.RFC3339), until.UTC().Format(time.RFC3339)}
	if owner != "" {
		q += ` AND owner = ?`
		args = append(args, owner)
	}
	q += ` GROUP BY client`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]PerfStats)
	for rows.Next() {
		var name string
		var p PerfStats
		dest := append([]any{&name}, perfStatsTargets(&p)...)
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		out[name] = p
	}
	return out, rows.Err()
}

// PerfByClientModel returns per-(client, model) performance counters, so the
// Clients page can break a machine's speed down by the model that produced it.
// The outer key is the client's "owner/name", the inner key the model.
func (s *State) PerfByClientModel(since, until time.Time, owner string) (map[string]map[string]PerfStats, error) {
	q := `SELECT client, model, ` + perfSumColumns + ` FROM perf_hourly
		WHERE bucket >= ? AND bucket < ? AND client <> ''`
	args := []any{since.UTC().Format(time.RFC3339), until.UTC().Format(time.RFC3339)}
	if owner != "" {
		q += ` AND owner = ?`
		args = append(args, owner)
	}
	q += ` GROUP BY client, model`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]map[string]PerfStats)
	for rows.Next() {
		var client, model string
		var p PerfStats
		dest := append([]any{&client, &model}, perfStatsTargets(&p)...)
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		if out[client] == nil {
			out[client] = make(map[string]PerfStats)
		}
		out[client][model] = p
	}
	return out, rows.Err()
}

// --- Buffered recorder ---

type perfKey struct {
	bucket   time.Time
	owner    string
	keyLabel string
	model    string
	client   string
}

// PerfRecorder buffers per-request performance samples in memory and flushes the
// merged deltas to the state database on an interval, keeping the request
// completion path off the database. Safe for concurrent use.
type PerfRecorder struct {
	state *State
	log   *slog.Logger

	mu  sync.Mutex
	buf map[perfKey]*PerfDelta

	stop     chan struct{}
	stopOnce sync.Once
	done     chan struct{}
}

// NewPerfRecorder creates a recorder and starts its flush loop.
func NewPerfRecorder(state *State, log *slog.Logger) *PerfRecorder {
	r := &PerfRecorder{
		state: state,
		log:   log,
		buf:   make(map[perfKey]*PerfDelta),
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	go r.loop()
	return r
}

// RecordPerf accumulates one completed request's measurements. Satisfies the
// hub's performance-recorder interface.
func (r *PerfRecorder) RecordPerf(s PerfSample) {
	if s.Model == "" {
		return
	}
	k := perfKey{
		bucket:   time.Now().UTC().Truncate(time.Hour),
		owner:    s.Owner,
		keyLabel: s.KeyLabel,
		model:    s.Model,
		client:   s.Client,
	}
	r.mu.Lock()
	d, ok := r.buf[k]
	if !ok {
		d = &PerfDelta{
			Bucket: k.bucket, Owner: s.Owner, KeyLabel: s.KeyLabel,
			Model: s.Model, Client: s.Client,
		}
		r.buf[k] = d
	}
	d.add(s)
	r.mu.Unlock()
}

func (r *PerfRecorder) loop() {
	defer close(r.done)
	ticker := time.NewTicker(perfFlushInterval)
	defer ticker.Stop()
	// Prune old buckets once per day; the first prune runs on startup.
	prune := time.NewTicker(24 * time.Hour)
	defer prune.Stop()
	r.pruneOld()
	for {
		select {
		case <-ticker.C:
			r.Flush()
		case <-prune.C:
			r.pruneOld()
		case <-r.stop:
			r.Flush()
			return
		}
	}
}

func (r *PerfRecorder) pruneOld() {
	if err := r.state.PrunePerfBefore(time.Now().Add(-perfRetention)); err != nil {
		r.log.Warn("perf: prune failed", "error", err)
	}
}

// Flush writes all buffered deltas to the database.
func (r *PerfRecorder) Flush() {
	r.mu.Lock()
	buf := r.buf
	r.buf = make(map[perfKey]*PerfDelta)
	r.mu.Unlock()
	for _, d := range buf {
		if err := r.state.AddPerfDelta(*d); err != nil {
			r.log.Warn("perf: flush failed", "error", err)
			// Re-buffer so a transient DB error doesn't lose the counters.
			r.mu.Lock()
			k := perfKey{
				bucket: d.Bucket, owner: d.Owner, keyLabel: d.KeyLabel,
				model: d.Model, client: d.Client,
			}
			if cur, ok := r.buf[k]; ok {
				cur.merge(*d)
			} else {
				r.buf[k] = d
			}
			r.mu.Unlock()
		}
	}
}

// Close flushes outstanding samples and stops the flush loop.
func (r *PerfRecorder) Close() {
	r.stopOnce.Do(func() { close(r.stop) })
	<-r.done
}
