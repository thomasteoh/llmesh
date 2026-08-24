package hub

import (
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TokenStates replaces three per-token accessors, so it has to agree with all
// three. Disagreeing would change what the dashboard reports without anything
// in the diff looking like a behaviour change.
func TestTokenStates_AgreesWithThePerTokenAccessors(t *testing.T) {
	h := newTestHub(t)

	tokens := map[string][]clientSpec{
		"tok-one":   {{name: "a", version: "v1", models: []string{"gemma", "qwen"}}},
		"tok-two":   {{name: "b", version: "v1", models: []string{"gemma"}}, {name: "c", version: "v1", models: []string{"medium"}}},
		"tok-mixed": {{name: "d", version: "v1", models: []string{"gemma"}}, {name: "e", version: "v2", models: []string{"gemma"}}},
		// A client that reports no version at all: a blank counts as a
		// disagreement, so the token is "mixed". Worth a fixture because it is
		// the case where an "obvious" tidy-up of the rule — skipping empty
		// versions — silently changes what the dashboard shows.
		"tok-blank": {{name: "f", version: "v1", models: []string{"gemma"}}, {name: "g", version: "", models: []string{"gemma"}}},
		"tok-empty": nil,
	}
	for token, specs := range tokens {
		for _, s := range specs {
			registerTestClient(t, h, token, s)
		}
	}

	states := h.TokenStates()

	for token := range tokens {
		st := states[token]
		if want := h.ConnectedCountByToken(token); st.Connections != want {
			t.Errorf("%s: Connections = %d, want %d", token, st.Connections, want)
		}
		if want := h.ConnectedVersion(token); st.Version != want {
			t.Errorf("%s: Version = %q, want %q", token, st.Version, want)
		}
		if want := h.LastSeenTime(token); !st.LastSeen.Equal(want) {
			t.Errorf("%s: LastSeen = %v, want %v", token, st.LastSeen, want)
		}
		want := h.ConnectedModels(token)
		if len(st.Models) != len(want) {
			t.Errorf("%s: Models = %v, want the same set as %v", token, st.Models, want)
			continue
		}
		have := make(map[string]bool, len(st.Models))
		for _, m := range st.Models {
			have[m] = true
		}
		for _, m := range want {
			if !have[m] {
				t.Errorf("%s: Models = %v, missing %q", token, st.Models, m)
			}
		}
	}
}

// Two connections reporting different versions is the case ConnectedVersion
// singles out, and the snapshot has to reach the same verdict.
func TestTokenStates_ReportsMixedVersions(t *testing.T) {
	h := newTestHub(t)
	registerTestClient(t, h, "tok", clientSpec{name: "a", version: "v1", models: []string{"gemma"}})
	registerTestClient(t, h, "tok", clientSpec{name: "b", version: "v2", models: []string{"gemma"}})

	if got := h.TokenStates()["tok"].Version; got != "mixed" {
		t.Errorf("Version = %q, want \"mixed\"", got)
	}
}

// Models are rendered in order, so they have to come back sorted rather than in
// map order, which would reshuffle the column between refreshes.
func TestTokenStates_ModelsAreSorted(t *testing.T) {
	h := newTestHub(t)
	registerTestClient(t, h, "tok", clientSpec{name: "a", version: "v1", models: []string{"zebra", "alpha", "mango"}})

	got := h.TokenStates()["tok"].Models
	want := []string{"alpha", "mango", "zebra"}
	if len(got) != len(want) {
		t.Fatalf("Models = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Models = %v, want %v", got, want)
		}
	}
}

// A token nobody has ever used must be absent rather than present-and-blank, so
// the caller can tell "never connected" from "connected with nothing to say".
func TestTokenStates_OmitsTokensItHasNeverSeen(t *testing.T) {
	h := newTestHub(t)
	registerTestClient(t, h, "known", clientSpec{name: "a", version: "v1", models: []string{"gemma"}})

	if _, ok := h.TokenStates()["never-used"]; ok {
		t.Error("a token the hub has never seen appears in the snapshot")
	}
}

// Read concurrently while the fleet changes under it. This does not prove
// atomicity — that needs a disconnect landing inside the read, which cannot be
// scheduled from here — but it does put the snapshot under -race against live
// mutation, and it pins the one invariant a torn read would break: models can
// only have been learned from a connection, so reporting them with none is a
// contradiction no single pass can produce.
//
// Mutation stays on the test goroutine: the readers call t.Errorf, which is
// safe from any goroutine, where t.Fatalf would not be.
func TestTokenStates_SafeUnderConcurrentChurn(t *testing.T) {
	h := newTestHub(t)
	registerTestClient(t, h, "steady", clientSpec{name: "steady", version: "v1", models: []string{"gemma"}})

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				for token, st := range h.TokenStates() {
					if st.Connections == 0 && len(st.Models) > 0 {
						t.Errorf("%s: %d models with no connections", token, len(st.Models))
					}
				}
			}
		}()
	}

	for i := 0; i < 20; i++ {
		c := dialHub(t, h, "churn", "alice", "churn-token")
		_ = c.WriteJSON(map[string]any{
			"type":           "register",
			"models":         []map[string]any{{"name": "qwen"}},
			"max_concurrent": 1,
			"version":        "v1",
		})
		time.Sleep(time.Millisecond)
		c.Close()
	}

	close(stop)
	wg.Wait()
}

type clientSpec struct {
	name    string
	version string
	models  []string
}

func newTestHub(t *testing.T) *Hub {
	t.Helper()
	return New(slog.New(slog.DiscardHandler))
}

// registerTestClient connects a client and waits for its register message to be
// applied, so a caller reading hub state straight afterwards sees it.
func registerTestClient(t *testing.T, h *Hub, token string, s clientSpec) *websocket.Conn {
	t.Helper()
	before := len(h.ConnectionsLoad())

	conn := dialHub(t, h, s.name, "alice", token)
	t.Cleanup(func() { conn.Close() })

	declared := make([]map[string]any, 0, len(s.models))
	for _, m := range s.models {
		declared = append(declared, map[string]any{"name": m})
	}
	if err := conn.WriteJSON(map[string]any{
		"type":           "register",
		"models":         declared,
		"max_concurrent": 2,
		"version":        s.version,
	}); err != nil {
		t.Fatalf("register %s: %v", s.name, err)
	}

	// ConnectionsLoad skips clients that have connected but not yet registered,
	// so counting it waits for the register to land rather than the socket.
	deadline := time.Now().Add(2 * time.Second)
	for len(h.ConnectionsLoad()) <= before {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s to register", s.name)
		}
		time.Sleep(2 * time.Millisecond)
	}
	return conn
}

// A token with one connection reporting a version and another reporting none
// must give the same answer every time. It used to depend on which the map
// happened to yield first, because the empty string doubled as "nothing seen
// yet": the Clients column flipped between the version and "mixed" across
// refreshes with nothing on the fleet having changed.
func TestConnectedVersion_StableWhenAConnectionReportsNoVersion(t *testing.T) {
	h := newTestHub(t)
	registerTestClient(t, h, "tok", clientSpec{name: "a", version: "v1", models: []string{"gemma"}})
	registerTestClient(t, h, "tok", clientSpec{name: "b", version: "", models: []string{"gemma"}})

	// Enough reads to make an order-dependent answer show itself: Go
	// deliberately randomises the start point of every map iteration.
	for i := 0; i < 200; i++ {
		if got := h.ConnectedVersion("tok"); got != "mixed" {
			t.Fatalf("read %d: ConnectedVersion = %q, want \"mixed\" every time", i, got)
		}
		if got := h.TokenStates()["tok"].Version; got != "mixed" {
			t.Fatalf("read %d: TokenStates version = %q, want \"mixed\" every time", i, got)
		}
	}
}
