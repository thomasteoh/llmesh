package admin

import (
	"path/filepath"
	"testing"
	"time"
)

func pricingTestState(t *testing.T) *State {
	t.Helper()
	s, err := LoadState(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	return s
}

func TestParseRatePerMtok(t *testing.T) {
	cases := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"2.50", 2_500_000, false},
		{"2.5", 2_500_000, false},
		{"10", 10_000_000, false},
		{"0", 0, false},
		{"", 0, false}, // blank clears a rate
		{"  1.25  ", 1_250_000, false},
		{"$3.00", 3_000_000, false}, // pasted from a pricing page
		{"1,250.00", 1_250_000_000, false},
		{"0.000001", 1, false},  // one micro-unit, the smallest we store
		{"0.0000004", 0, false}, // below resolution, rounds to zero
		{"-1", 0, true},
		{"abc", 0, true},
		{"2.5x", 0, true},
		{"1e400", 0, true},   // parses to +Inf
		{"2000000", 0, true}, // beyond the overflow-safety cap
	}
	for _, c := range cases {
		got, err := ParseRatePerMtok(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseRatePerMtok(%q): expected error, got %d", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseRatePerMtok(%q): unexpected error %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseRatePerMtok(%q): got %d, want %d", c.in, got, c.want)
		}
	}
}

// The form shows the stored rate back to the admin, so a save that does not
// round-trip would appear to silently change what they typed.
func TestRateRoundTripsThroughTheForm(t *testing.T) {
	for _, in := range []string{"2.50", "0.05", "10", "0.000001", "123.456789"} {
		ppm, err := ParseRatePerMtok(in)
		if err != nil {
			t.Fatalf("parse %q: %v", in, err)
		}
		shown := FormatRatePerMtok(ppm)
		again, err := ParseRatePerMtok(shown)
		if err != nil {
			t.Fatalf("reparse %q: %v", shown, err)
		}
		if again != ppm {
			t.Errorf("%q -> %d -> %q -> %d: not stable", in, ppm, shown, again)
		}
	}
	if got := FormatRatePerMtok(0); got != "" {
		t.Errorf("a zero rate should render as an empty field, got %q", got)
	}
	if got := FormatRatePerMtok(2_500_000); got != "2.5" {
		t.Errorf("trailing zeroes should be trimmed, got %q", got)
	}
}

func TestFormatMoney_KeepsPrecisionOnSmallAmounts(t *testing.T) {
	cases := []struct {
		micro int64
		want  string
	}{
		{0, "0.00"},
		{12_400_000, "12.40"},
		{1_000_000, "1.00"},
		{250_000, "0.2500"}, // under 1 unit: 4 dp
		{10_000, "0.0100"},
		{5_000, "0.005000"}, // under a cent: 6 dp, so it is not shown as 0.00
		{1, "0.000001"},
		{-12_400_000, "-12.40"},
	}
	for _, c := range cases {
		if got := FormatMoney(c.micro); got != c.want {
			t.Errorf("FormatMoney(%d): got %q, want %q", c.micro, got, c.want)
		}
	}
}

func TestModelPricing_CRUDAndBasisNormalisation(t *testing.T) {
	s := pricingTestState(t)

	if err := s.SetModelPricing("gpt-4o", 2_500_000, 10_000_000, BasisActual); err != nil {
		t.Fatalf("set: %v", err)
	}
	// An unrecognised basis must not be stored as-is; defaulting to estimated
	// keeps the system from claiming a charge it cannot substantiate.
	if err := s.SetModelPricing("qwen", 50_000, 150_000, "whatever"); err != nil {
		t.Fatalf("set: %v", err)
	}
	all, err := s.ModelPricingAll()
	if err != nil {
		t.Fatalf("all: %v", err)
	}
	if got := all["gpt-4o"]; got.InputPPM != 2_500_000 || got.OutputPPM != 10_000_000 || got.Basis != BasisActual {
		t.Errorf("gpt-4o: got %+v", got)
	}
	if got := all["qwen"].Basis; got != BasisEstimated {
		t.Errorf("unknown basis should normalise to estimated, got %q", got)
	}
	if all["gpt-4o"].UpdatedAt == "" {
		t.Error("UpdatedAt should be stamped on write")
	}

	// Update in place rather than inserting a second row.
	if err := s.SetModelPricing("gpt-4o", 3_000_000, 12_000_000, BasisActual); err != nil {
		t.Fatalf("update: %v", err)
	}
	all, _ = s.ModelPricingAll()
	if len(all) != 2 {
		t.Errorf("upsert created a duplicate row: %d rows", len(all))
	}
	if all["gpt-4o"].InputPPM != 3_000_000 {
		t.Errorf("update did not take: %+v", all["gpt-4o"])
	}

	if err := s.SetModelPricing("", 1, 1, BasisActual); err == nil {
		t.Error("blank model should be rejected")
	}
	if err := s.SetModelPricing("neg", -1, 0, BasisActual); err == nil {
		t.Error("negative rate should be rejected")
	}

	if err := s.DeleteModelPricing("gpt-4o"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := s.DeleteModelPricing("gpt-4o"); err == nil {
		t.Error("deleting an absent rate should report not-found")
	}
}

// A model deliberately set to zero is not the same as a model nobody has priced.
// Deleting the row on save would make "this is free" impossible to express.
func TestModelPricing_ExplicitZeroRateIsKept(t *testing.T) {
	s := pricingTestState(t)
	if err := s.SetModelPricing("free-local", 0, 0, BasisEstimated); err != nil {
		t.Fatalf("set: %v", err)
	}
	all, _ := s.ModelPricingAll()
	p, ok := all["free-local"]
	if !ok {
		t.Fatal("an explicit zero rate should persist as a row")
	}
	if p.Priced() {
		t.Error("a zero rate should still count as unpriced for cost purposes")
	}
}

func TestCostCurrency_DefaultsAndValidates(t *testing.T) {
	s := pricingTestState(t)
	if got := s.CostCurrency(); got != defaultCurrency {
		t.Errorf("default currency: got %q, want %q", got, defaultCurrency)
	}
	if err := s.SetCostCurrency("AUD"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if got := s.CostCurrency(); got != "AUD" {
		t.Errorf("got %q, want AUD", got)
	}
	// Blank resets to the default rather than leaving amounts unlabelled.
	if err := s.SetCostCurrency("  "); err != nil {
		t.Fatalf("set blank: %v", err)
	}
	if got := s.CostCurrency(); got != defaultCurrency {
		t.Errorf("blank should reset to default, got %q", got)
	}
	if err := s.SetCostCurrency("TOOLONGCODE"); err == nil {
		t.Error("an over-long currency code should be rejected")
	}
}

// --- Cost aggregation ---

// The whole point of pricing at read time: a rate set today prices traffic that
// ran before any rate existed.
func TestCost_SettingARatePricesExistingHistory(t *testing.T) {
	s := pricingTestState(t)
	b := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	s.AddUsageDelta(UsageDelta{Bucket: b, Owner: "alice", KeyLabel: "alice/prod", Model: "qwen",
		Requests: 1, PromptTokens: 1_000_000, CompletionTokens: 500_000})

	tot, err := s.UsageTotals(b.Add(-time.Hour), b.Add(time.Hour), "")
	if err != nil {
		t.Fatal(err)
	}
	if tot.TotalCostMicro() != 0 || tot.UnpricedRequests != 1 {
		t.Fatalf("before pricing: want zero cost and 1 unpriced, got %+v", tot.CostCounters)
	}

	// $0.05 in / $0.15 out per Mtok, modelled locally.
	if err := s.SetModelPricing("qwen", 50_000, 150_000, BasisEstimated); err != nil {
		t.Fatal(err)
	}
	tot, err = s.UsageTotals(b.Add(-time.Hour), b.Add(time.Hour), "")
	if err != nil {
		t.Fatal(err)
	}
	// 1M prompt at 0.05 = 50_000 micro; 0.5M completion at 0.15 = 75_000 micro.
	if tot.EstimatedCostMicro != 125_000 {
		t.Errorf("estimated cost: got %d, want 125000", tot.EstimatedCostMicro)
	}
	if tot.ActualCostMicro != 0 {
		t.Errorf("an estimated rate must not land in charged: got %d", tot.ActualCostMicro)
	}
	if tot.UnpricedRequests != 0 {
		t.Errorf("request should no longer count as unpriced, got %d", tot.UnpricedRequests)
	}
}

// Charged and estimated must never be merged, at any grouping. This is the
// invariant the feature exists for.
func TestCost_ChargedAndEstimatedStaySeparate(t *testing.T) {
	s := pricingTestState(t)
	b := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	// A real API bill and a locally modelled figure, same user, same hour.
	s.SetModelPricing("gpt-4o", 2_500_000, 10_000_000, BasisActual)
	s.SetModelPricing("qwen", 50_000, 150_000, BasisEstimated)
	s.AddUsageDelta(UsageDelta{Bucket: b, Owner: "alice", KeyLabel: "alice/prod", Model: "gpt-4o",
		Requests: 1, PromptTokens: 1_000_000, CompletionTokens: 1_000_000})
	s.AddUsageDelta(UsageDelta{Bucket: b, Owner: "alice", KeyLabel: "alice/prod", Model: "qwen",
		Requests: 1, PromptTokens: 1_000_000, CompletionTokens: 1_000_000})

	tot, err := s.UsageTotals(b.Add(-time.Hour), b.Add(time.Hour), "")
	if err != nil {
		t.Fatal(err)
	}
	if tot.ActualCostMicro != 12_500_000 {
		t.Errorf("charged: got %d, want 12500000", tot.ActualCostMicro)
	}
	if tot.EstimatedCostMicro != 200_000 {
		t.Errorf("estimated: got %d, want 200000", tot.EstimatedCostMicro)
	}
}

// Grouping by user or key sums across models, each of which may carry a different
// rate and basis. The rate has to be applied per row before aggregation, or every
// mixed-model group would be priced with whichever rate happened to join.
func TestCost_GroupingByUserAppliesEachModelsOwnRate(t *testing.T) {
	s := pricingTestState(t)
	b := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	s.SetModelPricing("gpt-4o", 2_000_000, 0, BasisActual)
	s.SetModelPricing("qwen", 1_000_000, 0, BasisEstimated)
	// alice uses both models; bob only the cheap one.
	s.AddUsageDelta(UsageDelta{Bucket: b, Owner: "alice", KeyLabel: "alice/prod", Model: "gpt-4o",
		Requests: 1, PromptTokens: 1_000_000})
	s.AddUsageDelta(UsageDelta{Bucket: b, Owner: "alice", KeyLabel: "alice/prod", Model: "qwen",
		Requests: 1, PromptTokens: 1_000_000})
	s.AddUsageDelta(UsageDelta{Bucket: b, Owner: "bob", KeyLabel: "bob/dev", Model: "qwen",
		Requests: 1, PromptTokens: 1_000_000})

	rows, err := s.QueryUsage(b.Add(-time.Hour), b.Add(time.Hour), "owner", false, "")
	if err != nil {
		t.Fatal(err)
	}
	byOwner := map[string]CostCounters{}
	for _, r := range rows {
		byOwner[r.Name] = r.CostCounters
	}
	if got := byOwner["alice"]; got.ActualCostMicro != 2_000_000 || got.EstimatedCostMicro != 1_000_000 {
		t.Errorf("alice: got %+v, want charged 2000000 / estimated 1000000", got)
	}
	if got := byOwner["bob"]; got.ActualCostMicro != 0 || got.EstimatedCostMicro != 1_000_000 {
		t.Errorf("bob: got %+v, want charged 0 / estimated 1000000", got)
	}
}

// An unpriced model must still contribute its tokens and requests, or the chart
// would lose traffic the moment cost was introduced.
func TestCost_UnpricedModelStillReportsTokens(t *testing.T) {
	s := pricingTestState(t)
	b := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	s.AddUsageDelta(UsageDelta{Bucket: b, Owner: "alice", KeyLabel: "alice/prod", Model: "mystery",
		Requests: 3, PromptTokens: 300, CompletionTokens: 30})

	rows, err := s.QueryUsage(b.Add(-time.Hour), b.Add(time.Hour), "model", false, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	r := rows[0]
	if r.Requests != 3 || r.PromptTokens != 300 || r.CompletionTokens != 30 {
		t.Errorf("token figures lost for an unpriced model: %+v", r)
	}
	if r.TotalCostMicro() != 0 {
		t.Errorf("unpriced model should cost nothing, got %d", r.TotalCostMicro())
	}
	if r.UnpricedRequests != 3 {
		t.Errorf("unpriced requests: got %d, want 3", r.UnpricedRequests)
	}
}

// Cost is summed before the divide, so many small requests do not each lose a
// fraction to integer truncation. Priced per row, 100 requests of 1000 tokens at
// $2/Mtok would truncate to 0 apiece and report nothing at all.
func TestCost_SumsBeforeDividingSoSmallRequestsAreNotLost(t *testing.T) {
	s := pricingTestState(t)
	b := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	s.SetModelPricing("cheap", 2_000_000, 0, BasisActual) // $2 per Mtok

	// 100 separate hourly buckets, each one request of 1000 prompt tokens.
	// Each is worth 2000 micro; a per-row divide by 1e6 would floor each to 0.
	for i := 0; i < 100; i++ {
		s.AddUsageDelta(UsageDelta{Bucket: b.Add(time.Duration(i) * time.Hour),
			Owner: "alice", KeyLabel: "alice/prod", Model: "cheap",
			Requests: 1, PromptTokens: 1000})
	}
	tot, err := s.UsageTotals(b.Add(-time.Hour), b.Add(200*time.Hour), "")
	if err != nil {
		t.Fatal(err)
	}
	// 100 * 1000 tokens = 100_000 tokens at $2/Mtok = $0.20 = 200_000 micro.
	if tot.ActualCostMicro != 200_000 {
		t.Errorf("charged: got %d, want 200000", tot.ActualCostMicro)
	}
}

func TestCost_OwnerFilterScopesCost(t *testing.T) {
	s := pricingTestState(t)
	b := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	s.SetModelPricing("gpt-4o", 1_000_000, 0, BasisActual)
	s.AddUsageDelta(UsageDelta{Bucket: b, Owner: "alice", KeyLabel: "alice/prod", Model: "gpt-4o",
		Requests: 1, PromptTokens: 1_000_000})
	s.AddUsageDelta(UsageDelta{Bucket: b, Owner: "bob", KeyLabel: "bob/dev", Model: "gpt-4o",
		Requests: 1, PromptTokens: 3_000_000})

	tot, err := s.UsageTotals(b.Add(-time.Hour), b.Add(time.Hour), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if tot.ActualCostMicro != 1_000_000 {
		t.Errorf("a member must see only their own cost: got %d, want 1000000", tot.ActualCostMicro)
	}
}

func TestModelPricingRows_UnionsPricedAndLiveModels(t *testing.T) {
	pricing := map[string]ModelPricing{
		"gpt-4o":  {Model: "gpt-4o", InputPPM: 2_500_000, OutputPPM: 10_000_000, Basis: BasisActual},
		"retired": {Model: "retired", InputPPM: 1, Basis: BasisEstimated},
	}
	// qwen is serving but has no rate; retired has a rate but no worker; and
	// history-only appears solely in recorded usage — no rate, no live worker.
	// Because rates apply retroactively, that last one must still be priceable.
	rows := modelPricingRows(pricing, []string{"qwen", "gpt-4o"}, []string{"history-only", "gpt-4o"})

	if len(rows) != 4 {
		t.Fatalf("want 4 rows (union of priced, live, and seen-in-usage), got %d: %+v", len(rows), rows)
	}
	if rows[0].Model != "gpt-4o" || rows[1].Model != "history-only" ||
		rows[2].Model != "qwen" || rows[3].Model != "retired" {
		t.Errorf("rows should be sorted by model, got %s/%s/%s/%s",
			rows[0].Model, rows[1].Model, rows[2].Model, rows[3].Model)
	}
	if hist := rows[1]; hist.Configured || hist.Live {
		t.Errorf("history-only should be neither configured nor live: got %+v", hist)
	}

	gpt := rows[0]
	if !gpt.Configured || !gpt.Live || !gpt.IsActual {
		t.Errorf("gpt-4o: got %+v", gpt)
	}
	if gpt.InputRate != "2.5" || gpt.OutputRate != "10" {
		t.Errorf("gpt-4o rates: got in=%q out=%q", gpt.InputRate, gpt.OutputRate)
	}

	// A live model with no rate is the case worth surfacing: it is silently
	// missing from every cost total until someone prices it.
	qwen := rows[2]
	if qwen.Configured || !qwen.Live || qwen.IsActual {
		t.Errorf("qwen should be live, unconfigured, estimated: got %+v", qwen)
	}
	if qwen.InputRate != "" || qwen.OutputRate != "" {
		t.Errorf("an unpriced model should have blank rate fields: got %+v", qwen)
	}

	retired := rows[3]
	if !retired.Configured || retired.Live {
		t.Errorf("retired should be configured but not live: got %+v", retired)
	}
}

// The pricing UI is driven off this list, so a model that only exists in history
// must be discoverable or its share of a past bill can never be entered.
func TestUsageModels_IncludesModelsNoLongerServed(t *testing.T) {
	s := pricingTestState(t)
	b := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	s.AddUsageDelta(UsageDelta{Bucket: b, Owner: "alice", KeyLabel: "alice/prod", Model: "retired-model", Requests: 1, PromptTokens: 10})
	s.AddUsageDelta(UsageDelta{Bucket: b, Owner: "bob", KeyLabel: "bob/dev", Model: "another", Requests: 1, PromptTokens: 10})
	s.AddUsageDelta(UsageDelta{Bucket: b.Add(time.Hour), Owner: "bob", KeyLabel: "bob/dev", Model: "another", Requests: 1, PromptTokens: 10})

	got, err := s.UsageModels()
	if err != nil {
		t.Fatal(err)
	}
	// Distinct and sorted, so "another" appears once despite two buckets.
	if len(got) != 2 || got[0] != "another" || got[1] != "retired-model" {
		t.Errorf("got %v, want [another retired-model]", got)
	}
}
