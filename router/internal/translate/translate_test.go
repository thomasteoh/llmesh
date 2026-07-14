package translate

import (
	"encoding/json"
	"llmesh/pkg/types"
	"strings"
	"testing"
)

func TestOpenAIInbound(t *testing.T) {
	body := `{
		"model": "llama3.2:3b",
		"messages": [{"role": "user", "content": "hello"}],
		"max_tokens": 100,
		"stream": true
	}`
	req, err := OpenAIInbound([]byte(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.Model != "llama3.2:3b" {
		t.Errorf("expected model llama3.2:3b, got %s", req.Model)
	}
	if len(req.Messages) != 1 || string(req.Messages[0].Content) != `"hello"` {
		t.Errorf("unexpected messages: %+v", req.Messages)
	}
	if req.MaxTokens != 100 {
		t.Errorf("expected max_tokens 100, got %d", req.MaxTokens)
	}
	if !req.Stream {
		t.Error("expected stream=true")
	}
	if req.SourceFmt != "openai" {
		t.Errorf("expected source_fmt openai, got %s", req.SourceFmt)
	}
}

func TestOpenAIInbound_ToolCall(t *testing.T) {
	body := `{
		"model": "llama3.2:3b",
		"messages": [
			{"role": "user", "content": "What's the weather?"},
			{"role": "assistant", "content": null, "tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "get_weather", "arguments": "{\"location\": \"Paris\"}"}}]},
			{"role": "tool", "content": "22C, sunny", "tool_call_id": "call_1"}
		],
		"tools": [{"type": "function", "function": {"name": "get_weather"}}],
		"stream": false
	}`
	req, err := OpenAIInbound([]byte(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(req.Messages))
	}
	// assistant message must preserve tool_calls
	if len(req.Messages[1].ToolCalls) == 0 {
		t.Error("tool_calls stripped from assistant message")
	}
	// tool result must preserve tool_call_id
	if req.Messages[2].ToolCallID != "call_1" {
		t.Errorf("tool_call_id stripped: got %q", req.Messages[2].ToolCallID)
	}
	// tools must be forwarded
	if len(req.Tools) == 0 {
		t.Error("tools stripped from request")
	}
}

func TestAnthropicInbound(t *testing.T) {
	body := `{
		"model": "claude-3-haiku",
		"system": "You are helpful.",
		"messages": [{"role": "user", "content": "hello"}],
		"max_tokens": 200
	}`
	req, err := AnthropicInbound([]byte(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.Model != "claude-3-haiku" {
		t.Errorf("unexpected model: %s", req.Model)
	}
	if len(req.Messages) != 2 || req.Messages[0].Role != "system" {
		t.Errorf("expected system message first, got: %+v", req.Messages)
	}
	if string(req.Messages[0].Content) != `"You are helpful."` {
		t.Errorf("unexpected system content: %s", req.Messages[0].Content)
	}
	if req.SourceFmt != "anthropic" {
		t.Errorf("expected source_fmt anthropic, got %s", req.SourceFmt)
	}
}

func TestAnthropicInbound_ToolUse(t *testing.T) {
	body := `{
		"model": "claude-3-haiku",
		"messages": [
			{"role": "user", "content": "What's the weather?"},
			{"role": "assistant", "content": [{"type": "tool_use", "id": "toolu_1", "name": "get_weather", "input": {"location": "Paris"}}]},
			{"role": "user", "content": [{"type": "tool_result", "tool_use_id": "toolu_1", "content": "22C"}]}
		],
		"tools": [{"name": "get_weather", "description": "Get weather", "input_schema": {}}],
		"max_tokens": 200
	}`
	req, err := AnthropicInbound([]byte(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(req.Messages))
	}
	// assistant content must be the raw array (tool_use block preserved)
	var assistantContent []map[string]any
	if err := json.Unmarshal(req.Messages[1].Content, &assistantContent); err != nil {
		t.Fatalf("assistant content not valid JSON array: %v", err)
	}
	if assistantContent[0]["type"] != "tool_use" {
		t.Errorf("tool_use block stripped from assistant content")
	}
	// tools must be forwarded
	if len(req.Tools) == 0 {
		t.Error("tools stripped from request")
	}
}

func TestResponsesInbound_StringInput(t *testing.T) {
	body := `{
		"model": "gpt-4o",
		"input": "hello world",
		"max_output_tokens": 50
	}`
	req, err := ResponsesInbound([]byte(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(req.Messages) != 1 || string(req.Messages[0].Content) != `"hello world"` {
		t.Errorf("unexpected messages: %+v", req.Messages)
	}
	if req.MaxTokens != 50 {
		t.Errorf("expected 50 max tokens, got %d", req.MaxTokens)
	}
	if req.SourceFmt != "openai-responses" {
		t.Errorf("unexpected source_fmt: %s", req.SourceFmt)
	}
}

func TestOpenAISSEChunk(t *testing.T) {
	chunk := types.ChunkMsg{Delta: "Hello", Done: false}
	line := OpenAISSEChunk("req-1", "llama3", chunk)
	if !strings.HasPrefix(line, "data: ") {
		t.Errorf("expected SSE prefix, got: %s", line)
	}
	jsonPart := strings.TrimPrefix(line, "data: ")
	var parsed map[string]any
	if err := json.Unmarshal([]byte(jsonPart), &parsed); err != nil {
		t.Errorf("invalid JSON in SSE: %v", err)
	}
	if parsed["model"] != "llama3" {
		t.Errorf("expected model field, got: %v", parsed["model"])
	}
	if _, ok := parsed["created"]; !ok {
		t.Error("created field missing from chunk")
	}
}

func TestOpenAISSEChunk_ToolCalls(t *testing.T) {
	toolDelta := json.RawMessage(`[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]`)
	chunk := types.ChunkMsg{ToolCallsDelta: toolDelta, Done: false}
	line := OpenAISSEChunk("req-1", "llama3", chunk)
	jsonPart := strings.TrimPrefix(line, "data: ")
	var parsed map[string]any
	if err := json.Unmarshal([]byte(jsonPart), &parsed); err != nil {
		t.Fatalf("invalid JSON in SSE: %v", err)
	}
	choices := parsed["choices"].([]any)
	delta := choices[0].(map[string]any)["delta"].(map[string]any)
	if _, ok := delta["tool_calls"]; !ok {
		t.Error("tool_calls missing from SSE delta")
	}
}

// parseSSE splits an "event: X\ndata: Y" string into its event name and parsed
// JSON payload.
func parseSSE(t *testing.T, s string) (string, map[string]any) {
	t.Helper()
	var event, data string
	for _, line := range strings.Split(s, "\n") {
		if rest, ok := strings.CutPrefix(line, "event: "); ok {
			event = rest
		} else if rest, ok := strings.CutPrefix(line, "data: "); ok {
			data = rest
		}
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		t.Fatalf("invalid JSON in SSE %q: %v", s, err)
	}
	return event, payload
}

func TestAnthropicStreamer_TextLifecycle(t *testing.T) {
	s := NewAnthropicStreamer("req-1", "claude-x")
	var events []string
	events = append(events, s.Delta(types.ChunkMsg{Delta: "Hello"})...)
	events = append(events, s.Delta(types.ChunkMsg{Delta: " world"})...)
	events = append(events, s.Done("stop", &types.UsageInfo{CompletionTokens: 2})...)

	var names []string
	for _, e := range events {
		name, _ := parseSSE(t, e)
		names = append(names, name)
	}
	got := strings.Join(names, ",")
	want := "message_start,content_block_start,content_block_delta,content_block_delta,content_block_stop,message_delta,message_stop"
	if got != want {
		t.Errorf("event sequence:\n got %s\nwant %s", got, want)
	}
	// stop_reason must be mapped to Anthropic's vocabulary.
	_, last := parseSSE(t, events[len(events)-2]) // message_delta
	delta := last["delta"].(map[string]any)
	if delta["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason = %v, want end_turn", delta["stop_reason"])
	}
}

func TestAnthropicStreamer_ToolUse(t *testing.T) {
	s := NewAnthropicStreamer("req-1", "claude-x")
	tc := json.RawMessage(`[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"SF\"}"}}]`)
	var events []string
	events = append(events, s.Delta(types.ChunkMsg{ToolCallsDelta: tc})...)
	events = append(events, s.Done("tool_calls", nil)...)

	sawToolUse := false
	stopReason := ""
	for _, e := range events {
		name, payload := parseSSE(t, e)
		if name == "content_block_start" {
			if cb, ok := payload["content_block"].(map[string]any); ok && cb["type"] == "tool_use" {
				sawToolUse = true
				if cb["name"] != "get_weather" {
					t.Errorf("tool name = %v", cb["name"])
				}
			}
		}
		if name == "message_delta" {
			stopReason = payload["delta"].(map[string]any)["stop_reason"].(string)
		}
	}
	if !sawToolUse {
		t.Error("no tool_use content block emitted")
	}
	if stopReason != "tool_use" {
		t.Errorf("stop_reason = %q, want tool_use", stopReason)
	}
}

func TestAnthropicFullResponse_StopReasonAndUsage(t *testing.T) {
	resp := AnthropicFullResponse("req-1", "claude-x", "hi", "length", nil, &types.UsageInfo{PromptTokens: 3, CompletionTokens: 4})
	if resp["stop_reason"] != "max_tokens" {
		t.Errorf("stop_reason = %v, want max_tokens", resp["stop_reason"])
	}
	usage, ok := resp["usage"].(map[string]any)
	if !ok || usage["output_tokens"] != 4 {
		t.Errorf("usage not populated correctly: %v", resp["usage"])
	}
}

func TestParseStop(t *testing.T) {
	if got := parseStop(json.RawMessage(`"STOP"`)); len(got) != 1 || got[0] != "STOP" {
		t.Errorf("single stop: got %v", got)
	}
	if got := parseStop(json.RawMessage(`["a","b"]`)); len(got) != 2 {
		t.Errorf("array stop: got %v", got)
	}
	if got := parseStop(json.RawMessage(`null`)); got != nil {
		t.Errorf("null stop: got %v", got)
	}
}

func TestOpenAIInbound_TemperaturePresence(t *testing.T) {
	// Explicit 0 must survive as a non-nil pointer; omission must stay nil.
	withZero, err := OpenAIInbound([]byte(`{"model":"m","messages":[],"temperature":0}`))
	if err != nil {
		t.Fatal(err)
	}
	if withZero.Temperature == nil || *withZero.Temperature != 0 {
		t.Errorf("explicit temperature:0 should be a non-nil 0 pointer, got %v", withZero.Temperature)
	}
	omitted, err := OpenAIInbound([]byte(`{"model":"m","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if omitted.Temperature != nil {
		t.Errorf("omitted temperature should be nil, got %v", *omitted.Temperature)
	}
}
