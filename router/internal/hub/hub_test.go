package hub

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"llmesh/pkg/types"
)

func dialHub(t *testing.T, h *Hub, name, owner, token string) *websocket.Conn {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeWS(w, r, name, owner, token)
	}))
	t.Cleanup(srv.Close)
	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return conn
}

func TestIsConnected(t *testing.T) {
	h := New(slog.Default())
	conn := dialHub(t, h, "mac", "alice", "ct-alice-abc")
	defer conn.Close()
	time.Sleep(20 * time.Millisecond)
	if !h.IsConnected("ct-alice-abc") {
		t.Fatal("expected connected")
	}
}

func TestLastSeenTime_AfterDisconnect(t *testing.T) {
	h := New(slog.Default())
	conn := dialHub(t, h, "mac", "alice", "ct-alice-def")
	time.Sleep(20 * time.Millisecond)
	conn.Close()
	time.Sleep(50 * time.Millisecond)
	if h.IsConnected("ct-alice-def") {
		t.Fatal("expected disconnected")
	}
	if h.LastSeenTime("ct-alice-def").IsZero() {
		t.Fatal("expected non-zero LastSeen after disconnect")
	}
}

func TestLastSeenTime_NeverConnected(t *testing.T) {
	h := New(slog.Default())
	if !h.LastSeenTime("ct-nobody").IsZero() {
		t.Fatal("expected zero for never-connected token")
	}
}

func TestCloseByToken(t *testing.T) {
	h := New(slog.Default())
	conn := dialHub(t, h, "mac", "alice", "ct-alice-xyz")
	defer conn.Close()
	time.Sleep(20 * time.Millisecond)
	h.CloseByToken("ct-alice-xyz")
	time.Sleep(50 * time.Millisecond)
	if h.IsConnected("ct-alice-xyz") {
		t.Fatal("expected disconnected after CloseByToken")
	}
}

func TestActiveClientCount(t *testing.T) {
	h := New(slog.Default())
	if h.ActiveClientCount() != 0 {
		t.Fatal("expected 0 initially")
	}
	conn := dialHub(t, h, "mac", "alice", "ct-alice-1")
	defer conn.Close()
	time.Sleep(20 * time.Millisecond)
	if h.ActiveClientCount() != 1 {
		t.Fatalf("expected 1, got %d", h.ActiveClientCount())
	}
}

func TestConnectedModels(t *testing.T) {
	h := New(slog.Default())
	conn := dialHub(t, h, "mac", "alice", "ct-alice-models")
	defer conn.Close()
	time.Sleep(20 * time.Millisecond)

	// Before register: no models advertised.
	if got := h.ConnectedModels("ct-alice-models"); len(got) != 0 {
		t.Fatalf("expected no models before register, got %v", got)
	}

	// Send register message with two models.
	msg := `{"type":"register","models":[{"name":"llama3.2:3b"},{"name":"mistral-7b"}],"max_concurrent":2}`
	if err := conn.WriteMessage(websocket.TextMessage, []byte(msg)); err != nil {
		t.Fatalf("write register: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	models := h.ConnectedModels("ct-alice-models")
	if len(models) != 2 {
		t.Fatalf("expected 2 models, got %v", models)
	}
	want := map[string]bool{"llama3.2:3b": true, "mistral-7b": true}
	for _, m := range models {
		if !want[m] {
			t.Errorf("unexpected model %q", m)
		}
	}
}

func TestLeaseReaper_ReclainsExpiredSlot(t *testing.T) {
	h := New(slog.Default())

	// Connect a client and register it.
	conn := dialHub(t, h, "mac", "alice", "ct-lease-test")
	defer conn.Close()
	time.Sleep(20 * time.Millisecond)

	reg := `{"type":"register","models":[{"name":"llama3"}],"max_concurrent":2}`
	if err := conn.WriteMessage(websocket.TextMessage, []byte(reg)); err != nil {
		t.Fatalf("register: %v", err)
	}
	time.Sleep(30 * time.Millisecond)

	// Find the client ID.
	h.mu.RLock()
	var clientID string
	for id := range h.clients {
		clientID = id
	}
	h.mu.RUnlock()
	if clientID == "" {
		t.Fatal("no client registered")
	}

	// Track a job with an already-expired lease.
	req := types.InferenceRequest{ID: "req-expired", Model: "llama3"}
	h.IncrInFlight(clientID)
	h.mu.Lock()
	h.jobs[req.ID] = InFlightRecord{
		ClientID:     clientID,
		ClientToken:  "ct-lease-test",
		Req:          req,
		DispatchedAt: time.Now().Add(-25 * time.Minute),
		LeaseExpiry:  time.Now().Add(-5 * time.Minute), // already expired
	}
	h.mu.Unlock()

	// Verify job is tracked and inflight is 1.
	h.mu.RLock()
	_, tracked := h.jobs[req.ID]
	h.mu.RUnlock()
	if !tracked {
		t.Fatal("job should be tracked before reaper")
	}

	h.mu.RLock()
	c := h.clients[clientID]
	h.mu.RUnlock()
	if c.InFlight() != 1 {
		t.Fatalf("expected inflight=1 before reaper, got %d", c.InFlight())
	}

	// Run the reaper directly.
	h.handleExpiredLeases()

	// Job should be gone and inflight decremented.
	h.mu.RLock()
	_, stillTracked := h.jobs[req.ID]
	h.mu.RUnlock()
	if stillTracked {
		t.Error("expired job should be removed by reaper")
	}
	if c.InFlight() != 0 {
		t.Errorf("expected inflight=0 after reaper, got %d", c.InFlight())
	}
}

func TestLeaseReaper_IgnoresActiveLeases(t *testing.T) {
	h := New(slog.Default())
	req := types.InferenceRequest{ID: "req-active", Model: "llama3"}
	h.mu.Lock()
	h.jobs[req.ID] = InFlightRecord{
		ClientID:    "client-x",
		Req:         req,
		LeaseExpiry: time.Now().Add(10 * time.Minute), // not expired
	}
	h.mu.Unlock()

	h.handleExpiredLeases()

	h.mu.RLock()
	_, still := h.jobs[req.ID]
	h.mu.RUnlock()
	if !still {
		t.Error("active lease should not be removed by reaper")
	}
}

func TestTrackJob_SetsLeaseFields(t *testing.T) {
	h := New(slog.Default())
	req := types.InferenceRequest{ID: "req-lease-1", Model: "llama3"}
	before := time.Now()
	h.TrackJob("client-1", req)
	after := time.Now()

	h.mu.RLock()
	rec, ok := h.jobs["req-lease-1"]
	h.mu.RUnlock()

	if !ok {
		t.Fatal("job not tracked")
	}
	if rec.DispatchedAt.Before(before) || rec.DispatchedAt.After(after) {
		t.Errorf("DispatchedAt out of range: %v", rec.DispatchedAt)
	}
	expectedExpiry := rec.DispatchedAt.Add(LeaseDuration)
	if !rec.LeaseExpiry.Equal(expectedExpiry) {
		t.Errorf("LeaseExpiry = %v, want %v", rec.LeaseExpiry, expectedExpiry)
	}
}

func TestDispatch_Release_CallsOnRelease(t *testing.T) {
	h := New(slog.Default())

	conn := dialHub(t, h, "mac", "alice", "ct-release-test")
	defer conn.Close()
	time.Sleep(20 * time.Millisecond)

	reg := `{"type":"register","models":[{"name":"llama3"}],"max_concurrent":2}`
	if err := conn.WriteMessage(websocket.TextMessage, []byte(reg)); err != nil {
		t.Fatalf("register: %v", err)
	}
	time.Sleep(30 * time.Millisecond)

	h.mu.RLock()
	var clientID string
	for id := range h.clients {
		clientID = id
	}
	h.mu.RUnlock()

	// Track a job and set inflight.
	req := types.InferenceRequest{ID: "req-release-1", Model: "llama3", Owner: "alice"}
	h.IncrInFlight(clientID)
	h.TrackJob(clientID, req)

	// Wire OnRelease.
	released := make(chan types.InferenceRequest, 1)
	h.OnRelease = func(r types.InferenceRequest) {
		released <- r
	}

	// Send a release message from the client.
	msg := `{"type":"release","request_id":"req-release-1","reason":"model_failed"}`
	if err := conn.WriteMessage(websocket.TextMessage, []byte(msg)); err != nil {
		t.Fatalf("send release: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	// OnRelease must have been called with the original request.
	select {
	case got := <-released:
		if got.ID != "req-release-1" {
			t.Errorf("released req ID = %q, want req-release-1", got.ID)
		}
	default:
		t.Error("OnRelease was not called")
	}

	// Job must be untracked.
	h.mu.RLock()
	_, stillTracked := h.jobs["req-release-1"]
	h.mu.RUnlock()
	if stillTracked {
		t.Error("job should be untracked after release")
	}

	// InFlight must be decremented.
	h.mu.RLock()
	client := h.clients[clientID]
	h.mu.RUnlock()
	if client.InFlight() != 0 {
		t.Errorf("InFlight = %d, want 0 after release", client.InFlight())
	}
}

func TestDispatch_Release_UnknownID_IsIgnored(t *testing.T) {
	h := New(slog.Default())
	called := false
	h.OnRelease = func(r types.InferenceRequest) { called = true }

	conn := dialHub(t, h, "mac", "alice", "ct-release-unknown")
	defer conn.Close()
	time.Sleep(20 * time.Millisecond)

	msg := `{"type":"release","request_id":"does-not-exist","reason":"model_failed"}`
	if err := conn.WriteMessage(websocket.TextMessage, []byte(msg)); err != nil {
		t.Fatalf("send release: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	if called {
		t.Error("OnRelease should not be called for unknown request ID")
	}
}
