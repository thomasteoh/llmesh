# llmesh — Lease & Release Design

## Overview

Three changes:

1. **Bug fix** — logs tab in the admin portal shows no entries (stale URL)
2. **Job leases** — requests dispatched to a client are locked with a time-bound lease; the router reclaims the slot if the lease expires
3. **Client release** — clients can hand a job back to the router queue rather than failing it; the router re-queues for another client

---

## 1. Bug Fix: Logs Tab

**Root cause:** `router/internal/admin/static/admin.js:157` fetches `/admin/api/logs?...`. The portal was renamed from `/admin` to `/portal` (commit `02523c2`), but this fetch URL was not updated. The backward-compat redirect at `/admin/` → `/portal/` is registered in the HTTP mux but applies only to full page navigations, not to fetch requests that need JSON responses.

**Fix:** Change the fetch URL to `/portal/api/logs?...`.

**File:** `router/internal/admin/static/admin.js` — one line.

---

## 2. Job Leases

### Purpose

When the scheduler dispatches a job to a client, the client's in-flight slot is incremented and the job is tracked in `hub.jobs`. If the client is still WS-connected but stops processing (llama.cpp hung, keepalive stopped), the slot stays occupied indefinitely — blocking new work from using that client.

A lease bounds how long a dispatched job can remain unresolved. After the lease expires, the router reclaims the slot, sends a cancel to the client, and wakes the scheduler.

### Lease duration

```
const LeaseDuration = 20 * time.Minute
```

Covers the worst-case router HTTP timeout:
- Stream: 15 min TTFT + 5 min activity = 20 min
- Batch: 10 min

The HTTP handler's own timer fires at the same time, so any lease expiry is cleanup only — the caller has already received a timeout error. The router does **not** re-queue on expiry.

### Changes

**`router/internal/hub/hub.go`**

Add to `InFlightRecord`:
```go
LeaseExpiry time.Time
```

Set in `TrackJob`:
```go
h.jobs[req.ID] = InFlightRecord{
    ClientID:    clientID,
    ClientToken: token,
    Req:         req,
    LeaseExpiry: time.Now().Add(LeaseDuration),
}
```

Add `StartLeaseReaper()` method — background goroutine, 30 s ticker:
1. Lock `h.mu`, collect expired records (those with `LeaseExpiry.Before(now)`)
2. For each expired record:
   - Delete from `h.jobs`
   - Decrement `inFlight` on the client (if still connected)
   - Send a `CancelMsg` to the client
3. If any leases expired, call `h.OnAvailable()` to wake the scheduler

No re-queue on expiry. The HTTP handler's timeout fires at the same wall-clock time; re-queuing would dispatch to a new client with no one waiting for the response.

---

## 3. Client Release

### Purpose

When a client's llama.cpp call fails (error from inference, timeout per existing keepalive rules), the client currently sends `ErrorMsg` — which fails the request and returns an error to the caller. With release, the client instead hands the job back; the router re-queues it and another client can pick it up.

### New type (`pkg/types/types.go`)

```go
// ReleaseMsg is sent by a client to return a job to the router queue.
// The router re-queues the request for another client to handle.
type ReleaseMsg struct {
    Type      string `json:"type"`       // "release"
    RequestID string `json:"request_id"`
    Reason    string `json:"reason"`     // "model_failed" | "timeout"
}
```

### Router: hub changes

**New callback on `Hub`:**
```go
OnRelease func(req types.InferenceRequest)
```

**`dispatch` new case `"release"`:**
```
1. Parse ReleaseMsg
2. Look up InFlightRecord by RequestID
3. If not found: ignore (already completed or expired)
4. untrackJob(requestID)
5. DecrInFlight(clientID)
6. Call h.OnRelease(rec.Req)   // re-queue
7. Call h.OnAvailable()        // wake scheduler
```

### Router: wiring (`router/cmd/router/main.go`)

After scheduler creation, add:

```go
h.OnRelease = func(req types.InferenceRequest) {
    q.Push(req)
    sched.Wake()
}
h.StartLeaseReaper()
```

### Client: worker changes (`client/internal/worker/worker.go`)

Current behaviour: `llmClient.Infer` error → `ErrorMsg`.

New behaviour:
- `llmClient.Infer` error → `ReleaseMsg{Reason: "model_failed"}` (re-queue)
- No endpoint found (pre-inference) → keep `ErrorMsg` (unrecoverable; no other client can fix it)

The keepalive goroutine (60 s ticker, sends empty chunks) already keeps the router's activity timer alive during long prompt evaluation. No changes to the keepalive are needed.

---

## Data flow with leases

```
Scheduler dispatches job
  → hub.TrackJob sets LeaseExpiry = now + 20min
  → client.InFlight++

Normal completion (chunk done=true):
  hub.dispatch("chunk") → untrack, InFlight--, OnAvailable

Client release (infer error):
  worker sends ReleaseMsg
  hub.dispatch("release") → untrack, InFlight--, OnRelease(req), OnAvailable
  OnRelease → q.Push(req), sched.Wake()
  → scheduler re-dispatches to next available client

Lease expiry (client hung, WS still open):
  leaseReaper fires → untrack, InFlight--, CancelMsg to client, OnAvailable
  (no re-queue; HTTP handler has already timed out)

Client disconnect:
  existing orphan cleanup path unchanged — fails in-flight jobs immediately
```

---

## Files changed

| File | Change |
|------|--------|
| `router/internal/admin/static/admin.js` | Fix fetch URL `/admin/` → `/portal/` |
| `pkg/types/types.go` | Add `ReleaseMsg` |
| `router/internal/hub/hub.go` | `LeaseDuration` const, `LeaseExpiry` on `InFlightRecord`, `OnRelease` callback, `dispatch` "release" case, `StartLeaseReaper` |
| `router/cmd/router/main.go` | Wire `h.OnRelease`, call `h.StartLeaseReaper()` |
| `client/internal/worker/worker.go` | Send `ReleaseMsg` on `Infer` error |

---

## Out of scope

- Retry limits per request (re-queue loops until HTTP timeout)
- Excluding the releasing client from re-queue candidates
- Persisting lease state across restarts
