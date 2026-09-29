# Access management v2

> **Status: approved 2026-09-29; all seven phases implemented.** Replaces the ad-hoc admin/member checks,
> per-user isolation flags, and per-token `owner_slots` with one authorization
> model covering the portal, inference admission, and dispatch.

## Why

The current model is two roles and a handful of per-feature switches, each
enforced where it is used:

- ~30 `Role == "admin"` string checks and five `isAdmin bool` parameters in
  `router/internal/admin`, with "members see their own" reimplemented per page.
- Sessions, API keys, and client tokens each decide separately whether their
  owner may still act, which is how disabling a user left their keys working.
- Sessions live in process memory: lost on restart, not revocable per user, and
  a blocker for running more than one router.
- Every API key can use every model and endpoint; keys never expire.
- Sharing capacity is two unrelated mechanisms — user isolation flags
  (`scheduler.isolationAllows`) and per-token reserved slots (`OwnerSlots`) —
  neither of which can say "share with my team" or "share only when idle".
- Request coalescing (`dedup.ContentHashOpts`) ignores the owner, so a follower
  can receive a response produced under someone else's permissions.
- Upstream router hops replace the caller with `upstream:<name>`, so nothing
  downstream can apply per-user rules.
- The per-key concurrency limit is compared against the owner's total in-flight
  count, not the key's (`api/handler.go:286-289`).

## Goals

1. One decision point for every access question, with a reason for every "no".
2. Roles that are named permission sets, including custom ones.
3. Teams that own keys and clients and share access.
4. Per-model permissions, enforced at admission, listing, and dispatch.
5. Attribute-based rules for the cases roles and ACLs cannot express.
6. A one-click way to share idle capacity, without reading any of the above.
7. Credentials with a lifecycle: expiry, revocation, last use, one owner check.
8. Identity providers able to drive roles, teams, attributes, and deactivation.
9. No behaviour change on upgrade until an admin changes something.

## Non-goals (this version)

- Hard multi-tenancy (teams that cannot see each other exist). Teams are groups
  within one router; see open question 1.
- Nested teams.
- Carrying end-user identity across upstream router hops (the upstream is a
  principal in its own right; see §8).
- Per-request token budgets / spend caps (the attribute model leaves room).

---

## 1. Concepts

**Principal** — something that acts:

| Kind | Examples | Authenticates with |
|---|---|---|
| `user` | a person | session (password, email link, OAuth/OIDC) |
| `team` | "research", "ops" | never directly; owns keys and clients |
| `router` | an upstream router connected as a client | client token |

A credential always acts **as** a principal. An API key owned by a team acts as
that team, narrowed by the key's own scope (§5). A user acts with the union of
their own grants and their teams' grants.

**Resource** — something acted on: `model`, `client`, `key`, `team`, `user`,
`usage`, `audit`, `settings`, `alias`, `policy`.

**Action** — a verb on a resource type, as a dotted string (catalogue in §3).

**Decision** — `allow` or `deny`, with the rule that produced it.

## 2. Data model

New tables; existing ones gain columns. All additive, migrated on start.

```
principals        id TEXT PK, kind (user|team|router), name, disabled,
                  attrs JSON, managed_by, created_at
                  -- users keep their row in `users`; principals.id = "user:<username>"
teams             id PK -> principals, display_name, description
team_members      team_id, user_id, team_role (maintainer|member), PK(team_id,user_id)

roles             id PK, name, description, builtin BOOL, permissions JSON
role_bindings     principal_id, role_id, scope (global|team:<id>), PK(all three)

policies          id PK, name, description, effect (allow|deny), priority INT,
                  enabled, subject JSON, action JSON, resource JSON,
                  condition JSON, created_by, updated_at
policy_version    single row, bumped on any change to the tables above

sessions          id_hash PK, principal_id, created_at, expires_at,
                  last_seen_at, ip, user_agent, method, revoked_at
api_keys          + owner_id (principal), expires_at, last_used_at,
                    revoked_at, created_by, scope JSON
client_tokens     + owner_id (principal), sharing JSON, tags JSON,
                    revoked_at, created_by, last_used_at
```

`users.role`, `users.send_isolation`, `users.receive_isolation`, and
`client_tokens.owner_slots` stay readable for one release and are migrated into
role bindings, policies, and `sharing` (§10), then dropped.

## 3. Permissions and roles (RBAC)

Permissions are `resource.action[.scope]`, where scope is `own`, `team`, or
`any`. A sample of the catalogue:

| Permission | Meaning |
|---|---|
| `model.use` | send inference to a model (further narrowed by §4, §6) |
| `key.create.own` / `key.manage.team` / `key.manage.any` | API keys |
| `client.create.own` / `client.manage.team` / `client.manage.any` | client tokens, sharing |
| `usage.view.own` / `.team` / `.any` | usage and performance data |
| `job.cancel.own` / `.team` / `.any` | in-flight and queued jobs |
| `fleet.view` | see other people's clients (today admin-only) |
| `team.create`, `team.manage.own`, `team.manage.any` | teams and membership |
| `user.manage` | create, disable, reset, bind roles |
| `alias.manage`, `pricing.manage`, `settings.manage`, `upstream.manage` | router config |
| `policy.manage`, `policy.simulate` | §6 |
| `audit.view` | audit log |

**Built-in roles** (not editable, can be cloned):

| Role | Summary |
|---|---|
| `owner` | everything; cannot be removed from the last holder |
| `admin` | everything except removing owners |
| `operator` | fleet, aliases, queue, pricing, upstreams; no users/policy |
| `auditor` | read-only on everything, including audit and usage.any |
| `member` | `model.use`, own keys/clients/usage/jobs, `team.create` |
| `viewer` | own usage only; cannot create keys |

Team-scoped bindings: `maintainer` (manage the team's keys, clients, members)
and `member` (use the team's keys and see its usage). A binding with scope
`team:<id>` grants its `.team` permissions for that team only.

**Custom roles** are permission sets an admin assembles in the portal.

## 4. Model permissions

`model.use` from a role says a principal may use models at all. Which models is
an ACL on the model, evaluated as policy (§6) but managed on its own page
because it is the common case:

```
model "gpt-4o"        allow: team:finance, role:admin
model "qwen3-*"       allow: everyone
model "*"             allow: everyone            -- the migrated default
```

- Patterns are globs on the concrete model name. Aliases are checked on their
  targets: an alias is usable if at least one target is, and the scheduler only
  resolves it to targets the request may use.
- Enforced at **admission** (403 with the reason), **listing** (`/v1/models`
  and `/v1/models/slots` show only usable models), and **dispatch** (never
  resolve an alias to a forbidden target, never pair with a client §7 forbids).
- Model attributes available to rules: `name`, `modality`, `context_size`,
  `pricing_basis` (actual = a paid API behind a shim), `served_by_kind`
  (llama.cpp client, shim, upstream router).

## 5. Credentials

- **One liveness check.** `authz.Active(principal)` — not disabled, not revoked,
  not expired, owner active, team active — is the only way any credential is
  accepted. Sessions, API keys, and client tokens all call it.
- **Sessions** move to SQLite (hashed id). Revocable individually and per user;
  "sign out everywhere" runs automatically on password change, disable, role
  change, and IdP deactivation. A small in-memory cache keeps the per-request
  cost where it is now.
- **API keys** gain `expires_at` (optional, admin-enforceable maximum),
  `last_used_at`, `revoked_at` (revoke keeps the row for audit), `created_by`,
  and `scope`: allowed models, endpoints, priority ceiling, max concurrency.
  **A key's scope can only narrow its owner's permissions, never widen them.**
- **Concurrency limits** are counted per key when set on a key, per principal
  when set on a principal (fixes the current mix-up).
- **Client tokens** gain `sharing` (§7), owner-assigned `tags`, revocation, and
  last use.

## 6. Policies (ABAC)

A policy is `effect` + `subject` + `action` + `resource` + optional
`condition`. Subject, action, and resource are matchers (exact, glob, or
set); the condition is a small expression tree over attributes:

```json
{
  "name": "Contractors: local models only, business hours",
  "effect": "deny",
  "subject":  { "attr": { "employment": "contractor" } },
  "action":   ["model.use"],
  "resource": { "model": "*" },
  "condition": { "any": [
    { "ne":  ["resource.served_by_kind", "llama.cpp"] },
    { "not": { "time_between": ["context.time", "08:00", "18:00", "Australia/Melbourne"] } }
  ]}
}
```

**Attributes**

| Namespace | Attributes |
|---|---|
| `subject.*` | `id`, `kind`, `roles`, `teams`, `managed_by`, and free-form `attrs` (set by admins or mapped from IdP claims, e.g. `department`) |
| `resource.*` | model attributes (§4); client `owner`, `team`, `tags`, `kind`; key `owner`, `labels` |
| `context.*` | `time`, `source_ip`, `endpoint`, `priority`, `prompt_tokens_estimate`, `credential_kind` (session/key/token), `via_upstream` |

Operators: `eq ne in not_in glob cidr lt le gt ge time_between all any not
has`. No loops, no calls, no regex — evaluation is bounded and cannot fail
open.

**Evaluation**

1. Collect policies matching subject+action+resource (indexed by action and
   resource type).
2. **Deny overrides allow.** Any matching, enabled deny whose condition holds
   wins. Otherwise any matching allow wins. Otherwise **deny** (default-deny,
   with migrated allow-all defaults so upgrades change nothing).
3. Roles are compiled into allow policies with no condition, so there is one
   evaluator, not two.
4. A condition that references a missing attribute evaluates to false for allow
   and **true for deny** — an absent attribute never grants and never escapes a
   restriction.

**Performance.** Policies compile into per-action decision tables at each
`policy_version` bump. Decisions without context-dependent conditions are
memoised per (subject, action, resource, version). The scheduler hot path (§7)
reads a precomputed pairing matrix, rebuilt on version bump or fleet change,
and never evaluates conditions per drain unless a rule depends on `context.*`.
Target: no measurable change in drain latency (benchmarked in phase 5).

**Explainability.** Every decision returns the deciding policy id. Denials are
logged (sampled) and shown to the caller as `403 {"error": ..., "policy": ...}`
without leaking other principals. A **simulator** in the portal answers "can
*X* do *Y* to *Z*, and why" and diffs a draft policy against live traffic from
the last N hours before it is enabled.

## 7. Sharing capacity

Every client token has a `sharing` setting. The portal shows three presets and
hides the rest behind "Advanced":

| Preset | Who may send work | When |
|---|---|---|
| **Private** | owner (and owner's team, if team-owned) | always |
| **Share when idle** (the simple case) | anyone permitted by policy | only while the owner has nothing queued for it; owner work always goes first, and N slots can be kept back |
| **Shared** | anyone permitted by policy | always, owner preferred |

Advanced adds: an allowlist (users, teams, roles), reserved slots per model
(what `owner_slots` does today), a per-requester concurrency cap, and
**reclaim**: when owner work arrives and no slot is free, release a non-owner
job that has not produced its first token back to the queue (uses the existing
lease/release path). Streaming jobs are never interrupted.

Dispatch pairs a request with a client only if **both** sides allow it:

- *request side*: the request's principal may `model.use` the resolved model
  and may use a client with that client's attributes (e.g. a rule keeping
  sensitive teams on their own hardware — today's send isolation);
- *client side*: the client's `sharing` admits the request's principal and the
  timing mode is satisfied (today's receive isolation and reserved slots).

This replaces `isolationAllows`, the `OwnerSlots` check, and
`AvailableSlotsByModel`'s owner logic with one `authz.CanPair`.

**Coalescing** only joins requests whose principals would be admitted to the
same model and clients; in practice, the same owner and key scope.

## 8. Upstream routers

An upstream router is a `router` principal on the downstream side. Its jobs are
subject to model ACLs and client sharing like anyone else's, and it can be
bound to roles and appear in policies (`subject.kind == "router"`). The
connector only advertises models and capacity the upstream principal is allowed
to reach through this router's local sharing rules (today it advertises
everything).

## 9. Identity providers

The OIDC provider (and, later, SCIM) maps into this model rather than beside it:

- **Roles**: claim values → role bindings (many-to-many, replacing the single
  member/admin mapping).
- **Teams**: a groups claim → team membership, for teams marked IdP-managed.
- **Attributes**: selected claims → `subject.attrs` for use in policies.
- **Deactivation**: refresh-token revalidation (hourly, configurable). A
  refused refresh or a lost required role disables the principal, which
  revokes sessions, keys, and client tokens through `authz.Active`.
- Accounts made locally keep their bindings (break-glass), as in the OIDC
  branch today.

## 10. Migration (no behaviour change)

| Today | Becomes |
|---|---|
| `role = admin` | binding to `admin` (first admin → `owner`) |
| `role = member` | binding to `member` |
| send isolation on user U | deny policy: U's requests to clients not owned by U |
| receive isolation on user U | U's clients → sharing `Private` |
| `owner_slots` on a token | sharing `Shared`, advanced reserved slots = same values |
| everything else | sharing `Shared` (today's default), model ACL `* → everyone` |
| keys/tokens `owner` | `owner_id = user:<owner>` |
| in-memory sessions | dropped once on upgrade (users sign in again) |

A migration report on first start lists what was converted, and the old columns
remain readable for one release so the upgrade can be reverted.

## 11. Enforcement points

| Where | Check |
|---|---|
| portal middleware | `can(subject, action, resource)` replaces `requireAdmin` and inline role checks |
| portal lists | one `authz.Filter` for "rows I may see" |
| `/ws/client` | `Active(token owner)`; client registers with its `sharing` |
| `api.enqueue` | `Active(key)`, key scope, `model.use` on requested model/alias targets, limits |
| `/v1/models`, `/v1/models/slots` | filtered by `model.use` and reachable sharing |
| dedup | coalesce only within the same admission scope |
| scheduler | `CanPair` matrix + timing mode; alias resolution restricted to permitted targets |
| upstream connector | advertise only reachable models; inbound jobs as the `router` principal |

## 12. Package layout

```
router/internal/authz/          pure: types, matchers, conditions, evaluator,
                                compiler, decision cache, CanPair matrix
router/internal/authz/store/    SQLite persistence + migrations for §2
router/internal/admin/          portal: roles, teams, policies, sharing, simulator
```

`authz` has no dependency on `admin`, `hub`, or `scheduler`; they depend on it
through small interfaces, so the scheduler can be tested with a fake.

## 13. Phases

Each phase is its own branch and PR, leaves `main` releasable, and preserves
behaviour until an admin changes a setting.

1. **Engine** — `authz` package: model, matchers, conditions, evaluator,
   compiler, cache, `CanPair`; exhaustive table tests and fuzzing. No wiring.
2. **Identity and credentials** — schema, migration, principals, teams, roles
   and bindings, persisted sessions, `Active`, key/token lifecycle and scope,
   per-key concurrency fix.
3. **Portal enforcement** — replace role checks with `can`/`Filter`; users,
   teams, roles pages; session management ("sign out everywhere").
4. **Inference enforcement** — admission, key scope, model ACLs page, listing
   filters, dedup scoping, 403 reasons.
5. **Dispatch and sharing** — `sharing` presets and advanced settings,
   `CanPair` in the scheduler, share-when-idle, reclaim, upstream principal;
   drain-latency benchmark before and after.
6. **Policies** — ABAC editor, simulator with traffic replay, decision audit.
7. **Identity providers** — role/team/attribute mapping, refresh-token
   revalidation, deactivation.

Phases 1–2 are prerequisites; 3, 4, and 5 can proceed in parallel after 2.

## 14. Decisions (2026-09-29)

1. **Tenancy:** teams are groups within one router, not hard tenants.
2. **Default model access:** every router, new or upgraded, starts with
   `* → everyone`. Admins narrow it.
3. **Sharing:** any client owner may choose any sharing mode, including
   Shared. (`client.share.own` is in the member role.)
4. **Sessions:** signing everyone out once when sessions move to SQLite is
   acceptable.
5. **Policy as code:** no YAML export/import.

Implementation notes (phase 2):

- Persistence for §2 lives in `router/internal/admin` beside the tables it
  joins (users, api_keys, client_tokens), not a separate `authz/store`
  package; `authz` itself stays pure.
- There is no `principals` table: users stay in `users`, teams are in
  `teams`, and principal ids are derived ("user:<name>", "team:<id>").
  Owner columns keep bare usernames for user-owned rows.
- Every existing admin becomes an **owner**, not only the first: today any
  admin can do what an owner can, so granting less would take power away.
- Revoking a key or token still deletes the row; the audit log records it.
- Isolation flags and `owner_slots` are converted in phase 5 with the
  scheduler, so the portal's current isolation controls cannot drift from
  policies in the meantime.
- Until phase 3, `users.role` remains what the portal reads; promotions,
  demotions, and OIDC role sync keep the bindings in step.

Implementation notes (phase 3):

- Portal decisions go through `Admin.can`; `requireAdmin` is gone. Routes use
  `requirePerm(action)`; templates branch on a `Can` capability map.
- Added `key.limits` (priority and concurrency on a key), which the portal
  previously reserved for admins.
- Bindings are now the source of truth; `users.role` is derived from them
  ("admin" when owner or admin is held) for OIDC sync and the legacy
  promote/demote endpoints.
- Custom roles can only contain permissions the author holds (a `.team`
  permission requires the `.any` scope to hand out).
- Owners are protected: changing an owner's account or granting/removing the
  owner role takes `owner.manage`. The router always keeps at least one enabled
  owner or admin.

Implementation notes (phase 4):

- Admission computes the permitted concrete models for the request (the
  model itself, an alias's targets, or every active model for `any`) and
  stores them on the request as `AllowedModels`. The queue and the
  scheduler's alias resolution only ever match a permitted model, so the
  dispatch-side half of §4 is already in place for aliases and `any`.
- Key scope is models (globs) and endpoints. A priority ceiling was dropped:
  key priority is already an admin-set `key.limits` field.
- New teams are bound to the member role, so a team's own keys can use
  models; an admin can change the team's roles.
- Model attributes available to policies today: `context_size`. Modality,
  pricing basis, and serving kind need plumbing from the hub and arrive with
  phase 6's policy editor.
- `context.source_ip` is the socket peer unless `trust_proxy_headers` is on,
  matching the portal; the API's older `clientIP` (used only for logging)
  still reads X-Forwarded-For.

Implementation notes (phase 5):

- The scheduler takes a `PairingPolicy`; when set (always, in `main.go`) it
  replaces the isolation filter and the per-token reserved-slot check. The
  answer is model-independent (`authz.ClientPairing`), since model access is
  settled at admission, and is memoised per requester per client per drain.
- "Owner side" is the client's owner or any member of the team that owns it;
  idle-only, reserved slots, and per-requester caps apply only to others.
- Share when idle means: serve others only while no owner-side job is
  running on the client. Queued owner work already wins the queue through
  owner affinity.
- Isolation flags became deny policies on `client.use`
  (`isolation-send-<user>`, `isolation-receive-<user>`); the portal toggles
  now write those policies, and deleting a user removes them.
- `owner_slots` became `sharing.reserved_slots`; the old `"any"` key became
  `"*"` (every model's default), which can only hold back more. The existing
  owner-slots API writes through to sharing.
- Jobs from an upstream router run as a `router:<name>` principal holding
  the member role, which is what they could do before.
- Reclaim (displacing a not-yet-started non-owner job) is deferred; the
  Advanced section does not offer it yet.
- Pairing inputs (requester subjects, client sharing) are cached in `State`
  and dropped wholesale on any access change.

Implementation notes (phase 6):

- Workers report a `kind` at registration (`llama.cpp` from llmesh-client,
  `shim` from llmesh-shim); older workers report none. Model attributes now
  include `served_by_kind` (`router` for an upstream hop, `mixed` when
  clients disagree, absent when any serving worker did not say),
  `modalities`, `context_size`, and `pricing_basis` from the pricing table.
- The policy editor is JSON with strict decoding (unknown fields refused)
  and full-set compilation before storing; the list shows every policy,
  including model-access and isolation rules.
- The simulator answers a single question against the live set and a draft,
  and replays the last 24 hours of usage (owner × model, weighted by request
  count) against a draft. Usage rows carry no context or key scope, so
  context-dependent rules replay as for a request without context.
- Recent denials are an in-memory ring of 200 admission refusals.
- Admins can set user attributes (`subject.attrs.*`) on the Users tab;
  phase 7 maps them from identity-provider claims.
- Settings forms carry their tab in the action (`…#tab-users`) and the
  portal script re-selects it when it swaps in the response.

Implementation notes (phase 7):

- Provider roles map to any llmesh roles (`RoleMap`, alongside the member and
  admin roles); holding any mapped role grants access. A managed account's
  router-wide roles become exactly the mapped set on each sign-in and each
  revalidation, subject to the last-owner/admin guard.
- `GroupsClaim` + `TeamMap` manage membership of the mapped teams only;
  membership of other teams is left alone. `AttrMap` maps claims to
  `subject.attrs`, replacing only the mapped attributes.
- Revalidation (`RevalidateMinutes`, off by default, minimum 5) refreshes
  each managed account's stored refresh token. It disables the account when
  the provider answers no (400/401 or `invalid_grant`), when the refreshed
  identity is a different subject, or when no mapped role remains; network
  and server errors disable no one. `offline_access` is requested
  automatically while it is on. Disabling records `disabled_by = oidc` and
  drops the refresh token; the next successful sign-in re-enables such an
  account, while an admin-disabled account stays disabled.
- Refresh tokens are stored in plaintext beside the other secrets in the
  state database.
- As before, only accounts created by sign-in (`managed_by = oidc`) are
  synced or revalidated; locally created accounts that link an identity keep
  their llmesh roles.

Security review fixes (after phase 7):

An independent review of the branch found escalation paths; all are fixed
and covered by tests.

- Members no longer hold `team.create`: a new team is a new principal with
  its own role, and a member who could mint one could mint keys escaping every
  restriction placed on them. Admins create teams; a maintainer an admin
  appoints can still create team keys, which is a deliberate trust boundary.
  Team enable/disable/delete takes router-wide `team.manage`.
- Team-scoped bindings grant no unscoped permissions at all, including
  `model.use`; the built-in team roles no longer list it.
- `client.use` is granted only by the client's sharing setting; allow
  policies can restrict it but not widen it.
- Granting or removing a role, changing an account (reset, disable, delete,
  sign-out, attributes), mapping a provider role, and writing or enabling an
  allow policy all require holding every permission involved beyond the
  member baseline. Saving a policy that would leave no enabled owner able to
  manage policies, users, roles, and owners is refused.
- Revalidation disables only on `invalid_grant`, never the last active
  owner/admin, and disables no one in a run that would refuse most accounts.
  Provider sync never removes the owner role.
- Jobs from upstream routers pass model access as their router principal;
  the upstream's own permitted set is discarded.
- Choosing a non-normal priority when creating a key takes `key.limits`;
  per-model reservations keep the every-model one; an admin's disable always
  overrides a provider's.
- Re-review follow-ups: deny policies covering the repair actions
  (`policy.manage`, `user.manage`, `role.manage`, `owner.manage`) need
  `owner.manage`, and the lockout check evaluates with the portal's session
  context; re-enabling a user and changing isolation apply the account-change
  rule; appointing a team maintainer takes router-wide `team.manage`.

