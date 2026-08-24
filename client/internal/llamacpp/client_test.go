package llamacpp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"llmesh/pkg/types"
)

func TestInferUsageToUsageInfo(t *testing.T) {
	// A nil backend usage passes straight through as nil.
	if (*inferUsage)(nil).toUsageInfo() != nil {
		t.Error("nil inferUsage should map to nil UsageInfo")
	}

	// prompt_tokens_details.cached_tokens is carried onto CacheReadTokens.
	var u inferUsage
	if err := json.Unmarshal([]byte(`{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"prompt_tokens_details":{"cached_tokens":80}}`), &u); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := u.toUsageInfo()
	if got.PromptTokens != 100 || got.CompletionTokens != 20 || got.TotalTokens != 120 {
		t.Errorf("base token counts wrong: %+v", got)
	}
	if got.CacheReadTokens != 80 {
		t.Errorf("CacheReadTokens = %d, want 80", got.CacheReadTokens)
	}

	// A backend that reports no cache details leaves CacheReadTokens zero.
	var noCache inferUsage
	if err := json.Unmarshal([]byte(`{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}`), &noCache); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if noCache.toUsageInfo().CacheReadTokens != 0 {
		t.Error("CacheReadTokens should be 0 when backend reports no cache details")
	}
}

func TestProbeModelID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"object":"list","data":[{"id":"deepseek-v4-flash","object":"model"}]}`))
	}))
	defer srv.Close()

	got := New(srv.URL, nil).ProbeModelID(context.Background())
	if got != "deepseek-v4-flash" {
		t.Errorf("ProbeModelID = %q, want deepseek-v4-flash", got)
	}
}

func TestApplyHeaders(t *testing.T) {
	var gotAuth, gotCustom string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCustom = r.Header.Get("X-Api-Key")
		w.Write([]byte(`{"object":"list","data":[{"id":"m","object":"model"}]}`))
	}))
	defer srv.Close()

	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer secret-key")
	hdr.Set("X-Api-Key", "gateway-token")
	New(srv.URL, hdr).ProbeModelID(context.Background())

	if gotAuth != "Bearer secret-key" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer secret-key")
	}
	if gotCustom != "gateway-token" {
		t.Errorf("X-Api-Key = %q, want %q", gotCustom, "gateway-token")
	}
}

func TestApplyHeaders_NilNoAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h := r.Header.Get("Authorization"); h != "" {
			t.Errorf("unexpected Authorization header %q for nil headers", h)
		}
		w.Write([]byte(`{"object":"list","data":[{"id":"m","object":"model"}]}`))
	}))
	defer srv.Close()
	New(srv.URL, nil).ProbeModelID(context.Background())
}

func TestProbeModelID_Errors(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"empty data", func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"object":"list","data":[]}`))
		}},
		{"server error", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}},
		{"malformed json", func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`not json`))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			if got := New(srv.URL, nil).ProbeModelID(context.Background()); got != "" {
				t.Errorf("expected empty model id, got %q", got)
			}
		})
	}
}

// --- Backend timings ---

// inferTo runs one inference against a stub server and returns the usage handed
// to the final callback, which is where timings ride back to the router.
func inferTo(t *testing.T, stream bool, body string) *types.UsageInfo {
	t.Helper()
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
		}
		io.WriteString(w, body)
	}))
	defer srv.Close()

	var final *types.UsageInfo
	err := New(srv.URL, nil).Infer(context.Background(),
		types.InferenceRequest{Model: "m", Stream: stream},
		"",
		func(c Chunk) {
			if c.Done {
				final = c.Usage
			}
		})
	if err != nil {
		t.Fatalf("infer: %v", err)
	}
	// The llama.cpp extension that asks for timings must actually be sent, or the
	// server has no reason to report them.
	if gotBody["timings_per_token"] != true {
		t.Fatalf("request did not ask for timings: %v", gotBody["timings_per_token"])
	}
	return final
}

func TestInfer_ParsesBatchTimings(t *testing.T) {
	usage := inferTo(t, false, `{
		"choices":[{"message":{"content":"hi"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":800,"completion_tokens":100,"total_tokens":900},
		"timings":{"prompt_n":780,"prompt_ms":250.5,"predicted_n":100,"predicted_ms":1800}
	}`)
	if usage == nil || usage.Timings == nil {
		t.Fatalf("timings not carried through: %+v", usage)
	}
	if usage.PromptTokens != 800 || usage.CompletionTokens != 100 {
		t.Fatalf("usage mangled: %+v", usage)
	}
	got := usage.Timings
	if got.PromptN != 780 || got.PromptMS != 250.5 || got.PredictedN != 100 || got.PredictedMS != 1800 {
		t.Fatalf("timings: %+v", got)
	}
}

func TestInfer_ParsesStreamTimingsAndKeepsLatest(t *testing.T) {
	// llama.cpp repeats `timings` as running totals while streaming, so the final
	// set observed is the complete one. The usage-only chunk arrives after the
	// finish_reason chunk and must not clobber the timings collected earlier.
	usage := inferTo(t, true, `data: {"choices":[{"delta":{"content":"a"}}],"timings":{"prompt_n":780,"prompt_ms":250,"predicted_n":1,"predicted_ms":20}}

data: {"choices":[{"delta":{"content":"b"}}],"timings":{"prompt_n":780,"prompt_ms":250,"predicted_n":2,"predicted_ms":40}}

data: {"choices":[{"delta":{},"finish_reason":"stop"}]}

data: {"choices":[],"usage":{"prompt_tokens":800,"completion_tokens":2,"total_tokens":802}}

data: [DONE]

`)
	if usage == nil || usage.Timings == nil {
		t.Fatalf("timings not carried through: %+v", usage)
	}
	if usage.PromptTokens != 800 || usage.CompletionTokens != 2 {
		t.Fatalf("usage lost when merging timings: %+v", usage)
	}
	// The second chunk's totals, not the first.
	if usage.Timings.PredictedN != 2 || usage.Timings.PredictedMS != 40 {
		t.Fatalf("stale timings kept: %+v", usage.Timings)
	}
}

func TestInfer_NoTimingsFromBackendLeavesUsageAlone(t *testing.T) {
	// A plain OpenAI-compatible server reports no timings; usage must survive
	// untouched and Timings must stay nil so the router knows to fall back.
	usage := inferTo(t, false, `{
		"choices":[{"message":{"content":"hi"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}
	}`)
	if usage == nil {
		t.Fatal("usage dropped")
	}
	if usage.Timings != nil {
		t.Fatalf("timings invented: %+v", usage.Timings)
	}
	if usage.PromptTokens != 10 || usage.CompletionTokens != 5 {
		t.Fatalf("usage mangled: %+v", usage)
	}
}

func TestInfer_AllZeroTimingsTreatedAsAbsent(t *testing.T) {
	// A server that emits the field but fills it with zeros carries no information;
	// reporting it as present would make the router trust a meaningless split.
	usage := inferTo(t, false, `{
		"choices":[{"message":{"content":"hi"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":10,"completion_tokens":5},
		"timings":{"prompt_n":0,"prompt_ms":0,"predicted_n":0,"predicted_ms":0}
	}`)
	if usage.Timings != nil {
		t.Fatalf("all-zero timings reported as present: %+v", usage.Timings)
	}
}

func TestInfer_TimingsWithoutUsageAreDropped(t *testing.T) {
	// Timings present, no `usage` object. The timings are discarded rather than
	// synthesising a zero-token carrier: downstream, a nil usage is the signal that
	// the backend reported no token counts, and a stand-in would both publish an
	// empty usage object to the caller and book a zero-token request into the usage
	// table. Preserving that signal matters more than the lost measurement, which
	// only arises on a truncated stream.
	usage := inferTo(t, false, `{
		"choices":[{"message":{"content":"hi"},"finish_reason":"stop"}],
		"timings":{"prompt_n":50,"prompt_ms":100,"predicted_n":10,"predicted_ms":200}
	}`)
	if usage != nil {
		t.Fatalf("a usage object was synthesised to carry timings: %+v", usage)
	}
}
