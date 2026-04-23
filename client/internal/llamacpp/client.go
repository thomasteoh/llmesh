package llamacpp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"llmesh/pkg/types"
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

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"/v1/chat/completions", bytes.NewReader(data))
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
		return c.readStream(resp, cb)
	}
	return c.readBatch(resp, cb)
}

func (c *Client) readStream(resp *http.Response, cb ChunkCallback) error {
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024) // 1 MB max line

	// With include_usage:true, llama.cpp sends the usage-only chunk AFTER the
	// finish_reason chunk, then [DONE]. We must not fire done=true until [DONE]
	// so we can collect the usage first.
	var pendingUsage *types.UsageInfo
	var finishReason string
	done := false

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			cb("", nil, true, finishReason, pendingUsage)
			done = true
			break
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
			cb(delta.Content, delta.ToolCalls, false, "", nil)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("stream read: %w", err)
	}
	// Stream ended without [DONE] (e.g. server closed early) — fire done so the
	// caller is never left hanging.
	if !done {
		cb("", nil, true, finishReason, pendingUsage)
	}
	return nil
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
