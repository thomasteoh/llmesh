package reqopt

import (
	"encoding/json"
	"testing"

	"llmesh/pkg/types"
)

func msg(role, content string) types.Message {
	b, _ := json.Marshal(content)
	return types.Message{Role: role, Content: b}
}

func contentOf(t *testing.T, m types.Message) string {
	t.Helper()
	var s string
	if err := json.Unmarshal(m.Content, &s); err != nil {
		t.Fatalf("content not a string: %s", m.Content)
	}
	return s
}

func TestClean_Disabled_NoOp(t *testing.T) {
	req := &types.InferenceRequest{
		Temperature: 9,
		Messages:    []types.Message{msg("user", "  hi  ")},
	}
	Clean(req, types.RequestOptimization{})
	if req.Temperature != 9 {
		t.Errorf("temperature changed with clamp off: %v", req.Temperature)
	}
	if got := contentOf(t, req.Messages[0]); got != "  hi  " {
		t.Errorf("content trimmed with clean off: %q", got)
	}
}

func TestClean_ClampParams(t *testing.T) {
	req := &types.InferenceRequest{Temperature: 5, TopP: 3}
	Clean(req, types.RequestOptimization{ClampParams: true})
	if req.Temperature != 2 {
		t.Errorf("temperature = %v, want 2", req.Temperature)
	}
	if req.TopP != 1 {
		t.Errorf("top_p = %v, want 1", req.TopP)
	}

	req = &types.InferenceRequest{Temperature: -1, TopP: -1}
	Clean(req, types.RequestOptimization{ClampParams: true})
	if req.Temperature != 0 || req.TopP != 0 {
		t.Errorf("negatives not clamped to 0: temp=%v top_p=%v", req.Temperature, req.TopP)
	}
}

func TestClean_TrimAndDropEmpty(t *testing.T) {
	req := &types.InferenceRequest{
		Messages: []types.Message{
			msg("system", "  be brief  "),
			msg("user", "   "), // whitespace-only → dropped
			msg("user", "hello"),
		},
	}
	Clean(req, types.RequestOptimization{CleanRequests: true})
	if len(req.Messages) != 2 {
		t.Fatalf("expected 2 messages after dropping empty, got %d", len(req.Messages))
	}
	if got := contentOf(t, req.Messages[0]); got != "be brief" {
		t.Errorf("system not trimmed: %q", got)
	}
	if got := contentOf(t, req.Messages[1]); got != "hello" {
		t.Errorf("user content altered: %q", got)
	}
}

func TestClean_NeverEmptiesRequest(t *testing.T) {
	req := &types.InferenceRequest{Messages: []types.Message{msg("user", "   ")}}
	Clean(req, types.RequestOptimization{CleanRequests: true})
	if len(req.Messages) != 1 {
		t.Fatalf("cleaning emptied the request; want original kept, got %d messages", len(req.Messages))
	}
}

func TestClean_Aggressive_CollapsesInteriorWhitespace(t *testing.T) {
	req := &types.InferenceRequest{
		Messages: []types.Message{msg("user", "foo    bar\t\tbaz\nqux   end")},
	}
	Clean(req, types.RequestOptimization{CleanRequests: true, CleanAggressive: true})
	want := "foo bar baz\nqux end"
	if got := contentOf(t, req.Messages[0]); got != want {
		t.Errorf("aggressive collapse = %q, want %q", got, want)
	}
}

func TestClean_Conservative_KeepsInteriorWhitespace(t *testing.T) {
	req := &types.InferenceRequest{
		Messages: []types.Message{msg("user", "foo    bar")},
	}
	Clean(req, types.RequestOptimization{CleanRequests: true})
	if got := contentOf(t, req.Messages[0]); got != "foo    bar" {
		t.Errorf("conservative clean changed interior whitespace: %q", got)
	}
}

func TestClean_StructuredContentUntouched(t *testing.T) {
	structured := json.RawMessage(`[{"type":"text","text":"  spaced  "}]`)
	req := &types.InferenceRequest{
		Messages: []types.Message{{Role: "user", Content: structured}},
	}
	Clean(req, types.RequestOptimization{CleanRequests: true, CleanAggressive: true})
	if string(req.Messages[0].Content) != string(structured) {
		t.Errorf("structured content was modified: %s", req.Messages[0].Content)
	}
}

func TestClean_DropsEmptyStructuredContent(t *testing.T) {
	req := &types.InferenceRequest{
		Messages: []types.Message{
			{Role: "user", Content: json.RawMessage(`[]`)},
			msg("user", "real"),
		},
	}
	Clean(req, types.RequestOptimization{CleanRequests: true})
	if len(req.Messages) != 1 {
		t.Fatalf("expected empty array message dropped, got %d", len(req.Messages))
	}
}

func TestClean_KeepsToolMessageWithEmptyContent(t *testing.T) {
	req := &types.InferenceRequest{
		Messages: []types.Message{
			{Role: "assistant", Content: json.RawMessage(`""`), ToolCalls: json.RawMessage(`[{"id":"1"}]`)},
		},
	}
	Clean(req, types.RequestOptimization{CleanRequests: true})
	if len(req.Messages) != 1 {
		t.Fatalf("tool-call message with empty content was dropped")
	}
}

func TestPrefixKey_SameConversationStablePrefix(t *testing.T) {
	turn1 := &types.InferenceRequest{
		Model: "llama3",
		Messages: []types.Message{
			msg("system", "be helpful"),
			msg("user", "hello"),
		},
	}
	turn2 := &types.InferenceRequest{
		Model: "llama3",
		Messages: []types.Message{
			msg("system", "be helpful"),
			msg("user", "hello"),
			msg("assistant", "hi there"),
			msg("user", "follow-up question"),
		},
	}
	if PrefixKey(turn1) != PrefixKey(turn2) {
		t.Error("same conversation prefix should yield the same key across turns")
	}
}

func TestPrefixKey_DifferentModelOrPrefixDiffers(t *testing.T) {
	base := &types.InferenceRequest{
		Model:    "llama3",
		Messages: []types.Message{msg("user", "hello")},
	}
	otherModel := &types.InferenceRequest{
		Model:    "qwen",
		Messages: []types.Message{msg("user", "hello")},
	}
	otherPrefix := &types.InferenceRequest{
		Model:    "llama3",
		Messages: []types.Message{msg("user", "different opening")},
	}
	if PrefixKey(base) == PrefixKey(otherModel) {
		t.Error("different model should yield a different key")
	}
	if PrefixKey(base) == PrefixKey(otherPrefix) {
		t.Error("different opening message should yield a different key")
	}
}

func TestPrefixKey_EmptyMessages(t *testing.T) {
	if PrefixKey(&types.InferenceRequest{Model: "x"}) != "" {
		t.Error("empty messages should yield empty key")
	}
}
