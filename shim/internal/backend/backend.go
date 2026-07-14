package backend

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"

	"llmesh/pkg/types"
)

const (
	batchTimeout   = 5 * time.Minute
	streamTimeout  = 15 * time.Minute
	maxBatchOutput = 10 << 20 // 10 MiB
	maxStreamLine  = 1 << 20  // 1 MiB per NDJSON line
)

var _log atomic.Pointer[slog.Logger]

func init() {
	l := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	_log.Store(l)
}

// SetLogger replaces the package logger. Safe to call before serving.
func SetLogger(l *slog.Logger) { _log.Store(l) }

func logger() *slog.Logger { return _log.Load() }

// Spec is the resolved backend descriptor for a single model.
type Spec struct {
	Type       string // "http" | "command"
	URL        string
	Format     string // "openai" | "anthropic"; type=http only
	AuthType   string // "bearer" | "header" | "none"; type=http only
	AuthHeader string
	AuthValue  string
	Command    string // type=command only
}

// ChunkFunc is called for each streaming chunk. On normal completion it is
// called exactly once with done=true; on a mid-stream failure the caller
// returns an error instead of synthesising a done chunk. toolCalls carries any
// tool-call payload for that chunk (OpenAI tool_calls JSON); usage is non-nil
// only on the final chunk, and only when the upstream provides token counts.
type ChunkFunc func(delta string, toolCalls json.RawMessage, finishReason string, done bool, usage *types.UsageInfo)

// adapterResponse is what shell command adapters write to stdout (one JSON line per chunk).
//
// Batch (stream=false):  {"content":"response text","finish_reason":"stop"}
// Streaming (stream=true): NDJSON, final line has "done":true
//
//	{"delta":"tok","done":false}
//	{"delta":"","done":true,"finish_reason":"stop","usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}
type adapterResponse struct {
	Content      string           `json:"content"`
	Delta        string           `json:"delta"`
	ToolCalls    json.RawMessage  `json:"tool_calls,omitempty"`
	Done         bool             `json:"done"`
	FinishReason string           `json:"finish_reason"`
	Usage        *types.UsageInfo `json:"usage,omitempty"`
}

// BatchResult is the full non-streaming response from a backend.
type BatchResult struct {
	Content      string
	ToolCalls    json.RawMessage
	FinishReason string
}

// RunBatch executes spec for req and returns the full response.
func RunBatch(ctx context.Context, spec *Spec, req *types.InferenceRequest) (BatchResult, error) {
	switch spec.Type {
	case "http":
		return runHTTPBatch(ctx, spec, req)
	case "command":
		return runCommandBatch(ctx, spec, req)
	default:
		return BatchResult{}, fmt.Errorf("unknown backend type: %q", spec.Type)
	}
}

// RunStream executes spec for req, calling fn for each chunk.
// fn is guaranteed to be called with done=true exactly once on normal completion.
func RunStream(ctx context.Context, spec *Spec, req *types.InferenceRequest, fn ChunkFunc) error {
	switch spec.Type {
	case "http":
		return runHTTPStream(ctx, spec, req, fn)
	case "command":
		return runCommandStream(ctx, spec, req, fn)
	default:
		return fmt.Errorf("unknown backend type: %q", spec.Type)
	}
}

// ─── Command executor ─────────────────────────────────────────────────────────

func runCommandBatch(ctx context.Context, spec *Spec, req *types.InferenceRequest) (BatchResult, error) {
	ctx, cancel := context.WithTimeout(ctx, batchTimeout)
	defer cancel()

	stdin, err := json.Marshal(req)
	if err != nil {
		return BatchResult{}, fmt.Errorf("marshal request: %w", err)
	}

	cmd := exec.CommandContext(ctx, "sh", "-c", spec.Command)
	cmd.Stdin = bytes.NewReader(stdin)

	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return BatchResult{}, fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return BatchResult{}, fmt.Errorf("adapter start: %w", err)
	}

	out, readErr := io.ReadAll(io.LimitReader(stdout, maxBatchOutput))
	io.Copy(io.Discard, stdout)
	waitErr := cmd.Wait()

	if se := strings.TrimSpace(stderrBuf.String()); se != "" {
		logger().Warn("backend: adapter stderr", "stderr", se)
	}
	if waitErr != nil && ctx.Err() == nil {
		return BatchResult{}, fmt.Errorf("adapter: %w", waitErr)
	}
	if readErr != nil {
		return BatchResult{}, fmt.Errorf("read adapter output: %w", readErr)
	}

	var resp adapterResponse
	if err := json.Unmarshal(bytes.TrimSpace(out), &resp); err != nil {
		return BatchResult{}, fmt.Errorf("adapter returned invalid JSON: %w", err)
	}
	fr := resp.FinishReason
	if fr == "" {
		fr = "stop"
	}
	c := resp.Content
	if c == "" {
		c = resp.Delta
	}
	return BatchResult{Content: c, ToolCalls: resp.ToolCalls, FinishReason: fr}, nil
}

func runCommandStream(ctx context.Context, spec *Spec, req *types.InferenceRequest, fn ChunkFunc) error {
	ctx, cancel := context.WithTimeout(ctx, streamTimeout)
	defer cancel()

	stdin, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	cmd := exec.CommandContext(ctx, "sh", "-c", spec.Command)
	cmd.Stdin = bytes.NewReader(stdin)

	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("adapter start: %w", err)
	}

	finishReason := "stop"
	doneEmitted := false

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), maxStreamLine)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var chunk adapterResponse
		if err := json.Unmarshal([]byte(line), &chunk); err != nil {
			logger().Warn("backend: skipping unparseable line", "error", err)
			continue
		}
		if chunk.FinishReason != "" {
			finishReason = chunk.FinishReason
		}
		fn(chunk.Delta, chunk.ToolCalls, finishReason, chunk.Done, chunk.Usage)
		if chunk.Done {
			doneEmitted = true
			break
		}
	}

	io.Copy(io.Discard, stdout)
	waitErr := cmd.Wait()

	if se := strings.TrimSpace(stderrBuf.String()); se != "" {
		logger().Warn("backend: adapter stderr", "stderr", se)
	}
	if waitErr != nil && ctx.Err() == nil {
		// A crashed adapter that never emitted a done chunk must surface as an
		// error, not a silently-truncated success.
		if !doneEmitted {
			return fmt.Errorf("adapter exited before completing: %w", waitErr)
		}
		logger().Warn("backend: adapter exited with error after completion", "error", waitErr)
	}
	if !doneEmitted {
		if err := scanner.Err(); err != nil {
			return fmt.Errorf("adapter stream read error: %w", err)
		}
		return fmt.Errorf("adapter stream ended without a done chunk")
	}
	return nil
}
