package llamacpp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"llmesh/pkg/types"
)

const (
	// idleTimeout is the maximum gap between tokens once streaming has started.
	// Fires only after the first content token — before that, health checks
	// (healthCheckInterval) detect a hung llamacpp process instead.
	idleTimeout = 60 * time.Second

	// healthCheckInterval is how often to probe /health while waiting for the
	// first token. Detects a crashed or hung llamacpp before inference begins.
	healthCheckInterval = 30 * time.Second

	// healthCheckTimeout is the per-probe deadline for /health requests.
	healthCheckTimeout = 10 * time.Second
)

// ChunkCallback is called for each token chunk received from llama.cpp.
// done=true signals the end of the stream. usage is non-nil on the final call.
type ChunkCallback func(delta string, toolCallsDelta json.RawMessage, done bool, finishReason string, usage *types.UsageInfo)

// Client is an OpenAI-compatible HTTP client for a llama.cpp server.
type Client struct {
	endpoint   string
	httpClient *http.Client
}

func New(endpoint string) *Client {
	return &Client{
		endpoint:   endpoint,
		httpClient: &http.Client{},
	}
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type inferRequest struct {
	Model         string          `json:"model"`
	Messages      []types.Message `json:"messages"`
	MaxTokens     int             `json:"max_tokens,omitempty"`
	Temperature   float64         `json:"temperature,omitempty"`
	TopP          float64         `json:"top_p,omitempty"`
	Stream        bool            `json:"stream"`
	Tools         json.RawMessage `json:"tools,omitempty"`
	ToolChoice    json.RawMessage `json:"tool_choice,omitempty"`
	ChatTemplate  string          `json:"chat_template,omitempty"`
	StreamOptions *streamOptions  `json:"stream_options,omitempty"`
	CachePrompt   bool            `json:"cache_prompt"`
}

type inferUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type inferChunkDelta struct {
	Content   string          `json:"content"`
	ToolCalls json.RawMessage `json:"tool_calls"`
}

type inferChunk struct {
	Choices []struct {
		Delta        inferChunkDelta `json:"delta"`
		FinishReason *string         `json:"finish_reason"`
	} `json:"choices"`
	Usage *inferUsage `json:"usage"`
}

// ProbeContextSize fetches /props from the llama.cpp endpoint and returns the
// configured context window size (n_ctx). Returns 0 on any error.
func (c *Client) ProbeContextSize(ctx context.Context) int {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint+"/props", nil)
	if err != nil {
		return 0
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	var props struct {
		NCtx int `json:"n_ctx"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&props); err != nil {
		return 0
	}
	return props.NCtx
}

// Infer sends the request to llama.cpp and calls cb for each chunk.
// If req.Stream is false, cb is called once with the full content and done=true.
// chatTemplate overrides the model's built-in Jinja chat template; pass "" to use the default.
func (c *Client) Infer(ctx context.Context, req types.InferenceRequest, chatTemplate string, cb ChunkCallback) error {
	body := inferRequest{
		Model:        req.Model,
		Messages:     req.Messages,
		MaxTokens:    req.MaxTokens,
		Temperature:  req.Temperature,
		TopP:         req.TopP,
		Stream:       req.Stream,
		Tools:        req.Tools,
		ToolChoice:   req.ToolChoice,
		ChatTemplate: chatTemplate,
		CachePrompt:  true,
	}
	if req.Stream {
		body.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	// inferCtx is a child of ctx so we can cancel just this HTTP request on
	// idle timeout without disturbing the caller's context.
	inferCtx, inferCancel := context.WithCancel(ctx)
	defer inferCancel()

	httpReq, err := http.NewRequestWithContext(inferCtx, http.MethodPost, c.endpoint+"/v1/chat/completions", bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("llama.cpp returned %d", resp.StatusCode)
	}

	if req.Stream {
		// firstToken is closed by readStream when the first content token arrives.
		// The health-check goroutine below uses it to stop polling once inference
		// is confirmed to be producing output.
		firstToken := make(chan struct{})
		go c.watchHealth(inferCtx, inferCancel, firstToken)
		return c.readStream(inferCtx, inferCancel, resp, cb, firstToken)
	}
	return c.readBatch(resp, cb)
}

// watchHealth polls /health every healthCheckInterval until firstToken is closed
// (first content token received) or ctx is done. If a health check fails, it
// calls cancel to abort the in-flight inference request immediately.
func (c *Client) watchHealth(ctx context.Context, cancel context.CancelFunc, firstToken <-chan struct{}) {
	ticker := time.NewTicker(healthCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			// Guard against a race where ticker fires as ctx is being cancelled.
			if ctx.Err() != nil {
				return
			}
			if err := c.checkHealth(ctx); err != nil {
				cancel()
				return
			}
		case <-firstToken:
			return
		case <-ctx.Done():
			return
		}
	}
}

// checkHealth probes the llamacpp /health endpoint with a short deadline.
// Any HTTP response (including 503 "loading model") means the process is alive.
// A connection error or timeout means it is hung or crashed.
func (c *Client) checkHealth(ctx context.Context) error {
	hctx, cancel := context.WithTimeout(ctx, healthCheckTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(hctx, http.MethodGet, c.endpoint+"/health", nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

type scanLine struct {
	text string
	err  error
}

func (c *Client) readStream(ctx context.Context, cancel context.CancelFunc, resp *http.Response, cb ChunkCallback, firstToken chan<- struct{}) error {
	lines := make(chan scanLine, 8)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 1024*1024), 1024*1024) // 1 MB max line
		for scanner.Scan() {
			select {
			case lines <- scanLine{text: scanner.Text()}:
			case <-ctx.Done():
				return
			}
		}
		if err := scanner.Err(); err != nil && ctx.Err() == nil {
			select {
			case lines <- scanLine{err: err}:
			case <-ctx.Done():
			}
		}
	}()

	// With include_usage:true, llama.cpp sends the usage-only chunk AFTER the
	// finish_reason chunk, then [DONE]. We must not fire done=true until [DONE]
	// so we can collect the usage first.
	var pendingUsage *types.UsageInfo
	var finishReason string
	done := false
	firstContent := false

	// Idle timer: only active after first content token. Before that, watchHealth
	// detects a hung llamacpp process via /health polling.
	idleTimer := time.NewTimer(0)
	idleTimer.Stop()
	defer idleTimer.Stop()

	for {
		select {
		case result, ok := <-lines:
			if !ok {
				// Scanner goroutine exited (EOF or ctx cancelled).
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if !done {
					cb("", nil, true, finishReason, pendingUsage)
				}
				return nil
			}
			if result.err != nil {
				return fmt.Errorf("stream read: %w", result.err)
			}

			line := result.text
			if !strings.HasPrefix(line, "data: ") {
				// Skip blank SSE separators and comment lines — do NOT reset
				// the idle timer; only actual data: lines prove the stream is alive.
				continue
			}
			// Reset idle timer on every data: line (not blank separators).
			if firstContent {
				if !idleTimer.Stop() {
					select {
					case <-idleTimer.C:
					default:
					}
				}
				idleTimer.Reset(idleTimeout)
			}
			payload := strings.TrimPrefix(line, "data: ")
			if payload == "[DONE]" {
				cb("", nil, true, finishReason, pendingUsage)
				done = true
				// Drain remaining lines so the scanner goroutine can exit.
				for range lines {
				}
				return nil
			}
			var chunk inferChunk
			if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
				continue
			}
			// Stash usage from any chunk that carries it (usage-only chunk has no choices).
			if chunk.Usage != nil {
				pendingUsage = &types.UsageInfo{
					PromptTokens:     chunk.Usage.PromptTokens,
					CompletionTokens: chunk.Usage.CompletionTokens,
					TotalTokens:      chunk.Usage.TotalTokens,
				}
			}
			if len(chunk.Choices) == 0 {
				continue
			}
			choice := chunk.Choices[0]
			// Accumulate finish_reason but do NOT fire done yet — wait for [DONE].
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				finishReason = *choice.FinishReason
			}
			delta := choice.Delta
			if delta.Content != "" || len(delta.ToolCalls) > 0 {
				if !firstContent {
					firstContent = true
					close(firstToken) // stop health-check polling
					idleTimer.Reset(idleTimeout)
				}
				cb(delta.Content, delta.ToolCalls, false, "", nil)
			}

		case <-idleTimer.C:
			cancel()
			return fmt.Errorf("stream stalled: no token for %v", idleTimeout)

		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (c *Client) readBatch(resp *http.Response, cb ChunkCallback) error {
	var result struct {
		Choices []struct {
			Message struct {
				Content   string          `json:"content"`
				ToolCalls json.RawMessage `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *inferUsage `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("decode batch: %w", err)
	}
	if len(result.Choices) > 0 {
		choice := result.Choices[0]
		var u *types.UsageInfo
		if result.Usage != nil {
			u = &types.UsageInfo{
				PromptTokens:     result.Usage.PromptTokens,
				CompletionTokens: result.Usage.CompletionTokens,
				TotalTokens:      result.Usage.TotalTokens,
			}
		}
		cb(choice.Message.Content, choice.Message.ToolCalls, true, choice.FinishReason, u)
	}
	return nil
}
