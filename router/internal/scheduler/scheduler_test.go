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
		{"unloaded beats loaded", cs(0, 2), cs(1, 2), true},
		{"loaded loses to unloaded", cs(1, 2), cs(0, 2), false},
		{"unloaded: higher cap wins", cs(0, 4), cs(0, 2), true},
		{"unloaded: lower cap loses", cs(0, 2), cs(0, 4), false},
		{"unloaded: equal cap is a tie", cs(0, 2), cs(0, 2), false},
		{"loaded: more free slots wins", cs(2, 4), cs(1, 2), true},
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

// registerModelModalities registers a single model with advertised modalities.
func registerModelModalities(t *testing.T, conn *websocket.Conn, model string, modalities []string) {
	t.Helper()
	type modelEntry struct {
		Name       string   `json:"name"`
		Modalities []string `json:"modalities,omitempty"`
	}
	type regMsg struct {
		Type          string       `json:"type"`
		Models        []modelEntry `json:"models"`
		MaxConcurrent int          `json:"max_concurrent"`
	}
	data, _ := json.Marshal(regMsg{Type: "register", Models: []modelEntry{{Name: model, Modalities: modalities}}, MaxConcurrent: 2})
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatalf("register: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
}

func TestDrainQueue_SkipsKnownTextOnlyForVision(t *testing.T) {
	h := hub.New(slog.Default())
	q := queue.New()
	s := New(q, h, noAlias{}, slog.Default())

	conn := dialClient(t, h, "alice", "ct-alice-txt", nil)
	registerModelModalities(t, conn, "llava", []string{"text"}) // known text-only

	q.Push(types.InferenceRequest{
		ID: "req-img", Model: "llava", Owner: "alice",
		Modalities: []string{"vision"}, EnqueuedAt: time.Now(),
	})
	s.drainQueue()

	if q.Len() == 0 {
		t.Error("vision request must not dispatch to a known text-only client; it should remain queued")
	}
	if job := readJob(t, conn, 100*time.Millisecond); job != nil {
		t.Errorf("text-only client received vision job: %s", job.Request.ID)
	}
}

func TestDrainQueue_DispatchesToVisionCapable(t *testing.T) {
	h := hub.New(slog.Default())
	q := queue.New()
	s := New(q, h, noAlias{}, slog.Default())

	conn := dialClient(t, h, "alice", "ct-alice-vis", nil)
	registerModelModalities(t, conn, "llava", []string{"text", "vision"})

	q.Push(types.InferenceRequest{
		ID: "req-img", Model: "llava", Owner: "alice",
		Modalities: []string{"vision"}, EnqueuedAt: time.Now(),
	})
	s.drainQueue()

	job := readJob(t, conn, 300*time.Millisecond)
	if job == nil {
		t.Fatal("vision-capable client should receive the vision job")
	}
	if job.Request.ID != "req-img" {
		t.Errorf("dispatched wrong job: got %s, want req-img", job.Request.ID)
	}
}

func TestDrainQueue_UnknownCapabilityNotExcluded(t *testing.T) {
	h := hub.New(slog.Default())
	q := queue.New()
	s := New(q, h, noAlias{}, slog.Default())

	conn := dialClient(t, h, "alice", "ct-alice-unk", nil)
	registerModels(t, conn, "llava") // no modalities advertised → unknown capability

	q.Push(types.InferenceRequest{
		ID: "req-img", Model: "llava", Owner: "alice",
		Modalities: []string{"vision"}, EnqueuedAt: time.Now(),
	})
	s.drainQueue()

	job := readJob(t, conn, 300*time.Millisecond)
	if job == nil {
		t.Fatal("unknown-capability client must still receive the job (pass-through preserved)")
	}
}

// mapIso is a test IsolationProvider backed by a static map.
type mapIso map[string]types.UserIsolation

func (m mapIso) IsolationMap() map[string]types.UserIsolation { return m }

func TestDrainQueue_SendIsolation_BlocksOtherOwnersClient(t *testing.T) {
	h := hub.New(slog.Default())
	q := queue.New()
	s := New(q, h, noAlias{}, slog.Default())
	s.SetIsolationProvider(mapIso{"bob": {SendIsolated: true}})

	// A fully shared client owned by alice would normally accept bob's job, but
	// bob is send-isolated so his request may only run on his own clients.
	conn := dialClient(t, h, "alice", "ct-alice-si", nil)
	registerModels(t, conn, "llama3")

	q.Push(types.InferenceRequest{ID: "req-bob-si", Model: "llama3", Owner: "bob", EnqueuedAt: time.Now()})
	s.drainQueue()

	if q.Len() == 0 {
		t.Error("send-isolated request must not dispatch to another owner's client")
	}
	if job := readJob(t, conn, 100*time.Millisecond); job != nil {
		t.Errorf("alice's client received send-isolated bob's job: %s", job.Request.ID)
	}
}

func TestDrainQueue_SendIsolation_AllowsOwnClient(t *testing.T) {
	h := hub.New(slog.Default())
	q := queue.New()
	s := New(q, h, noAlias{}, slog.Default())
	s.SetIsolationProvider(mapIso{"bob": {SendIsolated: true}})

	conn := dialClient(t, h, "bob", "ct-bob-si", nil)
	registerModels(t, conn, "llama3")

	q.Push(types.InferenceRequest{ID: "req-bob-own", Model: "llama3", Owner: "bob", EnqueuedAt: time.Now()})
	s.drainQueue()

	job := readJob(t, conn, 300*time.Millisecond)
	if job == nil || job.Request.ID != "req-bob-own" {
		t.Fatal("send-isolated request must still run on its own owner's client")
	}
}

func TestDrainQueue_ReceiveIsolation_BlocksOtherOwnersRequest(t *testing.T) {
	h := hub.New(slog.Default())
	q := queue.New()
	s := New(q, h, noAlias{}, slog.Default())
	// alice's clients are receive-isolated: they serve only alice's requests.
	s.SetIsolationProvider(mapIso{"alice": {ReceiveIsolated: true}})

	conn := dialClient(t, h, "alice", "ct-alice-ri", nil)
	registerModels(t, conn, "llama3")

	// bob is not isolated, but alice's client still won't serve him.
	q.Push(types.InferenceRequest{ID: "req-bob-ri", Model: "llama3", Owner: "bob", EnqueuedAt: time.Now()})
	s.drainQueue()

	if q.Len() == 0 {
		t.Error("receive-isolated client must not serve another owner's request")
	}
	if job := readJob(t, conn, 100*time.Millisecond); job != nil {
		t.Errorf("receive-isolated alice's client served bob's job: %s", job.Request.ID)
	}
}

func TestDrainQueue_ReceiveIsolation_AllowsOwnRequest(t *testing.T) {
	h := hub.New(slog.Default())
	q := queue.New()
	s := New(q, h, noAlias{}, slog.Default())
	s.SetIsolationProvider(mapIso{"alice": {ReceiveIsolated: true}})

	conn := dialClient(t, h, "alice", "ct-alice-ri2", nil)
	registerModels(t, conn, "llama3")

	q.Push(types.InferenceRequest{ID: "req-alice-own", Model: "llama3", Owner: "alice", EnqueuedAt: time.Now()})
	s.drainQueue()

	job := readJob(t, conn, 300*time.Millisecond)
	if job == nil || job.Request.ID != "req-alice-own" {
		t.Fatal("receive-isolated client must still serve its own owner's request")
	}
}

// TestDrainQueue_Isolation_NoStarvation guards the selection-time filter: an
// isolated request that a client may not serve must not prevent that client
// from serving another request it is allowed to run.
func TestDrainQueue_Isolation_NoStarvation(t *testing.T) {
	h := hub.New(slog.Default())
	q := queue.New()
	s := New(q, h, noAlias{}, slog.Default())
	s.SetIsolationProvider(mapIso{"bob": {SendIsolated: true}})

	conn := dialClient(t, h, "alice", "ct-alice-ns", nil)
	registerModels(t, conn, "llama3")

	// bob's send-isolated request is higher priority (older); carol's is eligible.
	q.Push(types.InferenceRequest{ID: "req-bob-block", Model: "llama3", Owner: "bob", EnqueuedAt: time.Now().Add(-time.Minute)})
	q.Push(types.InferenceRequest{ID: "req-carol-ok", Model: "llama3", Owner: "carol", EnqueuedAt: time.Now()})
	s.drainQueue()

	job := readJob(t, conn, 300*time.Millisecond)
	if job == nil || job.Request.ID != "req-carol-ok" {
		t.Fatalf("blocked isolated request must not shadow an eligible one; got %v", job)
	}
}

func TestIsolationAllows(t *testing.T) {
	iso := map[string]types.UserIsolation{
		"send": {SendIsolated: true},
		"recv": {ReceiveIsolated: true},
	}
	cases := []struct {
		req, client string
		want        bool
	}{
		{"alice", "alice", true}, // same owner always allowed
		{"send", "send", true},   // send-isolated to own client
		{"send", "alice", false}, // send-isolated to other client blocked
		{"alice", "recv", false}, // receive-isolated client blocks others
		{"recv", "recv", true},   // receive-isolated client serves its owner
		{"alice", "bob", true},   // neither isolated
		{"", "recv", false},      // anonymous request blocked by receive isolation
	}
	for _, c := range cases {
		if got := isolationAllows(iso, c.req, c.client); got != c.want {
			t.Errorf("isolationAllows(%q,%q) = %v, want %v", c.req, c.client, got, c.want)
		}
	}
}
