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

// dialClient connects to the hub test server. exclusive controls ExclusiveOwner on the client.
func dialClient(t *testing.T, h *hub.Hub, owner, token string, exclusive bool) *websocket.Conn {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeWS(w, r, "test-client", owner, token, exclusive)
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
	conn := dialClient(t, h, "alice", "ct-alice-excl", true)
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

	conn := dialClient(t, h, "alice", "ct-alice-excl2", true)
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
	conn := dialClient(t, h, "alice", "ct-alice-shared", false)
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

func TestSetClientExclusive_UpdatesInMemoryClient(t *testing.T) {
	h := hub.New(slog.Default())

	// Connect as shared (exclusive=false).
	conn := dialClient(t, h, "alice", "ct-alice-toggle", false)
	registerModels(t, conn, "llama3")

	// Initially the client summary should show ExclusiveOwner=false.
	summaries := h.AvailableClientList()
	if len(summaries) != 1 {
		t.Fatalf("expected 1 available client, got %d", len(summaries))
	}
	if summaries[0].ExclusiveOwner {
		t.Error("expected ExclusiveOwner=false before toggle")
	}

	// Toggle to exclusive.
	h.SetClientExclusive("ct-alice-toggle", true)

	summaries = h.AvailableClientList()
	if len(summaries) != 1 {
		t.Fatalf("expected 1 available client after toggle, got %d", len(summaries))
	}
	if !summaries[0].ExclusiveOwner {
		t.Error("expected ExclusiveOwner=true after SetClientExclusive")
	}
}
