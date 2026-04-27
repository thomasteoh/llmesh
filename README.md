# llmesh

A lightweight, self-hosted LLM router that pools your llama.cpp instances into a single OpenAI/Anthropic-compatible API endpoint.

## Architecture

```
[Caller (agent/tool)]    [llm-router]         [llm-client(s)]
        │                     │                      │
        │  POST /v1/...        │   wss://.../ws/client│
        │─────────────────────►│◄─────────────────────│
        │                     │  schedule job         │
        │                     │──────────────────────►│── llama.cpp
        │  SSE / response      │◄──────────────────────│
        │◄─────────────────────│
```

Callers only need to know the router URL. Inference runs on local llama.cpp nodes connected as clients over WebSocket.

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
git clone <repo> llmesh && cd llmesh
docker compose build
docker compose -f docker-compose.client.yml build
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

## License

Private / self-hosted only.
