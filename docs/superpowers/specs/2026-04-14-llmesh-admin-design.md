# llmesh Admin UI — Design Spec

## Overview

A web management console embedded in the router binary. Provides API key management, client token management, user management, and endpoint documentation — all behind a login wall.

**Not a separate process.** The UI is served by the router on the same port, under the `/admin` path prefix.

---

## Persistence: `state.json`

`config.yaml` remains read-only (loaded once at startup). A separate `state.json` file holds all mutable state. The router loads `state.json` at startup and writes it on every mutation. Both files live in the same directory (mounted into the container).

```json
{
  "users": [
    {
      "username": "admin",
      "password_hash": "$2a$...",
      "role": "admin",
      "disabled": false
    }
  ],
  "api_keys": [
    {
      "label": "prod-agents",
      "owner": "admin",
      "key": "sk-admin-a1b2c3d4e5f6g7h8",
      "priority": "high",
      "created_at": "2026-04-14T08:00:00Z"
    }
  ],
  "client_tokens": [
    {
      "name": "macbook-pro",
      "owner": "admin",
      "token": "ct-admin-a1b2c3d4e5f6g7h8",
      "created_at": "2026-04-14T08:00:00Z"
    }
  ]
}
```

`state.json` is created on first run (first `POST /admin/setup`). If it does not exist or has no users, all `/admin` routes redirect to `/admin/setup`.

---

## Authentication

### First-run setup

`GET /admin/setup` — rendered if `state.json` missing or has no users. Shows a form: username + password + confirm. On submit, creates the first user with role `admin`, bcrypt-hashes the password, writes `state.json`, and redirects to `/admin/login`.

### Login

`POST /admin/login` — checks username + bcrypt password against `state.json`. On success, creates a session ID (32-byte random hex), stores `map[sessionID]sessionEntry{username, expiry}` in-memory, sets `admin_session` cookie (HttpOnly, SameSite=Lax, 24h). Disabled accounts are rejected with "Account disabled."

### Session middleware

All `/admin` routes (except `/admin/login`, `/admin/setup`) require a valid non-expired session cookie. On failure: redirect to `/admin/login`. Sessions are checked against the in-memory map; expired entries are purged lazily.

### Sign out

`POST /admin/logout` — deletes session from map, clears cookie, redirects to `/admin/login`.

---

## Users & Roles

Two roles: `admin` and `member`.

| Capability | member | admin |
|---|---|---|
| View own API keys / client tokens | ✓ | ✓ |
| Create / revoke own keys & tokens | ✓ | ✓ |
| Change own password | ✓ | ✓ |
| View all users | — | ✓ |
| Add users | — | ✓ |
| Disable / enable users | — | ✓ |
| Promote / demote users | — | ✓ |
| View docs | ✓ | ✓ |

**Invariants (enforced server-side):**
- Cannot disable yourself.
- Cannot demote yourself.
- Must always be at least one active admin — operations that would violate this are rejected.
- New users are created as `member` by default; the add-user form does not expose a role selector (admins promote after creation).

---

## Namespacing

Both API keys and client tokens are namespaced to their owner.

**API keys:**
- Display label: `{username}/{label}` (e.g., `admin/prod-agents`)
- Token value: `sk-{username}-{16 random hex chars}` (e.g., `sk-admin-a1b2c3d4e5f6g7h8`)
- Members see only their own keys; admins see all keys.

**Client tokens:**
- Display name: `{username}/{name}` (e.g., `admin/macbook-pro`)
- Token value: `ct-{username}-{16 random hex chars}` (e.g., `ct-admin-a1b2c3d4e5f6g7h8`)
- Members see only their own tokens; admins see all tokens.

**Uniqueness:** Labels and names must be unique within `{username}/` scope (two different users can both have a key labeled `dev`).

---

## Changes to Existing Router Code

### API key validation

`/v1/*` handlers currently look up API keys from `config.yaml` (slice of key+priority). This must change: the handlers look up the bearer token against `state.json`'s `api_keys` list (via the shared `*admin.State`). Priority is read from the matched key's `priority` field. Unrecognised or revoked keys get `401`.

### Hub changes

The router's WebSocket hub currently validates a single `cfg.Server.ClientToken`. This must be replaced: `GET /ws/client` validates the `Authorization: Bearer <token>` header against the `client_tokens` list in `state.json`. A valid, non-revoked token is accepted; the client's `name` and `owner` are recorded in the registry entry.

The hub also gains a `CloseByToken(token string)` method used by the revoke handler to disconnect an active client immediately when its token is revoked.

---

## Pages

### Navigation (top bar)

All authenticated pages share a top nav: `llmesh` wordmark · Dashboard · API Keys · Client Tokens · Docs · Settings · `{username} · Sign out`

Members and admins see the same nav. The Settings page shows different content based on role.

### `/admin/` → Dashboard

Four stat cards: Total Requests (since startup, in-memory counter) · Active Clients · API Keys · Client Tokens.

Connected clients table: Name · Owner · Status · Last seen · Models.

Status values:
- `● connected` — client has an active WebSocket connection.
- `○ offline` — client registered a token, last WS connection closed. Last seen time tracked in-memory in the registry (not persisted).
- `○ never connected` — token exists in `state.json` but no WS connection has ever been made this process lifetime.

Dashboard auto-refreshes every 10 seconds via `setInterval` + `fetch('/admin/api/dashboard')`.

### `/admin/api-keys`

**Add key form:** `{username}/` prefix (non-editable) + label input + priority select (normal / high / low) + Generate button.

**Keys table:** Label (`{username}/{label}`) · Key (truncated, copy button) · Priority (coloured badge) · Created · Revoke button.

Generated key is shown in full once immediately after creation in a dismissible banner. It cannot be retrieved again.

Members see only their own keys. Admins see all keys, grouped or sorted by owner.

### `/admin/client-tokens`

**Add token form:** `{username}/` prefix + name input + Generate button.

**Tokens table:** Name (`{username}/{name}`) · Token (truncated, copy button) · Status · Last seen · Created · Revoke button.

Status is live (sourced from the client registry). Revoking removes the token from `state.json` and closes any active WebSocket connection for that token.

Members see only their own tokens. Admins see all tokens.

### `/admin/docs`

Static server-rendered page. Left sidebar nav:

**Endpoints**
- OpenAI compatible
- Anthropic compatible
- Responses API

**Setup**
- Router config
- Client setup
- Docker deploy
- Priority tiers

Content shown for each doc section: endpoint URL, auth header format, curl example, SDK usage snippet.

No JS required — pure server-rendered HTML.

### `/admin/settings`

**All users — Change password card:**
Current password · New password · Confirm new password · Update button.

**Admins only — Users section:**
Inline add-user form: username + password + Add button (creates member role).

Users table: Username · Role badge (admin / member) · Status badge (active / disabled) · Actions.

Actions per row:
- Self-row: no actions shown (marked "(you)").
- Other active admin: Demote · Disable.
- Other active member: Promote · Disable.
- Disabled user: Enable only.

Footer note: "Cannot disable or demote yourself. At least one active admin must remain."

---

## Implementation: Go + html/template

Templates are embedded with `embed.FS` (`router/internal/admin/templates/`). Stylesheets and any JS are also embedded (`router/internal/admin/static/`).

A single CSS file defines CSS variables for the dark theme (surfaces, border, accent, text colours) used across all pages. No build step — vanilla JS only, no frameworks.

All form submissions use standard HTML `POST` forms with a redirect-after-POST pattern (PRG). The dashboard auto-refresh uses `fetch` to a JSON endpoint (`/admin/api/dashboard`) and updates the DOM with `innerHTML`.

### New packages

```
router/internal/admin/
├── handler.go        # HTTP mux, middleware wiring
├── auth.go           # login, logout, setup, session middleware
├── state.go          # load/save state.json, mutex-protected
├── pages.go          # page handlers (dashboard, api-keys, client-tokens, docs, settings)
├── api.go            # JSON endpoints (/admin/api/dashboard)
├── templates/
│   ├── layout.html
│   ├── login.html
│   ├── setup.html
│   ├── dashboard.html
│   ├── api-keys.html
│   ├── client-tokens.html
│   ├── docs.html
│   └── settings.html
└── static/
    └── admin.css
```

### Wiring into main.go

`admin.New(statePath, hub, registry)` returns an `http.Handler`. Registered at `/admin/` in the main router mux.

The hub exposes a method to close a connection by token (needed for revoke). The registry exposes a read method for dashboard stats and client status.

---

## Security Notes

- All state mutations (key generation, revocation, user changes) are `POST`-only — `GET` handlers never mutate state.
- CSRF: since the UI is same-site forms only (no JS cross-origin requests), `SameSite=Lax` on the session cookie provides adequate protection for this single-user-ish admin tool.
- Passwords: bcrypt cost factor 12.
- Generated key/token values: 16 bytes from `crypto/rand`, hex-encoded.
- The full token value is only returned once (in the creation response). `state.json` stores the full value (it is itself a credential file that must be protected).
