# llmesh — LLM Request Router & Client Design

## Overview

llmesh is a self-hosted LLM API gateway compatible with OpenAI and Anthropic request formats. Agents and tools point at this gateway instead of the real APIs. Actual inference runs on local llama.cpp nodes. This enables:

- Portable agents (they only know the router endpoint)
- Horizontal scaling by adding more client nodes
- Per-API-key request priority (high/normal/low)
- Model-based routing (clients advertise supported models)

Both the router (server) and client (local node) ship as Docker images.

---

## Architecture

```
[Caller (agent/tool)]           [llm-router]             [llm-client(s)]
        │                            │                          │
        │  POST /v1/chat/completions │                          │
        │  POST /v1/messages         │   wss://.../ws/client    │
        │  POST /v1/responses        │◄─────────────────────────│
        │───────────────────────────►│  {register, models, cap} │
        │                            │◄─────────────────────────│
        │  (holds HTTP connection)   │                          │
        │                            │  schedule job            │
        │                            │─────────────────────────►│
        │                            │  {reqID, messages, model}│
        │                            │                          │── llama.cpp
        │                            │  {reqID, delta, done}    │◄─ server
        │  SSE / full response       │◄─────────────────────────│
        │◄───────────────────────────│                          │
```

---

## Project Layout

```
/home/tteoh/llmesh/
├── go.mod
├── pkg/
│   └── types/types.go            # Shared types: InferenceRequest, Message, WS messages
├── router/
│   ├── cmd/router/main.go        # Entry point: wires all subsystems, starts HTTP server
│   ├── config.go                 # Config loader (YAML)
│   ├── config.yaml.example
│   ├── internal/
│   │   ├── api/                  # HTTP handlers + auth helpers
│   │   │   ├── auth.go
│   │   │   └── handler.go
│   │   ├── correlation/store.go  # requestID → result channel map
│   │   ├── hub/hub.go            # WebSocket client hub + registry
│   │   ├── queue/queue.go        # In-memory priority queue
│   │   ├── scheduler/scheduler.go # Dispatch loop
│   │   └── translate/translate.go # 3-format inbound + SSE outbound formatters
│   └── Dockerfile
├── client/
│   ├── cmd/client/main.go        # Entry point: loads config, connects to router
│   ├── config.go                 # Config loader (YAML)
│   ├── config.yaml.example
│   ├── internal/
│   │   ├── llamacpp/client.go    # OpenAI-compat HTTP client for llama.cpp
│   │   ├── worker/worker.go      # Per-job handler
│   │   └── ws/conn.go            # WS connection to router + reconnect loop
│   └── Dockerfile
├── docker-compose.yml            # Router service (port 53002)
└── docker-compose.client.yml     # Client service (local machine)
```

---

## Shared Types (`pkg/types`)

```go
type Priority int
const (PriorityHigh Priority = 0; PriorityNormal Priority = 1; PriorityLow Priority = 2)

type InferenceRequest struct {
    ID          string
    Model       string
    Messages    []Message
    MaxTokens   int
    Temperature float64
    TopP        float64
    Stream      bool
    SourceFmt   string    // "openai" | "anthropic" | "openai-responses"
    Priority    Priority  // 0=high, 1=normal, 2=low
    EnqueuedAt  time.Time
}

type Message struct {
    Role    string  // "system" | "user" | "assistant"
    Content string
}

// WebSocket message types:
type RegisterMsg { Type: "register";  Models: []ModelInfo; MaxConcurrent: int }
type JobMsg      { Type: "job";       Request: InferenceRequest }
type ChunkMsg    { Type: "chunk";     RequestID, Delta string; Done bool; FinishReason string }
type ErrorMsg    { Type: "error";     RequestID, Message string }
```

---

## Router Design

### HTTP Endpoints

| Path | Method | Description |
|------|--------|-------------|
| `/v1/chat/completions` | POST | OpenAI chat completions |
| `/v1/messages` | POST | Anthropic messages |
| `/v1/responses` | POST | OpenAI Responses API |
| `/ws/client` | GET (WS upgrade) | Client registration |
| `/health` | GET | Health check |

**Auth**: `/v1/*` require `Authorization: Bearer <api-key>`. `/ws/client` requires `Authorization: Bearer <client-token>`.

### Router Config (`router/config.yaml.example`)

`config.yaml` covers infrastructure only. Credentials (admin users, API keys, client tokens) are managed via the admin UI and persisted in `state.json`.

```yaml
name: "llmesh"              # brand name shown on landing page and admin UI
host: "llmesh.example.com"  # public hostname — shown in admin UI client setup instructions
server:
  port: 53002
```

| Field | Default | Description |
|-------|---------|-------------|
| `name` | `llmesh` | Brand name shown on the landing page |
| `host` | `llmesh.example.com` | Public hostname used in admin UI when generating client config |
| `server.port` | `53002` | Port the router listens on |

### Model Routing

Requests are matched to clients using the `model` field in three ways:

| `model` value | Behaviour |
|---------------|-----------|
| Canonical name (e.g. `llama3.2:3b`) | Must exactly match a model the client has registered |
| Alias (e.g. `qwen`) | Resolved via the alias map in admin Settings; the scheduler rewrites the field to the canonical name before dispatch |
| `any` (pseudo-model) | Matches any client with at least one model loaded; the scheduler rewrites the field to the client's actual model name before dispatch |

**Alias configuration** — Portal → Settings → Model Aliases. Each alias maps to one or more canonical model names. If multiple targets are listed, the first one the selected client supports is used.

**`any` pseudo-model** — useful when the caller doesn't care which model handles the request (e.g. internal tooling, health probes, load tests). All normal dispatch constraints still apply:
- Exclusive clients only serve their own owner's requests — an `any` request from a different owner will not be dispatched to an exclusive client.
- Priority and FIFO ordering are unchanged.
- The `model` field in the forwarded job is always a concrete model name; the client never sees `"any"`.

### Priority Queue (`router/internal/queue`)

Flat-slice with O(n) linear scan. `PopBest(availableModels)` returns the highest-priority request whose model (or alias, or `any`) has an available client. Within the same priority tier, oldest enqueue time wins (FIFO).

A configurable `MaxDepth` cap limits queue size. New API requests use `TryPush` (returns HTTP 429 when full); internal re-queues (retries, lease releases) use `Push` and bypass the cap.

### Hub (`router/internal/hub`)

WebSocket hub + client registry. Each connected client tracks:
- Supported models (`map[string]bool`)
- Max concurrent jobs
- In-flight count (atomic)
- `ExclusiveOwner bool` — when true, the client only receives jobs from its own owner (configured per-client in the portal)

`FindAvailable(model)` returns a client ID with capacity. `OnChunk` / `OnError` callbacks wire to the correlation store.

### Scheduler (`router/internal/scheduler`)

Single-goroutine dispatch loop. Woken by:
1. New request enqueued (`Wake()`)
2. Client finishes a job (`hub.OnAvailable` callback)

On each wake: for each available client, finds the best matching request (affinity > priority > FIFO), picks the globally best (client, request) pair, rewrites alias/`any` to the concrete model name, and sends a `JobMsg` over WS.

### Correlation Store (`router/internal/correlation`)

`map[requestID]chan ChunkMsg` — HTTP handler creates the channel before enqueuing, reads from it until `done=true` or 60s timeout. Channel is deleted (closed) when the handler returns, unblocking any late-arriving chunks from the worker.

### Request Flow

```
HTTP request arrives
  → auth check (Bearer token lookup)
  → parse body (inbound translator: openai/anthropic/responses → InferenceRequest)
  → assign UUID, priority, timestamp
  → create correlation channel
  → push to queue, wake scheduler
  → block on channel (stream SSE or buffer for full response)
  → 60s timeout if no client picks up

Scheduler wakes
  → find available client for model
  → send JobMsg over WebSocket
  → client streams ChunkMsgs back
  → correlation store delivers to waiting HTTP handler
  → handler formats as SSE (or full JSON) and flushes to caller
```

### Streaming Response Formats

**OpenAI SSE:**
```
data: {"id":"...","object":"chat.completion.chunk","choices":[{"delta":{"content":"Hello"},"finish_reason":null}]}
data: [DONE]
```

**Anthropic SSE:**
```
data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"Hello"}}
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}
data: {"type":"message_stop"}
```

**Non-streaming**: router buffers all chunks, returns full response body.

---

## Client Design

### Client Config (`client/config.yaml.example`)

```yaml
router_url: "wss://llmesh.example.com/ws/client"  # WebSocket URL of the router
router_token: "ct-admin-xxxxxxxxxxxxxxxx"           # client token from router admin UI → Clients
max_concurrent: 4
models:
  - name: "llama3.2:3b"
    endpoint: "http://host.docker.internal:8080"
  - name: "unsloth/qwen3-30b-a3b"
    endpoint: "http://host.docker.internal:8081"
    # chat_template: "qwen2.5"   # optional: override model's built-in Jinja template
```

`router_token` must be created in the router admin UI (Clients tab) before starting the client.

### Lifecycle

1. Dial router WS with `Authorization: Bearer <router_token>`
2. Send `RegisterMsg` (models + max_concurrent)
3. Read loop: on `JobMsg` → acquire semaphore → goroutine calls `worker.Handle`
4. Worker calls llama.cpp `/v1/chat/completions`, streams ChunkMsgs back to router
5. On disconnect: exponential backoff reconnect (1s → 2s → 4s → … → 60s max)

Per-connection `context.Context` is cancelled on disconnect, aborting any in-flight llama.cpp requests.

---

## Deployment

### Router (server VM, teoh user)

```bash
cd /home/tteoh/llmesh
cp router/config.yaml.example router/config.yaml
# Edit: set name, host, server.port
docker compose up -d
caddy-request add llm.teoh.co 53002
```

Navigate to `http://[HOST]:[PORT]/admin`. On first run the setup wizard creates the initial admin account. From the admin dashboard:
- **Clients** → Create a client token for each `llm-client` instance
- **API Keys** → Create API keys for callers
- **Settings** → Configure model aliases if needed

All credentials live in `state.json` (mounted volume). There are no credentials in `config.yaml`.

### Client (local machine)

```bash
cp client/config.yaml.example client/config.yaml
# Edit: set router_url, router_token (from admin UI → Clients), model endpoints
docker compose -f docker-compose.client.yml up -d
```

---

## Verification

```bash
# Health check
curl https://llm.teoh.co/health

# OpenAI streaming
curl https://llm.teoh.co/v1/chat/completions \
  -H "Authorization: Bearer sk-prod-abc123" \
  -H "Content-Type: application/json" \
  -d '{"model":"llama3.2:3b","messages":[{"role":"user","content":"hi"}],"stream":true}'

# Anthropic format
curl https://llm.teoh.co/v1/messages \
  -H "Authorization: Bearer sk-prod-abc123" \
  -H "Content-Type: application/json" \
  -d '{"model":"llama3.2:3b","messages":[{"role":"user","content":"hi"}],"max_tokens":100}'
```

---

## Shim Design

### Overview

`llmesh-shim` is a second worker variant that uses the same WebSocket protocol as `llmesh-client` but dispatches jobs to external HTTP APIs or shell command adapters rather than a local llama.cpp server. No local GPU is required.

The key difference:

| | llmesh-client | llmesh-shim |
|---|---|---|
| GPU required | Yes | No |
| Backend | llama.cpp HTTP | OpenAI / Anthropic / shell command |
| Model discovery | Probes `/props` on llama.cpp | Reads model list from config |
| Context size | Read from llama.cpp `/props` | Set in config (`context_size`) |

### Shim Config (`shim/config.yaml.example`)

```yaml
router_url: "wss://llm.example.com/ws/client"
router_token: "ct-your-token-here"
max_concurrent: 4

models:
  - name: "gpt-4o"
    context_size: 128000
    backend:
      type: http
      url: "https://api.openai.com"
      format: openai          # "openai" or "anthropic"
      auth_type: bearer       # "bearer", "header", or "none"
      auth_value: "${OPENAI_API_KEY}"

  - name: "claude-sonnet-4-5"
    context_size: 200000
    backend:
      type: http
      url: "https://api.anthropic.com"
      format: anthropic
      auth_type: bearer
      auth_value: "${ANTHROPIC_API_KEY}"

  - name: "my-model"
    backend:
      type: command
      command: "python3 /path/to/adapter.py"
```

`${VAR}` references in `auth_value` and `command` are expanded from the environment at startup.

### Lifecycle

1. Load config; expand `${VAR}` in auth/command fields
2. Dial router WS with `Authorization: Bearer <router_token>`
3. Send `RegisterMsg` with model names and `context_size` values from config
4. Read loop: on `JobMsg` → acquire semaphore → goroutine calls `worker.Handle`
5. Worker dispatches to HTTP backend or shell command adapter, streams `ChunkMsg`s back
6. Keep-alive: empty `ChunkMsg` sent every 60 s to reset router's TTFT timer
7. On disconnect: exponential backoff reconnect (1 s → 2 s → … → 60 s max)

### Backends

**HTTP backend** — Translates `InferenceRequest` to the upstream wire format:
- `format: openai` → `POST /v1/chat/completions` (OpenAI, Ollama, vLLM, LM Studio, etc.)
- `format: anthropic` → `POST /v1/messages`

OpenAI streaming uses `stream_options: {include_usage: true}` to obtain token counts.
Anthropic streaming extracts `input_tokens` from `message_start` and `output_tokens` from `message_delta`.

**Command adapter** — Runs `sh -c <command>` per request, writing JSON-encoded `InferenceRequest` to stdin and reading NDJSON chunks from stdout. See `shim/man/llmesh-shim.1` for the full adapter protocol.

### Portal Integration

The shim binaries are built alongside the client binaries in `router/Dockerfile` (when `INCLUDE_CLIENTS=true`) for four platforms:

```
/downloads/llmesh-shim-linux-amd64
/downloads/llmesh-shim-linux-arm64
/downloads/llmesh-shim-darwin-amd64
/downloads/llmesh-shim-darwin-arm64
/downloads/llmesh-shim.1
/downloads/docker-compose.shim.yml
/downloads/config.yaml.example  (shim config example)
```

The admin portal's **Clients** page includes a shim downloads card (below the client downloads card) with per-platform download links, run instructions, and a pre-filled config download at `/portal/clients/shim-config`.
