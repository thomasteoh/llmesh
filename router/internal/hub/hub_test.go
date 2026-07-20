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
		h.ServeWS(w, r, name, owner, token, nil)
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
	// TrackJob only tracks jobs for currently-connected clients, so register one.
	h.mu.Lock()
	h.clients["client-1"] = &Client{ID: "client-1"}
	h.mu.Unlock()
	req := types.InferenceRequest{ID: "req-lease-1", Model: "llama3"}
	before := time.Now()
	if !h.TrackJob("client-1", req) {
		t.Fatal("TrackJob returned false for a connected client")
	}
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

func TestIsValidOrigin(t *testing.T) {
	tests := []struct {
		name   string
		origin string
		host   string
		want   bool
	}{
		{
			name:   "empty origin returns true",
			origin: "",
			host:   "localhost:8080",
			want:   true,
		},
		{
			name:   "matching origin returns true",
			origin: "http://localhost:8080",
			host:   "localhost:8080",
			want:   true,
		},
		{
			name:   "different origin returns false",
			origin: "http://evil.com",
			host:   "localhost:8080",
			want:   false,
		},
		{
			name:   "malformed origin returns false",
			origin: "not-a-url",
			host:   "localhost:8080",
			want:   false,
		},
		{
			name:   "missing scheme returns false",
			origin: "//localhost:8080",
			host:   "localhost:8080",
			want:   false,
		},
		{
			name:   "port mismatch returns false",
			origin: "http://localhost:9090",
			host:   "localhost:8080",
			want:   false,
		},
		{
			name:   "https origin with http host match returns true",
			origin: "https://example.com",
			host:   "example.com",
			want:   true,
		},
		{
			name:   "subdomain mismatch returns false",
			origin: "http://sub.example.com",
			host:   "example.com",
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isValidOrigin(tt.origin, tt.host); got != tt.want {
				t.Errorf("isValidOrigin(%q, %q) = %v, want %v", tt.origin, tt.host, got, tt.want)
			}
		})
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

// TestCancelRequest_FreesSlot verifies that CancelRequest untracks the job and
// decrements the in-flight counter immediately — not after the 20-min lease.
// This is the fix for the orphaned-lease bug where an app client that times out
// and disconnects leaves the llmesh-client slot occupied until the lease reaper runs.
func TestCancelRequest_FreesSlot(t *testing.T) {
	h := New(slog.Default())

	// Set OnAvailable before any connections so dispatch() never races on the field.
	// Use a large buffer so registration-triggered wakes don't block.
	available := make(chan struct{}, 10)
	h.OnAvailable = func() { available <- struct{}{} }

	// Connect and register a client.
	conn := dialHub(t, h, "mac", "alice", "ct-cancel-slot")
	defer conn.Close()
	time.Sleep(20 * time.Millisecond)

	reg := `{"type":"register","models":[{"name":"llama3"}],"max_concurrent":1}`
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
	if clientID == "" {
		t.Fatal("no client registered")
	}

	// Drain registration-triggered wakes so the post-cancel check is unambiguous.
	for len(available) > 0 {
		<-available
	}

	// Simulate a dispatched job: increment in-flight and track.
	req := types.InferenceRequest{ID: "req-cancel-1", Model: "llama3"}
	h.IncrInFlight(clientID)
	h.TrackJob(clientID, req)

	// App client disconnects — router calls CancelRequest.
	h.CancelRequest(req.ID)

	// Job must be untracked immediately.
	h.mu.RLock()
	_, stillTracked := h.jobs[req.ID]
	h.mu.RUnlock()
	if stillTracked {
		t.Error("job should be untracked after CancelRequest")
	}

	// Client in-flight counter must be back to zero.
	h.mu.RLock()
	client := h.clients[clientID]
	h.mu.RUnlock()
	if client.InFlight() != 0 {
		t.Errorf("InFlight = %d after CancelRequest, want 0", client.InFlight())
	}

	// Scheduler must have been woken (CancelRequest calls OnAvailable synchronously).
	select {
	case <-available:
	default:
		t.Error("OnAvailable should have been called after CancelRequest")
	}
}

// TestCancelRequest_NoopForUnknown verifies that CancelRequest on an already-completed
// or never-dispatched request does not panic and does not double-decrement.
func TestCancelRequest_NoopForUnknown(t *testing.T) {
	h := New(slog.Default())
	conn := dialHub(t, h, "mac", "alice", "ct-cancel-noop")
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

	// InFlight is currently 0. Calling CancelRequest for an unknown ID must not
	// decrement it below 0.
	h.CancelRequest("req-does-not-exist")

	h.mu.RLock()
	client := h.clients[clientID]
	h.mu.RUnlock()
	if client.InFlight() != 0 {
		t.Errorf("InFlight = %d, want 0 after CancelRequest on unknown ID", client.InFlight())
	}
}

// addSlotClient registers a fake client directly in the hub for slot-accounting
// tests, bypassing the WebSocket handshake.
func addSlotClient(h *Hub, id, owner string, maxConc int, models []string, ownerSlots map[string]int, ctx map[string]int) {
	mset := make(map[string]bool, len(models))
	for _, m := range models {
		mset[m] = true
	}
	c := &Client{
		ID:                id,
		Owner:             owner,
		Token:             "hash-" + id,
		MaxConcurrent:     maxConc,
		Models:            mset,
		OwnerSlots:        ownerSlots,
		ModelContextSizes: ctx,
	}
	h.mu.Lock()
	h.clients[id] = c
	h.mu.Unlock()
}

// addSlotJob tracks a fake in-flight job on a client for the given model/owner.
func addSlotJob(h *Hub, clientID, reqID, owner, model string) {
	h.IncrInFlight(clientID)
	h.mu.Lock()
	h.jobs[reqID] = InFlightRecord{
		ClientID: clientID,
		Req:      types.InferenceRequest{ID: reqID, Owner: owner, Model: model},
	}
	if h.jobsByClient[clientID] == nil {
		h.jobsByClient[clientID] = make(map[string]struct{})
	}
	h.jobsByClient[clientID][reqID] = struct{}{}
	h.mu.Unlock()
}

func findSlots(t *testing.T, list []types.ModelSlots, model string) types.ModelSlots {
	t.Helper()
	for _, s := range list {
		if s.Model == model {
			return s
		}
	}
	t.Fatalf("model %q not found in slot list %+v", model, list)
	return types.ModelSlots{}
}

func TestAvailableSlotsByModel(t *testing.T) {
	h := New(slog.Default())
	// alice's client: 4 slots, 2 reserved for alice on llama3, ctx 8192.
	addSlotClient(h, "ca", "alice", 4, []string{"llama3"}, map[string]int{"llama3": 2}, map[string]int{"llama3": 8192})
	// bob's client: 2 shared slots serving llama3 + qwen.
	addSlotClient(h, "cb", "bob", 2, []string{"llama3", "qwen"}, nil, map[string]int{"llama3": 4096, "qwen": 2048})
	// dave's client: 1 slot fully reserved for dave (exclusive).
	addSlotClient(h, "cd", "dave", 1, []string{"secret"}, map[string]int{"secret": 1}, nil)

	// ── alice (owns ca) ──
	// llama3: ca (own) → free 4, total 4; cb (non-owner) → cap 2, avail 2, total 2.
	al := h.AvailableSlotsByModel("alice")
	l := findSlots(t, al, "llama3")
	if l.AvailableSlots != 6 || l.TotalSlots != 6 {
		t.Errorf("alice llama3: got avail=%d total=%d, want 6/6", l.AvailableSlots, l.TotalSlots)
	}
	if l.ContextSize != 8192 {
		t.Errorf("alice llama3 ctx: got %d, want 8192", l.ContextSize)
	}
	q := findSlots(t, al, "qwen")
	if q.AvailableSlots != 2 || q.TotalSlots != 2 {
		t.Errorf("alice qwen: got avail=%d total=%d, want 2/2", q.AvailableSlots, q.TotalSlots)
	}
	// secret is exclusive to dave: alice has no access.
	s := findSlots(t, al, "secret")
	if s.AvailableSlots != 0 || s.TotalSlots != 0 {
		t.Errorf("alice secret: got avail=%d total=%d, want 0/0 (no access)", s.AvailableSlots, s.TotalSlots)
	}

	// ── carol (owns nothing) ──
	// llama3: ca (non-owner) → cap 4-2=2; cb → 2. avail 4, total 4.
	cl := h.AvailableSlotsByModel("carol")
	l = findSlots(t, cl, "llama3")
	if l.AvailableSlots != 4 || l.TotalSlots != 4 {
		t.Errorf("carol llama3: got avail=%d total=%d, want 4/4", l.AvailableSlots, l.TotalSlots)
	}

	// ── dave (owns cd, exclusive) ──
	dl := h.AvailableSlotsByModel("dave")
	sec := findSlots(t, dl, "secret")
	if sec.AvailableSlots != 1 || sec.TotalSlots != 1 {
		t.Errorf("dave secret: got avail=%d total=%d, want 1/1", sec.AvailableSlots, sec.TotalSlots)
	}

	// ── in-flight reduces availability ──
	// A non-owner (carol) job on ca for llama3 consumes 1 of alice's reserved-free
	// pool: ca free drops to 3 (alice sees 3), and carol's non-owner remaining on
	// ca drops to 1 (cap 2 - 1 used).
	addSlotJob(h, "ca", "job1", "carol", "llama3")

	al = h.AvailableSlotsByModel("alice")
	l = findSlots(t, al, "llama3")
	// ca: free 3 (owner uses any free); cb: 2. → 5.
	if l.AvailableSlots != 5 {
		t.Errorf("alice llama3 after 1 job: got avail=%d, want 5", l.AvailableSlots)
	}
	cl = h.AvailableSlotsByModel("carol")
	l = findSlots(t, cl, "llama3")
	// ca: cap 2 - 1 used = 1, min(1, free 3) = 1; cb: 2. → 3.
	if l.AvailableSlots != 3 {
		t.Errorf("carol llama3 after 1 job: got avail=%d, want 3", l.AvailableSlots)
	}
}

func TestModelModalityVerdict(t *testing.T) {
	h := New(slog.Default())

	// Text-only client (known capability): serves "vision-model" as text only.
	textConn := dialHub(t, h, "mac", "alice", "ct-text")
	defer textConn.Close()
	time.Sleep(20 * time.Millisecond)
	textReg := `{"type":"register","models":[{"name":"vision-model","modalities":["text"]}],"max_concurrent":2}`
	if err := textConn.WriteMessage(websocket.TextMessage, []byte(textReg)); err != nil {
		t.Fatalf("register text client: %v", err)
	}
	// Unknown-capability client: serves "mystery" with no advertised modalities.
	unkConn := dialHub(t, h, "mac2", "alice", "ct-unk")
	defer unkConn.Close()
	time.Sleep(20 * time.Millisecond)
	unkReg := `{"type":"register","models":[{"name":"mystery"}],"max_concurrent":2}`
	if err := unkConn.WriteMessage(websocket.TextMessage, []byte(unkReg)); err != nil {
		t.Fatalf("register unknown client: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	// No requirement: always compatible.
	if compat, unknown := h.ModelModalityVerdict("vision-model", nil, nil); !compat || unknown {
		t.Errorf("no requirement verdict = (%v,%v), want (true,false)", compat, unknown)
	}
	// Known text-only model asked for vision: neither compatible nor unknown → reject.
	if compat, unknown := h.ModelModalityVerdict("vision-model", nil, []string{"vision"}); compat || unknown {
		t.Errorf("text-only vision verdict = (%v,%v), want (false,false)", compat, unknown)
	}
	// Unknown-capability model asked for vision: not compatible, but unknown → don't reject.
	if compat, unknown := h.ModelModalityVerdict("mystery", nil, []string{"vision"}); compat || !unknown {
		t.Errorf("unknown model vision verdict = (%v,%v), want (false,true)", compat, unknown)
	}
	// "any" is never hard-rejected.
	if compat, unknown := h.ModelModalityVerdict("any", nil, []string{"vision"}); compat || !unknown {
		t.Errorf("any verdict = (%v,%v), want (false,true)", compat, unknown)
	}
}
