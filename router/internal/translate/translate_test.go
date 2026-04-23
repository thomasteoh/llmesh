package translate

import (
	"encoding/json"
	"strings"
	"testing"
	"llmesh/pkg/types"
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
	line := OpenAISSEChunk("req-1", chunk)
	if !strings.HasPrefix(line, "data: ") {
		t.Errorf("expected SSE prefix, got: %s", line)
	}
	jsonPart := strings.TrimPrefix(line, "data: ")
	var parsed map[string]any
	if err := json.Unmarshal([]byte(jsonPart), &parsed); err != nil {
		t.Errorf("invalid JSON in SSE: %v", err)
	}
}

func TestOpenAISSEChunk_ToolCalls(t *testing.T) {
	toolDelta := json.RawMessage(`[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]`)
	chunk := types.ChunkMsg{ToolCallsDelta: toolDelta, Done: false}
	line := OpenAISSEChunk("req-1", chunk)
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

func TestAnthropicSSEChunk(t *testing.T) {
	chunk := types.ChunkMsg{Delta: "Hello", Done: false}
	line := AnthropicSSEChunk(chunk)
	if !strings.HasPrefix(line, "data: ") {
		t.Errorf("expected SSE prefix, got: %s", line)
	}
}
