// router/e2e/perf_test.go
// End-to-end coverage for inference performance tracking: a real request through
// the real stack must land in the state database with correctly derived timings.
package e2e

import (
	"bufio"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"llmesh/pkg/types"
	"llmesh/router/internal/admin"
)

// perfStatsAfter flushes the recorder and returns the totals for a window wide
// enough to include anything just recorded.
func perfStatsAfter(t *testing.T, s *testStack) admin.PerfStats {
	t.Helper()
	s.Perf.Flush()
	now := time.Now()
	got, err := s.State.PerfTotals(now.Add(-2*time.Hour), now.Add(2*time.Hour), "")
	if err != nil {
		t.Fatalf("perf totals: %v", err)
	}
	return got
}

func approx(t *testing.T, label string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Fatalf("%s: got %v, want ~%v (±%v)", label, got, want, tol)
	}
}

// streamChunks is the response a mock client sends for a streaming request:
// three content tokens, then a terminal chunk carrying usage and timings.
func streamChunks(reqID string, timings *types.Timings) []types.ChunkMsg {
	usage := &types.UsageInfo{PromptTokens: 900, CompletionTokens: 3, TotalTokens: 903}
	usage.Timings = timings
	return []types.ChunkMsg{
		{Type: "chunk", RequestID: reqID, Delta: "one"},
		{Type: "chunk", RequestID: reqID, Delta: " two"},
		{Type: "chunk", RequestID: reqID, Delta: " three"},
		{Type: "chunk", RequestID: reqID, Done: true, FinishReason: "stop", Usage: usage},
	}
}

// drainStream reads an SSE response to completion so the router sees the request
// through to the end.
func drainStream(t *testing.T, body io.Reader) {
	t.Helper()
	sc := bufio.NewScanner(body)
	for sc.Scan() {
		if strings.TrimPrefix(sc.Text(), "data: ") == "[DONE]" {
			return
		}
	}
}

func TestE2E_Perf_StreamingRequestRecordsBackendTimings(t *testing.T) {
	s := setupTestStack(t)
	defer s.Cleanup()

	conn := mockClientSimulator(t, s.URL, s.ClientToken,
		[]types.ModelInfo{{Name: "test-model"}},
		func(reqID string) []types.ChunkMsg {
			// The backend reports 900 prompt tokens evaluated in 300ms and 3 tokens
			// generated in 150ms — 3000 tok/s prefill, 20 tok/s generation.
			return streamChunks(reqID, &types.Timings{
				PromptN: 900, PromptMS: 300, PredictedN: 3, PredictedMS: 150,
			})
		})
	defer conn.Close()

	body, _ := json.Marshal(map[string]any{
		"model":    "test-model",
		"messages": []map[string]string{{"role": "user", "content": "hello"}},
		"stream":   true,
	})
	resp, err := apiPost(s.URL+"/v1/chat/completions", s.APIKey, body)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	drainStream(t, resp.Body)

	got := perfStatsAfter(t, s)
	if got.Samples != 1 {
		t.Fatalf("samples: got %d, want 1", got.Samples)
	}
	// Throughput comes from the backend's own numbers, exactly.
	approx(t, "prefill throughput", got.PromptTokensPerSec(), 3000, 1)
	approx(t, "generation throughput", got.GenTokensPerSec(), 20, 0.1)
	if got.BackendSamples != 1 {
		t.Fatalf("sample not marked backend-reported: %+v", got)
	}
	// TTFT and end-to-end are router-side observations, so only sanity-check them.
	if got.TTFTSamples != 1 || got.AvgTTFTMS() <= 0 {
		t.Fatalf("no TTFT recorded for a streaming request: %+v", got)
	}
	if got.AvgTotalMS() < got.AvgTTFTMS() {
		t.Fatalf("end-to-end (%v) shorter than TTFT (%v)", got.AvgTotalMS(), got.AvgTTFTMS())
	}
}

func TestE2E_Perf_AttributesToCallerAndClient(t *testing.T) {
	s := setupTestStack(t)
	defer s.Cleanup()

	conn := mockClientSimulator(t, s.URL, s.ClientToken,
		[]types.ModelInfo{{Name: "test-model"}},
		func(reqID string) []types.ChunkMsg { return streamChunks(reqID, nil) })
	defer conn.Close()

	body, _ := json.Marshal(map[string]any{
		"model":    "test-model",
		"messages": []map[string]string{{"role": "user", "content": "hello"}},
		"stream":   true,
	})
	resp, err := apiPost(s.URL+"/v1/chat/completions", s.APIKey, body)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	drainStream(t, resp.Body)

	s.Perf.Flush()
	now := time.Now()
	since, until := now.Add(-2*time.Hour), now.Add(2*time.Hour)

	// The client token in the harness is testuser/test-client, which is how the
	// Clients page looks a machine's performance up.
	byClient, err := s.State.PerfByClient(since, until, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := byClient["testuser/test-client"]; !ok {
		t.Fatalf("not attributed to the serving machine: %v", byClient)
	}

	// And the caller sees it as their own, which is what scopes a member's view.
	mine, err := s.State.PerfTotals(since, until, "testuser")
	if err != nil {
		t.Fatal(err)
	}
	if mine.Samples != 1 {
		t.Fatalf("owner-scoped samples: got %d, want 1", mine.Samples)
	}
	other, err := s.State.PerfTotals(since, until, "somebody-else")
	if err != nil {
		t.Fatal(err)
	}
	if other.Samples != 0 {
		t.Fatalf("another user could see these requests: %d", other.Samples)
	}

	// Grouping by model reaches the same request from the chart's angle.
	rows, err := s.State.QueryPerf(since, until, "model", false, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Name != "test-model" {
		t.Fatalf("model grouping: %+v", rows)
	}
}

func TestE2E_Perf_StreamingFallbackWithoutBackendTimings(t *testing.T) {
	s := setupTestStack(t)
	defer s.Cleanup()

	// A backend that reports no timings — an external API behind a shim, say. The
	// router has to derive the prefill/decode split from what it observed.
	conn := mockClientSimulator(t, s.URL, s.ClientToken,
		[]types.ModelInfo{{Name: "test-model"}},
		func(reqID string) []types.ChunkMsg { return streamChunks(reqID, nil) })
	defer conn.Close()

	body, _ := json.Marshal(map[string]any{
		"model":    "test-model",
		"messages": []map[string]string{{"role": "user", "content": "hello"}},
		"stream":   true,
	})
	resp, err := apiPost(s.URL+"/v1/chat/completions", s.APIKey, body)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	drainStream(t, resp.Body)

	got := perfStatsAfter(t, s)
	if got.Samples != 1 {
		t.Fatalf("samples: got %d, want 1", got.Samples)
	}
	if got.BackendSamples != 0 {
		t.Fatalf("router-observed sample marked as backend-reported: %+v", got)
	}
	// The mock replies instantly, so the absolute rates are meaningless; what
	// matters is that both measures were captured with the right token counts.
	if got.PrefillSamples != 1 || got.PrefillTokens != 900 {
		t.Fatalf("prefill: %d samples, %d tokens (want 1, 900)", got.PrefillSamples, got.PrefillTokens)
	}
	if got.DecodeSamples != 1 || got.DecodeTokens != 3 {
		t.Fatalf("decode: %d samples, %d tokens (want 1, 3)", got.DecodeSamples, got.DecodeTokens)
	}
}

func TestE2E_Perf_BatchRequestRecordsNoTTFTButKeepsThroughput(t *testing.T) {
	s := setupTestStack(t)
	defer s.Cleanup()

	conn := mockClientSimulator(t, s.URL, s.ClientToken,
		[]types.ModelInfo{{Name: "test-model"}},
		func(reqID string) []types.ChunkMsg {
			// A non-streaming reply: one chunk with the whole body, plus timings.
			return []types.ChunkMsg{{
				Type: "chunk", RequestID: reqID, Delta: "the whole answer",
				Done: true, FinishReason: "stop",
				Usage: &types.UsageInfo{
					PromptTokens: 900, CompletionTokens: 4, TotalTokens: 904,
					Timings: &types.Timings{
						PromptN: 900, PromptMS: 450, PredictedN: 4, PredictedMS: 200,
					},
				},
			}}
		})
	defer conn.Close()

	body, _ := json.Marshal(map[string]any{
		"model":    "test-model",
		"messages": []map[string]string{{"role": "user", "content": "hello"}},
	})
	resp, err := apiPost(s.URL+"/v1/chat/completions", s.APIKey, body)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	io.ReadAll(resp.Body)

	got := perfStatsAfter(t, s)
	if got.Samples != 1 {
		t.Fatalf("samples: got %d, want 1", got.Samples)
	}
	// The single chunk arrives at completion, so there is no first-token signal to
	// measure — recording one would report a TTFT equal to the total duration.
	if got.TTFTSamples != 0 {
		t.Fatalf("batch request contributed a TTFT sample: %+v", got)
	}
	// The backend's timings still give a full throughput picture.
	approx(t, "prefill throughput", got.PromptTokensPerSec(), 2000, 1)
	approx(t, "generation throughput", got.GenTokensPerSec(), 20, 0.1)
}

func TestE2E_Perf_FailedRequestIsNotRecorded(t *testing.T) {
	s := setupTestStack(t)
	defer s.Cleanup()

	conn := connectMockClient(t, s.URL, s.ClientToken, []types.ModelInfo{{Name: "test-model"}})
	defer conn.Close()

	// Answer the job with an error rather than a completion.
	go func() {
		for {
			var msg struct {
				Type    string                 `json:"type"`
				Request types.InferenceRequest `json:"request"`
			}
			if err := conn.ReadJSON(&msg); err != nil {
				return
			}
			if msg.Type == "job" {
				conn.WriteJSON(types.ErrorMsg{
					Type: "error", RequestID: msg.Request.ID, Message: "backend exploded",
				})
			}
		}
	}()

	body, _ := json.Marshal(map[string]any{
		"model":    "test-model",
		"messages": []map[string]string{{"role": "user", "content": "hello"}},
	})
	resp, err := apiPost(s.URL+"/v1/chat/completions", s.APIKey, body)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	io.ReadAll(resp.Body)

	// A request that never completed has no meaningful speed; counting it would
	// pull the averages toward whatever happened before it failed.
	if got := perfStatsAfter(t, s); got.Samples != 0 {
		t.Fatalf("a failed request was recorded: %+v", got)
	}
}
