package llamacpp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"llmesh/pkg/types"
)

func collect(t *testing.T, stream bool, body string) []Chunk {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
		}
		io.WriteString(w, body)
	}))
	defer srv.Close()

	var got []Chunk
	err := New(srv.URL, nil).Infer(context.Background(),
		types.InferenceRequest{Model: "m", Stream: stream}, "",
		func(c Chunk) { got = append(got, c) })
	if err != nil {
		t.Fatalf("infer: %v", err)
	}
	return got
}

// A thinking model puts its reasoning in reasoning_content and leaves content
// empty until it starts answering. Reading only content dropped the bulk of the
// model's output and, with nothing to forward, left the router seeing silence
// from a worker that was in fact working.
func TestReadStream_ForwardsReasoningContent(t *testing.T) {
	got := collect(t, true, strings.Join([]string{
		`data: {"choices":[{"delta":{"reasoning_content":"first "}}]}`,
		`data: {"choices":[{"delta":{"reasoning_content":"second"}}]}`,
		`data: {"choices":[{"delta":{"content":"answer"}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
		"",
	}, "\n\n"))

	var reasoning, content string
	for _, c := range got {
		reasoning += c.ReasoningDelta
		content += c.Delta
	}
	if reasoning != "first second" {
		t.Errorf("reasoning = %q, want %q", reasoning, "first second")
	}
	if content != "answer" {
		t.Errorf("content = %q, want %q", content, "answer")
	}
}

// Reasoning must not be merged into the answer text — a caller that renders
// both would show the model's scratchpad as part of its reply.
func TestReadStream_KeepsReasoningSeparateFromContent(t *testing.T) {
	got := collect(t, true, strings.Join([]string{
		`data: {"choices":[{"delta":{"reasoning_content":"scratch"}}]}`,
		`data: {"choices":[{"delta":{"content":"reply"}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
		"",
	}, "\n\n"))

	for _, c := range got {
		if c.ReasoningDelta != "" && c.Delta != "" {
			t.Errorf("chunk mixes reasoning and content: %+v", c)
		}
	}
}

func TestReadBatch_ForwardsReasoningContent(t *testing.T) {
	got := collect(t, false,
		`{"choices":[{"message":{"content":"answer","reasoning_content":"thought"},"finish_reason":"stop"}]}`)

	if len(got) != 1 {
		t.Fatalf("got %d chunks, want 1", len(got))
	}
	if got[0].ReasoningDelta != "thought" {
		t.Errorf("ReasoningDelta = %q, want %q", got[0].ReasoningDelta, "thought")
	}
	if got[0].Delta != "answer" {
		t.Errorf("Delta = %q, want %q", got[0].Delta, "answer")
	}
}
