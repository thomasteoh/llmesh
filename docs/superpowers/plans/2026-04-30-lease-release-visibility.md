# Lease, Release & Visibility Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix two broken admin.js fetch URLs, add time-bound leases to dispatched jobs with client-initiated release-to-queue, display elapsed time on in-flight and queued requests, and show non-admins their own requests in the queue.

**Architecture:** Leases live in `hub.InFlightRecord` (DispatchedAt + LeaseExpiry); a background goroutine reclaims expired slots. Clients send a new `ReleaseMsg` type instead of `ErrorMsg` on inference failure; the router re-queues via a new `OnRelease` callback. Elapsed time is computed client-side from RFC3339 timestamps embedded as `data-since` attributes. Queue visibility is gated per-user in pages.go.

**Tech Stack:** Go 1.26, gorilla/websocket, Go `log/slog`, Go templates, vanilla JS

---

## File Map

| File | Change |
|------|--------|
| `router/internal/admin/static/admin.js` | Fix 2 stale `/admin/` URLs; add `formatElapsed` + elapsed `setInterval` |
| `pkg/types/types.go` | Add `ReleaseMsg` |
| `router/internal/hub/hub.go` | `LeaseDuration`, `DispatchedAt`/`LeaseExpiry` on `InFlightRecord`, `OnRelease`, `dispatch` "release" case, `handleExpiredLeases`, `StartLeaseReaper` |
| `router/internal/hub/hub_test.go` | Tests for lease tracking, reaper, and release dispatch |
| `router/cmd/router/main.go` | Wire `h.OnRelease`, call `h.StartLeaseReaper()` |
| `client/internal/worker/worker.go` | Send `ReleaseMsg` on `Infer` error |
| `client/internal/worker/worker_test.go` | New: test release and error paths |
| `router/internal/admin/pages.go` | Add `DispatchedAtISO`/`EnqueuedAtISO`/`CanCancel` to rows; filter queue for non-admins |
| `router/internal/admin/templates/clients.html` | Add `data-since` span on job rows |
| `router/internal/admin/templates/dashboard.html` | Add elapsed column, remove admin gate on queue, gate cancel on `CanCancel` |

---

## Task 1: Fix stale admin.js fetch URLs

**Files:**
- Modify: `router/internal/admin/static/admin.js:157,390`

- [ ] **Step 1: Fix the logs fetch URL**

In `admin.js`, change line 157:
```js
// Before:
fetch('/admin/api/logs?category=' + encodeURIComponent(_logCurrentCat) + '&limit=200')
// After:
fetch('/portal/api/logs?category=' + encodeURIComponent(_logCurrentCat) + '&limit=200')
```

- [ ] **Step 2: Fix the dashboard fetch URL**

In `admin.js`, change line 390:
```js
// Before:
fetch('/admin/api/dashboard').then(function(r) {
// After:
fetch('/portal/api/dashboard').then(function(r) {
```

- [ ] **Step 3: Commit**

```bash
cd /home/tteoh/llmesh
git add router/internal/admin/static/admin.js
git commit -m "fix(admin): update stale /admin/ fetch URLs to /portal/"
```

---

## Task 2: Add ReleaseMsg type

**Files:**
- Modify: `pkg/types/types.go`

- [ ] **Step 1: Write the failing test**

Create `pkg/types/types_test.go`:
```go
package types_test

import (
	"encoding/json"
	"testing"

	"llmesh/pkg/types"
)

func TestReleaseMsg_JSON(t *testing.T) {
	msg := types.ReleaseMsg{
		Type:      "release",
		RequestID: "req-123",
		Reason:    "model_failed",
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got types.ReleaseMsg
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Type != "release" || got.RequestID != "req-123" || got.Reason != "model_failed" {
		t.Errorf("roundtrip mismatch: %+v", got)
	}
}
```

- [ ] **Step 2: Run to confirm it fails**

```bash
cd /home/tteoh/llmesh
go test ./pkg/types/... -run TestReleaseMsg_JSON -v
```
Expected: compile error — `types.ReleaseMsg undefined`

- [ ] **Step 3: Add ReleaseMsg to types.go**

At the bottom of `pkg/types/types.go`, after `CancelMsg`, add:
```go
// ReleaseMsg is sent by a client to return a job to the router queue.
// The router re-queues the request for another client to handle.
type ReleaseMsg struct {
	Type      string `json:"type"`       // "release"
	RequestID string `json:"request_id"`
	Reason    string `json:"reason"`     // "model_failed" | "timeout"
}
```

- [ ] **Step 4: Run test to confirm it passes**

```bash
go test ./pkg/types/... -run TestReleaseMsg_JSON -v
```
Expected: `PASS`

- [ ] **Step 5: Commit**

```bash
git add pkg/types/types.go pkg/types/types_test.go
git commit -m "feat(types): add ReleaseMsg for client-initiated job release"
```

---

## Task 3: Hub — lease fields on InFlightRecord

**Files:**
- Modify: `router/internal/hub/hub.go`
- Modify: `router/internal/hub/hub_test.go`

- [ ] **Step 1: Write the failing test**

Add to `router/internal/hub/hub_test.go`:
```go
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
```

Also add the import `"llmesh/pkg/types"` to the test file if not already present.

- [ ] **Step 2: Run to confirm it fails**

```bash
go test ./router/internal/hub/... -run TestTrackJob_SetsLeaseFields -v
```
Expected: compile error — `LeaseDuration undefined`, `rec.DispatchedAt undefined`

- [ ] **Step 3: Add LeaseDuration constant and lease fields**

In `router/internal/hub/hub.go`:

After the `package hub` imports block, add the constant:
```go
// LeaseDuration is the maximum time a dispatched job may remain in-flight
// before the lease reaper reclaims the slot. Matches the worst-case HTTP handler
// timeout: 15 min TTFT + 5 min activity = 20 min.
const LeaseDuration = 20 * time.Minute
```

In `InFlightRecord`, add two fields after `Req`:
```go
type InFlightRecord struct {
	ClientID    string
	ClientToken string
	Req         types.InferenceRequest
	DispatchedAt time.Time // when the job was dispatched to this client
	LeaseExpiry  time.Time // DispatchedAt + LeaseDuration; slot reclaimed after this
}
```

Update `TrackJob` to set both fields:
```go
func (h *Hub) TrackJob(clientID string, req types.InferenceRequest) {
	h.mu.Lock()
	defer h.mu.Unlock()
	token := ""
	if c, ok := h.clients[clientID]; ok {
		token = c.Token
	}
	now := time.Now()
	h.jobs[req.ID] = InFlightRecord{
		ClientID:     clientID,
		ClientToken:  token,
		Req:          req,
		DispatchedAt: now,
		LeaseExpiry:  now.Add(LeaseDuration),
	}
}
```

- [ ] **Step 4: Run test to confirm it passes**

```bash
go test ./router/internal/hub/... -run TestTrackJob_SetsLeaseFields -v
```
Expected: `PASS`

- [ ] **Step 5: Run all hub tests**

```bash
go test ./router/internal/hub/... -v -race
```
Expected: all pass

- [ ] **Step 6: Commit**

```bash
git add router/internal/hub/hub.go router/internal/hub/hub_test.go
git commit -m "feat(hub): add DispatchedAt and LeaseExpiry to InFlightRecord"
```

---

## Task 4: Hub — lease reaper

**Files:**
- Modify: `router/internal/hub/hub.go`
- Modify: `router/internal/hub/hub_test.go`

- [ ] **Step 1: Write the failing test**

Add to `router/internal/hub/hub_test.go`:
```go
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

	c := h.clients[clientID] // safe: client still connected
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
```

- [ ] **Step 2: Run to confirm tests fail**

```bash
go test ./router/internal/hub/... -run "TestLeaseReaper" -v
```
Expected: compile error — `h.handleExpiredLeases undefined`

- [ ] **Step 3: Implement handleExpiredLeases and StartLeaseReaper**

Add to `router/internal/hub/hub.go`:

```go
// handleExpiredLeases scans all tracked jobs and reclaims slots for any whose
// LeaseExpiry has passed. Called by the lease reaper goroutine; also exposed for
// testing so tests can trigger it directly without waiting for the ticker.
func (h *Hub) handleExpiredLeases() {
	now := time.Now()

	h.mu.Lock()
	var expired []InFlightRecord
	for id, rec := range h.jobs {
		if rec.LeaseExpiry.Before(now) {
			expired = append(expired, rec)
			delete(h.jobs, id)
		}
	}
	h.mu.Unlock()

	if len(expired) == 0 {
		return
	}

	for _, rec := range expired {
		h.log.Warn("hub: lease expired, reclaiming slot",
			"request_id", rec.Req.ID,
			"client_id", rec.ClientID,
			"dispatched_at", rec.DispatchedAt,
		)
		h.DecrInFlight(rec.ClientID)
		// Cancel the job on the client (it may still be processing).
		h.SendToClient(rec.ClientID, types.CancelMsg{
			Type:      "cancel",
			RequestID: rec.Req.ID,
		})
	}

	if h.OnAvailable != nil {
		h.OnAvailable()
	}
}

// StartLeaseReaper starts a background goroutine that calls handleExpiredLeases
// every 30 seconds. It runs until the process exits.
func (h *Hub) StartLeaseReaper() {
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			h.handleExpiredLeases()
		}
	}()
}
```

- [ ] **Step 4: Run tests to confirm they pass**

```bash
go test ./router/internal/hub/... -run "TestLeaseReaper" -v -race
```
Expected: both `PASS`

- [ ] **Step 5: Run all hub tests**

```bash
go test ./router/internal/hub/... -v -race
```
Expected: all pass

- [ ] **Step 6: Commit**

```bash
git add router/internal/hub/hub.go router/internal/hub/hub_test.go
git commit -m "feat(hub): add lease reaper to reclaim expired in-flight slots"
```

---

## Task 5: Hub — OnRelease callback and "release" dispatch

**Files:**
- Modify: `router/internal/hub/hub.go`
- Modify: `router/internal/hub/hub_test.go`

- [ ] **Step 1: Write the failing test**

Add to `router/internal/hub/hub_test.go`:
```go
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
```

- [ ] **Step 2: Run to confirm tests fail**

```bash
go test ./router/internal/hub/... -run "TestDispatch_Release" -v
```
Expected: `FAIL` — `OnRelease` field does not exist yet

- [ ] **Step 3: Add OnRelease to Hub and handle "release" in dispatch**

In `hub.go`, add `OnRelease` to the Hub struct after `OnAvailable`:
```go
// OnRelease is called when a client releases a job back to the queue.
// The caller should push the request back to the queue and wake the scheduler.
OnRelease func(req types.InferenceRequest)
```

In `hub.go` `dispatch()`, add a new case after `case "error":`:
```go
case "release":
    var msg types.ReleaseMsg
    if err := json.Unmarshal(data, &msg); err != nil {
        return
    }
    h.mu.RLock()
    rec, ok := h.jobs[msg.RequestID]
    h.mu.RUnlock()
    if !ok {
        return // already completed, expired, or unknown
    }
    client.DecrInFlight()
    h.untrackJob(msg.RequestID)
    h.log.Info("hub: client released job",
        "request_id", msg.RequestID,
        "client_id", client.ID,
        "reason", msg.Reason,
    )
    if h.OnRelease != nil {
        h.OnRelease(rec.Req)
    }
    if h.OnAvailable != nil {
        h.OnAvailable()
    }
```

- [ ] **Step 4: Run tests to confirm they pass**

```bash
go test ./router/internal/hub/... -run "TestDispatch_Release" -v -race
```
Expected: both `PASS`

- [ ] **Step 5: Run all hub tests**

```bash
go test ./router/internal/hub/... -v -race
```
Expected: all pass

- [ ] **Step 6: Commit**

```bash
git add router/internal/hub/hub.go router/internal/hub/hub_test.go
git commit -m "feat(hub): add OnRelease callback and handle 'release' dispatch"
```

---

## Task 6: Wire main.go

**Files:**
- Modify: `router/cmd/router/main.go`

- [ ] **Step 1: Wire OnRelease and start the lease reaper**

In `router/cmd/router/main.go`, find the line `sched.Start()` (after `sched := scheduler.New(...)`). Add these two lines immediately after it:

```go
h.OnRelease = func(req types.InferenceRequest) { q.Push(req); sched.Wake() }
h.StartLeaseReaper()
```

- [ ] **Step 2: Build to confirm it compiles**

```bash
cd /home/tteoh/llmesh
go build ./router/cmd/router/...
```
Expected: no errors

- [ ] **Step 3: Run all tests**

```bash
go test -v -race -count=1 ./...
```
Expected: all pass

- [ ] **Step 4: Commit**

```bash
git add router/cmd/router/main.go
git commit -m "feat(router): wire OnRelease and start lease reaper"
```

---

## Task 7: Client — send ReleaseMsg on inference error

**Files:**
- Modify: `client/internal/worker/worker.go`
- Create: `client/internal/worker/worker_test.go`

- [ ] **Step 1: Write the failing tests**

Create `client/internal/worker/worker_test.go`:
```go
package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	clientPkg "llmesh/client"
	"llmesh/pkg/types"
)

// fakeLlamaCpp returns a test HTTP server that always responds with the given
// status code and body. Use this to simulate llama.cpp returning errors.
func fakeLlamaCpp(t *testing.T, statusCode int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(statusCode)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func configWith(model, endpoint string) *clientPkg.Config {
	return &clientPkg.Config{
		Models: []clientPkg.ModelConfig{
			{Name: model, Endpoint: endpoint},
		},
	}
}

func TestHandle_InferError_SendsReleaseMsg(t *testing.T) {
	// llama.cpp returns 500 → Infer fails → worker must send ReleaseMsg.
	srv := fakeLlamaCpp(t, 500, `{"error":"model overloaded"}`)
	cfg := configWith("llama3", srv.URL)

	job := types.JobMsg{
		Type: "job",
		Request: types.InferenceRequest{
			ID:    "req-worker-1",
			Model: "llama3",
		},
	}

	var sent []any
	sendFn := func(msg any) error {
		sent = append(sent, msg)
		return nil
	}

	Handle(context.Background(), job, cfg, sendFn)

	if len(sent) == 0 {
		t.Fatal("expected at least one message sent")
	}
	// Last message should be a ReleaseMsg.
	data, _ := json.Marshal(sent[len(sent)-1])
	var rel types.ReleaseMsg
	if err := json.Unmarshal(data, &rel); err != nil {
		t.Fatalf("last message is not JSON: %v", err)
	}
	if rel.Type != "release" {
		t.Errorf("expected type=release, got %q", rel.Type)
	}
	if rel.RequestID != "req-worker-1" {
		t.Errorf("expected request_id=req-worker-1, got %q", rel.RequestID)
	}
	if rel.Reason != "model_failed" {
		t.Errorf("expected reason=model_failed, got %q", rel.Reason)
	}
}

func TestHandle_NoEndpoint_SendsErrorMsg(t *testing.T) {
	// No endpoint configured for the requested model → must send ErrorMsg (not release).
	cfg := configWith("other-model", "http://localhost:9999")

	job := types.JobMsg{
		Type: "job",
		Request: types.InferenceRequest{
			ID:    "req-worker-2",
			Model: "llama3", // not in cfg
		},
	}

	var sent []any
	sendFn := func(msg any) error {
		sent = append(sent, msg)
		return nil
	}

	Handle(context.Background(), job, cfg, sendFn)

	if len(sent) != 1 {
		t.Fatalf("expected 1 message, got %d", len(sent))
	}
	data, _ := json.Marshal(sent[0])
	var errMsg types.ErrorMsg
	if err := json.Unmarshal(data, &errMsg); err != nil {
		t.Fatalf("message is not ErrorMsg JSON: %v", err)
	}
	if errMsg.Type != "error" {
		t.Errorf("expected type=error, got %q", errMsg.Type)
	}
}
```

- [ ] **Step 2: Run to confirm tests fail**

```bash
go test ./client/internal/worker/... -v
```
Expected: `TestHandle_InferError_SendsReleaseMsg` FAIL (sends ErrorMsg, not ReleaseMsg); `TestHandle_NoEndpoint_SendsErrorMsg` PASS

- [ ] **Step 3: Update worker.go to send ReleaseMsg on Infer error**

In `client/internal/worker/worker.go`, find the error handling block after `err := llmClient.Infer(...)`:

```go
// Before:
if err != nil {
    if ctx.Err() == nil {
        log.Error("worker: infer error", "request_id", req.ID, "error", err)
    }
    _ = send(types.ErrorMsg{
        Type:      "error",
        RequestID: req.ID,
        Message:   err.Error(),
    })
}

// After:
if err != nil {
    if ctx.Err() == nil {
        log.Error("worker: infer error", "request_id", req.ID, "error", err)
    }
    _ = send(types.ReleaseMsg{
        Type:      "release",
        RequestID: req.ID,
        Reason:    "model_failed",
    })
}
```

- [ ] **Step 4: Run tests to confirm they pass**

```bash
go test ./client/internal/worker/... -v -race
```
Expected: both `PASS`

- [ ] **Step 5: Run all tests**

```bash
go test -v -race -count=1 ./...
```
Expected: all pass

- [ ] **Step 6: Commit**

```bash
git add client/internal/worker/worker.go client/internal/worker/worker_test.go
git commit -m "feat(client): send ReleaseMsg on inference error to enable re-queue"
```

---

## Task 8: Elapsed time — data layer (pages.go)

**Files:**
- Modify: `router/internal/admin/pages.go`

- [ ] **Step 1: Add ISO timestamp fields to row types**

In `pages.go`, update `InFlightJobRow`:
```go
type InFlightJobRow struct {
	ID             string
	Owner          string
	APIKeyLabel    string
	Model          string
	EnqueuedAt     string
	DispatchedAtISO string // RFC3339, for JS elapsed computation
	WordCount      int
	CanCancel      bool
}
```

Update `QueuedJobRow`:
```go
type QueuedJobRow struct {
	ID            string
	Owner         string
	APIKeyLabel   string
	Model         string
	Priority      string
	EnqueuedAt    string
	EnqueuedAtISO string // RFC3339, for JS elapsed computation
	WordCount     int
}
```

- [ ] **Step 2: Populate DispatchedAtISO in renderClientTokens**

In `renderClientTokens`, find the `InFlightJobRow` construction (around line 357) and add `DispatchedAtISO`:
```go
jobs = append(jobs, InFlightJobRow{
    ID:              rec.Req.ID,
    Owner:           rec.Req.Owner,
    APIKeyLabel:     rec.Req.APIKeyLabel,
    Model:           rec.Req.Model,
    EnqueuedAt:      humanTime(rec.Req.EnqueuedAt),
    DispatchedAtISO: rec.DispatchedAt.UTC().Format(time.RFC3339),
    WordCount:       rec.Req.WordCount,
    CanCancel:       isAdmin || rec.Req.Owner == u.Username || isTokenOwner,
})
```

Ensure `"time"` is imported (it already is).

- [ ] **Step 3: Populate EnqueuedAtISO and CanCancel in renderDashboard**

In `renderDashboard`, find the `QueuedJobRow` construction (around line 229) and add the new fields:
```go
data.QueueItems = append(data.QueueItems, QueuedJobRow{
    ID:            req.ID,
    Owner:         req.Owner,
    APIKeyLabel:   req.APIKeyLabel,
    Model:         req.Model,
    Priority:      priorityName(int(req.Priority)),
    EnqueuedAt:    humanTime(req.EnqueuedAt),
    EnqueuedAtISO: req.EnqueuedAt.UTC().Format(time.RFC3339),
    WordCount:     req.WordCount,
    CanCancel:     u.Role == "admin",
})
```

- [ ] **Step 4: Build to confirm it compiles**

```bash
go build ./router/...
```
Expected: no errors

- [ ] **Step 5: Commit**

```bash
git add router/internal/admin/pages.go
git commit -m "feat(admin): add ISO timestamps to job rows for elapsed time display"
```

---

## Task 9: Elapsed time — templates and JS

**Files:**
- Modify: `router/internal/admin/templates/clients.html`
- Modify: `router/internal/admin/templates/dashboard.html`
- Modify: `router/internal/admin/static/admin.js`

- [ ] **Step 1: Update clients.html job row**

In `clients.html`, find the `.job-row` div (around line 18). Replace the `job-time` span with an elapsed span that uses `data-since`:

```html
{{/* Before: */}}
<span class="job-time muted">{{.EnqueuedAt}}</span>

{{/* After: */}}
{{if .DispatchedAtISO}}
<span class="job-elapsed muted" data-since="{{.DispatchedAtISO}}" title="dispatched {{.EnqueuedAt}}">—</span>
{{end}}
```

- [ ] **Step 2: Update dashboard.html queue table**

In `dashboard.html`, find the queue table header and rows.

Update the header to replace the "Queued" column:
```html
{{/* Before: */}}
<thead><tr><th>User</th><th>Key</th><th>Model</th><th>Priority</th><th>Words</th><th>Queued</th><th></th></tr></thead>

{{/* After: */}}
<thead><tr><th>User</th><th>Key</th><th>Model</th><th>Priority</th><th>Words</th><th>Waiting</th><th></th></tr></thead>
```

Update the queue row to use `data-since` on the Queued cell:
```html
{{/* Before: */}}
<td data-label="Queued" class="muted">{{.EnqueuedAt}}</td>

{{/* After: */}}
<td data-label="Waiting" class="muted">
  {{if .EnqueuedAtISO}}
  <span data-since="{{.EnqueuedAtISO}}" title="queued {{.EnqueuedAt}}">—</span>
  {{else}}{{.EnqueuedAt}}{{end}}
</td>
```

- [ ] **Step 3: Add formatElapsed and elapsed ticker to admin.js**

At the bottom of `router/internal/admin/static/admin.js`, add:

```js
/* ─── Elapsed time display ───────────────────────────────────── */

function formatElapsed(isoString) {
  var ms = Date.now() - new Date(isoString).getTime();
  if (ms < 1000) return '< 1s';
  var s = Math.floor(ms / 1000);
  var m = Math.floor(s / 60);
  s = s % 60;
  if (m > 0) return m + 'm ' + (s < 10 ? '0' : '') + s + 's';
  return s + 's';
}

function updateElapsed() {
  document.querySelectorAll('[data-since]').forEach(function(el) {
    el.textContent = formatElapsed(el.getAttribute('data-since'));
  });
}

(function() {
  if (!document.querySelector('[data-since]')) return;
  updateElapsed();
  setInterval(updateElapsed, 1000);
})();
```

- [ ] **Step 4: Build to confirm it compiles**

```bash
go build ./router/...
```
Expected: no errors

- [ ] **Step 5: Commit**

```bash
git add router/internal/admin/templates/clients.html \
        router/internal/admin/templates/dashboard.html \
        router/internal/admin/static/admin.js
git commit -m "feat(admin): show elapsed running/waiting time on in-flight and queued jobs"
```

---

## Task 10: Queue visibility for non-admins

**Files:**
- Modify: `router/internal/admin/pages.go`
- Modify: `router/internal/admin/templates/dashboard.html`

- [ ] **Step 1: Write the failing test**

Create `router/internal/admin/pages_test.go`:

```go
package admin

import (
	"testing"

	"llmesh/pkg/types"
)

func TestFilterQueueForUser(t *testing.T) {
	items := []types.InferenceRequest{
		{ID: "req-alice", Owner: "alice"},
		{ID: "req-bob", Owner: "bob"},
	}

	// Admin sees all.
	got := filterQueueForUser(items, User{Username: "admin", Role: "admin"})
	if len(got) != 2 {
		t.Errorf("admin: expected 2, got %d", len(got))
	}

	// Member sees only own.
	got = filterQueueForUser(items, User{Username: "alice", Role: "member"})
	if len(got) != 1 {
		t.Fatalf("alice: expected 1, got %d", len(got))
	}
	if got[0].Owner != "alice" {
		t.Errorf("alice: expected own item, got owner=%q", got[0].Owner)
	}

	// Member with no items sees nothing.
	got = filterQueueForUser(items, User{Username: "carol", Role: "member"})
	if len(got) != 0 {
		t.Errorf("carol: expected 0, got %d", len(got))
	}
}
```

- [ ] **Step 2: Run to confirm it fails**

```bash
go test ./router/internal/admin/... -run TestQueueVisibility -v
```
Expected: compile error — `filterQueueForUser undefined`

- [ ] **Step 3: Add CanCancel to QueuedJobRow and add filterQueueForUser helper**

In `pages.go`, update `QueuedJobRow` to add `CanCancel`:
```go
type QueuedJobRow struct {
	ID            string
	Owner         string
	APIKeyLabel   string
	Model         string
	Priority      string
	EnqueuedAt    string
	EnqueuedAtISO string
	WordCount     int
	CanCancel     bool // true only for admins
}
```

Then add the `filterQueueForUser` helper near `renderDashboard`:

```go
// filterQueueForUser returns only the queue items visible to u.
// Admins see all items; members see only their own.
func filterQueueForUser(items []types.InferenceRequest, u User) []types.InferenceRequest {
	if u.Role == "admin" {
		return items
	}
	var out []types.InferenceRequest
	for _, req := range items {
		if req.Owner == u.Username {
			out = append(out, req)
		}
	}
	return out
}
```

`"llmesh/pkg/types"` is already imported in `pages.go`.

- [ ] **Step 4: Update renderDashboard to use filterQueueForUser**

In `renderDashboard`, replace the existing `if a.queue != nil` block entirely:

```go
if a.queue != nil {
    snap := a.queue.Snapshot()
    visible := filterQueueForUser(snap, u)
    data.QueueLen = len(snap) // total depth for header badge
    data.QueueItems = make([]QueuedJobRow, 0, len(visible))
    for _, req := range visible {
        data.QueueItems = append(data.QueueItems, QueuedJobRow{
            ID:            req.ID,
            Owner:         req.Owner,
            APIKeyLabel:   req.APIKeyLabel,
            Model:         req.Model,
            Priority:      priorityName(int(req.Priority)),
            EnqueuedAt:    humanTime(req.EnqueuedAt),
            EnqueuedAtISO: req.EnqueuedAt.UTC().Format(time.RFC3339),
            WordCount:     req.WordCount,
            CanCancel:     u.Role == "admin",
        })
    }
}
```

- [ ] **Step 5: Update dashboard.html to show queue to all users**

In `dashboard.html`, find the outer `{{if .IsAdmin}}` guard around the queue section and remove it (keep the section itself):

```html
{{/* Before: */}}
{{if .IsAdmin}}
<div class="collapsible" style="margin-top:24px;" id="queue-section-wrap">
  ...
</div>
{{end}}

{{/* After: */}}
{{if or .IsAdmin .QueueItems}}
<div class="collapsible" style="margin-top:24px;" id="queue-section-wrap">
  ...
</div>
{{end}}
```

> `{{if or .IsAdmin .QueueItems}}` keeps the section visible for admins even when the queue is empty (so they see "Queue is empty"), while hiding it for non-admins who have nothing queued.

Also update the cancel button in the queue table to be gated on `CanCancel`:

```html
{{/* Before: */}}
<td>
  <form method="POST" action="/portal/queue/cancel" ...>
    ...
    <button ...>&#x2715;</button>
  </form>
</td>

{{/* After: */}}
<td>
  {{if .CanCancel}}
  <form method="POST" action="/portal/queue/cancel" ...>
    ...
    <button ...>&#x2715;</button>
  </form>
  {{end}}
</td>
```

- [ ] **Step 6: Run tests to confirm they pass**

```bash
go test ./router/internal/admin/... -run TestFilterQueueForUser -v
```
Expected: `PASS`

- [ ] **Step 7: Run all tests**

```bash
go test -v -race -count=1 ./...
```
Expected: all pass

- [ ] **Step 8: Build both binaries**

```bash
go build ./router/cmd/router/... && go build ./client/cmd/client/...
```
Expected: no errors

- [ ] **Step 9: Commit**

```bash
git add router/internal/admin/pages.go \
        router/internal/admin/templates/dashboard.html
git commit -m "feat(admin): show non-admins their own queued requests in the dashboard"
```

---

## Task 11: End-to-end smoke test

- [ ] **Step 1: Run full test suite with race detector**

```bash
cd /home/tteoh/llmesh
go test -v -race -count=1 ./...
```
Expected: all pass, no race conditions

- [ ] **Step 2: Run end-to-end tests**

```bash
go test -v -timeout 120s ./router/e2e/...
```
Expected: all pass

- [ ] **Step 3: Verify admin portal manually**

Start the router (or use existing deployment). Navigate to `/portal`. Check:
- Dashboard logs tab loads (not blank) → bug fix verified
- Dashboard stats section polls correctly → bug fix verified
- Queue section visible to a member user (showing only their own items)
- In-flight job rows show "Xm Ys" elapsed and update every second
- Queued job rows show "Xm Ys" waiting time

- [ ] **Step 4: Tag completion**

```bash
git log --oneline -10
```
Review the commit history is clean and sensible.
