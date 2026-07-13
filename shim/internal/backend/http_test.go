package backend

import (
	"encoding/json"
	"strings"
	"testing"

	"llmesh/pkg/types"
)

// chunkRecord captures one ChunkFunc invocation.
type chunkRecord struct {
	delta     string
	toolCalls json.RawMessage
	finish    string
	done      bool
	usage     *types.UsageInfo
}

func TestReadOpenAIStream_UsageAfterFinish(t *testing.T) {
	// OpenAI sends the usage-only chunk *after* the finish_reason chunk, then
	// [DONE]. The reader must not break early and lose usage.
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"Hi"}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`,
		`data: [DONE]`,
		"",
	}, "\n")

	var got []chunkRecord
	err := readOpenAIStream(strings.NewReader(sse), func(delta string, tc json.RawMessage, finish string, done bool, usage *types.UsageInfo) {
		got = append(got, chunkRecord{delta, tc, finish, done, usage})
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	last := got[len(got)-1]
	if !last.done {
		t.Fatal("final chunk not marked done")
	}
	if last.usage == nil || last.usage.TotalTokens != 7 {
		t.Errorf("usage lost or wrong: %+v", last.usage)
	}
	if last.finish != "stop" {
		t.Errorf("finish_reason = %q, want stop", last.finish)
	}
}

func TestReadOpenAIStream_PrematureEOFIsError(t *testing.T) {
	// A stream that stops mid-generation with no finish_reason and no [DONE]
	// must surface as an error, not a silently-truncated success.
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n"
	err := readOpenAIStream(strings.NewReader(sse), func(string, json.RawMessage, string, bool, *types.UsageInfo) {})
	if err == nil {
		t.Fatal("expected error on premature EOF, got nil")
	}
}

func TestReadOpenAIStream_ToolCalls(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"f","arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
		"",
	}, "\n")
	var sawTool bool
	err := readOpenAIStream(strings.NewReader(sse), func(delta string, tc json.RawMessage, finish string, done bool, usage *types.UsageInfo) {
		if len(tc) > 0 {
			sawTool = true
		}
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !sawTool {
		t.Error("tool_calls delta not forwarded")
	}
}

func TestParseAnthropicBatch_ToolUse(t *testing.T) {
	body := `{"content":[{"type":"text","text":"sure"},{"type":"tool_use","id":"t1","name":"get","input":{"x":1}}],"stop_reason":"tool_use"}`
	res, err := parseAnthropicBatch([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "sure" {
		t.Errorf("content = %q", res.Content)
	}
	if len(res.ToolCalls) == 0 {
		t.Fatal("tool_calls not extracted")
	}
	var calls []map[string]any
	if err := json.Unmarshal(res.ToolCalls, &calls); err != nil {
		t.Fatalf("tool_calls not valid JSON: %v", err)
	}
	if calls[0]["id"] != "t1" {
		t.Errorf("tool id = %v", calls[0]["id"])
	}
}

func TestReadAnthropicStream_PrematureEOFIsError(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"message_start","message":{"usage":{"input_tokens":3}}}`,
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}`,
		"", // ends without message_stop
	}, "\n")
	err := readAnthropicStream(strings.NewReader(sse), func(string, json.RawMessage, string, bool, *types.UsageInfo) {})
	if err == nil {
		t.Fatal("expected error on stream ending before message_stop")
	}
}

func TestJoinURL_NoDoubleV1(t *testing.T) {
	if got := joinURL("https://api.openai.com/v1", "/v1/chat/completions"); got != "https://api.openai.com/v1/chat/completions" {
		t.Errorf("joinURL doubled /v1: %s", got)
	}
	if got := joinURL("https://api.openai.com", "/v1/chat/completions"); got != "https://api.openai.com/v1/chat/completions" {
		t.Errorf("joinURL: %s", got)
	}
}
