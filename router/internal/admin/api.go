package admin

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"llmesh/router/internal/logring"
)

type statRowJSON struct {
	Name             string `json:"name"`
	Requests         int64  `json:"requests"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
}

type dashboardJSON struct {
	TotalRequests int64               `json:"total_requests"`
	ActiveClients int                 `json:"active_clients"`
	APIKeyCount   int                 `json:"api_key_count"`
	TokenCount    int                 `json:"token_count"`
	ActiveModels  []string            `json:"active_models"`
	ActiveAliases map[string][]string `json:"active_aliases"`
	Clients       []clientJSON        `json:"clients"`
	StatsByModel  []statRowJSON       `json:"stats_by_model,omitempty"`
	StatsByUser   []statRowJSON       `json:"stats_by_user,omitempty"`
}

type clientJSON struct {
	Name        string `json:"name"`
	Status      string `json:"status"`
	StatusClass string `json:"status_class"`
	StatusLabel string `json:"status_label"`
	LastSeen    string `json:"last_seen,omitempty"`
	Models      string `json:"models,omitempty"`
	Version     string `json:"version,omitempty"`
}

func toStatRowJSON(rows []StatRow) []statRowJSON {
	if len(rows) == 0 {
		return nil
	}
	out := make([]statRowJSON, len(rows))
	for i, r := range rows {
		out[i] = statRowJSON{
			Name:             r.Name,
			Requests:         r.Requests,
			PromptTokens:     r.PromptTokens,
			CompletionTokens: r.CompletionTokens,
		}
	}
	return out
}

// ─── Logs API ─────────────────────────────────────────────────────────────────

type logEntryJSON struct {
	Time    string            `json:"time"`
	Level   string            `json:"level"`
	Message string            `json:"msg"`
	Attrs   map[string]string `json:"attrs,omitempty"`
}

type logsResponseJSON struct {
	Category string         `json:"category"`
	Entries  []logEntryJSON `json:"entries"`
}

func (a *Admin) handleLogsJSON(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	category := r.URL.Query().Get("category")
	valid := false
	for _, c := range logring.Categories() {
		if c == category {
			valid = true
			break
		}
	}
	if !valid {
		http.Error(w, "invalid category", http.StatusBadRequest)
		return
	}
	limit := 200
	if ls := r.URL.Query().Get("limit"); ls != "" {
		if n, err := strconv.Atoi(ls); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	entries := a.sink.Query(category, limit)
	out := make([]logEntryJSON, len(entries))
	for i, e := range entries {
		out[i] = logEntryJSON{
			Time:    e.Time.Format("2006-01-02T15:04:05.000Z07:00"),
			Level:   e.Level,
			Message: e.Message,
			Attrs:   e.Attrs,
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(logsResponseJSON{Category: category, Entries: out})
}

// ─── Dashboard API ────────────────────────────────────────────────────────────

func (a *Admin) handleDashboardJSON(w http.ResponseWriter, r *http.Request) {
	tokens := a.state.ClientTokensFor("", true)
	clients := make([]clientJSON, 0, len(tokens))
	for _, t := range tokens {
		c := clientJSON{Name: t.Owner + "/" + t.Name}
		connCount := a.hub.ConnectedCountByToken(t.TokenHash)
		ls := a.hub.LastSeenTime(t.TokenHash)
		c.Status, c.StatusClass, c.StatusLabel = clientStatusBadge(connCount, !ls.IsZero())
		if connCount > 0 {
			mods := a.hub.ConnectedModels(t.TokenHash)
			sort.Strings(mods)
			c.Models = strings.Join(mods, ", ")
			c.Version = a.hub.ConnectedVersion(t.TokenHash)
		} else if !ls.IsZero() {
			c.LastSeen = humanTime(ls)
		}
		clients = append(clients, c)
	}

	activeModels := a.hub.ActiveModels()
	sort.Strings(activeModels)

	resp := dashboardJSON{
		TotalRequests: a.reqCount(),
		ActiveClients: a.hub.ActiveClientCount(),
		APIKeyCount:   a.state.APIKeyCount(),
		TokenCount:    a.state.ClientTokenCount(),
		ActiveModels:  activeModels,
		ActiveAliases: a.state.AliasMap(),
		Clients:       clients,
		StatsByModel:  toStatRowJSON(statsRows(a.stats, true)),
		StatsByUser:   toStatRowJSON(statsRows(a.stats, false)),
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// ─── Jobs API ─────────────────────────────────────────────────────────────────

type jobStatJSON struct {
	ID              string `json:"id"`
	Phase           string `json:"phase"`                    // "processing" | "generating"
	DeltaCount      int64  `json:"delta_count"`              // tokens generated so far
	TTFTMs          int64  `json:"ttft_ms,omitempty"`        // time-to-first-token in ms
	FirstChunkAtISO string `json:"first_chunk_at,omitempty"` // RFC3339
}

// handleJobsJSON returns live stats for all in-flight jobs visible to the caller.
func (a *Admin) handleJobsJSON(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	u := ctxGetUser(r)
	recs := a.hub.AllInFlightJobs()
	out := make([]jobStatJSON, 0, len(recs))
	for _, rec := range recs {
		if u.Role != "admin" && rec.Req.Owner != u.Username && rec.ClientOwner != u.Username {
			continue
		}
		stat := jobStatJSON{
			ID:         rec.Req.ID,
			Phase:      "processing",
			DeltaCount: rec.DeltaCount(),
		}
		if fc := rec.FirstChunkAt(); fc != nil {
			stat.Phase = "generating"
			stat.FirstChunkAtISO = fc.UTC().Format("2006-01-02T15:04:05Z07:00")
		}
		// Output means generating either way, but only a streaming request has a
		// first-token moment distinct from its completion, so only it has a TTFT to
		// show — the same rule the histograms and hourly buckets follow.
		if ft := rec.FirstTokenAt(); ft != nil {
			stat.TTFTMs = ft.Sub(rec.DispatchedAt).Milliseconds()
		}
		out = append(out, stat)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(out)
}

// ─── Usage API ────────────────────────────────────────────────────────────────

type usageSeriesJSON struct {
	Name             string  `json:"name"`
	Requests         []int64 `json:"requests"`
	PromptTokens     []int64 `json:"prompt_tokens"`
	CompletionTokens []int64 `json:"completion_tokens"`
	// Cost is charged plus estimated per bucket, in micro-units, for charting.
	// The split is not per-bucket: a stacked chart of one series cannot show two
	// bases at once, and the split that matters is reported in the totals.
	CostMicro          []int64 `json:"cost_micro"`
	TotalRequests      int64   `json:"total_requests"`
	TotalTokens        int64   `json:"total_tokens"`
	ActualCostMicro    int64   `json:"actual_cost_micro"`
	EstimatedCostMicro int64   `json:"estimated_cost_micro"`
}

type usageResponseJSON struct {
	Range   string            `json:"range"`
	Group   string            `json:"group"`
	Buckets []string          `json:"buckets"`
	Series  []usageSeriesJSON `json:"series"`
	// Currency is a display label; llmesh does no conversion.
	Currency string `json:"currency"`
	Totals   struct {
		Requests         int64 `json:"requests"`
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
		// Charged and estimated stay separate so the portal can never present a
		// modelled figure as money actually spent.
		ActualCostMicro    int64 `json:"actual_cost_micro"`
		EstimatedCostMicro int64 `json:"estimated_cost_micro"`
		// UnpricedRequests is how many requests contributed no cost because their
		// model has no rate. Without it a total looks complete when it is not.
		UnpricedRequests int64 `json:"unpriced_requests"`
	} `json:"totals"`
}

// usageWindow converts a range name into a dense bucket grid.
func usageWindow(rng string, now time.Time) (since, until time.Time, buckets []string, daily bool, ok bool) {
	now = now.UTC()
	switch rng {
	case "24h", "7d":
		hours := 24
		if rng == "7d" {
			hours = 7 * 24
		}
		until = now.Truncate(time.Hour).Add(time.Hour)
		since = until.Add(-time.Duration(hours) * time.Hour)
		for t := since; t.Before(until); t = t.Add(time.Hour) {
			buckets = append(buckets, t.Format(time.RFC3339))
		}
		return since, until, buckets, false, true
	case "30d", "90d":
		days := 30
		if rng == "90d" {
			days = 90
		}
		until = now.Truncate(24 * time.Hour).Add(24 * time.Hour)
		since = until.AddDate(0, 0, -days)
		for t := since; t.Before(until); t = t.AddDate(0, 0, 1) {
			buckets = append(buckets, t.Format("2006-01-02"))
		}
		return since, until, buckets, true, true
	}
	return time.Time{}, time.Time{}, nil, false, false
}

// maxUsageSeries caps how many named series are returned; the remainder is
// folded into an "other" series so charts stay readable.
const maxUsageSeries = 10

// handleUsageJSON returns time-series usage for the dashboard. Members see
// only their own usage; admins see everything.
func (a *Admin) handleUsageJSON(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	u := ctxGetUser(r)

	rng := r.URL.Query().Get("range")
	if rng == "" {
		rng = "7d"
	}
	group := r.URL.Query().Get("group")
	if group == "" {
		group = "model"
	}
	groupBy := group
	if group == "user" {
		groupBy = "owner"
	}
	since, until, buckets, daily, ok := usageWindow(rng, time.Now())
	if !ok {
		http.Error(w, "invalid range", http.StatusBadRequest)
		return
	}
	ownerFilter := ""
	if u.Role != "admin" {
		ownerFilter = u.Username
	}
	rows, err := a.state.QueryUsage(since, until, groupBy, daily, ownerFilter)
	if err != nil {
		a.log.Error("admin: usage query", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	bucketIdx := make(map[string]int, len(buckets))
	for i, b := range buckets {
		bucketIdx[b] = i
	}
	seriesMap := make(map[string]*usageSeriesJSON)
	var order []string
	resp := usageResponseJSON{Range: rng, Group: group, Buckets: buckets, Currency: a.state.CostCurrency()}
	for _, row := range rows {
		i, ok := bucketIdx[row.Bucket]
		if !ok {
			continue
		}
		s, ok := seriesMap[row.Name]
		if !ok {
			s = &usageSeriesJSON{
				Name:             row.Name,
				Requests:         make([]int64, len(buckets)),
				PromptTokens:     make([]int64, len(buckets)),
				CompletionTokens: make([]int64, len(buckets)),
				CostMicro:        make([]int64, len(buckets)),
			}
			seriesMap[row.Name] = s
			order = append(order, row.Name)
		}
		s.Requests[i] += row.Requests
		s.PromptTokens[i] += row.PromptTokens
		s.CompletionTokens[i] += row.CompletionTokens
		s.CostMicro[i] += row.TotalCostMicro()
		s.TotalRequests += row.Requests
		s.TotalTokens += row.PromptTokens + row.CompletionTokens
		s.ActualCostMicro += row.ActualCostMicro
		s.EstimatedCostMicro += row.EstimatedCostMicro
		resp.Totals.Requests += row.Requests
		resp.Totals.PromptTokens += row.PromptTokens
		resp.Totals.CompletionTokens += row.CompletionTokens
		resp.Totals.ActualCostMicro += row.ActualCostMicro
		resp.Totals.EstimatedCostMicro += row.EstimatedCostMicro
		resp.Totals.UnpricedRequests += row.UnpricedRequests
	}

	all := make([]usageSeriesJSON, 0, len(order))
	for _, name := range order {
		all = append(all, *seriesMap[name])
	}
	sort.Slice(all, func(i, j int) bool { return all[i].TotalTokens > all[j].TotalTokens })
	if len(all) > maxUsageSeries {
		other := usageSeriesJSON{
			Name:             "other",
			Requests:         make([]int64, len(buckets)),
			PromptTokens:     make([]int64, len(buckets)),
			CompletionTokens: make([]int64, len(buckets)),
			CostMicro:        make([]int64, len(buckets)),
		}
		for _, s := range all[maxUsageSeries:] {
			for i := range buckets {
				other.Requests[i] += s.Requests[i]
				other.PromptTokens[i] += s.PromptTokens[i]
				other.CompletionTokens[i] += s.CompletionTokens[i]
				other.CostMicro[i] += s.CostMicro[i]
			}
			other.TotalRequests += s.TotalRequests
			other.TotalTokens += s.TotalTokens
			other.ActualCostMicro += s.ActualCostMicro
			other.EstimatedCostMicro += s.EstimatedCostMicro
		}
		all = append(all[:maxUsageSeries], other)
	}
	resp.Series = all
	if resp.Series == nil {
		resp.Series = []usageSeriesJSON{}
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(resp)
}

// ─── Performance API ──────────────────────────────────────────────────────────

// perfMetric describes one selectable performance series: how to pull a value out
// of a bucket's counters, and how the portal should label it.
type perfMetric struct {
	Unit string
	// value extracts the plotted number from one bucket's counters, returning
	// false when that bucket holds no measurement for this metric. Distinguishing
	// "no data" from zero matters for rates: plotting a gap as 0 tok/s would draw
	// a dip that never happened.
	value func(PerfStats) (float64, bool)
}

var perfMetrics = map[string]perfMetric{
	"gen_tps": {
		Unit: "tok/s",
		value: func(p PerfStats) (float64, bool) {
			v := p.GenTokensPerSec()
			return v, v > 0
		},
	},
	"prompt_tps": {
		Unit: "tok/s",
		value: func(p PerfStats) (float64, bool) {
			v := p.PromptTokensPerSec()
			return v, v > 0
		},
	},
	"ttft": {
		Unit: "ms",
		value: func(p PerfStats) (float64, bool) {
			return p.AvgTTFTMS(), p.TTFTSamples > 0
		},
	},
	"queue": {
		Unit: "ms",
		value: func(p PerfStats) (float64, bool) {
			return p.AvgQueueMS(), p.Samples > 0
		},
	},
	"total": {
		Unit: "ms",
		value: func(p PerfStats) (float64, bool) {
			return p.AvgTotalMS(), p.Samples > 0
		},
	},
}

type perfSeriesJSON struct {
	Name string `json:"name"`
	// Values is one entry per bucket, nil where that bucket has no measurement so
	// the chart can break the line instead of drawing a false zero.
	Values  []*float64 `json:"values"`
	Average float64    `json:"average"`
	Samples int64      `json:"samples"`
}

// perfTotalsJSON is the summary row shown as tiles above the chart. Averages are
// weighted across every request in the window, not across buckets.
type perfTotalsJSON struct {
	Samples             int64   `json:"samples"`
	AvgTTFTMS           float64 `json:"avg_ttft_ms"`
	MaxTTFTMS           float64 `json:"max_ttft_ms"`
	AvgQueueMS          float64 `json:"avg_queue_ms"`
	MaxQueueMS          float64 `json:"max_queue_ms"`
	AvgTotalMS          float64 `json:"avg_total_ms"`
	MaxTotalMS          float64 `json:"max_total_ms"`
	GenTokensPerSec     float64 `json:"gen_tokens_per_sec"`
	PromptTokensPerSec  float64 `json:"prompt_tokens_per_sec"`
	BackendMeasuredFrac float64 `json:"backend_measured_frac"`
}

type perfResponseJSON struct {
	Range   string           `json:"range"`
	Group   string           `json:"group"`
	Metric  string           `json:"metric"`
	Unit    string           `json:"unit"`
	Buckets []string         `json:"buckets"`
	Series  []perfSeriesJSON `json:"series"`
	Totals  perfTotalsJSON   `json:"totals"`
}

// maxPerfSeries caps how many named series the chart receives. Unlike token
// counts, rates cannot be meaningfully folded into an "other" bucket — averaging
// unrelated models' speeds produces a number that describes nothing — so the
// tail is dropped and the response says how many were left out.
const maxPerfSeries = 8

// handlePerfJSON returns time-series inference performance for the dashboard
// chart. Members see only their own requests; admins see everything.
func (a *Admin) handlePerfJSON(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	u := ctxGetUser(r)

	rng := r.URL.Query().Get("range")
	if rng == "" {
		rng = "7d"
	}
	group := r.URL.Query().Get("group")
	if group == "" {
		group = "model"
	}
	metricName := r.URL.Query().Get("metric")
	if metricName == "" {
		metricName = "gen_tps"
	}
	metric, ok := perfMetrics[metricName]
	if !ok {
		http.Error(w, "invalid metric", http.StatusBadRequest)
		return
	}
	// Grouping by client names other people's machines ("owner/name"). The owner
	// filter below keeps a member's rows to their own requests, but those requests
	// may have been served by anyone's hardware, so the series names would still
	// disclose the fleet — which the Clients page deliberately does not show a
	// member. Admin-only, matching that page's scoping.
	if group == "client" && u.Role != "admin" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	// The chart's grouping control is shared with the usage chart, which calls the
	// owner dimension "user"; the perf table stores it as "owner".
	groupBy := group
	if group == "user" {
		groupBy = "owner"
	}
	since, until, buckets, daily, ok := usageWindow(rng, time.Now())
	if !ok {
		http.Error(w, "invalid range", http.StatusBadRequest)
		return
	}
	ownerFilter := ""
	if u.Role != "admin" {
		ownerFilter = u.Username
	}
	rows, err := a.state.QueryPerf(since, until, groupBy, daily, ownerFilter)
	if err != nil {
		a.log.Error("admin: perf query", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	totals, err := a.state.PerfTotals(since, until, ownerFilter)
	if err != nil {
		a.log.Error("admin: perf totals", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	bucketIdx := make(map[string]int, len(buckets))
	for i, b := range buckets {
		bucketIdx[b] = i
	}
	// Accumulate raw counters per (series, bucket) first. Rates have to be derived
	// from summed tokens and summed time, so they cannot be averaged after the
	// fact — a bucket's value is only correct once all its rows are folded in.
	cells := make(map[string][]PerfStats)
	agg := make(map[string]*PerfStats)
	var order []string
	for _, row := range rows {
		i, ok := bucketIdx[row.Bucket]
		if !ok {
			continue
		}
		if _, seen := cells[row.Name]; !seen {
			cells[row.Name] = make([]PerfStats, len(buckets))
			agg[row.Name] = &PerfStats{}
			order = append(order, row.Name)
		}
		cells[row.Name][i].Add(row.PerfStats)
		agg[row.Name].Add(row.PerfStats)
	}

	all := make([]perfSeriesJSON, 0, len(order))
	for _, name := range order {
		s := perfSeriesJSON{
			Name:    name,
			Values:  make([]*float64, len(buckets)),
			Samples: agg[name].Samples,
		}
		for i, c := range cells[name] {
			if v, ok := metric.value(c); ok {
				s.Values[i] = &v
			}
		}
		if v, ok := metric.value(*agg[name]); ok {
			s.Average = v
		}
		all = append(all, s)
	}
	// Busiest series first, so truncation drops the least-used ones. Ties break on
	// name: the chart assigns colours by position, so a non-deterministic order
	// would reshuffle the legend between polls.
	sort.Slice(all, func(i, j int) bool {
		if all[i].Samples != all[j].Samples {
			return all[i].Samples > all[j].Samples
		}
		return all[i].Name < all[j].Name
	})
	if len(all) > maxPerfSeries {
		a.log.Debug("admin: perf series truncated",
			"shown", maxPerfSeries, "dropped", len(all)-maxPerfSeries, "group", group)
		all = all[:maxPerfSeries]
	}

	resp := perfResponseJSON{
		Range:   rng,
		Group:   group,
		Metric:  metricName,
		Unit:    metric.Unit,
		Buckets: buckets,
		Series:  all,
		Totals: perfTotalsJSON{
			Samples:             totals.Samples,
			AvgTTFTMS:           totals.AvgTTFTMS(),
			MaxTTFTMS:           totals.TTFTMSMax,
			AvgQueueMS:          totals.AvgQueueMS(),
			MaxQueueMS:          totals.QueueMSMax,
			AvgTotalMS:          totals.AvgTotalMS(),
			MaxTotalMS:          totals.TotalMSMax,
			GenTokensPerSec:     totals.GenTokensPerSec(),
			PromptTokensPerSec:  totals.PromptTokensPerSec(),
			BackendMeasuredFrac: totals.BackendMeasuredFrac(),
		},
	}
	if resp.Series == nil {
		resp.Series = []perfSeriesJSON{}
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(resp)
}

func (a *Admin) handleAuditLogJSON(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	limit := 200
	if ls := r.URL.Query().Get("limit"); ls != "" {
		if n, err := strconv.Atoi(ls); err == nil && n > 0 && n <= 1000 {
			limit = n
		}
	}
	entries, err := a.state.GetAuditLog(limit)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if entries == nil {
		entries = []AuditEntry{}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(entries)
}
