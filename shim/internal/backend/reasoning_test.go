package backend

import (
	"io"
	"strings"
	"testing"
)

func streamChunks(t *testing.T, read func(r io.Reader, fn ChunkFunc) error, body string) []Chunk {
	t.Helper()
	var got []Chunk
	if err := read(strings.NewReader(body), func(c Chunk) { got = append(got, c) }); err != nil {
		t.Fatalf("read stream: %v", err)
	}
	return got
}

func joined(chunks []Chunk) (content, reasoning string) {
	for _, c := range chunks {
		content += c.Delta
		reasoning += c.Reasoning
	}
	return content, reasoning
}

// An upstream that separates thinking from the answer leaves content empty for
// the whole thinking phase, so reading only content discarded most of a
// reasoning model's output and left the router seeing silence from a backend
// that was working.
func TestReadOpenAIStream_ForwardsReasoningContent(t *testing.T) {
	got := streamChunks(t, readOpenAIStream, strings.Join([]string{
		`data: {"choices":[{"delta":{"reasoning_content":"weighing "}}]}`,
		`data: {"choices":[{"delta":{"reasoning_content":"options"}}]}`,
		`data: {"choices":[{"delta":{"content":"the answer"}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
		"",
	}, "\n"))

	content, reasoning := joined(got)
	if reasoning != "weighing options" {
		t.Errorf("reasoning = %q, want %q", reasoning, "weighing options")
	}
	if content != "the answer" {
		t.Errorf("content = %q, want %q", content, "the answer")
	}
	for _, c := range got {
		if c.Delta != "" && c.Reasoning != "" {
			t.Errorf("chunk mixes reasoning into the answer: %+v", c)
		}
	}
}

// Anthropic extended thinking arrives as thinking_delta events.
func TestReadAnthropicStream_ForwardsThinkingDelta(t *testing.T) {
	got := streamChunks(t, readAnthropicStream, strings.Join([]string{
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"step one"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","delta":{"type":"signature_delta","signature":"abc"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"the answer"}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		"",
	}, "\n"))

	content, reasoning := joined(got)
	if reasoning != "step one" {
		t.Errorf("reasoning = %q, want %q", reasoning, "step one")
	}
	if content != "the answer" {
		t.Errorf("content = %q, want %q — a signature_delta leaked into the answer", content, "the answer")
	}
}

func TestParseOpenAIBatch_CarriesReasoning(t *testing.T) {
	res, err := parseOpenAIBatch([]byte(
		`{"choices":[{"message":{"content":"answer","reasoning_content":"thought"},"finish_reason":"stop"}]}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if res.Reasoning != "thought" {
		t.Errorf("Reasoning = %q, want %q", res.Reasoning, "thought")
	}
	if res.Content != "answer" {
		t.Errorf("Content = %q, want %q", res.Content, "answer")
	}
}

func TestParseAnthropicBatch_CarriesThinkingBlocks(t *testing.T) {
	res, err := parseAnthropicBatch([]byte(
		`{"content":[{"type":"thinking","thinking":"thought"},{"type":"text","text":"answer"}],"stop_reason":"end_turn"}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if res.Reasoning != "thought" {
		t.Errorf("Reasoning = %q, want %q", res.Reasoning, "thought")
	}
	if res.Content != "answer" {
		t.Errorf("Content = %q, want %q — the thinking block leaked into the answer", res.Content, "answer")
	}
}
