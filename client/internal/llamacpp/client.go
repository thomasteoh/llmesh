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

// ChunkCallback is called for each token chunk received from llama.cpp.
// done=true signals the end of the stream.
type ChunkCallback func(delta string, done bool, finishReason string)

// Client is an OpenAI-compatible HTTP client for a llama.cpp server.
type Client struct {
	endpoint   string
	httpClient *http.Client
}

func New(endpoint string) *Client {
	return &Client{
		endpoint: endpoint,
		httpClient: &http.Client{
			Timeout: 5 * time.Minute,
		},
	}
}

type inferRequest struct {
	Model       string          `json:"model"`
	Messages    []types.Message `json:"messages"`
	MaxTokens   int             `json:"max_tokens,omitempty"`
	Temperature float64         `json:"temperature,omitempty"`
	TopP        float64         `json:"top_p,omitempty"`
	Stream      bool            `json:"stream"`
}

type inferChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
}

// Infer sends the request to llama.cpp and calls cb for each chunk.
// If req.Stream is false, cb is called once with the full content and done=true.
func (c *Client) Infer(ctx context.Context, req types.InferenceRequest, cb ChunkCallback) error {
	body := inferRequest{
		Model:       req.Model,
		Messages:    req.Messages,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stream:      req.Stream,
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
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			cb("", true, "stop")
			return nil
		}
		var chunk inferChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		choice := chunk.Choices[0]
		finishReason := ""
		if choice.FinishReason != nil {
			finishReason = *choice.FinishReason
		}
		if choice.Delta.Content != "" || finishReason != "" {
			done := finishReason != ""
			cb(choice.Delta.Content, done, finishReason)
			if done {
				return nil
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("stream read: %w", err)
	}
	return nil
}

func (c *Client) readBatch(resp *http.Response, cb ChunkCallback) error {
	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("decode batch: %w", err)
	}
	if len(result.Choices) > 0 {
		choice := result.Choices[0]
		cb(choice.Message.Content, true, choice.FinishReason)
	}
	return nil
}
