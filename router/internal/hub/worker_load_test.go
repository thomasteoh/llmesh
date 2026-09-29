package hub

import (
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"llmesh/pkg/types"
)

// loadTestClient connects and registers a worker serving llama3 with the
// given slots, and returns its connection and hub-side ID.
func loadTestClient(t *testing.T, h *Hub, token string, slots int) (*websocket.Conn, string) {
	t.Helper()
	conn := dialHub(t, h, "mac", "alice", token)
	t.Cleanup(func() { conn.Close() })
	time.Sleep(20 * time.Millisecond)
	reg, _ := json.Marshal(types.RegisterMsg{Type: "register", Models: []types.ModelInfo{{Name: "llama3"}}, MaxConcurrent: slots})
	if err := conn.WriteMessage(websocket.TextMessage, reg); err != nil {
		t.Fatalf("register: %v", err)
	}
	time.Sleep(30 * time.Millisecond)
	h.mu.RLock()
	defer h.mu.RUnlock()
	for id, c := range h.clients {
		if c.Token == token {
			return conn, id
		}
	}
	t.Fatal("client not registered")
	return nil, ""
}

func send(t *testing.T, conn *websocket.Conn, msg any) {
	t.Helper()
	data, _ := json.Marshal(msg)
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatalf("send: %v", err)
	}
	time.Sleep(40 * time.Millisecond)
}

func TestRegisterIsAcknowledgedWithFeatures(t *testing.T) {
	h := New(slog.Default())
	conn, _ := loadTestClient(t, h, "ct-ack", 1)
	conn.SetReadDeadline(time.Now().Add(time.Second))
	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("no acknowledgement: %v", err)
	}
	var ack types.RegisteredMsg
	json.Unmarshal(data, &ack)
	if ack.Type != "registered" || len(ack.Features) != 1 || ack.Features[0] != types.FeatureLocalBusy {
		t.Errorf("acknowledgement: %s", data)
	}
}

func TestLocalLoadTakesSlots(t *testing.T) {
	h := New(slog.Default())
	conn, _ := loadTestClient(t, h, "ct-local", 2)
	if n := len(h.AvailableClientList()); n != 1 {
		t.Fatalf("idle worker not available: %d", n)
	}
	send(t, conn, types.LocalBusyMsg{Type: "local_busy", Slots: 1})
	if l := h.AvailableClientList(); len(l) != 1 || l[0].InFlight != 1 {
		t.Fatalf("one local slot should leave one free: %+v", l)
	}
	send(t, conn, types.LocalBusyMsg{Type: "local_busy", Slots: 5}) // clamped to 2
	if n := len(h.AvailableClientList()); n != 0 {
		t.Fatalf("a worker whose slots are all local was offered jobs")
	}
	send(t, conn, types.LocalBusyMsg{Type: "local_busy", Slots: 0})
	if n := len(h.AvailableClientList()); n != 1 {
		t.Fatalf("worker not available again after local load ended")
	}
}

func TestBusyReleaseCostsNoAttempt(t *testing.T) {
	h := New(slog.Default())
	conn, id := loadTestClient(t, h, "ct-busy", 1)
	released := make(chan types.InferenceRequest, 2)
	h.OnRelease = func(r types.InferenceRequest) { released <- r }

	h.IncrInFlight(id)
	h.TrackJob(id, types.InferenceRequest{ID: "j1", Model: "llama3", RequestedModel: "any"})
	send(t, conn, types.LocalBusyMsg{Type: "local_busy", Slots: 1})
	send(t, conn, types.ReleaseMsg{Type: "release", RequestID: "j1", Reason: types.ReleaseBusy})
	select {
	case r := <-released:
		if r.Attempts != 0 || r.Model != "any" {
			t.Errorf("busy release: attempts %d model %q, want 0 and the requested alias", r.Attempts, r.Model)
		}
	default:
		t.Fatal("busy release was not requeued")
	}

	// Claiming busy while reporting no local load is an ordinary release.
	send(t, conn, types.LocalBusyMsg{Type: "local_busy", Slots: 0})
	h.IncrInFlight(id)
	h.TrackJob(id, types.InferenceRequest{ID: "j2", Model: "llama3"})
	send(t, conn, types.ReleaseMsg{Type: "release", RequestID: "j2", Reason: types.ReleaseBusy})
	select {
	case r := <-released:
		if r.Attempts != 1 {
			t.Errorf("busy release with no local load cost no attempt, so a worker could bounce a job forever")
		}
	default:
		t.Fatal("release not requeued")
	}
}

func TestShutdownReleaseStopsNewJobs(t *testing.T) {
	h := New(slog.Default())
	conn, id := loadTestClient(t, h, "ct-drain", 2)
	h.OnRelease = func(types.InferenceRequest) {}
	h.IncrInFlight(id)
	h.TrackJob(id, types.InferenceRequest{ID: "j1", Model: "llama3"})
	send(t, conn, types.ReleaseMsg{Type: "release", RequestID: "j1", Reason: types.ReleaseShutdown})
	if n := len(h.AvailableClientList()); n != 0 {
		t.Error("a worker that is shutting down was still offered jobs")
	}
	if len(h.AvailableModels()) != 0 {
		t.Error("a shutting-down worker's models still count as available")
	}
}

func TestFinalErrorIsNotRetried(t *testing.T) {
	h := New(slog.Default())
	conn, id := loadTestClient(t, h, "ct-final", 1)
	retried := make(chan types.InferenceRequest, 1)
	failed := make(chan types.ErrorMsg, 1)
	h.OnRelease = func(r types.InferenceRequest) { retried <- r }
	h.OnError = func(m types.ErrorMsg) { failed <- m }

	h.IncrInFlight(id)
	h.TrackJob(id, types.InferenceRequest{ID: "j1", Model: "llama3"})
	send(t, conn, types.ErrorMsg{Type: "error", RequestID: "j1", Message: "llama.cpp returned 400: too long", Final: true})
	select {
	case m := <-failed:
		if !m.Final || m.Message != "llama.cpp returned 400: too long" {
			t.Errorf("error passed on as %+v", m)
		}
	case <-retried:
		t.Fatal("a final error was retried")
	default:
		t.Fatal("final error neither failed nor retried")
	}
}

// Reasoning is output the caller has seen; retrying after it would stream the
// thinking a second time.
func TestErrorAfterReasoningIsNotRetried(t *testing.T) {
	h := New(slog.Default())
	conn, id := loadTestClient(t, h, "ct-think", 1)
	retried := make(chan types.InferenceRequest, 1)
	failed := make(chan types.ErrorMsg, 1)
	h.OnRelease = func(r types.InferenceRequest) { retried <- r }
	h.OnError = func(m types.ErrorMsg) { failed <- m }

	h.IncrInFlight(id)
	h.TrackJob(id, types.InferenceRequest{ID: "j1", Model: "llama3"})
	send(t, conn, types.ChunkMsg{Type: "chunk", RequestID: "j1", ReasoningDelta: "Let me think"})
	send(t, conn, types.ErrorMsg{Type: "error", RequestID: "j1", Message: "stream ended"})
	select {
	case <-failed:
	case <-retried:
		t.Fatal("retried after reasoning was streamed")
	default:
		t.Fatal("error neither failed nor retried")
	}

	// A keep-alive is not output: an error after only keep-alives retries.
	h.IncrInFlight(id)
	h.TrackJob(id, types.InferenceRequest{ID: "j2", Model: "llama3"})
	send(t, conn, types.ChunkMsg{Type: "chunk", RequestID: "j2"})
	send(t, conn, types.ErrorMsg{Type: "error", RequestID: "j2", Message: "backend down"})
	select {
	case <-retried:
	default:
		t.Fatal("an error after keep-alives alone was not retried")
	}
}
