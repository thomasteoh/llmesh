package translate

import (
	"encoding/json"
	"strings"
	"testing"

	"llmesh/pkg/types"
)

func TestOpenAISSEChunk_CarriesReasoning(t *testing.T) {
	line := OpenAISSEChunk("req-1", "qwen", types.ChunkMsg{ReasoningDelta: "thinking"})

	var payload struct {
		Choices []struct {
			Delta struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := payload.Choices[0].Delta.ReasoningContent; got != "thinking" {
		t.Errorf("reasoning_content = %q, want %q", got, "thinking")
	}
	if got := payload.Choices[0].Delta.Content; got != "" {
		t.Errorf("content = %q, want empty — reasoning must not leak into the answer", got)
	}
}

// event is one decoded Anthropic SSE payload, reduced to the fields these
// tests assert on.
type event struct {
	Type         string `json:"type"`
	Index        int    `json:"index"`
	ContentBlock struct {
		Type string `json:"type"`
	} `json:"content_block"`
	Delta struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		Thinking string `json:"thinking"`
	} `json:"delta"`
}

func decodeEvents(t *testing.T, lines []string) []event {
	t.Helper()
	var out []event
	for _, l := range lines {
		_, data, ok := strings.Cut(l, "\ndata: ")
		if !ok {
			t.Fatalf("malformed SSE event: %q", l)
		}
		var e event
		if err := json.Unmarshal([]byte(data), &e); err != nil {
			t.Fatalf("unmarshal %q: %v", data, err)
		}
		out = append(out, e)
	}
	return out
}

// Anthropic callers expect chain of thought in a thinking block, opened and
// closed around the reasoning and distinct from the text block that follows.
func TestAnthropicStreamer_EmitsThinkingBlock(t *testing.T) {
	s := NewAnthropicStreamer("req-1", "qwen")
	var out []string
	out = append(out, s.Delta(types.ChunkMsg{ReasoningDelta: "step one "})...)
	out = append(out, s.Delta(types.ChunkMsg{ReasoningDelta: "step two"})...)
	out = append(out, s.Delta(types.ChunkMsg{Delta: "the answer"})...)
	out = append(out, s.Done("stop", nil)...)
	events := decodeEvents(t, out)

	var thinking string
	var thinkStart, thinkStop, textStart = -1, -1, -1
	for i, e := range events {
		switch {
		case e.Type == "content_block_start" && e.ContentBlock.Type == "thinking":
			thinkStart, _ = i, e
			if e.Index != 0 {
				t.Errorf("thinking block at index %d, want 0", e.Index)
			}
		case e.Type == "content_block_start" && e.ContentBlock.Type == "text":
			textStart = i
			if e.Index != 1 {
				t.Errorf("text block at index %d, want 1 — it must follow the thinking block", e.Index)
			}
		case e.Type == "content_block_stop" && e.Index == 0:
			thinkStop = i
		case e.Type == "content_block_delta" && e.Delta.Type == "thinking_delta":
			thinking += e.Delta.Thinking
		}
	}

	if thinking != "step one step two" {
		t.Errorf("thinking = %q, want %q", thinking, "step one step two")
	}
	if thinkStart < 0 {
		t.Fatal("no thinking block was opened")
	}
	if thinkStop < 0 || textStart < 0 || thinkStop > textStart {
		t.Errorf("thinking block did not close before the text block opened (stop=%d, textStart=%d)", thinkStop, textStart)
	}
}

// A model cut off mid-thought, or one that thinks and then only calls a tool,
// leaves the thinking block open. Done has to close it or the stream is
// malformed.
func TestAnthropicStreamer_ClosesDanglingThinkingBlock(t *testing.T) {
	s := NewAnthropicStreamer("req-1", "qwen")
	var out []string
	out = append(out, s.Delta(types.ChunkMsg{ReasoningDelta: "half a thou"})...)
	out = append(out, s.Done("stop", nil)...)
	joined := strings.Join(out, "\n")

	if starts, stops := strings.Count(joined, "content_block_start"), strings.Count(joined, "content_block_stop"); starts != stops {
		t.Errorf("%d content_block_start vs %d content_block_stop:\n%s", starts, stops, joined)
	}
}

// A response with no reasoning must stream exactly as it did before.
func TestAnthropicStreamer_TextOnlyUnchanged(t *testing.T) {
	s := NewAnthropicStreamer("req-1", "qwen")
	out := append(s.Delta(types.ChunkMsg{Delta: "hello"}), s.Done("stop", nil)...)

	if joined := strings.Join(out, "\n"); strings.Contains(joined, "thinking") {
		t.Errorf("text-only stream emitted a thinking block:\n%s", joined)
	}
	var sawText bool
	for _, e := range decodeEvents(t, out) {
		if e.Type == "content_block_start" && e.ContentBlock.Type == "text" {
			sawText = true
			if e.Index != 0 {
				t.Errorf("text block at index %d, want 0", e.Index)
			}
		}
	}
	if !sawText {
		t.Error("no text block was opened")
	}
}
