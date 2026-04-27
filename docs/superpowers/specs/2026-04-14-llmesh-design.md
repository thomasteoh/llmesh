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

### Priority Queue (`router/internal/queue`)

Flat-slice with O(n) linear scan. `PopBest(availableModels)` returns the highest-priority request whose model has an available client. Within the same priority tier, oldest enqueue time wins (FIFO).

### Hub (`router/internal/hub`)

WebSocket hub + client registry. Each connected client tracks:
- Supported models (`map[string]bool`)
- Max concurrent jobs
- In-flight count (atomic)

`FindAvailable(model)` returns a client ID with capacity. `OnChunk` / `OnError` callbacks wire to the correlation store.

### Scheduler (`router/internal/scheduler`)

Single-goroutine dispatch loop. Woken by:
1. New request enqueued (`Wake()`)
2. Client finishes a job (`hub.OnAvailable` callback)

On each wake: calls `AvailableModels()`, `PopBest`, `FindAvailable`, increments in-flight, sends `JobMsg` over WS.

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
