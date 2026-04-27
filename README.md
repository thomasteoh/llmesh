# llmesh

llmesh lets you share your local AI models across a team without anyone needing to know which machine is running what.

You point all your tools — agents, scripts, IDE plugins — at one URL. llmesh takes care of routing each request to whichever of your machines has the right model free. When one machine is busy, requests wait in a queue and go to the next available one. When a machine goes offline, the rest keep working.

It speaks the same API formats as OpenAI and Anthropic, so anything that already works with those services works with llmesh without changes. Everything runs on your own hardware. Nothing leaves your network.

---

## Design philosophy

**Thin and focused.** The router routes requests — it does not run inference, transform outputs, or call external services. One job, done reliably.

**Self-hosted by default.** No accounts to create, no telemetry, no external dependencies at runtime. State lives in a single JSON file on disk.

**Protocol-compatible.** OpenAI and Anthropic wire formats are supported natively. If a tool works with those APIs, it works with llmesh.

**Simple to operate.** One binary per role, configuration in YAML, state in JSON. No database, no message broker, no service mesh required.

**Network-friendly.** Client machines connect *out* to the router over WebSocket — no inbound firewall rules or port forwarding needed on client machines.

---

## System requirements

### Router

The router does not run inference and is deliberately lightweight.

- **OS:** Linux or macOS (x86-64 or ARM64)
- **RAM:** ~64 MB minimum under load
- **CPU:** Any — the router is I/O-bound, not compute-bound
- **Runtime:** Docker (for the published image), or Go 1.26+ to build from source
- **Network:** Reachable by both callers and client machines; TLS termination recommended (e.g. via a reverse proxy)

### Client (llm-client)

The client is a small binary that runs alongside your llama.cpp instance.

- **OS:** Linux (x86-64, ARM64) or macOS (Intel, Apple Silicon)
- **RAM:** ~10 MB for the client binary itself — GPU/RAM requirements are determined by your llama.cpp models
- **Runtime:** Requires a running llama.cpp HTTP server (`llama-server`) on the same machine
- **Network:** Outbound HTTPS/WSS to the router only; no inbound ports required

---

## Architecture

llmesh sits between callers (agents, tools, scripts) and your llama.cpp instances:

- **Router** — single API endpoint, pools all connected clients, handles authentication, request queuing, and affinity-based scheduling.
- **Client** — lightweight agent running on each machine with llama.cpp; connects to the router over WebSocket and dispatches inference jobs.

Callers only need to know the router URL. Inference runs on local llama.cpp nodes connected as clients over WebSocket.

### Request Flow

```mermaid
sequenceDiagram
    participant C as Caller
    participant R as Router
    participant W as llm-client
    participant L as llama.cpp

    C->>R: POST /v1/chat/completions
    R->>R: validate API key
    R->>R: queue + schedule job
    R->>W: WebSocket "job" (JSON)
    W->>L: POST /v1/chat/completions (SSE)
    L-->>W: SSE stream of tokens
    W-->>R: WebSocket "chunk" messages
    R-->>C: SSE stream
    R->>W: WebSocket "cancel" (on client disconnect)
```

### Scheduling Strategy

The router dispatches requests to available clients using **client-centric affinity scheduling**:

1. **Owner affinity** — a request from user X prefers a client registered by user X
2. **Priority tier** — requests can be tagged `high`, `normal`, or `low`
3. **FIFO** — within the same tier, oldest first

Model aliases allow multiple clients serving different implementations of the same model to be addressed by a single logical name (e.g., `gpt-4o` → `unsloth/qwen3-30b` or `llama3.1:70b`).

---

## Deployment

### 1. Router

The router runs on your server and exposes the API endpoint.

**Configure**

```bash
cp router/config.yaml.example router/config.yaml
```

Edit `router/config.yaml`:

```yaml
name: "llmesh"              # brand name shown on landing page and admin UI
host: "llmesh.example.com"  # public hostname (used in admin UI client setup instructions)
server:
  port: 53002
```

| Field | Required | Default | Description |
|-------|----------|---------|-------------|
| `name` | No | `llmesh` | Brand name shown on the landing page |
| `host` | No | `llmesh.example.com` | Public hostname — shown in admin UI when generating client config |
| `server.port` | No | `53002` | Port the router listens on |

**Start**

```bash
docker compose up -d
```

The `state.json` file (admin users, API keys, client tokens) is created automatically on first run. It is mounted as a volume and persists across container restarts.

**First-run setup**

Navigate to `http://[HOST]:[PORT]/admin`. On first run you are redirected to the setup wizard to create the initial admin account. All credentials are managed via this UI — there are no credentials in `config.yaml`.

From the admin dashboard you can:
- **Clients** → Create client tokens (needed to configure each `llm-client`)
- **API Keys** → Create API keys (needed by callers to authenticate requests)
- **Settings** → Configure model aliases

---

### 2. Client

The client runs on any machine with llama.cpp and connects back to the router. Run one client per machine.

**Configure**

```bash
cp client/config.yaml.example client/config.yaml
```

Edit `client/config.yaml`:

```yaml
router_url: "wss://llmesh.example.com/ws/client"  # WebSocket URL of the router
router_token: "ct-admin-xxxxxxxxxxxxxxxx"           # client token from router admin UI
max_concurrent: 4                                   # max simultaneous inference jobs
models:
  - name: "llama3.2:3b"
    endpoint: "http://host.docker.internal:8080"    # llama.cpp HTTP server
  - name: "unsloth/qwen3-30b-a3b"
    endpoint: "http://host.docker.internal:8081"
    # chat_template: "qwen2.5"                      # optional: override model's built-in Jinja template
```

| Field | Required | Default | Description |
|-------|----------|---------|-------------|
| `router_url` | Yes | — | WebSocket URL of the router (`wss://` for TLS, `ws://` for plain) |
| `router_token` | Yes | — | Client token created in the router admin UI |
| `max_concurrent` | No | `4` | Maximum simultaneous inference jobs this client will handle |
| `models[].name` | Yes | — | Model name exactly as callers will request it |
| `models[].endpoint` | Yes | — | HTTP base URL of the llama.cpp server for this model |
| `models[].chat_template` | No | — | Override the model's built-in Jinja chat template (e.g. `"qwen2.5"`) |

The `router_token` must be created first in the router's admin UI under **Clients**.

**Start**

```bash
docker compose -f docker-compose.client.yml up -d
```

`host.docker.internal` resolves to the Docker host — use this to reach llama.cpp servers running on the same machine outside Docker.

---

## Build from source

```bash
git clone https://github.com/thomasteoh/llmesh && cd llmesh
docker compose build
docker compose -f docker-compose.client.yml build
```

---

## Testing

### Unit tests

```bash
go test -v -race -count=1 ./...
```

Run on every pull request. Includes race detection; coverage is uploaded to Codecov.

### End-to-end tests

```bash
go test -v -timeout 120s ./router/e2e/...
```

Spins up an in-process router with a mock llama.cpp client and exercises the full request path: HTTP → auth → queue → scheduler → WebSocket → response translation. Run on push to `master`.

---

## Releases

Docker images are published to the GitHub Container Registry on every GitHub release:

```
ghcr.io/thomasteoh/llmesh:<version>
```

Tags generated per release:

| Tag | Example | Description |
|-----|---------|-------------|
| `{{version}}` | `0.1.0` | Exact release version |
| `{{major}}.{{minor}}` | `0.1` | Major.minor track |
| `latest` | `latest` | Most recent stable release (not applied to release candidates) |

To use the published image instead of building from source, replace the `build:` block in `docker-compose.yml`:

```yaml
services:
  llm-router:
    image: ghcr.io/thomasteoh/llmesh:latest
```

---

## API Endpoints

Replace `[HOST]` and `[PORT]` with your router's address (port default: `53002`).

| Endpoint | Description |
|----------|-------------|
| `POST /v1/chat/completions` | OpenAI chat completions (streaming + non-streaming) |
| `POST /v1/messages` | Anthropic messages API |
| `POST /v1/responses` | OpenAI Responses API |
| `GET /health` | Health check |
| `GET /admin` | Admin dashboard |

All `/v1/*` endpoints require `Authorization: Bearer <api-key>`.

---

## Project Structure

```
llmesh/
├── router/                       # Router server
│   ├── config.yaml.example       # Config template
│   ├── Dockerfile
│   └── internal/
│       ├── api/                  # HTTP handlers + auth
│       ├── admin/                # Admin UI
│       ├── hub/                  # WebSocket client registry
│       ├── queue/                # Priority request queue
│       ├── scheduler/            # Dispatch loop
│       └── translate/            # OpenAI/Anthropic/Responses format translation
├── client/                       # Client binary
│   ├── config.yaml.example       # Config template
│   ├── Dockerfile
│   └── internal/
│       ├── llamacpp/             # llama.cpp HTTP client
│       ├── worker/               # Per-job handler
│       └── ws/                   # WebSocket connection + reconnect
├── pkg/types/                    # Shared message types
├── docker-compose.yml            # Router service
└── docker-compose.client.yml     # Client service
```

---

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

[MIT](LICENSE)
