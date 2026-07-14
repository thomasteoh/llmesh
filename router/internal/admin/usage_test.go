package admin

import (
	"log/slog"
	"path/filepath"
	"testing"
	"time"
)

func TestUsage_AddDeltaUpsert(t *testing.T) {
	s, _ := LoadState(filepath.Join(t.TempDir(), "state.json"))
	bucket := time.Date(2026, 7, 14, 9, 30, 0, 0, time.UTC) // truncates to 09:00
	d := UsageDelta{Bucket: bucket, Owner: "alice", KeyLabel: "alice/prod", Model: "llama", Requests: 1, PromptTokens: 100, CompletionTokens: 50}
	if err := s.AddUsageDelta(d); err != nil {
		t.Fatal(err)
	}
	if err := s.AddUsageDelta(d); err != nil {
		t.Fatal(err)
	}
	rows, err := s.QueryUsage(bucket.Add(-time.Hour), bucket.Add(time.Hour), "model", false, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	r := rows[0]
	if r.Requests != 2 || r.PromptTokens != 200 || r.CompletionTokens != 100 {
		t.Fatalf("upsert did not accumulate: %+v", r)
	}
	if r.Bucket != "2026-07-14T09:00:00Z" {
		t.Fatalf("bucket not truncated to hour: %s", r.Bucket)
	}
}

func TestUsage_QueryGroupingAndOwnerFilter(t *testing.T) {
	s, _ := LoadState(filepath.Join(t.TempDir(), "state.json"))
	b := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	s.AddUsageDelta(UsageDelta{Bucket: b, Owner: "alice", KeyLabel: "alice/prod", Model: "llama", Requests: 1, PromptTokens: 10, CompletionTokens: 1})
	s.AddUsageDelta(UsageDelta{Bucket: b, Owner: "bob", KeyLabel: "bob/dev", Model: "llama", Requests: 2, PromptTokens: 20, CompletionTokens: 2})
	s.AddUsageDelta(UsageDelta{Bucket: b.Add(time.Hour), Owner: "alice", KeyLabel: "alice/prod", Model: "qwen", Requests: 4, PromptTokens: 40, CompletionTokens: 4})

	// Group by model over both hours: llama has combined counts.
	rows, err := s.QueryUsage(b, b.Add(2*time.Hour), "model", false, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 rows (llama, qwen), got %d: %+v", len(rows), rows)
	}
	if rows[0].Name != "llama" || rows[0].Requests != 3 {
		t.Fatalf("llama row wrong: %+v", rows[0])
	}

	// Owner filter: alice only.
	rows, err = s.QueryUsage(b, b.Add(2*time.Hour), "owner", false, "alice")
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, r := range rows {
		if r.Name != "alice" {
			t.Fatalf("owner filter leaked row: %+v", r)
		}
		total += r.Requests
	}
	if total != 5 {
		t.Fatalf("alice total requests = %d, want 5", total)
	}

	// Daily aggregation folds both hours into one day bucket.
	rows, err = s.QueryUsage(b, b.Add(2*time.Hour), "owner", true, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Bucket != "2026-07-14" || rows[0].Requests != 5 {
		t.Fatalf("daily aggregation wrong: %+v", rows)
	}
}

func TestUsage_Prune(t *testing.T) {
	s, _ := LoadState(filepath.Join(t.TempDir(), "state.json"))
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	recent := time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC)
	s.AddUsageDelta(UsageDelta{Bucket: old, Model: "m", Requests: 1})
	s.AddUsageDelta(UsageDelta{Bucket: recent, Model: "m", Requests: 1})
	if err := s.PruneUsageBefore(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	tot, err := s.UsageTotals(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), "")
	if err != nil {
		t.Fatal(err)
	}
	if tot.Requests != 1 {
		t.Fatalf("prune left %d requests, want 1", tot.Requests)
	}
}

func TestUsageRecorder_FlushOnClose(t *testing.T) {
	s, _ := LoadState(filepath.Join(t.TempDir(), "state.json"))
	rec := NewUsageRecorder(s, slog.Default())
	rec.RecordUsage("llama", "alice", "alice/prod", 100, 10)
	rec.RecordUsage("llama", "alice", "alice/prod", 50, 5)
	rec.Close()

	now := time.Now().UTC()
	tot, err := s.UsageTotals(now.Add(-2*time.Hour), now.Add(time.Hour), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if tot.Requests != 2 || tot.PromptTokens != 150 || tot.CompletionTokens != 15 {
		t.Fatalf("flushed totals wrong: %+v", tot)
	}
}

func TestUsageWindow(t *testing.T) {
	now := time.Date(2026, 7, 14, 9, 30, 0, 0, time.UTC)
	_, _, buckets, daily, ok := usageWindow("24h", now)
	if !ok || daily || len(buckets) != 24 {
		t.Fatalf("24h window wrong: ok=%v daily=%v len=%d", ok, daily, len(buckets))
	}
	if buckets[len(buckets)-1] != "2026-07-14T09:00:00Z" {
		t.Fatalf("last hourly bucket should be the current hour, got %s", buckets[len(buckets)-1])
	}
	_, _, buckets, daily, ok = usageWindow("30d", now)
	if !ok || !daily || len(buckets) != 30 {
		t.Fatalf("30d window wrong: ok=%v daily=%v len=%d", ok, daily, len(buckets))
	}
	if buckets[len(buckets)-1] != "2026-07-14" {
		t.Fatalf("last daily bucket should be today, got %s", buckets[len(buckets)-1])
	}
	if _, _, _, _, ok := usageWindow("bogus", now); ok {
		t.Fatal("bogus range accepted")
	}
}
