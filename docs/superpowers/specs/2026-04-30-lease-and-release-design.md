# llmesh — Lease, Release & Request Visibility Design

## Overview

Five changes in this spec:

1. **Bug fixes** — two stale `/admin/` URLs in `admin.js` break logs tab and dashboard polling
2. **Job leases** — dispatched requests are time-bounded; the router reclaims the slot on expiry
3. **Client release** — clients return failed jobs to the queue instead of failing the caller
4. **Elapsed time display** — show "running Xm Ys" on in-flight jobs, "waiting Xm Ys" on queued jobs
5. **Queue visibility for non-admins** — users see their own queued and in-flight requests

---

## 1. Bug Fixes

Two fetch URLs in `router/internal/admin/static/admin.js` were not updated when the portal moved from `/admin` to `/portal`.

| Line | Current (broken) | Fixed |
|------|-----------------|-------|
| 157 | `/admin/api/logs?...` | `/portal/api/logs?...` |
| 390 | `/admin/api/dashboard` | `/portal/api/dashboard` |

The backward-compat redirect (`/admin/` → `/portal/`) is registered in the HTTP mux but does not apply to JSON API fetch requests.

---

## 2. Job Leases

### Purpose

When the scheduler dispatches a job, the client's in-flight counter is incremented and the job is tracked in `hub.jobs`. If the client is still WS-connected but stops processing (llama.cpp hung, keepalive goroutine died), the slot stays occupied indefinitely. A lease bounds how long a dispatched job can remain unresolved; on expiry, the router reclaims the slot, sends a cancel to the client, and wakes the scheduler.

### Lease duration

```go
const LeaseDuration = 20 * time.Minute
```

Covers the worst-case router HTTP timeout: stream 15 min TTFT + 5 min activity = 20 min; batch 10 min. The HTTP handler's own timer fires at the same wall-clock time, so lease expiry is cleanup only — the caller has already received a timeout error. The router does **not** re-queue on expiry.

### `InFlightRecord` changes

Add two fields to `hub.InFlightRecord`:

```go
DispatchedAt time.Time  // set in TrackJob; used for elapsed-time display and lease
LeaseExpiry  time.Time  // = DispatchedAt + LeaseDuration
```

`TrackJob` sets both:

```go
now := time.Now()
h.jobs[req.ID] = InFlightRecord{
    ClientID:     clientID,
    ClientToken:  token,
    Req:          req,
    DispatchedAt: now,
    LeaseExpiry:  now.Add(LeaseDuration),
}
```

### Lease reaper

`StartLeaseReaper()` — background goroutine, 30 s ticker:

1. Lock `h.mu`, collect records where `rec.LeaseExpiry.Before(now)`
2. For each expired record: delete from `h.jobs`, decrement inflight on the client (if still connected), send `CancelMsg` to the client
3. If any leases expired, call `h.OnAvailable()` to wake the scheduler

No re-queue on expiry. The HTTP handler has already timed out; re-queuing would dispatch work with no waiter.

---

## 3. Client Release

### Purpose

When a client's `llmClient.Infer` call fails (model error, timeout), the client currently sends `ErrorMsg` which fails the request immediately. With release, the client returns the job to the queue and another client can pick it up.

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

1. Parse `ReleaseMsg`
2. Look up `InFlightRecord` by `RequestID`; if not found, ignore (already completed or lease expired)
3. `untrackJob(requestID)`
4. `DecrInFlight(clientID)`
5. Call `h.OnRelease(rec.Req)` — re-queue
6. Call `h.OnAvailable()` — wake scheduler

### Router: wiring (`router/cmd/router/main.go`)

After scheduler creation:

```go
h.OnRelease = func(req types.InferenceRequest) {
    q.Push(req)
    sched.Wake()
}
h.StartLeaseReaper()
```

### Client: worker changes (`client/internal/worker/worker.go`)

| Failure path | Before | After |
|---|---|---|
| `llmClient.Infer` returns error | `ErrorMsg` | `ReleaseMsg{Reason: "model_failed"}` |
| No endpoint found (pre-inference) | `ErrorMsg` | `ErrorMsg` (unchanged — unrecoverable) |

---

## 4. Elapsed Time Display

### Purpose

Show how long each request has been running (in-flight) or waiting (queued), so operators can spot stuck or slow requests at a glance.

### Data changes

**`hub.InFlightRecord`** already gains `DispatchedAt` from section 2.

**`admin.InFlightJobRow`** — add:
```go
DispatchedAtISO string  // RFC3339, for JS elapsed computation
```

**`admin.QueuedJobRow`** — add:
```go
EnqueuedAtISO string  // RFC3339, for JS elapsed computation
```

Populated in `pages.go` alongside the existing `EnqueuedAt`/`DispatchedAt` humanTime fields:
```go
EnqueuedAtISO:  req.EnqueuedAt.UTC().Format(time.RFC3339),
DispatchedAtISO: rec.DispatchedAt.UTC().Format(time.RFC3339),
```

### Template changes

**`clients.html`** — in `.job-row`, add after the existing `job-time` span:
```html
<span class="job-elapsed muted" data-since="{{.DispatchedAtISO}}">—</span>
```

**`dashboard.html`** — in the queue table row, add an "Elapsed" column:
```html
<span class="job-elapsed muted" data-since="{{.EnqueuedAtISO}}">—</span>
```

### JS changes (`admin.js`)

Add a shared `formatElapsed(isoString)` helper and a `setInterval(updateElapsed, 1000)` that finds all `[data-since]` elements and sets their `textContent` to "Xm Ys" or "< 1s".

The format: `2m 14s` (minutes + seconds). No hours display needed (requests time out within 20 min).

---

## 5. Queue Visibility for Non-Admins

### Scoping rules

| Role | Queue (waiting) | In-flight (running) |
|------|----------------|---------------------|
| Admin | All requests | All requests on all clients |
| Non-admin | Own submitted requests | All requests on own client tokens |

In-flight visibility on non-owned clients (jobs submitted by the user but running on another user's client) is out of scope — this edge case is rare in practice and the queue view covers the "where is my request?" question.

### Changes

**`pages.go` — `renderDashboard`:**
- Remove the `u.Role == "admin"` guard on `QueueItems` population
- For non-admins, filter: `if u.Role != "admin" && req.Owner != u.Username { continue }`
- Keep the `cancel` button gated on `u.Role == "admin"` (non-admins can't cancel others' jobs from the queue)

**`dashboard.html`:**
- Remove `{{if .IsAdmin}}` outer guard on the queue section
- Gate the cancel button on `{{if .CanCancel}}` (add `CanCancel bool` to `QueuedJobRow`)

**`pages.go` — `QueuedJobRow`:**
- Add `CanCancel bool` (set to `u.Role == "admin"`)

---

## Data flow with leases

```
Scheduler dispatches job
  → hub.TrackJob sets DispatchedAt = now, LeaseExpiry = now + 20min
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
| `router/internal/admin/static/admin.js` | Fix 2 stale `/admin/` URLs; add `formatElapsed` + `setInterval` |
| `pkg/types/types.go` | Add `ReleaseMsg` |
| `router/internal/hub/hub.go` | `LeaseDuration`, `DispatchedAt`/`LeaseExpiry` on `InFlightRecord`, `OnRelease` callback, `dispatch` "release" case, `StartLeaseReaper` |
| `router/internal/admin/pages.go` | Add `DispatchedAtISO`/`EnqueuedAtISO` to rows; queue visible to non-admins (filtered); add `CanCancel` to `QueuedJobRow` |
| `router/internal/admin/templates/clients.html` | Add `data-since` span to job rows |
| `router/internal/admin/templates/dashboard.html` | Add `data-since` span, remove admin gate on queue section, gate cancel on `CanCancel` |
| `router/cmd/router/main.go` | Wire `h.OnRelease`, call `h.StartLeaseReaper()` |
| `client/internal/worker/worker.go` | Send `ReleaseMsg` on `Infer` error |

---

## Out of scope

- Retry limits per request (re-queue loops until HTTP timeout fires)
- Excluding the releasing client from re-queue candidates
- Persisting lease state across restarts
- In-flight visibility for own jobs running on another user's client
