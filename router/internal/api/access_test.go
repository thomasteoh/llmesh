package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"llmesh/pkg/types"
	"llmesh/router/internal/dedup"
)

// fakeAccess permits a fixed set of models.
type fakeAccess struct {
	allow   map[string]bool
	gotIP   string
	gotPath string
}

func (f *fakeAccess) AuthorizeModels(key, endpoint, ip string, candidates []string, _ map[string]map[string]any) ([]string, string) {
	f.gotIP, f.gotPath = ip, endpoint
	var out []string
	for _, c := range candidates {
		if f.allow[c] {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return nil, `denied by policy "test"`
	}
	return out, ""
}

type fakeModels struct{ infos []types.ModelInfo }

func (m fakeModels) ActiveModels() []string {
	var out []string
	for _, i := range m.infos {
		out = append(out, i.Name)
	}
	return out
}
func (m fakeModels) ActiveModelInfos() []types.ModelInfo { return m.infos }
func (m fakeModels) AvailableSlotsByModel(string) []types.ModelSlots {
	var out []types.ModelSlots
	for _, i := range m.infos {
		out = append(out, types.ModelSlots{Model: i.Name, AvailableSlots: 1, TotalSlots: 1})
	}
	return out
}

func TestModelAccessAtAdmission(t *testing.T) {
	keys := limitKeys{labels: map[string]string{"sk-a": "alice/k"}}
	access := &fakeAccess{allow: map[string]bool{"qwen3": true}}
	h := &Handler{Keys: keys, Access: access,
		Models:  fakeModels{infos: []types.ModelInfo{{Name: "gpt-4o"}, {Name: "qwen3"}}},
		Aliases: fakeAliases{"smart": {"gpt-4o", "qwen3"}, "paid": {"gpt-4o"}}}

	send := func(model string) (w *httptest.ResponseRecorder) {
		body := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`
		r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer sk-a")
		r.Header.Set("X-Forwarded-For", "6.6.6.6")
		r.RemoteAddr = "10.1.1.1:5555"
		w = httptest.NewRecorder()
		defer func() { recover() }() // a permitted request goes on to parts this test does not wire
		h.OpenAI()(w, r)
		return w
	}
	for _, m := range []string{"gpt-4o", "paid"} {
		w := send(m)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "permission_error") {
			t.Errorf("%s: got %d %s, want 403", m, w.Code, w.Body.String())
		}
	}
	if access.gotPath != "/v1/chat/completions" {
		t.Errorf("endpoint passed as %q", access.gotPath)
	}
	// Proxy headers are ignored unless the router trusts them.
	if access.gotIP != "10.1.1.1" {
		t.Errorf("policy IP %q: an untrusted X-Forwarded-For was believed", access.gotIP)
	}
	h.TrustProxy = true
	send("gpt-4o")
	if access.gotIP != "6.6.6.6" {
		t.Errorf("policy IP %q with a trusted proxy", access.gotIP)
	}
	// An alias with one permitted target is admitted.
	if w := send("smart"); w.Code == http.StatusForbidden {
		t.Errorf("an alias with a permitted target was refused: %s", w.Body.String())
	}
}

func TestModelListIsFiltered(t *testing.T) {
	keys := limitKeys{labels: map[string]string{"sk-a": "alice/k"}}
	h := &Handler{Keys: keys, Access: &fakeAccess{allow: map[string]bool{"qwen3": true}},
		Models:  fakeModels{infos: []types.ModelInfo{{Name: "gpt-4o"}, {Name: "qwen3"}}},
		Aliases: fakeAliases{"smart": {"gpt-4o", "qwen3"}, "paid": {"gpt-4o"}}}
	for _, path := range []string{"/v1/models", "/v1/models/slots"} {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("Authorization", "Bearer sk-a")
		w := httptest.NewRecorder()
		if path == "/v1/models" {
			h.ModelList()(w, r)
		} else {
			h.ModelSlots()(w, r)
		}
		var resp struct {
			Data []map[string]any `json:"data"`
		}
		json.Unmarshal(w.Body.Bytes(), &resp)
		var names []string
		for _, e := range resp.Data {
			if id, ok := e["id"].(string); ok {
				names = append(names, id)
			} else if m, ok := e["model"].(string); ok {
				names = append(names, m)
			}
		}
		got := strings.Join(names, ",")
		if strings.Contains(got, "gpt-4o") || strings.Contains(got, "paid") || !strings.Contains(got, "qwen3") {
			t.Errorf("%s listed %q", path, got)
		}
		if path == "/v1/models" && !strings.Contains(got, "smart") {
			t.Errorf("an alias with a permitted target was hidden: %q", got)
		}
	}
}

func TestDedupScope(t *testing.T) {
	req := &types.InferenceRequest{Model: "m", Messages: []types.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}}}
	a := dedup.ContentHashScoped(req, false, "alice|m")
	b := dedup.ContentHashScoped(req, false, "bob|m")
	if a == b {
		t.Error("identical requests from different owners share a coalescing hash")
	}
	if a != dedup.ContentHashScoped(req, false, "alice|m") {
		t.Error("the scoped hash is not stable")
	}
}
