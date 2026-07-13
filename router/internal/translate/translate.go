package translate

import (
	"encoding/json"
	"fmt"
	"time"

	"llmesh/pkg/types"
)

// --- Inbound translators ---

type openAIMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	ToolCalls  json.RawMessage `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Name       string          `json:"name,omitempty"`
}

type openAIRequest struct {
	Model       string          `json:"model"`
	Messages    []openAIMessage `json:"messages"`
	MaxTokens   int             `json:"max_tokens"`
	Temperature *float64        `json:"temperature"`
	TopP        *float64        `json:"top_p"`
	Stop        json.RawMessage `json:"stop"`
	Stream      bool            `json:"stream"`
	Tools       json.RawMessage `json:"tools,omitempty"`
	ToolChoice  json.RawMessage `json:"tool_choice,omitempty"`
}

// OpenAIInbound converts an OpenAI chat completions body to an InferenceRequest.
func OpenAIInbound(body []byte) (*types.InferenceRequest, error) {
	var r openAIRequest
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("openai parse: %w", err)
	}
	req := &types.InferenceRequest{
		Model:       r.Model,
		MaxTokens:   r.MaxTokens,
		Temperature: r.Temperature,
		TopP:        r.TopP,
		Stop:        parseStop(r.Stop),
		Stream:      r.Stream,
		SourceFmt:   "openai",
		Tools:       r.Tools,
		ToolChoice:  r.ToolChoice,
	}
	for _, m := range r.Messages {
		req.Messages = append(req.Messages, types.Message{
			Role:       m.Role,
			Content:    m.Content,
			ToolCalls:  m.ToolCalls,
			ToolCallID: m.ToolCallID,
			Name:       m.Name,
		})
	}
	return req, nil
}

// parseStop normalises the OpenAI "stop" field, which may be a single string or
// an array of strings, into a string slice. Returns nil when absent.
func parseStop(raw json.RawMessage) []string {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		if one == "" {
			return nil
		}
		return []string{one}
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err == nil {
		return many
	}
	return nil
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicRequest struct {
	Model         string             `json:"model"`
	System        json.RawMessage    `json:"system,omitempty"`
	Messages      []anthropicMessage `json:"messages"`
	MaxTokens     int                `json:"max_tokens"`
	Temperature   *float64           `json:"temperature"`
	TopP          *float64           `json:"top_p"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
	Stream        bool               `json:"stream"`
	Tools         json.RawMessage    `json:"tools,omitempty"`
	ToolChoice    json.RawMessage    `json:"tool_choice,omitempty"`
}

// AnthropicInbound converts an Anthropic messages body to an InferenceRequest.
func AnthropicInbound(body []byte) (*types.InferenceRequest, error) {
	var r anthropicRequest
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("anthropic parse: %w", err)
	}
	req := &types.InferenceRequest{
		Model:       r.Model,
		MaxTokens:   r.MaxTokens,
		Temperature: r.Temperature,
		TopP:        r.TopP,
		Stop:        r.StopSequences,
		Stream:      r.Stream,
		SourceFmt:   "anthropic",
		Tools:       r.Tools,
		ToolChoice:  r.ToolChoice,
	}
	if len(r.System) > 0 && string(r.System) != "null" {
		req.Messages = append(req.Messages, types.Message{Role: "system", Content: r.System})
	}
	for _, m := range r.Messages {
		req.Messages = append(req.Messages, types.Message{Role: m.Role, Content: m.Content})
	}
	return req, nil
}

type responsesRequest struct {
	Model           string          `json:"model"`
	Input           json.RawMessage `json:"input"`
	MaxOutputTokens int             `json:"max_output_tokens"`
	Stream          bool            `json:"stream"`
}

// ResponsesInbound converts an OpenAI Responses API body to an InferenceRequest.
func ResponsesInbound(body []byte) (*types.InferenceRequest, error) {
	var r responsesRequest
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("responses parse: %w", err)
	}
	req := &types.InferenceRequest{
		Model:     r.Model,
		MaxTokens: r.MaxOutputTokens,
		Stream:    r.Stream,
		SourceFmt: "openai-responses",
	}
	// Input is either a plain string or an array of message objects.
	var inputStr string
	if err := json.Unmarshal(r.Input, &inputStr); err == nil {
		// r.Input is already a JSON string literal — use directly as content.
		req.Messages = []types.Message{{Role: "user", Content: r.Input}}
	} else {
		var msgs []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(r.Input, &msgs); err != nil {
			return nil, fmt.Errorf("responses input parse: %w", err)
		}
		for _, m := range msgs {
			req.Messages = append(req.Messages, types.Message{Role: m.Role, Content: m.Content})
		}
	}
	return req, nil
}

// mustMarshal marshals v to JSON and panics if it fails.
// Used for SSE formatters where the payload shape is fixed and marshal failure is a programmer error.
func mustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("translate: marshal error: %v", err))
	}
	return b
}

// nowUnix is overridable in tests; the created timestamp is otherwise wall-clock.
var nowUnix = func() int64 { return time.Now().Unix() }

// --- Outbound SSE formatters ---

type openAIChunkChoice struct {
	Index        int         `json:"index"`
	Delta        openAIDelta `json:"delta"`
	FinishReason *string     `json:"finish_reason"`
}

type openAIDelta struct {
	Content   string          `json:"content,omitempty"`
	ToolCalls json.RawMessage `json:"tool_calls,omitempty"`
}

// OpenAISSEChunk formats a ChunkMsg as an OpenAI SSE line (without trailing newlines).
func OpenAISSEChunk(requestID, model string, chunk types.ChunkMsg) string {
	var finishReason *string
	if chunk.Done && chunk.FinishReason != "" {
		finishReason = &chunk.FinishReason
	}
	delta := openAIDelta{Content: chunk.Delta}
	if len(chunk.ToolCallsDelta) > 0 {
		delta.ToolCalls = chunk.ToolCallsDelta
	}
	payload := map[string]any{
		"id":      requestID,
		"object":  "chat.completion.chunk",
		"created": nowUnix(),
		"model":   model,
		"choices": []openAIChunkChoice{{
			Index:        0,
			Delta:        delta,
			FinishReason: finishReason,
		}},
	}
	if chunk.Usage != nil {
		payload["usage"] = chunk.Usage
	}
	b, _ := json.Marshal(payload)
	return "data: " + string(b)
}

// OpenAISSEDone returns the terminal SSE line for OpenAI streaming.
func OpenAISSEDone() string {
	return "data: [DONE]"
}

// OpenAIFullResponse assembles a complete (non-streaming) OpenAI chat response.
func OpenAIFullResponse(requestID, model, content, finishReason string, toolCalls json.RawMessage, usage *types.UsageInfo) map[string]any {
	message := map[string]any{"role": "assistant", "content": content}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	resp := map[string]any{
		"id":      requestID,
		"object":  "chat.completion",
		"created": nowUnix(),
		"model":   model,
		"choices": []map[string]any{
			{
				"index":         0,
				"message":       message,
				"finish_reason": finishReason,
			},
		},
	}
	if usage != nil {
		resp["usage"] = usage
	}
	return resp
}

// anthropicStopReason maps an OpenAI finish_reason onto the Anthropic vocabulary.
// Anthropic SDK response models reject unknown values, so an empty or unrecognised
// reason defaults to end_turn.
func anthropicStopReason(finishReason string) string {
	switch finishReason {
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	case "stop", "", "content_filter":
		return "end_turn"
	default:
		return "end_turn"
	}
}

// toolCall is a decoded OpenAI-format tool call.
type toolCall struct {
	ID   string
	Name string
	Args string // raw JSON arguments string
}

// parseOpenAIToolCalls decodes the OpenAI tool_calls array (as produced by the
// worker) into a flat slice. Malformed input yields nil rather than an error;
// the caller treats "no tool calls" as the safe default.
func parseOpenAIToolCalls(raw json.RawMessage) []toolCall {
	if len(raw) == 0 {
		return nil
	}
	var arr []struct {
		ID       string `json:"id"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil
	}
	out := make([]toolCall, 0, len(arr))
	for _, c := range arr {
		out = append(out, toolCall{ID: c.ID, Name: c.Function.Name, Args: c.Function.Arguments})
	}
	return out
}

// AnthropicStreamer emits a spec-compliant Anthropic message stream: named
// events (event: line + data: line) covering message_start, content_block_start,
// content_block_delta, content_block_stop, message_delta, and message_stop, with
// block indices. Drive it from the streaming handler: Start once, Delta per
// chunk, Done at completion. It is used by a single request goroutine and needs
// no synchronisation.
type AnthropicStreamer struct {
	requestID string
	model     string
	started   bool
	textOpen  bool
	index     int
	toolBuf   []byte // accumulated tool_calls delta bytes (last non-empty wins)
}

// NewAnthropicStreamer returns a streamer for the given request and model.
func NewAnthropicStreamer(requestID, model string) *AnthropicStreamer {
	return &AnthropicStreamer{requestID: requestID, model: model}
}

func sseEvent(name string, payload any) string {
	return "event: " + name + "\ndata: " + string(mustMarshal(payload))
}

// start returns the message_start event, emitted lazily before the first output.
func (s *AnthropicStreamer) start() []string {
	if s.started {
		return nil
	}
	s.started = true
	return []string{sseEvent("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            s.requestID,
			"type":          "message",
			"role":          "assistant",
			"model":         s.model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})}
}

// Delta returns the events for one chunk of streamed output. Tool-call deltas are
// buffered and flushed as a tool_use block by Done.
func (s *AnthropicStreamer) Delta(chunk types.ChunkMsg) []string {
	var out []string
	out = append(out, s.start()...)
	if len(chunk.ToolCallsDelta) > 0 {
		s.toolBuf = append(s.toolBuf[:0], chunk.ToolCallsDelta...)
	}
	if chunk.Delta == "" {
		return out
	}
	if !s.textOpen {
		s.textOpen = true
		out = append(out, sseEvent("content_block_start", map[string]any{
			"type":          "content_block_start",
			"index":         s.index,
			"content_block": map[string]any{"type": "text", "text": ""},
		}))
	}
	out = append(out, sseEvent("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": s.index,
		"delta": map[string]any{"type": "text_delta", "text": chunk.Delta},
	}))
	return out
}

// Done returns the closing events: any pending content_block_stop, tool_use
// blocks flushed from buffered tool-call deltas, then message_delta and
// message_stop. usage may be nil.
func (s *AnthropicStreamer) Done(finishReason string, usage *types.UsageInfo) []string {
	var out []string
	out = append(out, s.start()...) // ensure message_start even for empty output
	if s.textOpen {
		out = append(out, sseEvent("content_block_stop", map[string]any{
			"type": "content_block_stop", "index": s.index,
		}))
		s.textOpen = false
		s.index++
	}
	stopReason := anthropicStopReason(finishReason)
	for _, tc := range parseOpenAIToolCalls(s.toolBuf) {
		stopReason = "tool_use"
		out = append(out, sseEvent("content_block_start", map[string]any{
			"type":          "content_block_start",
			"index":         s.index,
			"content_block": map[string]any{"type": "tool_use", "id": tc.ID, "name": tc.Name, "input": map[string]any{}},
		}))
		if tc.Args != "" {
			out = append(out, sseEvent("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": s.index,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": tc.Args},
			}))
		}
		out = append(out, sseEvent("content_block_stop", map[string]any{
			"type": "content_block_stop", "index": s.index,
		}))
		s.index++
	}
	messageDelta := map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
	}
	if usage != nil {
		messageDelta["usage"] = map[string]any{"output_tokens": usage.CompletionTokens}
	}
	out = append(out, sseEvent("message_delta", messageDelta))
	out = append(out, sseEvent("message_stop", map[string]any{"type": "message_stop"}))
	return out
}

// AnthropicErrorEvent returns a single Anthropic error event for mid-stream
// failures, so clients see a typed error instead of a truncated stream.
func AnthropicErrorEvent(message string) string {
	return sseEvent("error", map[string]any{
		"type":  "error",
		"error": map[string]any{"type": "api_error", "message": message},
	})
}

// AnthropicFullResponse assembles a complete non-streaming Anthropic response,
// including tool_use content blocks, mapped stop_reason, and usage.
func AnthropicFullResponse(requestID, model, content, finishReason string, toolCalls json.RawMessage, usage *types.UsageInfo) map[string]any {
	content2 := make([]map[string]any, 0, 1)
	if content != "" {
		content2 = append(content2, map[string]any{"type": "text", "text": content})
	}
	stopReason := anthropicStopReason(finishReason)
	for _, tc := range parseOpenAIToolCalls(toolCalls) {
		stopReason = "tool_use"
		input := json.RawMessage("{}")
		if tc.Args != "" {
			input = json.RawMessage(tc.Args)
		}
		content2 = append(content2, map[string]any{
			"type":  "tool_use",
			"id":    tc.ID,
			"name":  tc.Name,
			"input": input,
		})
	}
	resp := map[string]any{
		"id":            requestID,
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content2,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
	}
	if usage != nil {
		resp["usage"] = map[string]any{
			"input_tokens":  usage.PromptTokens,
			"output_tokens": usage.CompletionTokens,
		}
	}
	return resp
}

// OpenAIResponsesSSEChunk formats a ChunkMsg as an OpenAI Responses API SSE line.
func OpenAIResponsesSSEChunk(requestID string, chunk types.ChunkMsg) string {
	payload := map[string]any{
		"type":  "response.output_text.delta",
		"delta": chunk.Delta,
	}
	return "data: " + string(mustMarshal(payload))
}

// OpenAIResponsesSSEDone returns the terminal SSE event for OpenAI Responses API streaming.
func OpenAIResponsesSSEDone() string {
	return "data: " + string(mustMarshal(map[string]any{"type": "response.completed"}))
}

// OpenAIResponsesFullResponse assembles a complete non-streaming Responses API response.
func OpenAIResponsesFullResponse(requestID, model, content string, usage *types.UsageInfo) map[string]any {
	resp := map[string]any{
		"id":         requestID,
		"object":     "response",
		"created_at": nowUnix(),
		"model":      model,
		"status":     "completed",
		"output": []map[string]any{
			{
				"id":      "msg_" + requestID,
				"type":    "message",
				"role":    "assistant",
				"status":  "completed",
				"content": []map[string]any{{"type": "output_text", "text": content}},
			},
		},
	}
	if usage != nil {
		resp["usage"] = map[string]any{
			"input_tokens":  usage.PromptTokens,
			"output_tokens": usage.CompletionTokens,
			"total_tokens":  usage.TotalTokens,
		}
	}
	return resp
}
