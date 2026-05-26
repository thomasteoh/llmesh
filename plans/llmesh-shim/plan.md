# llmesh-shim

A WebSocket client binary that connects to the llmesh router and dispatches jobs to external HTTP API endpoints (OpenAI, Anthropic, etc.) or local shell command adapters. Same WebSocket protocol as `llmesh-client`; no local GPU required.

Distributed as a binary download from the router portal, alongside the existing `llmesh-client` downloads.

---

## Repository layout after completion

```
llmesh/
├── shim/
│   ├── cmd/shim/main.go
│   ├── config.go
│   ├── config.yaml.example
│   ├── Dockerfile
│   ├── docker-compose.shim.yml
│   ├── internal/
│   │   ├── backend/
│   │   │   ├── backend.go       ← Spec type, RunBatch/RunStream dispatch
│   │   │   ├── http.go          ← OpenAI/Anthropic HTTP executor
│   │   │   └── command.go       ← Shell command adapter executor
│   │   ├── worker/
│   │   │   └── worker.go        ← Job handler + keep-alive ticker
│   │   └── ws/
│   │       └── conn.go          ← WebSocket connection + reconnect loop
│   └── man/
│       └── llmesh-shim.1
├── router/
│   ├── Dockerfile               ← modified: build + bundle shim binaries
│   └── internal/admin/
│       ├── handler.go           ← modified: register /portal/clients/shim-config
│       ├── pages.go             ← modified: add handleShimConfig()
│       └── templates/
│           └── clients.html     ← modified: add shim downloads section
└── design.md                    ← modified: add shim section
```

---

## Phase 1 — shim binary

### 1.1 `shim/config.go`

Package `shim`. Define all config types and the loader.

```go
type Config struct {
    RouterURL     string        `yaml:"router_url"`
    RouterToken   string        `yaml:"router_token"`
    MaxConcurrent int           `yaml:"max_concurrent"`
    Models        []ModelConfig `yaml:"models"`
    MetricsAddr   string        `yaml:"metrics_addr"` // optional, e.g. ":9090"
}

type ModelConfig struct {
    Name        string        `yaml:"name"`
    ContextSize int           `yaml:"context_size"` // reported to router; 0 = unknown
    Backend     BackendConfig `yaml:"backend"`
}

type BackendConfig struct {
    Type       string `yaml:"type"`        // "http" | "command"
    URL        string `yaml:"url"`         // http/https; type=http only
    Format     string `yaml:"format"`      // "openai" | "anthropic"; type=http only
    AuthType   string `yaml:"auth_type"`   // "bearer" | "header" | "none"; type=http only
    AuthHeader string `yaml:"auth_header"` // header name; auth_type=header only
    AuthValue  string `yaml:"auth_value"`  // value; ${VAR} expanded from env
    Command    string `yaml:"command"`     // shell command; type=command only; ${VAR} expanded
}
```

`LoadConfig(path string) (*Config, error)`:
- Read YAML from path.
- Expand `${VAR}` in every `BackendConfig.AuthValue` and `BackendConfig.Command` field using `os.Expand(s, os.Getenv)`.
- Apply default: `MaxConcurrent = 4` if zero.
- Validate: `RouterURL` non-empty; `RouterToken` non-empty; at least one model; each model has non-empty `Name` and `Backend.Type` in `{"http","command"}`; `type=http` requires non-empty `URL` and `Format` in `{"openai","anthropic"}`; `type=command` requires non-empty `Command`. Return descriptive errors.

### 1.2 `shim/cmd/shim/main.go`

Package `main`. Version injected at build time: `var version = "dev"`.

- Parse flags: `-config string` (default `/config.yaml`), `-version` (prints version and exits).
- Call `shim.LoadConfig`.
- Build model→BackendSpec lookup map from config.
- Create `ws.Conn`.
- Start status line goroutine (see design notes).
- Set up `signal.Notify` for `SIGTERM`/`SIGINT` → cancel root context.
- Call `conn.Run(ctx)` (blocks until shutdown completes).

### 1.3 `shim/internal/ws/conn.go`

Package `ws`. Adapted from `client/internal/ws/conn.go`. Key differences from client:

1. **No model probing.** Client probes each model's `/props` endpoint to discover context size. Shim skips this — context sizes come from config.
2. **Direct registration.** On connect, immediately send `types.RegisterMsg`:
   ```go
   types.RegisterMsg{
       Type:          "register",
       Models:        []types.ModelInfo{{Name: m.Name, ContextSize: m.ContextSize}, ...},
       MaxConcurrent: cfg.MaxConcurrent,
       Version:       version,
   }
   ```
3. Everything else is identical to the client's conn.go:
   - Dial with header `Authorization: Bearer {RouterToken}`.
   - Read loop: `"job"` → acquire semaphore slot → goroutine → `worker.Handle`; `"cancel"` → cancel the job's context.
   - Ping every 30 s, pong deadline 60 s.
   - On shutdown: send `types.ReleaseMsg` for each in-flight job, then drain with a 10 s timeout before closing.
   - Reconnect with exponential backoff: start 1 s, double each attempt, cap at 60 s, reset on clean connection.

`Conn` exposes a `Stats()` method returning current state (connected bool, active int, total int64) for the status line.

### 1.4 `shim/internal/backend/backend.go`

Package `backend`. Imports `llmesh/pkg/types`.

```go
// Spec is the resolved backend descriptor for a single model.
type Spec struct {
    Type       string // "http" | "command"
    URL        string
    Format     string // "openai" | "anthropic"
    AuthType   string // "bearer" | "header" | "none"
    AuthHeader string
    AuthValue  string
    Command    string
}

// ChunkFunc receives each streaming chunk. Called exactly once with done=true.
type ChunkFunc func(delta, finishReason string, done bool)

// RunBatch executes spec for req, returns full content and finish reason.
func RunBatch(ctx context.Context, spec *Spec, req *types.InferenceRequest) (content, finishReason string, err error)

// RunStream executes spec for req, calling fn for each chunk.
// fn is guaranteed to be called with done=true exactly once.
func RunStream(ctx context.Context, spec *Spec, req *types.InferenceRequest, fn ChunkFunc) error
```

`RunBatch` and `RunStream` dispatch: `type == "http"` → functions in http.go; `type == "command"` → functions in command.go; default → return error `"unknown backend type: %s"`.

Constants:
```go
const (
    batchTimeout   = 5 * time.Minute
    streamTimeout  = 15 * time.Minute
    maxBatchOutput = 10 << 20 // 10 MiB
    maxStreamLine  = 1 << 20  // 1 MiB per NDJSON line
)
```

Package-level logger via `atomic.Pointer[slog.Logger]` + `SetLogger(l *slog.Logger)`, identical to llmshim pattern.

### 1.5 `shim/internal/backend/http.go`

Package `backend`. Adapted from `llmshim/internal/backend/http.go`. Replace `translate.Request` parameter with `*types.InferenceRequest` throughout.

**Package-level HTTP client:**
```go
var httpClient = &http.Client{Timeout: 0} // context controls deadlines
```

**`runHTTPBatch(ctx, spec, req) (content, finishReason string, err error)`**
- Build request body via `buildOpenAIBody` or `buildAnthropicBody` based on `spec.Format`.
- POST to `spec.URL` with appropriate headers (Content-Type, auth header).
- Read full response body; parse as OpenAI or Anthropic batch JSON.
- Return content string and finish reason.

**`runHTTPStream(ctx, spec, req, fn ChunkFunc) error`**
- Same setup as batch, with `"stream": true` in body.
- Read SSE line by line; parse delta chunks; call `fn` for each.
- On `[DONE]` (OpenAI) or `message_stop` event (Anthropic), call `fn("", finishReason, true)` and return.
- If stream ends without a done signal, call `fn("", "stop", true)` before returning.

**`buildOpenAIBody(req *types.InferenceRequest, stream bool) ([]byte, error)`**
Maps fields from `types.InferenceRequest`:
```json
{
  "model": req.Model,
  "messages": req.Messages,       // []{"role", "content"} — pass as-is
  "max_tokens": req.MaxTokens,    // omit if zero
  "temperature": req.Temperature, // omit if zero
  "top_p": req.TopP,              // omit if zero
  "stream": stream,
  "tools": req.Tools,             // omit if nil
  "tool_choice": req.ToolChoice   // omit if nil
}
```

**`buildAnthropicBody(req *types.InferenceRequest, stream bool) ([]byte, error)`**
- `max_tokens` is required by Anthropic; default to 4096 if `req.MaxTokens == 0`.
- Wrap each message's `Content` (string) as `[{"type":"text","text":"..."}]` for the Anthropic format.
- System message: extract any `role=="system"` message as top-level `"system"` field; remove from messages array.
- Pass `tools` and `tool_choice` if present.

**Auth header injection** (shared helper):
```go
func applyAuth(req *http.Request, spec *Spec) {
    switch spec.AuthType {
    case "bearer":
        req.Header.Set("Authorization", "Bearer "+spec.AuthValue)
    case "header":
        req.Header.Set(spec.AuthHeader, spec.AuthValue)
    // "none": no-op
    }
}
```

**URL validation** (fixing llmshim review issue): validate `spec.URL` at `RunBatch`/`RunStream` entry — parse with `url.Parse`, reject if scheme is not `http` or `https`. Return descriptive error.

### 1.6 `shim/internal/backend/command.go`

Package `backend`. Adapted from `llmshim/internal/backend/backend.go` command executor. Replace `translate.Request` with `*types.InferenceRequest`.

**`runCommandBatch(ctx, spec, req) (content, finishReason string, err error)`**
- Wrap ctx with batchTimeout.
- `json.Marshal(req)` → stdin of `exec.CommandContext(ctx, "sh", "-c", spec.Command)`.
- Capture stdout via `io.LimitReader(stdout, maxBatchOutput)`.
- **Capture stderr** to a `bytes.Buffer`; log it as a warning if non-empty (fixing llmshim review issue — stderr was previously discarded).
- On non-zero exit and `ctx.Err() == nil`: return `fmt.Errorf("adapter: %w (stderr: %s)", waitErr, stderrBuf)`.
- Parse stdout JSON: `{"content":"...","finish_reason":"stop"}`. Default `finish_reason` to `"stop"` if empty.

**`runCommandStream(ctx, spec, req, fn ChunkFunc) error`**
- Wrap ctx with streamTimeout.
- Pipe stdin; read stdout line-by-line via `bufio.Scanner` with `maxStreamLine` buffer.
- Capture stderr to buffer; log as warning if non-empty after command exits.
- Parse each line as `{"delta":"...","done":false}` or `{"delta":"","done":true,"finish_reason":"stop"}`.
- Call `fn(chunk.Delta, finishReason, chunk.Done)` for each line.
- If scanner exhausts without a `done=true` line, call `fn("", finishReason, true)` before returning.

### 1.7 `shim/internal/worker/worker.go`

Package `worker`. Imports `llmesh/pkg/types` and `llmesh/shim/internal/backend`.

```go
// Handle processes a single job from the router.
// send transmits ChunkMsgs back to the router over the WebSocket.
// sendErr transmits an ErrorMsg back to the router.
func Handle(
    ctx       context.Context,
    job       types.JobMsg,
    spec      *backend.Spec,
    send      func(types.ChunkMsg),
    sendErr   func(types.ErrorMsg),
)
```

Steps:

1. **Start keep-alive ticker** at 60 s interval. Each tick sends:
   ```go
   send(types.ChunkMsg{Type: "chunk", RequestID: job.Request.ID, Delta: "", Done: false})
   ```
   This resets the router's TTFT and activity timers during long upstream calls.

2. **Dispatch** based on `job.Request.Stream`:

   **Streaming (`Stream == true`):**
   - Call `backend.RunStream(ctx, spec, &job.Request, fn)`.
   - In `fn`: on first non-empty delta, stop the keep-alive ticker. For every call (including done), send:
     ```go
     send(types.ChunkMsg{
         Type:         "chunk",
         RequestID:    job.Request.ID,
         Delta:        delta,
         Done:         done,
         FinishReason: finishReason,
     })
     ```
   - On the final chunk (`done=true`), populate `Usage` if the upstream provided token counts (HTTP backends may include usage in the final SSE chunk; extract and map to `types.UsageInfo`).

   **Batch (`Stream == false`):**
   - Call `backend.RunBatch(ctx, spec, &job.Request)`.
   - Stop keep-alive ticker.
   - Send single `ChunkMsg{Delta: content, Done: true, FinishReason: finishReason}`.

3. **Error handling:**
   - If backend returns error and `ctx.Err() == nil` (not a cancel): stop keep-alive, send `types.ErrorMsg{Type: "error", RequestID: job.Request.ID, Message: err.Error()}`.
   - If `ctx.Err() != nil` (router cancelled): stop keep-alive, return silently. Do not send ErrorMsg — router already knows.

4. **Usage extraction** (HTTP backends):
   - OpenAI streams include a final `usage` object in the last data chunk before `[DONE]`. Extract `prompt_tokens`, `completion_tokens`, `total_tokens` and populate `types.UsageInfo`.
   - Anthropic streams include `usage` in the `message_delta` event. Extract similarly.
   - Command adapters: include `{"usage": {"prompt_tokens": N, "completion_tokens": N}}` in the final done line (optional; skip if absent).

### 1.8 `shim/config.yaml.example`

```yaml
# llmesh-shim configuration
# Connect to your llmesh router and register models backed by external APIs or scripts.

router_url: "wss://llm.example.com/ws/client"
router_token: "ct-your-token-here"
max_concurrent: 4

models:
  # ── OpenAI ──────────────────────────────────────────────────────────────────
  - name: "gpt-4o"
    context_size: 128000
    backend:
      type: http
      url: "https://api.openai.com/v1/chat/completions"
      format: openai
      auth_type: bearer
      auth_value: "${OPENAI_API_KEY}"

  # ── Anthropic ────────────────────────────────────────────────────────────────
  - name: "claude-sonnet-4-5"
    context_size: 200000
    backend:
      type: http
      url: "https://api.anthropic.com/v1/messages"
      format: anthropic
      auth_type: bearer
      auth_value: "${ANTHROPIC_API_KEY}"

  # ── Custom HTTP endpoint (e.g. another OpenAI-compatible server) ──────────
  # - name: "my-local-server"
  #   backend:
  #     type: http
  #     url: "http://localhost:11434/v1/chat/completions"
  #     format: openai
  #     auth_type: none

  # ── Shell command adapter ────────────────────────────────────────────────────
  # Adapter receives a JSON InferenceRequest on stdin.
  # Batch output (stream=false): {"content":"...","finish_reason":"stop"}
  # Streaming output (stream=true): NDJSON, one line per chunk:
  #   {"delta":"Hello","done":false}
  #   {"delta":" world","done":true,"finish_reason":"stop"}
  #
  # - name: "my-script"
  #   backend:
  #     type: command
  #     command: "python3 /path/to/adapter.py"
```

---

## Phase 2 — Build pipeline

### 2.1 `router/Dockerfile` modifications

In the `INCLUDE_CLIENTS` build block, after the existing four `llmesh-client` build commands, add four `llmesh-shim` build commands:

```dockerfile
RUN if [ "${INCLUDE_CLIENTS}" = "true" ]; then \
  GOOS=linux  GOARCH=amd64 go build -ldflags="-s -w -X main.version=${VERSION}" \
    -o /dist/llmesh-shim-linux-amd64  ./shim/cmd/shim/ && \
  GOOS=linux  GOARCH=arm64 go build -ldflags="-s -w -X main.version=${VERSION}" \
    -o /dist/llmesh-shim-linux-arm64  ./shim/cmd/shim/ && \
  GOOS=darwin GOARCH=amd64 go build -ldflags="-s -w -X main.version=${VERSION}" \
    -o /dist/llmesh-shim-darwin-amd64 ./shim/cmd/shim/ && \
  GOOS=darwin GOARCH=arm64 go build -ldflags="-s -w -X main.version=${VERSION}" \
    -o /dist/llmesh-shim-darwin-arm64 ./shim/cmd/shim/; \
fi
```

In the `/downloads` prep block, add:

```dockerfile
RUN if [ "${INCLUDE_CLIENTS}" = "true" ]; then \
  cp shim/man/llmesh-shim.1             /downloads/ && \
  cp shim/docker-compose.shim.yml       /downloads/ && \
  cp shim/config.yaml.example           /downloads/llmesh-shim-config.yaml.example; \
fi
```

No changes to the final `COPY` stages — `/dist` and `/downloads` are already copied in full.

### 2.2 `shim/Dockerfile`

Standalone container image for users who want to run the shim via Docker without the router image.

```dockerfile
FROM golang:1.26-alpine AS builder
WORKDIR /src
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /shim \
    ./shim/cmd/shim/

FROM alpine:3.19
COPY --from=builder /shim /shim
ENTRYPOINT ["/shim"]
CMD ["-config", "/config.yaml"]
```

### 2.3 `shim/docker-compose.shim.yml`

Bundled as a portal download alongside `shim-config.yaml.example`.

```yaml
services:
  llmesh-shim:
    image: ghcr.io/thomasteoh/llmesh-shim:latest
    restart: unless-stopped
    volumes:
      - ./config.yaml:/config.yaml:ro
    environment:
      - OPENAI_API_KEY=${OPENAI_API_KEY}
      - ANTHROPIC_API_KEY=${ANTHROPIC_API_KEY}
```

### 2.4 `build-router.sh` — no changes required

The script already passes `INCLUDE_CLIENTS` and calls `docker build -f router/Dockerfile`. The Dockerfile changes in 2.1 cover it.

---

## Phase 3 — Portal integration

### 3.1 `router/internal/admin/handler.go`

Register the new route in the `mux` setup inside `New()`, alongside the existing `/portal/clients/config` route:

```go
mux.HandleFunc("/portal/clients/shim-config", a.requireAuth(a.handleShimConfig))
```

### 3.2 `router/internal/admin/pages.go` — `handleShimConfig()`

Add method `func (a *Admin) handleShimConfig(w http.ResponseWriter, r *http.Request)`.

- Read `token` query parameter.
- Call `a.state.LookupClientToken(token)`. If not found, return HTTP 404.
- Enforce ownership: non-admin users may only download config for their own tokens (same check as `handleClientTokenConfig`).
- Generate YAML content:
  ```
  router_url: "wss://{a.host}/ws/client"
  router_token: "{token}"
  max_concurrent: 4

  models:
    # OpenAI example
    - name: "gpt-4o"
      context_size: 128000
      backend:
        type: http
        url: "https://api.openai.com/v1/chat/completions"
        format: openai
        auth_type: bearer
        auth_value: "${OPENAI_API_KEY}"

    # Anthropic example
    - name: "claude-sonnet-4-5"
      context_size: 200000
      backend:
        type: http
        url: "https://api.anthropic.com/v1/messages"
        format: anthropic
        auth_type: bearer
        auth_value: "${ANTHROPIC_API_KEY}"

    # Command adapter example (uncomment and edit)
    # - name: "my-script"
    #   backend:
    #     type: command
    #     command: "/path/to/adapter.sh"
  ```
- Respond with:
  ```go
  w.Header().Set("Content-Type", "application/x-yaml")
  w.Header().Set("Content-Disposition", `attachment; filename="shim-config.yaml"`)
  fmt.Fprint(w, yamlContent)
  ```

### 3.3 `router/internal/admin/templates/clients.html`

Three additions:

**A. New token banner** — add a "Download shim config" link alongside the existing "Download config.yaml" link:
```html
<a href="/portal/clients/shim-config?token={{.NewToken}}" download="shim-config.yaml"
   class="btn btn-sm">Download shim config</a>
```

**B. Shim downloads card** — add a new `<div class="card">` section below the existing client downloads card. Structure mirrors the client card exactly:
- Card title: "llmesh-shim"
- Subtitle: "Routes requests to external HTTP APIs or command adapters. No GPU required."
- Four platform tabs: `linux-amd64`, `linux-arm64`, `darwin-amd64`, `darwin-arm64`
- Each tab contains:
  - Download link: `<a href="/downloads/llmesh-shim-{platform}">llmesh-shim-{platform}</a>`
  - Run instructions:
    ```
    chmod +x llmesh-shim-{platform}
    ./llmesh-shim-{platform} -config ./shim-config.yaml
    ```
  - macOS note (darwin tabs only): `xattr -cr llmesh-shim-darwin-{arch}` to remove quarantine flag
- Collapsible "Config & setup" section containing:
  - Download shim-config.yaml link: `/downloads/llmesh-shim-config.yaml.example`
  - Inline config snippet showing the two-backend example (OpenAI + Anthropic), referencing env var pattern
  - Man page download link: `/downloads/llmesh-shim.1`
  - Docker compose download: `/downloads/docker-compose.shim.yml`

---

## Phase 4 — Documentation

### 4.1 `shim/man/llmesh-shim.1`

Standard roff man page. Sections:

- **NAME**: `llmesh-shim \- llmesh WebSocket client for external API and command backends`
- **SYNOPSIS**: `llmesh-shim [-config path] [-version]`
- **DESCRIPTION**: Connects to an llmesh router as a WebSocket client. Registers configured models with the router. Receives inference jobs and dispatches them to external HTTP APIs or local shell command adapters. Streams responses back to the router over the same WebSocket connection. Does not require local GPU hardware.
- **OPTIONS**:
  - `-config path` — path to YAML configuration file (default: `/config.yaml`)
  - `-version` — print version and exit
- **CONFIGURATION**: Document every field in Config and BackendConfig with types, defaults, and description. Note `${VAR}` env expansion in `auth_value` and `command`.
- **BACKENDS**:
  - `type: http` — posts to an OpenAI-compatible (`format: openai`) or Anthropic-compatible (`format: anthropic`) endpoint. Supports bearer token, custom header, or no auth.
  - `type: command` — executes a shell command for each request. Request is written as JSON to stdin. Response is read from stdout.
- **COMMAND ADAPTER PROTOCOL**:
  - Stdin: JSON-encoded `InferenceRequest` (fields: `id`, `model`, `messages`, `max_tokens`, `temperature`, `top_p`, `stream`, `tools`, `tool_choice`).
  - Stdout (batch, `stream=false`): single JSON line: `{"content":"...","finish_reason":"stop"}`.
  - Stdout (streaming, `stream=true`): NDJSON, one line per chunk, final line has `"done":true`. Optional `"finish_reason"` and `"usage"` on the done line.
  - Stderr: captured and logged as warnings; does not affect response handling.
- **EXAMPLES**: Full config examples for OpenAI, Anthropic, custom HTTP server, shell script adapter.
- **ENVIRONMENT**: `OPENAI_API_KEY`, `ANTHROPIC_API_KEY` — referenced via `${VAR}` in config.
- **SEE ALSO**: `llmesh-client(1)`

### 4.2 `design.md` additions

Add a "llmesh-shim" section after the "Client" section. Include:
- Purpose: route requests to external APIs or command adapters without local GPU
- Architecture diagram (text): router ↔ shim ↔ HTTP API / command
- Config file reference
- Backend types table (http openai, http anthropic, command)
- Command adapter protocol (stdin/stdout)
- Deployment: binary from portal downloads, or standalone Docker image

---

## Phase 5 — Verify

- [ ] 5.1 `go build ./shim/cmd/shim/` compiles cleanly
- [ ] 5.2 `go vet ./shim/...` passes with no warnings
- [ ] 5.3 `./build-router.sh` succeeds; confirm `/downloads/llmesh-shim-linux-amd64` and three other platform binaries exist in the built image
- [ ] 5.4 Run router locally; open portal Clients page; confirm shim downloads card renders with correct download links
- [ ] 5.5 Confirm `/portal/clients/shim-config?token=...` returns a valid YAML file with the correct `router_url` and `router_token`
- [ ] 5.6 Run `llmesh-shim -config ./test-config.yaml` pointed at the local router; confirm it connects, registers, and appears in the portal Clients tab
- [ ] 5.7 Test HTTP backend: route a request through the router to the shim using an Anthropic HTTP backend; confirm streaming and batch both work
- [ ] 5.8 Test command backend: route a request to a simple echo adapter shell script; confirm streaming and batch
- [ ] 5.9 Test cancellation: cancel an in-flight request from the router; confirm shim aborts the backend call cleanly
- [ ] 5.10 Push to branch; user reviews and merges; publish release to trigger CI image build

---

## Design notes

**Why no model probing:** The llmesh-client probes llama.cpp's `/props` endpoint to discover `n_ctx` (context size) before registering. External APIs don't expose an equivalent endpoint. Context size is instead declared in the shim config as `context_size` (optional, defaults to 0 = unknown). The router uses context size for scheduling hints but does not require it.

**Keep-alive requirement:** The router enforces a TTFT timeout (time to first token, default 15 min) and an activity timeout (silence between tokens, default 5 min). Some API backends have long latencies (especially when rate-limited or during high load). The keep-alive ticker sends empty `ChunkMsg` (delta="", done=false) every 60 s to reset these timers. The client uses the same mechanism. The ticker is stopped immediately when the first real content arrives.

**`${VAR}` expansion:** API keys must not be hardcoded in config files checked into version control. The `${VAR}` expansion at load time means secrets live only in environment variables. The Docker compose example pre-declares the standard env vars (`OPENAI_API_KEY`, `ANTHROPIC_API_KEY`) to make this pattern explicit.

**Streaming vs batch:** The shim honours `job.Request.Stream` exactly as instructed by the router. The router sets this based on the original caller's request. The shim passes `stream=true/false` to the upstream backend unchanged. For command adapters, the adapter is responsible for respecting the `"stream"` field in its JSON input.

**Usage reporting:** Token usage (prompt + completion counts) flows back to the router in the final `ChunkMsg.Usage`. OpenAI includes usage in the last SSE chunk; Anthropic includes it in the `message_delta` event. Command adapters may optionally include `"usage"` in their done line. If usage is absent, the field is nil and the router records zero counts for that request.

**State:** The shim is stateless. No `state.json`. All configuration is in `config.yaml`. Admin portal for the shim is not needed — the shim is configured per-deployment, not centrally managed. The router portal's client tokens page handles token lifecycle (create, revoke, view connections).

**`save()` rename not applicable:** The llmshim bug (atomic rename fails on single-file bind mounts) does not affect the shim since it has no write-back state.

**Separate binary from llmesh-client:** The shim and client are distinct binaries with distinct identities. They share `pkg/types` (the WebSocket protocol) but nothing else in the binary. The client's ws/conn.go and the shim's ws/conn.go are similar but evolved independently — do not attempt to share them via a `pkg/wsclient` abstraction during this build; that refactor is a separate future task if needed.
