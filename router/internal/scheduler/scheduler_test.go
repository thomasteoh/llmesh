package scheduler

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"llmesh/pkg/types"
	"llmesh/router/internal/hub"
	"llmesh/router/internal/queue"
)

// noAlias satisfies AliasProvider with no aliases.
type noAlias struct{}

func (noAlias) AliasMap() map[string][]string { return nil }

// dialClient connects to the hub test server with the given ownerSlots map.
// Pass nil for fully shared (no slots reserved for owner).
func dialClient(t *testing.T, h *hub.Hub, owner, token string, ownerSlots map[string]int) *websocket.Conn {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeWS(w, r, "test-client", owner, token, ownerSlots)
	}))
	t.Cleanup(srv.Close)
	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial hub: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// registerModels sends a register message to the hub via conn.
func registerModels(t *testing.T, conn *websocket.Conn, models ...string) {
	t.Helper()
	type modelEntry struct {
		Name string `json:"name"`
	}
	type regMsg struct {
		Type          string       `json:"type"`
		Models        []modelEntry `json:"models"`
		MaxConcurrent int          `json:"max_concurrent"`
	}
	entries := make([]modelEntry, len(models))
	for i, m := range models {
		entries[i] = modelEntry{Name: m}
	}
	data, _ := json.Marshal(regMsg{Type: "register", Models: entries, MaxConcurrent: 2})
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatalf("register: %v", err)
	}
	time.Sleep(50 * time.Millisecond) // let hub process the register message
}

// readJob reads one message from conn and unmarshals it as a JobMsg.
func readJob(t *testing.T, conn *websocket.Conn, timeout time.Duration) *types.JobMsg {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(timeout))
	_, data, err := conn.ReadMessage()
	if err != nil {
		return nil
	}
	var job types.JobMsg
	if err := json.Unmarshal(data, &job); err != nil || job.Type != "job" {
		return nil
	}
	return &job
}

func TestDrainQueue_ExclusiveClient_SkipsNonOwnerJob(t *testing.T) {
	h := hub.New(slog.Default())
	q := queue.New()
	s := New(q, h, noAlias{}, slog.Default())

	// Connect an exclusive client owned by alice: reserve both slots for owner on llama3.
	// registerModels sends max_concurrent=2, so OwnerSlots["llama3"]=2 → nonOwnerCap=0.
	conn := dialClient(t, h, "alice", "ct-alice-excl", map[string]int{"llama3": 2})
	registerModels(t, conn, "llama3")

	// Push a job owned by bob — alice's exclusive client must not accept it.
	q.Push(types.InferenceRequest{
		ID:         "req-bob-1",
		Model:      "llama3",
		Owner:      "bob",
		EnqueuedAt: time.Now(),
	})

	s.drainQueue()

	if q.Len() == 0 {
		t.Error("exclusive client should not dispatch non-owner job; job must remain in queue")
	}
	if job := readJob(t, conn, 100*time.Millisecond); job != nil {
		t.Errorf("exclusive client received non-owner job: %s", job.Request.ID)
	}
}

func TestDrainQueue_ExclusiveClient_DispatchesOwnerJob(t *testing.T) {
	h := hub.New(slog.Default())
	q := queue.New()
	s := New(q, h, noAlias{}, slog.Default())

	conn := dialClient(t, h, "alice", "ct-alice-excl2", map[string]int{"llama3": 2})
	registerModels(t, conn, "llama3")

	q.Push(types.InferenceRequest{
		ID:         "req-alice-1",
		Model:      "llama3",
		Owner:      "alice",
		EnqueuedAt: time.Now(),
	})

	s.drainQueue()

	job := readJob(t, conn, 300*time.Millisecond)
	if job == nil {
		t.Fatal("exclusive client should dispatch its own owner's job")
	}
	if job.Request.ID != "req-alice-1" {
		t.Errorf("dispatched wrong job: got %s, want req-alice-1", job.Request.ID)
	}
}

func TestDrainQueue_SharedClient_DispatchesAnyOwnerJob(t *testing.T) {
	h := hub.New(slog.Default())
	q := queue.New()
	s := New(q, h, noAlias{}, slog.Default())

	// Shared client (nil ownerSlots) owned by alice should accept bob's job.
	conn := dialClient(t, h, "alice", "ct-alice-shared", nil)
	registerModels(t, conn, "llama3")

	q.Push(types.InferenceRequest{
		ID:         "req-bob-shared",
		Model:      "llama3",
		Owner:      "bob",
		EnqueuedAt: time.Now(),
	})

	s.drainQueue()

	job := readJob(t, conn, 300*time.Millisecond)
	if job == nil {
		t.Fatal("shared client should dispatch any job regardless of owner")
	}
	if job.Request.ID != "req-bob-shared" {
		t.Errorf("dispatched wrong job: got %s, want req-bob-shared", job.Request.ID)
	}
}

func TestDrainQueue_AnyModel_ExclusiveClient_SkipsNonOwner(t *testing.T) {
	h := hub.New(slog.Default())
	q := queue.New()
	s := New(q, h, noAlias{}, slog.Default())

	// Exclusive client owned by alice — must not accept bob's "any" request.
	// OwnerSlots["any"]=2 makes nonOwnerCap=0 for the "any" pseudo-model.
	conn := dialClient(t, h, "alice", "ct-alice-any-excl", map[string]int{"any": 2})
	registerModels(t, conn, "llama3")

	q.Push(types.InferenceRequest{
		ID:         "req-bob-any",
		Model:      "any",
		Owner:      "bob",
		EnqueuedAt: time.Now(),
	})

	s.drainQueue()

	if q.Len() == 0 {
		t.Error("exclusive client should not dispatch non-owner 'any' request; job must remain in queue")
	}
	if job := readJob(t, conn, 100*time.Millisecond); job != nil {
		t.Errorf("exclusive client received non-owner 'any' request: %s", job.Request.ID)
	}
}

func TestDrainQueue_AnyModel_SharedClient_RewritesModel(t *testing.T) {
	h := hub.New(slog.Default())
	q := queue.New()
	s := New(q, h, noAlias{}, slog.Default())

	// Shared client — should accept "any" and receive job with rewritten model name.
	conn := dialClient(t, h, "alice", "ct-alice-any-shared", nil)
	registerModels(t, conn, "llama3")

	q.Push(types.InferenceRequest{
		ID:         "req-bob-any-shared",
		Model:      "any",
		Owner:      "bob",
		EnqueuedAt: time.Now(),
	})

	s.drainQueue()

	job := readJob(t, conn, 300*time.Millisecond)
	if job == nil {
		t.Fatal("shared client should dispatch 'any' request")
	}
	if job.Request.Model == "any" {
		t.Error("scheduler should rewrite 'any' to the client's actual model before dispatch")
	}
	if job.Request.Model != "llama3" {
		t.Errorf("expected model rewritten to 'llama3', got %q", job.Request.Model)
	}
}

func TestSetClientOwnerSlots_UpdatesInMemoryClient(t *testing.T) {
	h := hub.New(slog.Default())

	// Connect as fully shared (nil ownerSlots).
	conn := dialClient(t, h, "alice", "ct-alice-toggle", nil)
	registerModels(t, conn, "llama3")

	summaries := h.AvailableClientList()
	if len(summaries) != 1 {
		t.Fatalf("expected 1 available client, got %d", len(summaries))
	}
	if v := summaries[0].OwnerSlots["llama3"]; v != 0 {
		t.Errorf("expected OwnerSlots[llama3]=0 (fully shared), got %d", v)
	}

	// Reserve 2 slots for owner on llama3 (= max_concurrent → exclusive).
	h.SetClientOwnerSlots("ct-alice-toggle", "llama3", 2)

	summaries = h.AvailableClientList()
	if len(summaries) != 1 {
		t.Fatalf("expected 1 available client after update, got %d", len(summaries))
	}
	if v := summaries[0].OwnerSlots["llama3"]; v != 2 {
		t.Errorf("expected OwnerSlots[llama3]=2 after SetClientOwnerSlots, got %d", v)
	}

	// Clear reservation (restore full sharing).
	h.SetClientOwnerSlots("ct-alice-toggle", "llama3", 0)

	summaries = h.AvailableClientList()
	if v := summaries[0].OwnerSlots["llama3"]; v != 0 {
		t.Errorf("expected OwnerSlots[llama3]=0 after clearing, got %d", v)
	}
}

func TestDrainQueue_OwnerSlots_PartialLimit(t *testing.T) {
	h := hub.New(slog.Default())
	q := queue.New()
	s := New(q, h, noAlias{}, slog.Default())

	// Client owned by alice, no initial reservation; max_concurrent=2 (from registerModels).
	conn := dialClient(t, h, "alice", "ct-alice-partial", nil)
	registerModels(t, conn, "llama3")

	// Reserve 1 slot for owner on llama3 → nonOwnerCap = 2−1 = 1.
	h.SetClientOwnerSlots("ct-alice-partial", "llama3", 1)

	// Fill the one non-owner slot with a bob job.
	q.Push(types.InferenceRequest{ID: "req-bob-1", Model: "llama3", Owner: "bob", EnqueuedAt: time.Now()})
	s.drainQueue()
	if readJob(t, conn, 300*time.Millisecond) == nil {
		t.Fatal("first non-owner job should be dispatched (one non-owner slot available)")
	}

	// Non-owner cap now reached. A second bob job must wait.
	q.Push(types.InferenceRequest{ID: "req-bob-2", Model: "llama3", Owner: "bob", EnqueuedAt: time.Now()})
	s.drainQueue()
	if q.Len() == 0 {
		t.Error("second non-owner job should not be dispatched when owner-slot cap is reached")
	}

	// An owner (alice) job must still be dispatchable despite the non-owner slot being full.
	q.Push(types.InferenceRequest{ID: "req-alice-1", Model: "llama3", Owner: "alice", EnqueuedAt: time.Now()})
	s.drainQueue()
	job := readJob(t, conn, 300*time.Millisecond)
	if job == nil {
		t.Fatal("owner job should be dispatchable regardless of owner-slot cap")
	}
	if job.Request.ID != "req-alice-1" {
		t.Errorf("expected alice's job, got %s", job.Request.ID)
	}
}

func cs(inFlight, maxConc int) types.ClientSummary {
	return types.ClientSummary{InFlight: inFlight, MaxConcurrent: maxConc}
}

func TestBetterClient(t *testing.T) {
	cases := []struct {
		name  string
		a, b  types.ClientSummary
		wantA bool
	}{
		{"unloaded beats loaded",         cs(0, 2), cs(1, 2), true},
		{"loaded loses to unloaded",      cs(1, 2), cs(0, 2), false},
		{"unloaded: higher cap wins",     cs(0, 4), cs(0, 2), true},
		{"unloaded: lower cap loses",     cs(0, 2), cs(0, 4), false},
		{"unloaded: equal cap is a tie",  cs(0, 2), cs(0, 2), false},
		{"loaded: more free slots wins",  cs(2, 4), cs(1, 2), true},
		{"loaded: fewer free slots loses", cs(1, 2), cs(2, 4), false},
		{"loaded: equal free slots is a tie", cs(1, 2), cs(1, 2), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := betterClient(tc.a, tc.b)
			if got != tc.wantA {
				t.Errorf("betterClient(%d/%d, %d/%d) = %v, want %v",
					tc.a.InFlight, tc.a.MaxConcurrent,
					tc.b.InFlight, tc.b.MaxConcurrent,
					got, tc.wantA)
			}
		})
	}
}
