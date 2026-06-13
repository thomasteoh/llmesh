package upstream

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"llmesh/pkg/types"
	"llmesh/router/internal/admin"
	"llmesh/router/internal/correlation"
	"llmesh/router/internal/hub"
	"llmesh/router/internal/queue"
)

// mockWaker counts Wake calls.
type mockWaker struct{ n int }

func (m *mockWaker) Wake() { m.n++ }

var wsUpgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

// newTestUpstream starts an httptest.Server that speaks the upstream router
// WebSocket protocol at /ws/client. onConn is called with the connection
// after the register message is received; it may send jobs and will be
// called in its own goroutine. Returning from onConn closes the connection.
func newTestUpstream(t *testing.T, onConn func(ws *websocket.Conn)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ws/client" {
			http.NotFound(w, r)
			return
		}
		ws, err := wsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		// Consume the register message.
		_, data, err := ws.ReadMessage()
		if err != nil {
			return
		}
		var env struct{ Type string `json:"type"` }
		if err := json.Unmarshal(data, &env); err != nil || env.Type != "register" {
			return
		}
		if onConn != nil {
			onConn(ws)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// sendJob writes a job message to the upstream WS connection.
func sendJob(ws *websocket.Conn, jobID, model, priority string) {
	msg := types.JobMsg{
		Type: "job",
		Request: types.InferenceRequest{
			ID:    jobID,
			Model: model,
		},
	}
	data, _ := json.Marshal(msg)
	ws.WriteMessage(websocket.TextMessage, data)
}

func newConnector(t *testing.T) (*Connector, *hub.Hub, *queue.Queue) {
	t.Helper()
	h := hub.New(slog.Default())
	q := queue.New()
	store := correlation.New(slog.Default())
	waker := &mockWaker{}
	conn := New(h, q, store, waker, "test", slog.Default())
	return conn, h, q
}

// TestToWSURL verifies http/https→ws/wss conversion and trailing slash removal.
func TestToWSURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"http://example.com", "ws://example.com"},
		{"https://example.com", "wss://example.com"},
		{"http://example.com/", "ws://example.com"},
		{"https://example.com/trailing/", "wss://example.com/trailing"},
	}
	for _, tc := range cases {
		got := toWSURL(tc.in)
		if got != tc.want {
			t.Errorf("toWSURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestModelInfoKey verifies order-independence and format.
func TestModelInfoKey(t *testing.T) {
	a := []types.ModelInfo{{Name: "llama3"}, {Name: "mistral"}}
	b := []types.ModelInfo{{Name: "mistral"}, {Name: "llama3"}}
	if modelInfoKey(a) != modelInfoKey(b) {
		t.Error("modelInfoKey should be order-independent")
	}
	if modelInfoKey(nil) != "" {
		t.Error("empty model set should return empty key")
	}
}

// TestConnector_ConnectedStatus verifies Connected transitions true→false on disconnect.
func TestConnector_ConnectedStatus(t *testing.T) {
	connected := make(chan struct{})
	srv := newTestUpstream(t, func(ws *websocket.Conn) {
		close(connected)
		// Stay connected briefly, then return (which closes ws).
		time.Sleep(150 * time.Millisecond)
	})

	conn, _, _ := newConnector(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	conn.Reload(ctx, []admin.UpstreamRouter{
		{URL: srv.URL, Token: "tok", Name: "test", Priority: "normal"},
	})

	select {
	case <-connected:
	case <-ctx.Done():
		t.Fatal("upstream never connected")
	}

	// Poll until Connected returns true (may take a moment after register).
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if conn.Connected(srv.URL) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !conn.Connected(srv.URL) {
		t.Error("expected Connected=true after register")
	}

	// Wait for the server to close the connection.
	time.Sleep(300 * time.Millisecond)

	if conn.Connected(srv.URL) {
		t.Error("expected Connected=false after server closed")
	}
}

// TestConnector_JobOwnerNamespacing verifies jobs receive "upstream:<name>" owner.
func TestConnector_JobOwnerNamespacing(t *testing.T) {
	jobSent := make(chan struct{})
	srv := newTestUpstream(t, func(ws *websocket.Conn) {
		sendJob(ws, "req-1", "llama3", "normal")
		close(jobSent)
		time.Sleep(400 * time.Millisecond)
	})

	conn, _, q := newConnector(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	conn.Reload(ctx, []admin.UpstreamRouter{
		{URL: srv.URL, Token: "tok", Name: "myupstream", Priority: "high"},
	})

	select {
	case <-jobSent:
	case <-ctx.Done():
		t.Fatal("job was never sent by test upstream")
	}

	// Give the connector a moment to push the job to the queue.
	time.Sleep(100 * time.Millisecond)

	snap := q.Snapshot()
	if len(snap) == 0 {
		t.Fatal("expected job in queue, got none")
	}
	job := snap[0]
	if job.Owner != "upstream:myupstream" {
		t.Errorf("owner = %q, want %q", job.Owner, "upstream:myupstream")
	}
	if job.Priority != types.PriorityHigh {
		t.Errorf("priority = %v, want PriorityHigh", job.Priority)
	}
	// OriginID must be preserved; APIKeyLabel must be stripped.
	if job.OriginID != "req-1" {
		t.Errorf("OriginID = %q, want req-1", job.OriginID)
	}
	if job.APIKeyLabel != "" {
		t.Errorf("APIKeyLabel should be empty, got %q", job.APIKeyLabel)
	}
}

// TestConnector_DuplicateJobRejection verifies that a second job with the same
// ID is silently dropped.
func TestConnector_DuplicateJobRejection(t *testing.T) {
	jobsSent := make(chan struct{})
	srv := newTestUpstream(t, func(ws *websocket.Conn) {
		sendJob(ws, "req-dup", "llama3", "normal")
		sendJob(ws, "req-dup", "llama3", "normal") // duplicate
		close(jobsSent)
		time.Sleep(400 * time.Millisecond)
	})

	conn, _, q := newConnector(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	conn.Reload(ctx, []admin.UpstreamRouter{
		{URL: srv.URL, Token: "tok", Name: "u", Priority: "normal"},
	})

	select {
	case <-jobsSent:
	case <-ctx.Done():
		t.Fatal("jobs never sent")
	}

	time.Sleep(100 * time.Millisecond)

	snap := q.Snapshot()
	if len(snap) != 1 {
		t.Errorf("expected 1 job in queue (duplicate rejected), got %d", len(snap))
	}
}

// TestConnector_Reload_AddRemove verifies that Reload starts/stops goroutines correctly.
func TestConnector_Reload_AddRemove(t *testing.T) {
	conn, _, _ := newConnector(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := newTestUpstream(t, func(ws *websocket.Conn) {
		// Stay alive until connection is closed.
		for {
			if _, _, err := ws.ReadMessage(); err != nil {
				return
			}
		}
	})

	upstream := admin.UpstreamRouter{URL: srv.URL, Token: "tok", Name: "u", Priority: "normal"}

	// Add upstream.
	conn.Reload(ctx, []admin.UpstreamRouter{upstream})
	conn.mu.Lock()
	n := len(conn.cancels)
	conn.mu.Unlock()
	if n != 1 {
		t.Errorf("after add: expected 1 cancel, got %d", n)
	}

	// Reload without the upstream — it should be removed.
	conn.Reload(ctx, nil)
	conn.mu.Lock()
	n = len(conn.cancels)
	conn.mu.Unlock()
	if n != 0 {
		t.Errorf("after remove: expected 0 cancels, got %d", n)
	}

	// Adding same URL twice should not create duplicate goroutines.
	conn.Reload(ctx, []admin.UpstreamRouter{upstream})
	conn.Reload(ctx, []admin.UpstreamRouter{upstream})
	conn.mu.Lock()
	n = len(conn.cancels)
	conn.mu.Unlock()
	if n != 1 {
		t.Errorf("after double-add: expected 1 cancel, got %d", n)
	}
}

// TestConnector_SkipBlankURLOrToken verifies that entries without URL or token are ignored.
func TestConnector_SkipBlankURLOrToken(t *testing.T) {
	conn, _, _ := newConnector(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	conn.Reload(ctx, []admin.UpstreamRouter{
		{URL: "", Token: "tok"},
		{URL: "http://x", Token: ""},
		{URL: "", Token: ""},
	})

	conn.mu.Lock()
	n := len(conn.cancels)
	conn.mu.Unlock()
	if n != 0 {
		t.Errorf("expected 0 goroutines for blank URL/token entries, got %d", n)
	}
}

// TestConnector_TokenPassedInHeader verifies the Bearer token is sent on connect.
func TestConnector_TokenPassedInHeader(t *testing.T) {
	var gotToken string
	tokenCh := make(chan string, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/ws/client", func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if strings.HasPrefix(auth, "Bearer ") {
			tokenCh <- strings.TrimPrefix(auth, "Bearer ")
		}
		ws, err := wsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		ws.ReadMessage()
		ws.Close()
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	conn, _, _ := newConnector(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	conn.Reload(ctx, []admin.UpstreamRouter{
		{URL: srv.URL, Token: "secret-token", Name: "u", Priority: "normal"},
	})

	select {
	case gotToken = <-tokenCh:
	case <-ctx.Done():
		t.Fatal("upstream never received connection")
	}

	if gotToken != "secret-token" {
		t.Errorf("token = %q, want secret-token", gotToken)
	}
}
