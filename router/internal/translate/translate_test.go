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
	if len(req.Messages) != 1 || req.Messages[0].Content != "hello" {
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
	if req.Messages[0].Content != "You are helpful." {
		t.Errorf("unexpected system content: %s", req.Messages[0].Content)
	}
	if req.SourceFmt != "anthropic" {
		t.Errorf("expected source_fmt anthropic, got %s", req.SourceFmt)
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
	if len(req.Messages) != 1 || req.Messages[0].Content != "hello world" {
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

func TestAnthropicSSEChunk(t *testing.T) {
	chunk := types.ChunkMsg{Delta: "Hello", Done: false}
	line := AnthropicSSEChunk(chunk)
	if !strings.HasPrefix(line, "data: ") {
		t.Errorf("expected SSE prefix, got: %s", line)
	}
}
