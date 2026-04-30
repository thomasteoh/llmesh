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
