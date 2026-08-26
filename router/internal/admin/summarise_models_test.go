package admin

import (
	"strings"
	"testing"
	"time"
)

func fleetOf(models ...string) map[string]bool {
	f := make(map[string]bool, len(models))
	for _, m := range models {
		f[m] = true
	}
	return f
}

func TestSummariseModels(t *testing.T) {
	tests := []struct {
		name      string
		mine      []string
		fleet     []string
		wantLabel string
		wantTitle string
	}{
		{
			// The case the summary exists for: every machine runs the same
			// models, so the list was printed once per machine under a card that
			// had just listed them.
			name: "serving everything the fleet serves",
			mine: []string{"gemma", "medium", "qwen"}, fleet: []string{"gemma", "medium", "qwen"},
			wantLabel: "all", wantTitle: "gemma, medium, qwen",
		},
		{
			// The case the summary must not bury.
			name: "missing one model",
			mine: []string{"gemma", "qwen"}, fleet: []string{"gemma", "medium", "qwen"},
			wantLabel: "all except medium", wantTitle: "gemma, qwen",
		},
		{
			name: "missing several",
			mine: []string{"gemma", "qwen"}, fleet: []string{"a", "b", "gemma", "medium", "qwen"},
			// Three missing against two present: listing what it has is shorter
			// and reads more directly than a long exception.
			wantLabel: "gemma, qwen", wantTitle: "gemma, qwen",
		},
		{
			name: "serving one of many",
			mine: []string{"qwen"}, fleet: []string{"gemma", "medium", "qwen"},
			wantLabel: "qwen", wantTitle: "qwen",
		},
		{
			// Nothing to contrast against, and the name is shorter than "all".
			name: "only one model in the whole fleet",
			mine: []string{"qwen"}, fleet: []string{"qwen"},
			wantLabel: "qwen", wantTitle: "qwen",
		},
		{
			name: "offline client serves nothing",
			mine: nil, fleet: []string{"gemma", "qwen"},
			wantLabel: "", wantTitle: "",
		},
		{
			// Exactly half missing: "all except a, b" is no shorter than the
			// list, so the list wins as the plainer statement.
			name: "half missing",
			mine: []string{"c", "d"}, fleet: []string{"a", "b", "c", "d"},
			wantLabel: "c, d", wantTitle: "c, d",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			label, title := summariseModels(tc.mine, fleetOf(tc.fleet...))
			if label != tc.wantLabel {
				t.Errorf("label = %q, want %q", label, tc.wantLabel)
			}
			if title != tc.wantTitle {
				t.Errorf("title = %q, want %q", title, tc.wantTitle)
			}
		})
	}
}

// Whatever the column says, the full list has to remain reachable, or
// summarising has cost the reader information rather than noise.
func TestSummariseModels_TitleAlwaysHoldsTheFullList(t *testing.T) {
	fleet := fleetOf("a", "b", "c")
	for _, mine := range [][]string{{"a", "b", "c"}, {"a", "b"}, {"a"}} {
		_, title := summariseModels(mine, fleet)
		for _, m := range mine {
			if !strings.Contains(title, m) {
				t.Errorf("mine=%v: title %q omits %q", mine, title, m)
			}
		}
	}
}

// summariseModels is only correct if it is given the whole fleet, and the fleet
// is only knowable once every client has been read. Building it per row instead
// — the obvious way to write the loop — leaves each client trivially serving
// "all" of its own models, which passes every unit test above and is wrong on
// screen for the one machine that matters.
func TestDashboardClientRows_SummarisesAgainstTheWholeFleet(t *testing.T) {
	a, h := connTestAdmin(t)
	if err := a.state.AddUser(User{Username: "alice", Role: "member"}); err != nil {
		t.Fatal(err)
	}
	for _, ct := range []ClientToken{
		{Name: "full", Owner: "alice", TokenHash: "hash-full", CreatedAt: time.Now()},
		{Name: "partial", Owner: "alice", TokenHash: "hash-partial", CreatedAt: time.Now()},
	} {
		if err := a.state.AddClientToken(ct); err != nil {
			t.Fatal(err)
		}
	}
	dialClientWithModels(t, h, "full", "alice", "hash-full", "gemma", "medium", "qwen")
	dialClientWithModels(t, h, "partial", "alice", "hash-partial", "gemma", "qwen")
	// Both clients must be registered, not merely connected. Waiting on the
	// model count returned as soon as "full" registered, since it alone serves
	// all three; "partial" was still mid-handshake and its row came back empty.
	waitFor(t, func() bool { return len(h.ConnectionsLoad()) == 2 })

	byName := make(map[string]ClientRow)
	for _, row := range a.dashboardClientRows() {
		byName[row.Name] = row
	}

	if got := byName["alice/full"].Models; got != "all" {
		t.Errorf("client serving every model: Models = %q, want \"all\"", got)
	}
	if got := byName["alice/partial"].Models; got != "all except medium" {
		t.Errorf("client missing one model: Models = %q, want \"all except medium\"", got)
	}
	if got := byName["alice/partial"].ModelsTitle; got != "gemma, qwen" {
		t.Errorf("title = %q, want the full list \"gemma, qwen\"", got)
	}
}
