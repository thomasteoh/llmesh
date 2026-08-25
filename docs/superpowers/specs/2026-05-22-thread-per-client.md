# Thread-per-client reorganization

> **Status: not implemented, and Phases 1–2 no longer apply.** Recovered from a
> stash dated 2026-05-22 and archived unchanged below this note.
>
> The plan is built on an `ExclusiveOwner` boolean on both `ClientToken` and
> `Client`. That field no longer exists anywhere in the tree: per-token
> exclusivity was replaced by `OwnerSlots map[string]int`, a per-model count of
> slots reserved for the owner, where 0 means fully shared. So "flip a thread to
> owner-only" is now "reserve N slots for this model", and Phases 1, 2, 5.2 and
> 5.3 would have to be rewritten against that model before any of this could be
> built.
>
> Phases 3–4, the thread-card UI, are still coherent as a direction, but they
> overlap the Clients page as it now stands: the per-model rows added since
> reorganised the same sub-rows this plan proposes replacing.

Each WebSocket connection (thread) under a client token gets its own card with its own shared/owner-only toggle. Jobs are nested under their respective threads.

---

## Phase 1 — Hub plumbing

Expose per-thread `ExclusiveOwner` state and add a single-client toggle.

- [ ] 1.1 Add `ExclusiveOwner bool` to `ConnectedClientInfo` struct (`hub.go`)
- [ ] 1.2 Populate `ExclusiveOwner` when building `ConnectedClientInfo` in `ConnectedClientsByToken()`
- [ ] 1.3 Add `SetClientExclusiveByID(clientID string, exclusive bool)` to `Hub`
- [ ] 1.4 Run existing tests to confirm no regressions

## Phase 2 — Backend data + handler

Wire per-thread exclusive state into the admin page model.

- [ ] 2.1 Add `ID string` and `ExclusiveOwner bool` to `ConnectedClientRow` (`pages.go`)
- [ ] 2.2 Populate new fields from `ConnectedClientInfo` in `renderClientTokens()`
- [ ] 2.3 Add `handleThreadExclusive()` handler — reads `client_id` (UUID) + `exclusive` form fields, calls `hub.SetClientExclusiveByID`
- [ ] 2.4 Register `POST /portal/clients/thread-exclusive` in `handler.go`
- [ ] 2.5 Run `go vet ./...` to confirm clean

## Phase 3 — Template restructure

Replace table-based layout with thread cards.

- [ ] 3.1 Remove `conn-subrow` named template (no longer needed)
- [ ] 3.2 Admin view: replace `<table>` inside `.user-group` with `<div class="thread-card">` per token
  - Header: token name, status badge, last seen, copy button, token-level exclusive toggle ("owner only (all)" / "shared (all)"), revoke
  - Models line under header
  - Each connection as `<div class="thread-conn">` with: conn name, version, capacity, **per-thread exclusive toggle**, nested jobs
- [ ] 3.3 Non-admin view: same thread-card structure, no `.user-group` wrapper
- [ ] 3.4 Verify admin filter/pagination JS still works (filters `.user-group` elements — still present, just contain cards now)
- [ ] 3.5 Verify `data-since` elapsed-time JS still works (selectors unchanged)

## Phase 4 — CSS

Style the new thread card layout.

- [ ] 4.1 `.thread-card` — card container with border, radius, margin-bottom
- [ ] 4.2 `.thread-card-header` — flex row with background, border-bottom
- [ ] 4.3 `.thread-name`, `.thread-last-seen`, `.thread-actions` — header internals
- [ ] 4.4 `.thread-models` — muted monospace sub-line
- [ ] 4.5 `.thread-conn` / `.thread-conn-header` / `.thread-conn-name` — connection block
- [ ] 4.6 `.thread-jobs` — indented job rows under each connection
- [ ] 4.7 Responsive check: cards stack naturally on mobile (no table-to-card conversion needed)

## Phase 5 — Verify + deploy

- [ ] 5.1 Build + local smoke test (admin + non-admin views, connected + disconnected tokens)
- [ ] 5.2 Test per-thread toggle: click → redirect → state flips
- [ ] 5.3 Test token-level toggle: flips all threads + persists to state.json
- [ ] 5.4 Run `rsync` + `podman build` + `systemctl --user restart llm-router` per CLAUDE.md

---

## Design notes

- **Token-level toggle** (existing) flips `ExclusiveOwner` on the persisted `ClientToken` record + all live connections. Permanent.
- **Thread-level toggle** (new) flips `Client.ExclusiveOwner` in-memory only via `SetClientExclusiveByID`. Does not persist — threads reconnect with the token-level setting. Intentional: it's an operational override.
- No JS changes needed. Filter/pagination operates on `.user-group` wrappers; elapsed-time uses `[data-since]` selectors — both untouched.
