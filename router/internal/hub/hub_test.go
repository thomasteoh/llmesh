package hub

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
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
	h := New()
	conn := dialHub(t, h, "mac", "alice", "ct-alice-abc")
	defer conn.Close()
	time.Sleep(20 * time.Millisecond)
	if !h.IsConnected("ct-alice-abc") {
		t.Fatal("expected connected")
	}
}

func TestLastSeenTime_AfterDisconnect(t *testing.T) {
	h := New()
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
	h := New()
	if !h.LastSeenTime("ct-nobody").IsZero() {
		t.Fatal("expected zero for never-connected token")
	}
}

func TestCloseByToken(t *testing.T) {
	h := New()
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
	h := New()
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
