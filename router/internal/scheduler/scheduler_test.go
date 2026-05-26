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

// dialClient connects to the hub test server. sharedSlots controls sharing:
// -1=unlimited, 0=exclusive (owner only), N=up to N concurrent non-owner slots.
func dialClient(t *testing.T, h *hub.Hub, owner, token string, sharedSlots int) *websocket.Conn {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeWS(w, r, "test-client", owner, token, sharedSlots)
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

	// Connect an exclusive client owned by alice.
	conn := dialClient(t, h, "alice", "ct-alice-excl", 0)
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

	conn := dialClient(t, h, "alice", "ct-alice-excl2", 0)
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

	// Shared client (exclusive=false) owned by alice should accept bob's job.
	conn := dialClient(t, h, "alice", "ct-alice-shared", -1)
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
	conn := dialClient(t, h, "alice", "ct-alice-any-excl", 0)
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
	conn := dialClient(t, h, "alice", "ct-alice-any-shared", -1)
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

func TestSetClientSharedSlots_UpdatesInMemoryClient(t *testing.T) {
	h := hub.New(slog.Default())

	// Connect as fully shared (SharedSlots=-1).
	conn := dialClient(t, h, "alice", "ct-alice-toggle", -1)
	registerModels(t, conn, "llama3")

	summaries := h.AvailableClientList()
	if len(summaries) != 1 {
		t.Fatalf("expected 1 available client, got %d", len(summaries))
	}
	if summaries[0].SharedSlots != -1 {
		t.Errorf("expected SharedSlots=-1 (unlimited), got %d", summaries[0].SharedSlots)
	}

	// Set to exclusive (0).
	h.SetClientSharedSlots("ct-alice-toggle", 0)

	summaries = h.AvailableClientList()
	if len(summaries) != 1 {
		t.Fatalf("expected 1 available client after update, got %d", len(summaries))
	}
	if summaries[0].SharedSlots != 0 {
		t.Errorf("expected SharedSlots=0 (exclusive) after SetClientSharedSlots, got %d", summaries[0].SharedSlots)
	}

	// Set to partial (2 shared slots).
	h.SetClientSharedSlots("ct-alice-toggle", 2)

	summaries = h.AvailableClientList()
	if summaries[0].SharedSlots != 2 {
		t.Errorf("expected SharedSlots=2, got %d", summaries[0].SharedSlots)
	}
}

func TestDrainQueue_SharedSlots_PartialLimit(t *testing.T) {
	h := hub.New(slog.Default())
	q := queue.New()
	s := New(q, h, noAlias{}, slog.Default())

	// Client owned by alice, 1 shared slot, max_concurrent=3.
	conn := dialClient(t, h, "alice", "ct-alice-partial", 1)
	registerModels(t, conn, "llama3")

	// Fill the one shared slot with a bob job.
	q.Push(types.InferenceRequest{ID: "req-bob-1", Model: "llama3", Owner: "bob", EnqueuedAt: time.Now()})
	s.drainQueue()
	if readJob(t, conn, 300*time.Millisecond) == nil {
		t.Fatal("first non-owner job should be dispatched (slot available)")
	}

	// Shared slot is now occupied. A second bob job must wait.
	q.Push(types.InferenceRequest{ID: "req-bob-2", Model: "llama3", Owner: "bob", EnqueuedAt: time.Now()})
	s.drainQueue()
	if q.Len() == 0 {
		t.Error("second non-owner job should not be dispatched when shared slot limit is reached")
	}

	// An owner (alice) job must still be dispatchable despite the shared slot being full.
	q.Push(types.InferenceRequest{ID: "req-alice-1", Model: "llama3", Owner: "alice", EnqueuedAt: time.Now()})
	s.drainQueue()
	job := readJob(t, conn, 300*time.Millisecond)
	if job == nil {
		t.Fatal("owner job should be dispatchable regardless of shared slot limit")
	}
	if job.Request.ID != "req-alice-1" {
		t.Errorf("expected alice's job, got %s", job.Request.ID)
	}
}
