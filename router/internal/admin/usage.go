package admin

import (
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Usage tracking: every completed inference request is accumulated into an
// hourly bucket keyed by (owner, api-key label, model) and persisted in the
// state database, so token usage can be charted over time in the portal.
// Writes are buffered in memory and flushed periodically to keep the request
// completion path off the database.

// usageRetention is how long hourly usage buckets are kept before pruning.
const usageRetention = 90 * 24 * time.Hour

// usageFlushInterval is how often buffered usage deltas are written out.
const usageFlushInterval = 30 * time.Second

// UsageDelta is one accumulated bucket of usage counters.
type UsageDelta struct {
	Bucket           time.Time // UTC, truncated to the hour
	Owner            string
	KeyLabel         string // "owner/label" of the API key
	Model            string
	Requests         int64
	PromptTokens     int64
	CompletionTokens int64
}

// AddUsageDelta merges a delta into its hourly bucket (upsert-add).
func (s *State) AddUsageDelta(d UsageDelta) error {
	_, err := s.db.Exec(`
		INSERT INTO usage_hourly (bucket, owner, key_label, model, requests, prompt_tokens, completion_tokens)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(bucket, owner, key_label, model) DO UPDATE SET
			requests          = requests + excluded.requests,
			prompt_tokens     = prompt_tokens + excluded.prompt_tokens,
			completion_tokens = completion_tokens + excluded.completion_tokens`,
		d.Bucket.UTC().Truncate(time.Hour).Format(time.RFC3339),
		d.Owner, d.KeyLabel, d.Model,
		d.Requests, d.PromptTokens, d.CompletionTokens,
	)
	return err
}

// UsageModels returns every distinct model with recorded usage, oldest retained
// bucket onward. Needed by the pricing UI: because rates apply retroactively, the
// models worth pricing include ones no longer served by any client — a retired
// model still owns its share of a historical bill, and would otherwise be
// unpriceable through the portal.
func (s *State) UsageModels() ([]string, error) {
	rows, err := s.db.Query(`SELECT DISTINCT model FROM usage_hourly WHERE model <> '' ORDER BY model`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// PruneUsageBefore deletes usage buckets older than cutoff.
func (s *State) PruneUsageBefore(cutoff time.Time) error {
	_, err := s.db.Exec(`DELETE FROM usage_hourly WHERE bucket < ?`,
		cutoff.UTC().Format(time.RFC3339))
	return err
}

// UsageRow is one (name, bucket) cell of aggregated usage.
type UsageRow struct {
	Bucket           string // RFC3339 hour or YYYY-MM-DD day, per query granularity
	Name             string // model, owner, or key label depending on grouping
	Requests         int64
	PromptTokens     int64
	CompletionTokens int64
	CostCounters
}

// CostCounters are the money figures derived from token counts and the model
// rates in force at query time. Charged and estimated are kept apart at every
// grouping so a modelled figure is never added into a real one; unpriced counts
// the requests that contributed no cost at all, which is what stops a total from
// looking complete when it is not.
type CostCounters struct {
	ActualCostMicro    int64 `json:"actual_cost_micro"`
	EstimatedCostMicro int64 `json:"estimated_cost_micro"`
	UnpricedRequests   int64 `json:"unpriced_requests"`
}

// TotalCostMicro is charged plus estimated. Only meaningful where the split has
// already been shown, or the estimate would masquerade as spend.
func (c CostCounters) TotalCostMicro() int64 { return c.ActualCostMicro + c.EstimatedCostMicro }

// costSelect is the cost arithmetic shared by every usage query. The division by
// tokensPerRateUnit is applied to the summed product rather than per row, so a
// group of many small requests is not eroded by repeated integer truncation.
//
// A model with no pricing row joins to NULL and contributes zero via COALESCE,
// while still being counted in unpriced_requests. Rows are attributed to
// estimated unless the basis says otherwise, so an unrecognised basis value
// errs toward "not a real charge".
const costSelect = `,
	SUM(CASE WHEN p.basis = 'actual'
		THEN u.prompt_tokens * COALESCE(p.input_ppm, 0) + u.completion_tokens * COALESCE(p.output_ppm, 0)
		ELSE 0 END) / 1000000,
	SUM(CASE WHEN p.basis = 'actual' THEN 0
		ELSE u.prompt_tokens * COALESCE(p.input_ppm, 0) + u.completion_tokens * COALESCE(p.output_ppm, 0)
		END) / 1000000,
	SUM(CASE WHEN COALESCE(p.input_ppm, 0) = 0 AND COALESCE(p.output_ppm, 0) = 0
		THEN u.requests ELSE 0 END)`

// usageFrom joins recorded usage to the rates currently in force. A LEFT JOIN is
// required, not an inner one: unpriced models must still appear in token and
// request figures.
const usageFrom = ` FROM usage_hourly u LEFT JOIN model_pricing p ON p.model = u.model`

// QueryUsage returns usage between since and until (exclusive), grouped by
// groupBy ("model", "owner", or "key") per bucket. daily=true aggregates the
// hourly buckets into days (bucket format YYYY-MM-DD); otherwise buckets are
// RFC3339 hours. If owner is non-empty, only that owner's usage is included.
func (s *State) QueryUsage(since, until time.Time, groupBy string, daily bool, owner string) ([]UsageRow, error) {
	// Columns are qualified because the pricing join puts a second `model` in scope.
	var nameCol string
	switch groupBy {
	case "model":
		nameCol = "u.model"
	case "owner":
		nameCol = "u.owner"
	case "key":
		nameCol = "u.key_label"
	default:
		return nil, fmt.Errorf("invalid group %q", groupBy)
	}
	bucketExpr := "u.bucket"
	if daily {
		bucketExpr = "substr(u.bucket, 1, 10)"
	}
	q := `SELECT ` + bucketExpr + ` AS b, ` + nameCol + `, SUM(u.requests), SUM(u.prompt_tokens), SUM(u.completion_tokens)` +
		costSelect + usageFrom + ` WHERE u.bucket >= ? AND u.bucket < ?`
	args := []any{since.UTC().Format(time.RFC3339), until.UTC().Format(time.RFC3339)}
	if owner != "" {
		q += ` AND u.owner = ?`
		args = append(args, owner)
	}
	q += ` GROUP BY b, ` + nameCol + ` ORDER BY b, ` + nameCol
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsageRow
	for rows.Next() {
		var r UsageRow
		if err := rows.Scan(&r.Bucket, &r.Name, &r.Requests, &r.PromptTokens, &r.CompletionTokens,
			&r.ActualCostMicro, &r.EstimatedCostMicro, &r.UnpricedRequests); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UsageTotalsResult is the aggregate read shape. Deliberately not UsageDelta:
// cost is derived at read time and never written, so a struct that AddUsageDelta
// accepts must not carry money fields someone could set and expect to persist.
type UsageTotalsResult struct {
	Requests         int64
	PromptTokens     int64
	CompletionTokens int64
	CostCounters
}

// UsageTotals sums usage between since and until for one owner ("" = all),
// including cost under the rates currently in force.
func (s *State) UsageTotals(since, until time.Time, owner string) (UsageTotalsResult, error) {
	q := `SELECT COALESCE(SUM(u.requests),0), COALESCE(SUM(u.prompt_tokens),0), COALESCE(SUM(u.completion_tokens),0),
		COALESCE(SUM(CASE WHEN p.basis = 'actual'
			THEN u.prompt_tokens * COALESCE(p.input_ppm, 0) + u.completion_tokens * COALESCE(p.output_ppm, 0)
			ELSE 0 END), 0) / 1000000,
		COALESCE(SUM(CASE WHEN p.basis = 'actual' THEN 0
			ELSE u.prompt_tokens * COALESCE(p.input_ppm, 0) + u.completion_tokens * COALESCE(p.output_ppm, 0)
			END), 0) / 1000000,
		COALESCE(SUM(CASE WHEN COALESCE(p.input_ppm, 0) = 0 AND COALESCE(p.output_ppm, 0) = 0
			THEN u.requests ELSE 0 END), 0)` +
		usageFrom + ` WHERE u.bucket >= ? AND u.bucket < ?`
	args := []any{since.UTC().Format(time.RFC3339), until.UTC().Format(time.RFC3339)}
	if owner != "" {
		q += ` AND u.owner = ?`
		args = append(args, owner)
	}
	var t UsageTotalsResult
	err := s.db.QueryRow(q, args...).Scan(&t.Requests, &t.PromptTokens, &t.CompletionTokens,
		&t.ActualCostMicro, &t.EstimatedCostMicro, &t.UnpricedRequests)
	return t, err
}

// --- Buffered recorder ---

type usageKey struct {
	bucket   time.Time
	owner    string
	keyLabel string
	model    string
}

// UsageRecorder buffers per-request usage in memory and flushes the merged
// deltas to the state database on an interval. Safe for concurrent use.
type UsageRecorder struct {
	state *State
	log   *slog.Logger

	mu  sync.Mutex
	buf map[usageKey]*UsageDelta

	stop     chan struct{}
	stopOnce sync.Once
	done     chan struct{}
}

// NewUsageRecorder creates a recorder and starts its flush loop.
func NewUsageRecorder(state *State, log *slog.Logger) *UsageRecorder {
	r := &UsageRecorder{
		state: state,
		log:   log,
		buf:   make(map[usageKey]*UsageDelta),
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	go r.loop()
	return r
}

// RecordUsage accumulates usage for one completed request. Satisfies the API
// handler's usage-recorder interface.
func (r *UsageRecorder) RecordUsage(model, owner, keyLabel string, prompt, completion int) {
	if model == "" {
		return
	}
	k := usageKey{
		bucket:   time.Now().UTC().Truncate(time.Hour),
		owner:    owner,
		keyLabel: keyLabel,
		model:    model,
	}
	r.mu.Lock()
	d, ok := r.buf[k]
	if !ok {
		d = &UsageDelta{Bucket: k.bucket, Owner: owner, KeyLabel: keyLabel, Model: model}
		r.buf[k] = d
	}
	d.Requests++
	d.PromptTokens += int64(prompt)
	d.CompletionTokens += int64(completion)
	r.mu.Unlock()
}

func (r *UsageRecorder) loop() {
	defer close(r.done)
	ticker := time.NewTicker(usageFlushInterval)
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

func (r *UsageRecorder) pruneOld() {
	if err := r.state.PruneUsageBefore(time.Now().Add(-usageRetention)); err != nil {
		r.log.Warn("usage: prune failed", "error", err)
	}
}

// Flush writes all buffered deltas to the database.
func (r *UsageRecorder) Flush() {
	r.mu.Lock()
	buf := r.buf
	r.buf = make(map[usageKey]*UsageDelta)
	r.mu.Unlock()
	for _, d := range buf {
		if err := r.state.AddUsageDelta(*d); err != nil {
			r.log.Warn("usage: flush failed", "error", err)
			// Re-buffer so a transient DB error doesn't lose the counters.
			r.mu.Lock()
			k := usageKey{bucket: d.Bucket, owner: d.Owner, keyLabel: d.KeyLabel, model: d.Model}
			if cur, ok := r.buf[k]; ok {
				cur.Requests += d.Requests
				cur.PromptTokens += d.PromptTokens
				cur.CompletionTokens += d.CompletionTokens
			} else {
				r.buf[k] = d
			}
			r.mu.Unlock()
		}
	}
}

// Close flushes outstanding usage and stops the flush loop.
func (r *UsageRecorder) Close() {
	r.stopOnce.Do(func() { close(r.stop) })
	<-r.done
}
