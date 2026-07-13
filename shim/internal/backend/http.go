package backend

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"llmesh/pkg/types"
)

var httpClient = &http.Client{Timeout: 0} // timeouts are handled by context

// ─── HTTP executor ────────────────────────────────────────────────────────────

func runHTTPBatch(ctx context.Context, spec *Spec, req *types.InferenceRequest) (content, finishReason string, err error) {
	if err := validateURL(spec.URL); err != nil {
		return "", "", err
	}
	ctx, cancel := context.WithTimeout(ctx, batchTimeout)
	defer cancel()

	var body []byte
	var endpoint string

	switch spec.Format {
	case "anthropic":
		body, err = buildAnthropicBody(req, false)
		endpoint = joinURL(spec.URL, "/v1/messages")
	default: // "openai"
		body, err = buildOpenAIBody(req, false)
		endpoint = joinURL(spec.URL, "/v1/chat/completions")
	}
	if err != nil {
		return "", "", fmt.Errorf("build request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", "", fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	applyAuth(httpReq, spec)

	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return "", "", fmt.Errorf("upstream request: %w", err)
	}
	defer resp.Body.Close()

	out, err := io.ReadAll(io.LimitReader(resp.Body, maxBatchOutput))
	if err != nil {
		return "", "", fmt.Errorf("read upstream response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return "", "", fmt.Errorf("upstream %d: %s", resp.StatusCode, truncate(string(out), 200))
	}

	switch spec.Format {
	case "anthropic":
		return parseAnthropicBatch(out)
	default:
		return parseOpenAIBatch(out)
	}
}

func runHTTPStream(ctx context.Context, spec *Spec, req *types.InferenceRequest, fn ChunkFunc) error {
	if err := validateURL(spec.URL); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, streamTimeout)
	defer cancel()

	var body []byte
	var endpoint string
	var err error

	switch spec.Format {
	case "anthropic":
		body, err = buildAnthropicBody(req, true)
		endpoint = joinURL(spec.URL, "/v1/messages")
	default:
		body, err = buildOpenAIBody(req, true)
		endpoint = joinURL(spec.URL, "/v1/chat/completions")
	}
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	applyAuth(httpReq, spec)

	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("upstream request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		out, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("upstream %d: %s", resp.StatusCode, truncate(string(out), 200))
	}

	switch spec.Format {
	case "anthropic":
		return readAnthropicStream(resp.Body, fn)
	default:
		return readOpenAIStream(resp.Body, fn)
	}
}

// ─── Request builders ─────────────────────────────────────────────────────────

// openAIMessage mirrors types.Message but only includes fields the OpenAI API expects.
type openAIMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	ToolCalls  json.RawMessage `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Name       string          `json:"name,omitempty"`
}

func buildOpenAIBody(req *types.InferenceRequest, stream bool) ([]byte, error) {
	msgs := make([]openAIMessage, len(req.Messages))
	for i, m := range req.Messages {
		msgs[i] = openAIMessage{
			Role:       m.Role,
			Content:    m.Content,
			ToolCalls:  m.ToolCalls,
			ToolCallID: m.ToolCallID,
			Name:       m.Name,
		}
	}
	body := map[string]any{
		"model":    req.Model,
		"messages": msgs,
		"stream":   stream,
	}
	if stream {
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	if req.MaxTokens > 0 {
		body["max_tokens"] = req.MaxTokens
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		body["top_p"] = *req.TopP
	}
	if len(req.Stop) > 0 {
		body["stop"] = req.Stop
	}
	if len(req.Tools) > 0 {
		body["tools"] = req.Tools
	}
	if len(req.ToolChoice) > 0 {
		body["tool_choice"] = req.ToolChoice
	}
	return json.Marshal(body)
}

// anthropicMessage only includes the fields Anthropic's API expects.
type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

func buildAnthropicBody(req *types.InferenceRequest, stream bool) ([]byte, error) {
	var systemContent json.RawMessage
	var msgs []anthropicMessage
	for _, m := range req.Messages {
		if m.Role == "system" {
			systemContent = m.Content
			continue
		}
		msgs = append(msgs, anthropicMessage{Role: m.Role, Content: m.Content})
	}

	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = 4096
	}

	body := map[string]any{
		"model":      req.Model,
		"messages":   msgs,
		"max_tokens": maxTokens,
		"stream":     stream,
	}
	if len(systemContent) > 0 {
		body["system"] = systemContent
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		body["top_p"] = *req.TopP
	}
	if len(req.Stop) > 0 {
		body["stop_sequences"] = req.Stop
	}
	if len(req.Tools) > 0 {
		body["tools"] = req.Tools
	}
	if len(req.ToolChoice) > 0 {
		body["tool_choice"] = req.ToolChoice
	}
	return json.Marshal(body)
}

// ─── Response parsers ─────────────────────────────────────────────────────────

func parseOpenAIBatch(out []byte) (content, finishReason string, err error) {
	var r struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		return "", "", fmt.Errorf("parse openai response: %w", err)
	}
	if len(r.Choices) == 0 {
		return "", "", fmt.Errorf("openai response has no choices")
	}
	fr := r.Choices[0].FinishReason
	if fr == "" {
		fr = "stop"
	}
	return r.Choices[0].Message.Content, fr, nil
}

func parseAnthropicBatch(out []byte) (content, finishReason string, err error) {
	var r struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		return "", "", fmt.Errorf("parse anthropic response: %w", err)
	}
	var parts []string
	for _, c := range r.Content {
		parts = append(parts, c.Text)
	}
	fr := r.StopReason
	if fr == "" {
		fr = "stop"
	}
	return strings.Join(parts, ""), fr, nil
}

func readOpenAIStream(body io.Reader, fn ChunkFunc) error {
	finishReason := "stop"
	doneEmitted := false
	var usage *types.UsageInfo

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), maxStreamLine)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				TotalTokens      int `json:"total_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if chunk.Usage != nil {
			usage = &types.UsageInfo{
				PromptTokens:     chunk.Usage.PromptTokens,
				CompletionTokens: chunk.Usage.CompletionTokens,
				TotalTokens:      chunk.Usage.TotalTokens,
			}
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		if fr := chunk.Choices[0].FinishReason; fr != nil && *fr != "" {
			finishReason = *fr
		}
		delta := chunk.Choices[0].Delta.Content
		done := chunk.Choices[0].FinishReason != nil
		if done {
			fn(delta, finishReason, true, usage)
			doneEmitted = true
			break
		}
		if delta != "" {
			fn(delta, finishReason, false, nil)
		}
	}
	if !doneEmitted {
		fn("", finishReason, true, usage)
	}
	return scanner.Err()
}

func readAnthropicStream(body io.Reader, fn ChunkFunc) error {
	finishReason := "stop"
	doneEmitted := false
	var promptTokens int
	var usage *types.UsageInfo

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), maxStreamLine)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		var ev struct {
			Type    string `json:"type"`
			Message *struct {
				Usage *struct {
					InputTokens int `json:"input_tokens"`
				} `json:"usage"`
			} `json:"message"`
			Delta *struct {
				Type       string `json:"type"`
				Text       string `json:"text"`
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
			Usage *struct {
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "message_start":
			if ev.Message != nil && ev.Message.Usage != nil {
				promptTokens = ev.Message.Usage.InputTokens
			}
		case "content_block_delta":
			if ev.Delta != nil {
				fn(ev.Delta.Text, finishReason, false, nil)
			}
		case "message_delta":
			if ev.Delta != nil && ev.Delta.StopReason != "" {
				finishReason = ev.Delta.StopReason
			}
			if ev.Usage != nil && ev.Usage.OutputTokens > 0 {
				completionTokens := ev.Usage.OutputTokens
				usage = &types.UsageInfo{
					PromptTokens:     promptTokens,
					CompletionTokens: completionTokens,
					TotalTokens:      promptTokens + completionTokens,
				}
			}
		case "message_stop":
			fn("", finishReason, true, usage)
			doneEmitted = true
		}
	}
	if !doneEmitted {
		fn("", finishReason, true, usage)
	}
	return scanner.Err()
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

func applyAuth(req *http.Request, spec *Spec) {
	switch spec.AuthType {
	case "bearer":
		req.Header.Set("Authorization", "Bearer "+spec.AuthValue)
	case "header":
		req.Header.Set(spec.AuthHeader, spec.AuthValue)
	}
}

func validateURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid backend URL %q: %w", rawURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("backend URL %q must use http or https scheme", rawURL)
	}
	return nil
}

func joinURL(base, path string) string {
	base = strings.TrimRight(base, "/")
	if strings.HasSuffix(base, path) {
		return base
	}
	return base + path
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
