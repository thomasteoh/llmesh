package admin

import (
	"testing"

	"llmesh/router/internal/hub"
)

func conns(specs ...[]string) []hub.ConnectedClientInfo {
	out := make([]hub.ConnectedClientInfo, 0, len(specs))
	for _, s := range specs {
		out = append(out, hub.ConnectedClientInfo{Name: s[0], Models: s[1:]})
	}
	return out
}

func findModel(t *testing.T, rows []ClientModelRow, name string) ClientModelRow {
	t.Helper()
	for _, r := range rows {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no row for model %q in %+v", name, rows)
	return ClientModelRow{}
}

func modelNames(rows []ClientModelRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Name
	}
	return out
}

// The three sources cover different models, so the rows have to be their union.
// An intersection would hide exactly the cases an operator is looking for: a
// model with a slot limit that stopped being served, or one with traffic in the
// window that has since been removed.
func TestBuildClientModelRows_UnionsAllThreeSources(t *testing.T) {
	rows := buildClientModelRows(
		conns([]string{"gpu-box", "live-only"}),
		map[string]int{"slots-only": 2},
		&ClientPerfRow{ByModel: []ModelPerfRow{{Name: "perf-only", Requests: 7}}},
	)

	if got := modelNames(rows); len(got) != 3 {
		t.Fatalf("rows = %v, want one per distinct model", got)
	}
	if r := findModel(t, rows, "live-only"); !r.Live || r.OwnerSlots != 0 || r.Requests != 0 {
		t.Errorf("live-only = %+v, want live with no slots or traffic", r)
	}
	if r := findModel(t, rows, "slots-only"); r.Live || r.OwnerSlots != 2 {
		t.Errorf("slots-only = %+v, want not live with 2 reserved", r)
	}
	if r := findModel(t, rows, "perf-only"); r.Live || r.Requests != 7 {
		t.Errorf("perf-only = %+v, want not live with 7 requests", r)
	}
}

// Rows are rendered in order, so they must be deterministic: the sources are
// maps, and iterating them unordered would reshuffle the table every reload.
func TestBuildClientModelRows_SortedByName(t *testing.T) {
	rows := buildClientModelRows(
		conns([]string{"gpu-box", "zebra", "alpha"}),
		map[string]int{"mango": 1, "banana": 2},
		&ClientPerfRow{ByModel: []ModelPerfRow{{Name: "cherry"}}},
	)

	want := []string{"alpha", "banana", "cherry", "mango", "zebra"}
	got := modelNames(rows)
	if len(got) != len(want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("rows = %v, want %v", got, want)
		}
	}
}

// With one connection its name is the same on every row, so the column is
// suppressed rather than repeating it.
func TestBuildClientModelRows_NoServedByForSingleConnection(t *testing.T) {
	rows := buildClientModelRows(conns([]string{"gpu-box", "llama3", "qwen"}), nil, nil)

	for _, r := range rows {
		if r.ServedBy != "" {
			t.Errorf("model %q: ServedBy = %q, want empty for a single connection", r.Name, r.ServedBy)
		}
		if !r.Live {
			t.Errorf("model %q: not marked live", r.Name)
		}
	}
}

// With several connections the column is what tells the operator which machine
// serves what, including that only one of them has a given model.
func TestBuildClientModelRows_ServedByNamesEachConnection(t *testing.T) {
	rows := buildClientModelRows(conns(
		[]string{"beta", "shared", "beta-only"},
		[]string{"alpha", "shared"},
	), nil, nil)

	// Sorted, so the cell does not reorder between reloads either.
	if r := findModel(t, rows, "shared"); r.ServedBy != "alpha, beta" {
		t.Errorf("shared.ServedBy = %q, want \"alpha, beta\"", r.ServedBy)
	}
	if r := findModel(t, rows, "beta-only"); r.ServedBy != "beta" {
		t.Errorf("beta-only.ServedBy = %q, want \"beta\"", r.ServedBy)
	}
}

// Connection names are unique only within an owner, so two connections can share
// one. The same name twice in a cell reads as a rendering fault.
func TestBuildClientModelRows_DeduplicatesRepeatedConnectionNames(t *testing.T) {
	rows := buildClientModelRows(conns(
		[]string{"gpu-box", "llama3"},
		[]string{"gpu-box", "llama3"},
	), nil, nil)

	if r := findModel(t, rows, "llama3"); r.ServedBy != "gpu-box" {
		t.Errorf("ServedBy = %q, want the name once", r.ServedBy)
	}
}

// A token with nothing to show must produce no rows, so the template skips the
// whole sub-row instead of rendering an empty table.
func TestBuildClientModelRows_EmptyWhenNothingToShow(t *testing.T) {
	if rows := buildClientModelRows(nil, nil, nil); len(rows) != 0 {
		t.Errorf("rows = %v, want none", rows)
	}
	// A perf record with no per-model breakdown is the common offline case.
	if rows := buildClientModelRows(nil, nil, &ClientPerfRow{Requests: 5}); len(rows) != 0 {
		t.Errorf("rows = %v, want none", rows)
	}
}

// A model can be live and carry both a slot limit and traffic; the row has to
// merge all three rather than letting the last source win.
func TestBuildClientModelRows_MergesEverySourceForOneModel(t *testing.T) {
	rows := buildClientModelRows(
		conns([]string{"gpu-box", "llama3"}),
		map[string]int{"llama3": 3},
		&ClientPerfRow{ByModel: []ModelPerfRow{{
			Name: "llama3", Requests: 40, GenTPS: "38.5 tok/s",
			PromptTPS: "1.2k tok/s", AvgTTFT: "410 ms",
		}}},
	)

	r := findModel(t, rows, "llama3")
	if !r.Live {
		t.Error("not marked live")
	}
	if r.OwnerSlots != 3 {
		t.Errorf("OwnerSlots = %d, want 3", r.OwnerSlots)
	}
	if r.Requests != 40 || r.GenTPS != "38.5 tok/s" || r.PromptTPS != "1.2k tok/s" || r.AvgTTFT != "410 ms" {
		t.Errorf("performance not carried through: %+v", r)
	}
}
