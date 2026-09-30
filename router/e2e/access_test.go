package e2e

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"llmesh/pkg/types"
	"llmesh/router/internal/authz"
)

// A model-access deny rule stops a request at the door, through the whole
// stack, and removing it lets the same request through to a client.
func TestE2E_ModelAccessDenied(t *testing.T) {
	routerURL, apiKey, clientToken, st, cleanup := setupTestRouterState(t)
	defer cleanup()
	conn := mockClientSimulator(t, routerURL, clientToken, []types.ModelInfo{{Name: "test-llama"}},
		func(reqID string) []types.ChunkMsg {
			return []types.ChunkMsg{{Type: "chunk", RequestID: reqID, Delta: "ok", Done: true, FinishReason: "stop"}}
		})
	defer conn.Close()
	waitForModel(t, routerURL, apiKey, "test-llama")

	if err := st.SavePolicy(authz.Policy{ID: "no-llama", Name: "No llama", Effect: authz.Deny, Enabled: true,
		Actions: []string{"model.use"}, Resource: authz.ResourceMatcher{Type: "model", IDs: []string{"test-*"}}}, "test"); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"model": "test-llama", "messages": []map[string]string{{"role": "user", "content": "hi"}}})
	resp, err := apiPost(routerURL+"/v1/chat/completions", apiKey, body)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(b), "No llama") {
		t.Fatalf("denied model: %d %s", resp.StatusCode, b)
	}
	// Hidden from the listing as well.
	req, _ := http.NewRequest("GET", routerURL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	lr, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	lb, _ := io.ReadAll(lr.Body)
	lr.Body.Close()
	if strings.Contains(string(lb), "test-llama") {
		t.Errorf("a denied model is still listed: %s", lb)
	}

	if err := st.DeletePolicy("no-llama"); err != nil {
		t.Fatal(err)
	}
	resp, err = apiPost(routerURL+"/v1/chat/completions", apiKey, body)
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("after removing the rule: %d %s", resp.StatusCode, b)
	}
}
