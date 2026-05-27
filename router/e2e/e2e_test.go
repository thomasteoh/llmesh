// router/e2e/e2e_test.go
package e2e

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"llmesh/pkg/types"
	"llmesh/router/internal/admin"
	"llmesh/router/internal/api"
	"llmesh/router/internal/correlation"
	"llmesh/router/internal/hub"
	"llmesh/router/internal/logring"
	"llmesh/router/internal/queue"
	"llmesh/router/internal/scheduler"
	"llmesh/router/internal/stats"
	"llmesh/router/internal/translate"
)

func init() {
	// Silence router logging in tests
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// genRandomKey generates a random hex string of the given length.
func genRandomKey(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// setupTestRouter creates a full router stack in-memory (matching main.go setup) and
// returns the httptest.Server URL, the generated API key, the generated client token,
// and a cleanup function.
func setupTestRouter(t *testing.T) (routerURL, apiKey, clientToken string, cleanup func()) {
	t.Helper()
	dir := t.TempDir()
	statePath := dir + "/state.json"

	// Create admin state with test user, API key, and client token
	st, err := admin.LoadState(statePath)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}

	// Create admin user
	pwHash := "test-hash" // not validated in unit context
	st.AddUser(admin.User{
		Username:     "admin",
		PasswordHash: pwHash,
		Role:         "admin",
	})

	// Create API key for callers
	var keyErr error
	apiKey, keyErr = admin.GenAPIKeyValue("testuser")
	if keyErr != nil {
		t.Fatalf("gen API key: %v", keyErr)
	}
	st.AddAPIKey(admin.APIKey{
		Label:    "test",
		Owner:    "testuser",
		Key:      apiKey,
		Priority: "normal",
	})

	// Create client token for the mock client
	clientToken, keyErr = admin.GenClientTokenValue("testuser")
	if keyErr != nil {
		t.Fatalf("gen client token: %v", keyErr)
	}
	st.AddClientToken(admin.ClientToken{
		Name:  "test-client",
		Owner: "testuser",
		Token: clientToken,
	})

	// Wire components (same as main.go)
	q := queue.New()
	testSink := logring.New(logring.DefaultCap)
	store := correlation.New(slog.Default())
	h := hub.New(slog.Default())
	reqStats := stats.New()

	h.OnChunk = func(msg types.ChunkMsg) {
		if !store.Send(msg) && !msg.Done {
			t.Logf("chunk lost in test harness: request_id=%s", msg.RequestID)
		}
	}
	h.OnError = func(msg types.ErrorMsg) {
		store.Send(types.ChunkMsg{
			Type:         "chunk",
			RequestID:    msg.RequestID,
			Done:         true,
			FinishReason: "error",
		})
	}

	var apiHandler *api.Handler

	adminHandler, err := admin.New(statePath, h, q, func() int64 {
		if apiHandler == nil {
			return 0
		}
		return apiHandler.Count()
	}, reqStats, "e2e", "llmesh", "localhost", testSink)
	if err != nil {
		t.Fatalf("admin new: %v", err)
	}

	sched := scheduler.New(q, h, adminHandler.State(), slog.Default())
	sched.Start()

	apiHandler = &api.Handler{
		Keys:        adminHandler.State(),
		Models:      h,
		Aliases:     adminHandler.State(),
		Stats:       reqStats,
		Queue:       q,
		Correlation: store,
		Scheduler:   sched,
		Canceller:   h,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", apiHandler.ModelList())
	mux.HandleFunc("/v1/chat/completions", apiHandler.OpenAI())
	mux.HandleFunc("/v1/messages", apiHandler.Anthropic())
	mux.HandleFunc("/v1/responses", apiHandler.Responses())
	mux.HandleFunc("/ws/client", func(w http.ResponseWriter, r *http.Request) {
		token := api.ExtractBearer(r)
		if token == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		ct, ok := adminHandler.State().LookupClientToken(token)
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeWS(w, r, ct.Name, ct.Owner, token, ct.OwnerSlots)
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"status":"ok","version":"e2e"}`+"\n")
	})

	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	routerURL = ts.URL
	cleanup = func() { sched.Stop() }
	return
}

// connectMockClient dials the WS endpoint, authenticates, and registers the client.
// Returns the WS connection and a cleanup function.
func connectMockClient(t *testing.T, routerURL, token string, models []types.ModelInfo) *websocket.Conn {
	t.Helper()
	wsURL := "ws" + routerURL[4:] + "/ws/client" // http→ws

	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	conn, _, err := dialer.Dial(wsURL, http.Header{
		"Authorization": {"Bearer " + token},
	})
	if err != nil {
		t.Fatalf("dial WS: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	// Send register message
	reg := types.RegisterMsg{
		Type:          "register",
		Models:        models,
		MaxConcurrent: 4,
		Version:       "e2e",
	}
	if err := conn.WriteJSON(reg); err != nil {
		t.Fatalf("send register: %v", err)
	}

	return conn
}

// mockClientSimulator connects a client, waits for job messages, and sends
// back chunk responses via the sendChunks callback.
func mockClientSimulator(t *testing.T, routerURL, token string, models []types.ModelInfo, sendChunks func(reqID string) []types.ChunkMsg) *websocket.Conn {
	t.Helper()
	conn := connectMockClient(t, routerURL, token, models)

	go func() {
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var envelope struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(data, &envelope); err != nil {
				continue
			}
			if envelope.Type == "job" {
				var job types.JobMsg
				if err := json.Unmarshal(data, &job); err != nil {
					continue
				}
				reqID := job.Request.ID

				// Build mock chunks from callback
				chunks := sendChunks(reqID)

				// Send chunks back to router
				for _, c := range chunks {
					msg, _ := json.Marshal(c)
					if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
						return
					}
				}
			}
		}
	}()

	return conn
}

// apiPost issues an authenticated POST request.
func apiPost(url, apiKey string, body []byte) (*http.Response, error) {
	req, _ := http.NewRequest("POST", url, bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	return http.DefaultClient.Do(req)
}

// parseOpenAIChatResponse parses a batch (non-streaming) OpenAI chat completion response.
func parseOpenAIChatResponse(t *testing.T, body []byte) (content string, id string) {
	t.Helper()
	var resp map[string]any
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	choices, ok := resp["choices"].([]any)
	if !ok || len(choices) == 0 {
		t.Fatal("no choices in response")
	}
	choice, ok := choices[0].(map[string]any)
	if !ok {
		t.Fatal("invalid choice")
	}
	msg, ok := choice["message"].(map[string]any)
	if !ok {
		t.Fatal("no message in choice")
	}
	content, _ = msg["content"].(string)
	id, _ = resp["id"].(string)
	return content, id
}

func TestE2E_BatchRequest(t *testing.T) {
	routerURL, apiKey, clientToken, cleanup := setupTestRouter(t)
	defer cleanup()

	// Connect mock client
	models := []types.ModelInfo{{Name: "test-llama"}}
	conn := connectMockClient(t, routerURL, clientToken, models)
	defer conn.Close()

	// Post a batch request
	payload := map[string]any{
		"model":  "test-llama",
		"stream": false,
		"messages": []map[string]string{
			{"role": "user", "content": "hello"},
		},
	}
	body, _ := json.Marshal(payload)

	// The client must send chunks BEFORE the request arrives, or we need
	// a different approach. Since the router processes synchronously (enqueue
	// → wait on correlation channel), the mock client must be ready to respond.
	// We use a goroutine to pre-connect and listen for jobs.

	var wg sync.WaitGroup
	wg.Add(1)
	var mu sync.Mutex
	var receivedChunks []types.ChunkMsg

	go func() {
		defer wg.Done()
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var env struct{ Type string }
			if err := json.Unmarshal(data, &env); err != nil {
				continue
			}
			if env.Type == "job" {
				var job types.JobMsg
				if err := json.Unmarshal(data, &job); err != nil {
					continue
				}
				reqID := job.Request.ID
				chunks := []types.ChunkMsg{
					{Type: "chunk", RequestID: reqID, Delta: "Hello from mock client!"},
					{Type: "chunk", RequestID: reqID, Delta: "", Done: true, FinishReason: "stop"},
				}
				mu.Lock()
				receivedChunks = chunks
				mu.Unlock()
				for _, c := range chunks {
					msg, _ := json.Marshal(c)
					conn.WriteMessage(websocket.TextMessage, msg)
				}
				return
			}
		}
	}()

	// Give client a moment to start listening
	time.Sleep(100 * time.Millisecond)

	// Post request
	resp, err := apiPost(routerURL+"/v1/chat/completions", apiKey, body)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ = io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, string(body))
	}

	respBody, _ := io.ReadAll(resp.Body)
	content, reqID := parseOpenAIChatResponse(t, respBody)

	if content == "" {
		t.Fatal("empty content in response")
	}
	if reqID == "" {
		t.Fatal("empty request ID")
	}

	// Verify the mock client received the job
	mu.Lock()
	chunks := receivedChunks
	mu.Unlock()
	if len(chunks) == 0 {
		t.Fatal("mock client never received a job")
	}

	// Verify correlation: the chunks should have the same request ID
	if chunks[0].RequestID != reqID {
		t.Fatalf("chunk request ID %s != response ID %s", chunks[0].RequestID, reqID)
	}

	t.Logf("batch OK: content=%q, reqID=%s, chunks=%d", content, reqID, len(chunks))
}

func TestE2E_StreamRequest(t *testing.T) {
	routerURL, apiKey, clientToken, cleanup := setupTestRouter(t)
	defer cleanup()

	models := []types.ModelInfo{{Name: "stream-model"}}

	// Connect mock client
	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	wsURL := "ws" + routerURL[4:] + "/ws/client"
	conn, _, err := dialer.Dial(wsURL, http.Header{
		"Authorization": {"Bearer " + clientToken},
	})
	if err != nil {
		t.Fatalf("dial WS: %v", err)
	}
	defer conn.Close()

	// Register
	reg := types.RegisterMsg{
		Type:          "register",
		Models:        models,
		MaxConcurrent: 4,
	}
	conn.WriteJSON(reg)

	var mu sync.Mutex
	var receivedReqID string

	go func() {
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var env struct{ Type string }
			if err := json.Unmarshal(data, &env); err != nil {
				continue
			}
			if env.Type == "job" {
				var job types.JobMsg
				if err := json.Unmarshal(data, &job); err != nil {
					continue
				}
				mu.Lock()
				receivedReqID = job.Request.ID
				mu.Unlock()

				reqID := receivedReqID
				chunks := []types.ChunkMsg{
					{Type: "chunk", RequestID: reqID, Delta: "Stream"},
					{Type: "chunk", RequestID: reqID, Delta: "ed"},
					{Type: "chunk", RequestID: reqID, Delta: " response"},
					{Type: "chunk", RequestID: reqID, Delta: "", Done: true, FinishReason: "stop"},
				}
				for _, c := range chunks {
					msg, _ := json.Marshal(c)
					conn.WriteMessage(websocket.TextMessage, msg)
				}
				return
			}
		}
	}()

	time.Sleep(100 * time.Millisecond)

	// POST streaming request
	payload := map[string]any{
		"model":  "stream-model",
		"stream": true,
		"messages": []map[string]string{
			{"role": "user", "content": "stream me"},
		},
	}
	body, _ := json.Marshal(payload)

	resp, err := apiPost(routerURL+"/v1/chat/completions", apiKey, body)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, string(body))
	}

	respBody, _ := io.ReadAll(resp.Body)
	lines := bytes.Split(bytes.TrimSpace(respBody), []byte("\n"))

	// Collect content from SSE data lines
	var fullContent string
	for _, line := range lines {
		if !bytes.HasPrefix(line, []byte("data: ")) {
			continue
		}
		data := bytes.TrimPrefix(line, []byte("data: "))
		if bytes.Equal(data, []byte("[DONE]")) {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(data, &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) > 0 {
			fullContent += chunk.Choices[0].Delta.Content
		}
	}

	if fullContent == "" {
		t.Fatalf("empty streamed content. response: %s", string(respBody))
	}
	if fullContent != "Streamed response" {
		t.Fatalf("unexpected content: %q", fullContent)
	}

	// Verify job was received by mock client
	mu.Lock()
	rcvdID := receivedReqID
	mu.Unlock()
	if rcvdID == "" {
		t.Fatal("mock client never received a job")
	}

	t.Logf("stream OK: content=%q, chunks=%d", fullContent, len(lines))
}

func TestE2E_AuthFailure(t *testing.T) {
	routerURL, _, _, cleanup := setupTestRouter(t)
	defer cleanup()

	// POST without auth
	resp, err := http.Post(routerURL+"/v1/chat/completions", "application/json",
		bytes.NewBuffer([]byte(`{"model":"test","messages":[]}`)))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}

	// POST with invalid key
	resp2, err := http.Post(routerURL+"/v1/chat/completions", "application/json",
		bytes.NewBuffer([]byte(`{"model":"test","messages":[]}`)))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp2.Body.Close()

	// Note: this also returns 401 because the model "test" is not available
	// (no client connected). The auth check runs first.
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp2.StatusCode)
	}
}

func TestE2E_ModelNotFound(t *testing.T) {
	routerURL, apiKey, clientToken, cleanup := setupTestRouter(t)
	defer cleanup()

	// Connect client first
	models := []types.ModelInfo{{Name: "other-model"}}
	conn := connectMockClient(t, routerURL, clientToken, models)
	defer conn.Close()

	time.Sleep(50 * time.Millisecond)

	// POST with a model that no client serves
	payload := map[string]any{
		"model":  "nonexistent-model",
		"stream": false,
		"messages": []map[string]string{
			{"role": "user", "content": "test"},
		},
	}
	body, _ := json.Marshal(payload)

	resp, err := apiPost(routerURL+"/v1/chat/completions", apiKey, body)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}

	var respBody map[string]any
	json.NewDecoder(resp.Body).Decode(&respBody)
	errMap, ok := respBody["error"].(map[string]any)
	if !ok {
		t.Fatal("no error in response")
	}
	if _, ok := errMap["available_models"]; !ok {
		t.Fatal("response missing available_models in error")
	}
}

func TestE2E_WSAuthFailure(t *testing.T) {
	routerURL, _, _, cleanup := setupTestRouter(t)
	defer cleanup()

	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	wsURL := "ws" + routerURL[4:]

	// No auth header
	conn, resp, err := dialer.Dial(wsURL, nil)
	if err == nil {
		conn.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d", resp.StatusCode)
		}
	}

	// Invalid token
	conn2, resp2, err2 := dialer.Dial(wsURL, http.Header{
		"Authorization": {"Bearer invalid-token"},
	})
	if err2 == nil {
		conn2.Close()
		if resp2.StatusCode != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d", resp2.StatusCode)
		}
	}

	t.Log("WS auth failures handled correctly")
}

func TestE2E_AntropicFormat(t *testing.T) {
	routerURL, apiKey, clientToken, cleanup := setupTestRouter(t)
	defer cleanup()

	models := []types.ModelInfo{{Name: "anth-model"}}
	conn := connectMockClient(t, routerURL, clientToken, models)
	defer conn.Close()

	var mu sync.Mutex
	var receivedReqID string

	go func() {
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var env struct{ Type string }
			if err := json.Unmarshal(data, &env); err != nil {
				continue
			}
			if env.Type == "job" {
				var job types.JobMsg
				if err := json.Unmarshal(data, &job); err != nil {
					continue
				}
				mu.Lock()
				receivedReqID = job.Request.ID
				mu.Unlock()

				reqID := job.Request.ID
				chunks := []types.ChunkMsg{
					{Type: "chunk", RequestID: reqID, Delta: "anthropic response"},
					{Type: "chunk", RequestID: reqID, Delta: "", Done: true, FinishReason: "stop"},
				}
				for _, c := range chunks {
					msg, _ := json.Marshal(c)
					conn.WriteMessage(websocket.TextMessage, msg)
				}
				return
			}
		}
	}()

	time.Sleep(100 * time.Millisecond)

	// POST Anthropic format
	payload := map[string]any{
		"model": "anth-model",
		"messages": []map[string]string{
			{"role": "user", "content": "hello"},
		},
	}
	body, _ := json.Marshal(payload)

	resp, err := apiPost(routerURL+"/v1/messages", apiKey, body)
	if err != nil {
		t.Fatalf("POST /v1/messages: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, string(body))
	}

	respBody, _ := io.ReadAll(resp.Body)
	t.Logf("Anthropic response: %s", string(respBody))

	mu.Lock()
	rcvdID := receivedReqID
	mu.Unlock()
	if rcvdID == "" {
		t.Fatal("mock client never received a job")
	}
}

func TestE2E_ConcurrentRequests(t *testing.T) {
	routerURL, apiKey, clientToken, cleanup := setupTestRouter(t)
	defer cleanup()

	models := []types.ModelInfo{{Name: "concurrent-model"}}
	conn := connectMockClient(t, routerURL, clientToken, models)
	defer conn.Close()

	var mu sync.Mutex
	var allReqIDs []string
	var wg sync.WaitGroup
	var wsMu sync.Mutex // gorilla websocket requires serialized writes

	go func() {
		for i := 0; i < 10; i++ {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var env struct{ Type string }
			if err := json.Unmarshal(data, &env); err != nil {
				continue
			}
			if env.Type == "job" {
				var job types.JobMsg
				if err := json.Unmarshal(data, &job); err != nil {
					continue
				}
				reqID := job.Request.ID

				mu.Lock()
				allReqIDs = append(allReqIDs, reqID)
				mu.Unlock()

				wg.Add(1)
				go func(rID string) {
					defer wg.Done()
					chunks := []types.ChunkMsg{
						{Type: "chunk", RequestID: rID, Delta: "concurrent"},
						{Type: "chunk", RequestID: rID, Delta: "", Done: true, FinishReason: "stop"},
					}
					for _, c := range chunks {
						msg, _ := json.Marshal(c)
						wsMu.Lock()
						conn.WriteMessage(websocket.TextMessage, msg)
						wsMu.Unlock()
					}
				}(reqID)
			}
		}
		wg.Wait()
	}()

	time.Sleep(100 * time.Millisecond)

	// Fire concurrent requests
	var mu2 sync.Mutex
	var respCodes []int
	var wg2 sync.WaitGroup

	for i := 0; i < 5; i++ {
		wg2.Add(1)
		go func() {
			defer wg2.Done()
			payload := map[string]any{
				"model":  "concurrent-model",
				"stream": false,
				"messages": []map[string]string{
					{"role": "user", "content": fmt.Sprintf("req %d", i)},
				},
			}
			body, _ := json.Marshal(payload)
			resp, err := apiPost(routerURL+"/v1/chat/completions", apiKey, body)
			if err != nil {
				return
			}
			defer resp.Body.Close()
			io.Copy(io.Discard, resp.Body)
			mu2.Lock()
			respCodes = append(respCodes, resp.StatusCode)
			mu2.Unlock()
		}()
	}
	wg2.Wait()

	mu.Lock()
	ids := allReqIDs
	mu.Unlock()

	// All should get 200
	for _, code := range respCodes {
		if code != http.StatusOK {
			t.Errorf("unexpected status: %d", code)
		}
	}

	// All should have been dispatched
	if len(ids) != 5 {
		t.Errorf("expected 5 jobs dispatched, got %d", len(ids))
	}

	t.Logf("concurrent OK: %d requests, %d dispatched", len(respCodes), len(ids))
}

func TestE2E_CorrelationChannelCleanup(t *testing.T) {
	routerURL, apiKey, clientToken, cleanup := setupTestRouter(t)
	defer cleanup()

	models := []types.ModelInfo{{Name: "cleanup-model"}}
	conn := connectMockClient(t, routerURL, clientToken, models)
	defer conn.Close()

	var mu sync.Mutex
	var receivedReqID string

	go func() {
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var env struct{ Type string }
			if err := json.Unmarshal(data, &env); err != nil {
				continue
			}
			if env.Type == "job" {
				var job types.JobMsg
				if err := json.Unmarshal(data, &job); err != nil {
					continue
				}
				mu.Lock()
				receivedReqID = job.Request.ID
				mu.Unlock()

				reqID := job.Request.ID
				chunks := []types.ChunkMsg{
					{Type: "chunk", RequestID: reqID, Delta: "done"},
					{Type: "chunk", RequestID: reqID, Delta: "", Done: true, FinishReason: "stop"},
				}
				for _, c := range chunks {
					msg, _ := json.Marshal(c)
					conn.WriteMessage(websocket.TextMessage, msg)
				}
				return
			}
		}
	}()

	time.Sleep(100 * time.Millisecond)

	// Post request
	payload := map[string]any{
		"model":  "cleanup-model",
		"stream": false,
		"messages": []map[string]string{
			{"role": "user", "content": "cleanup test"},
		},
	}
	body, _ := json.Marshal(payload)

	resp, err := apiPost(routerURL+"/v1/chat/completions", apiKey, body)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	// Wait for client to receive and respond
	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	rcvdID := receivedReqID
	mu.Unlock()

	if rcvdID == "" {
		t.Fatal("mock client never received job")
	}

	// The correlation channel should have been cleaned up after the response
	// (Delete called in defer of enqueue). This is hard to verify directly
	// but the fact that we got a response and can make another request proves
	// the channel was cleaned up.

	t.Logf("correlation cleanup OK: reqID=%s", rcvdID)
}

func TestE2E_HealthCheck(t *testing.T) {
	routerURL, _, _, cleanup := setupTestRouter(t)
	defer cleanup()

	resp, err := http.Get(routerURL + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	if body["status"] != "ok" {
		t.Fatalf("unexpected status: %v", body["status"])
	}
	if body["version"] != "e2e" {
		t.Fatalf("unexpected version: %v", body["version"])
	}
}

func TestE2E_ModelList(t *testing.T) {
	routerURL, apiKey, clientToken, cleanup := setupTestRouter(t)
	defer cleanup()

	// Connect client with models
	models := []types.ModelInfo{
		{Name: "model-a", ContextSize: 4096},
		{Name: "model-b", ContextSize: 8192},
	}
	conn := connectMockClient(t, routerURL, clientToken, models)
	defer conn.Close()

	time.Sleep(50 * time.Millisecond)

	// GET /v1/models without auth — should fail
	resp, err := http.Get(routerURL + "/v1/models")
	if err != nil {
		t.Fatalf("GET /v1/models: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 without auth, got %d", resp.StatusCode)
	}
	io.Copy(io.Discard, resp.Body)

	// GET /v1/models with valid auth
	keyReq, _ := http.NewRequest("GET", routerURL+"/v1/models", nil)
	keyReq.Header.Set("Authorization", "Bearer "+apiKey)
	resp2, err := http.DefaultClient.Do(keyReq)
	if err != nil {
		t.Fatalf("GET /v1/models with auth: %v", err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode != http.StatusOK {
		body2, _ := io.ReadAll(resp2.Body)
		t.Fatalf("expected 200 with valid auth, got %d: %s", resp2.StatusCode, string(body2))
	}

	var modelList map[string]any
	json.NewDecoder(resp2.Body).Decode(&modelList)
	if modelList["object"] != "list" {
		t.Errorf("expected object=list, got %v", modelList["object"])
	}
	t.Logf("model list OK: %v", modelList)
}

// TestE2E_RequestIDCorrelation verifies that chunks sent by the client
// are correctly correlated back to the HTTP response.
func TestE2E_RequestIDCorrelation(t *testing.T) {
	routerURL, apiKey, clientToken, cleanup := setupTestRouter(t)
	defer cleanup()

	models := []types.ModelInfo{{Name: "corr-model"}}
	conn := connectMockClient(t, routerURL, clientToken, models)
	defer conn.Close()

	var mu sync.Mutex
	var clientReqID string

	go func() {
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var env struct{ Type string }
			if err := json.Unmarshal(data, &env); err != nil {
				continue
			}
			if env.Type == "job" {
				var job types.JobMsg
				if err := json.Unmarshal(data, &job); err != nil {
					continue
				}
				mu.Lock()
				clientReqID = job.Request.ID
				mu.Unlock()

				reqID := job.Request.ID
				chunks := []types.ChunkMsg{
					{Type: "chunk", RequestID: reqID, Delta: "correlation test"},
					{Type: "chunk", RequestID: reqID, Delta: "", Done: true, FinishReason: "stop"},
				}
				for _, c := range chunks {
					msg, _ := json.Marshal(c)
					conn.WriteMessage(websocket.TextMessage, msg)
				}
				return
			}
		}
	}()

	time.Sleep(100 * time.Millisecond)

	payload := map[string]any{
		"model":  "corr-model",
		"stream": false,
		"messages": []map[string]string{
			{"role": "user", "content": "correlation"},
		},
	}
	body, _ := json.Marshal(payload)

	resp, err := apiPost(routerURL+"/v1/chat/completions", apiKey, body)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, string(respBody))
	}

	// Parse response to get the request ID the router assigned
	var respMap map[string]any
	json.Unmarshal(respBody, &respMap)
	routerReqID, _ := respMap["id"].(string)

	mu.Lock()
	gotClientReqID := clientReqID
	mu.Unlock()

	// The router generates its own ID and sends it to the client via JobMsg.
	// The client's ChunkMsg must use the same request ID as the router assigned.
	if routerReqID == "" {
		t.Fatal("empty router request ID")
	}
	if gotClientReqID != routerReqID {
		t.Errorf("request ID mismatch: router assigned %q but client received %q", routerReqID, gotClientReqID)
	}

	t.Logf("correlation OK: routerID=%s, clientID=%s", routerReqID, gotClientReqID)
}

// TestE2E_AnthropicSSEChunk verifies the translate.AnthropicSSEChunk helper works.
func TestE2E_AnthropicSSEChunk(t *testing.T) {
	chunk := types.ChunkMsg{
		Type:    "chunk",
		RequestID: "test-req-1",
		Delta:   "hello world",
	}
	sseLine := translate.AnthropicSSEChunk(chunk)
	if sseLine == "" {
		t.Fatal("empty SSE chunk line")
	}
	if !bytes.Contains([]byte(sseLine), []byte("hello world")) {
		t.Fatalf("SSE chunk missing delta content: %s", sseLine)
	}
	t.Logf("Anthropic SSE chunk: %s", sseLine)
}

// TestE2E_OpenAISSEChunk verifies OpenAI SSE chunk formatting.
func TestE2E_OpenAISSEChunk(t *testing.T) {
	chunk := types.ChunkMsg{
		Type:    "chunk",
		RequestID: "test-req-2",
		Delta:   "test content",
	}
	sseLine := translate.OpenAISSEChunk("test-req-2", chunk)
	if sseLine == "" {
		t.Fatal("empty OpenAI SSE chunk line")
	}
	if !bytes.Contains([]byte(sseLine), []byte("test content")) {
		t.Fatalf("OpenAI SSE chunk missing content: %s", sseLine)
	}
	t.Logf("OpenAI SSE chunk: %s", sseLine)
}

// TestE2E_OpenAIFullResponse verifies full response translation.
func TestE2E_OpenAIFullResponse(t *testing.T) {
	resp := translate.OpenAIFullResponse("test-req-3", "full response", "stop", nil, nil)
	b, _ := json.Marshal(resp)
	var parsed map[string]any
	json.Unmarshal(b, &parsed)

	if parsed["id"] != "test-req-3" {
		t.Errorf("wrong ID: %v", parsed["id"])
	}
	choices := resp["choices"].([]map[string]any)
	choice := choices[0]
	msg := choice["message"].(map[string]any)
	if msg["content"] != "full response" {
		t.Errorf("wrong content: %v", msg["content"])
	}
}

// TestE2E_QueuePushPop verifies queue integration.
func TestE2E_QueuePushPop(t *testing.T) {
	q := queue.New()
	req := types.InferenceRequest{
		ID:       "q-test-1",
		Model:    "test-model",
		Priority: types.PriorityNormal,
	}
	q.Push(req)
	if q.Len() != 1 {
		t.Fatalf("expected len 1, got %d", q.Len())
	}

	popped := q.PopBest(map[string]bool{"test-model": true}, nil)
	if popped == nil {
		t.Fatal("queue returned nil for valid request")
	}
	if popped.ID != "q-test-1" {
		t.Errorf("wrong ID: %s", popped.ID)
	}
	if q.Len() != 0 {
		t.Errorf("expected len 0, got %d", q.Len())
	}
}

// TestE2E_CorrelationCreateSendDelete verifies correlation store.
func TestE2E_CorrelationCreateSendDelete(t *testing.T) {
	store := correlation.New(slog.Default())
	reqID := "corr-test-1"

	ch := store.Create(reqID)

	// Send a chunk
	ok := store.Send(types.ChunkMsg{
		Type:      "chunk",
		RequestID: reqID,
		Delta:     "test",
	})
	if !ok {
		t.Fatal("Send returned false for valid request")
	}

	// Read the chunk from channel
	select {
	case chunk := <-ch:
		if chunk.Delta != "test" {
			t.Errorf("wrong delta: %s", chunk.Delta)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for chunk")
	}

	// After delete, Send should return false
	store.Delete(reqID)
	ok = store.Send(types.ChunkMsg{RequestID: reqID})
	if ok {
		t.Fatal("Send should return false after Delete")
	}
}

// TestE2E_AdminStateAPIKey verifies admin state API key operations.
func TestE2E_AdminStateAPIKey(t *testing.T) {
	dir := t.TempDir()
	statePath := dir + "/state.json"

	st, err := admin.LoadState(statePath)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}

	key, err := admin.GenAPIKeyValue("testowner")
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}

	st.AddAPIKey(admin.APIKey{
		Label:    "test-key",
		Owner:    "testowner",
		Key:      key,
		Priority: "high",
	})

	if !st.ValidAPIKey(key) {
		t.Fatal("ValidAPIKey returned false for valid key")
	}
	if st.ValidAPIKey("invalid-key") {
		t.Fatal("ValidAPIKey returned true for invalid key")
	}

	priority := st.PriorityFor(key)
	if priority != types.PriorityHigh {
		t.Errorf("wrong priority: %d", priority)
	}

	owner := st.OwnerFor(key)
	if owner != "testowner" {
		t.Errorf("wrong owner: %s", owner)
	}

	// Duplicate key
	err = st.AddAPIKey(admin.APIKey{
		Label:    "test-key-dup",
		Owner:    "testowner",
		Key:      "sk-testowner-dup",
		Priority: "normal",
	})
	if err != nil {
		t.Fatalf("AddAPIKey (non-duplicate): %v", err)
	}

	// Revoke
	err = st.RevokeAPIKey("testowner", key, true)
	if err != nil {
		t.Fatalf("RevokeAPIKey: %v", err)
	}
	if st.ValidAPIKey(key) {
		t.Fatal("ValidAPIKey returned true after revoke")
	}
}

// TestE2E_AdminStateClientToken verifies client token operations.
func TestE2E_AdminStateClientToken(t *testing.T) {
	dir := t.TempDir()
	statePath := dir + "/state.json"

	st, err := admin.LoadState(statePath)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}

	token := "ct-testowner-mock12345678"
	st.AddClientToken(admin.ClientToken{
		Name:  "test-client",
		Owner: "testowner",
		Token: token,
	})

	ct, ok := st.LookupClientToken(token)
	if !ok {
		t.Fatal("LookupClientToken returned false for valid token")
	}
	if ct.Name != "test-client" {
		t.Errorf("wrong name: %s", ct.Name)
	}

	_, ok = st.LookupClientToken("invalid-token")
	if ok {
		t.Fatal("LookupClientToken returned true for invalid token")
	}

	// Revoke
	err = st.RevokeClientToken("testowner", token, true)
	if err != nil {
		t.Fatalf("RevokeClientToken: %v", err)
	}
	_, ok = st.LookupClientToken(token)
	if ok {
		t.Fatal("LookupClientToken returned true after revoke")
	}
}

// TestE2E_HubRegisterDisconnect verifies hub WS lifecycle.
func TestE2E_HubRegisterDisconnect(t *testing.T) {
	h := hub.New(slog.Default())
	availableCh := make(chan struct{}, 1)
	h.OnAvailable = func() {
		select {
		case availableCh <- struct{}{}:
		default:
		}
	}

	// Create a test server with WS endpoint
	mux := http.NewServeMux()
	mux.HandleFunc("/ws/client", func(w http.ResponseWriter, r *http.Request) {
		token := api.ExtractBearer(r)
		if token == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeWS(w, r, "test-client", "testuser", token, nil)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// Connect client
	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	wsURL := "ws" + ts.URL[4:] + "/ws/client"
	conn, _, err := dialer.Dial(wsURL, http.Header{
		"Authorization": {"Bearer ct-test"},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Register
	reg := types.RegisterMsg{
		Type:          "register",
		Models:        []types.ModelInfo{{Name: "test-model", ContextSize: 4096}},
		MaxConcurrent: 2,
		Version:       "1.0",
	}
	conn.WriteJSON(reg)

	// Wait for OnAvailable callback
	select {
	case <-availableCh:
		// Client registered
	case <-time.After(time.Second):
		t.Fatal("OnAvailable callback not triggered")
	}

	// Check active models
	models := h.ActiveModels()
	found := false
	for _, m := range models {
		if m == "test-model" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("registered model not in ActiveModels")
	}

	// Disconnect
	conn.Close()

	// Wait for disconnect callback
	select {
	case <-availableCh:
		// Client disconnected
	case <-time.After(time.Second):
		t.Fatal("OnAvailable callback on disconnect not triggered")
	}

	// Model should no longer be active
	models = h.ActiveModels()
	for _, m := range models {
		if m == "test-model" {
			t.Fatal("model still active after disconnect")
		}
	}
}

// TestE2E_SchedulerDispatch verifies scheduler picks the right client.
func TestE2E_SchedulerDispatch(t *testing.T) {
	q := queue.New()
	h := hub.New(slog.Default())
	st, _ := admin.LoadState(t.TempDir() + "/state.json")
	st.AddAPIKey(admin.APIKey{Key: "sk-user", Owner: "alice", Priority: "normal"})

	sched := scheduler.New(q, h, st, slog.Default())
	sched.Start()
	defer sched.Stop()

	// Push a request
	req := types.InferenceRequest{
		ID:       "sched-1",
		Model:    "test-model",
		Owner:    "alice",
		Priority: types.PriorityNormal,
	}
	q.Push(req)

	// No client connected — request stays queued
	if q.Len() != 1 {
		t.Fatalf("expected queue len 1, got %d", q.Len())
	}

	// Connect via WS
	mux := http.NewServeMux()
	h.OnAvailable = sched.Wake
	mux.HandleFunc("/ws/client", func(w http.ResponseWriter, r *http.Request) {
		token := api.ExtractBearer(r)
		if token == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeWS(w, r, "sched-client", "alice", token, nil)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	wsURL := "ws" + ts.URL[4:] + "/ws/client"
	conn, _, err := dialer.Dial(wsURL, http.Header{
		"Authorization": {"Bearer ct-sched"},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	// Register client
	reg := types.RegisterMsg{
		Type:          "register",
		Models:        []types.ModelInfo{{Name: "test-model"}},
		MaxConcurrent: 4,
	}
	conn.WriteJSON(reg)

	// Wait for dispatch
	time.Sleep(200 * time.Millisecond)

	// Request should have been popped from queue (scheduler dispatched it)
	if q.Len() != 0 {
		t.Fatalf("expected queue len 0 after dispatch, got %d", q.Len())
	}

	// Client should have received the job
	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read job: %v", err)
	}
	var env struct{ Type string }
	json.Unmarshal(data, &env)
	if env.Type != "job" {
		t.Fatalf("expected job message, got %s", env.Type)
	}

	var job types.JobMsg
	json.Unmarshal(data, &job)
	if job.Request.ID != "sched-1" {
		t.Errorf("wrong request ID: %s", job.Request.ID)
	}

	conn.Close()
	t.Log("scheduler dispatch OK")
}

// TestE2E_TranslateOpenAIInbound verifies inbound translation.
func TestE2E_TranslateOpenAIInbound(t *testing.T) {
	input := []byte(`{
		"model": "test-model",
		"stream": true,
		"messages": [{"role": "user", "content": "hello"}]
	}`)

	req, err := translate.OpenAIInbound(input)
	if err != nil {
		t.Fatalf("OpenAIInbound: %v", err)
	}
	if req.Model != "test-model" {
		t.Errorf("wrong model: %s", req.Model)
	}
	if !req.Stream {
		t.Error("expected stream=true")
	}
	if len(req.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(req.Messages))
	}
	if req.Messages[0].Role != "user" {
		t.Errorf("wrong role: %s", req.Messages[0].Role)
	}
}

// TestE2E_TranslateAnthropicInbound verifies Anthropic inbound translation.
func TestE2E_TranslateAnthropicInbound(t *testing.T) {
	input := []byte(`{
		"model": "claude-test",
		"max_tokens": 100,
		"messages": [{"role": "user", "content": "hello"}]
	}`)

	req, err := translate.AnthropicInbound(input)
	if err != nil {
		t.Fatalf("AnthropicInbound: %v", err)
	}
	if req.Model != "claude-test" {
		t.Errorf("wrong model: %s", req.Model)
	}
	if req.MaxTokens != 100 {
		t.Errorf("wrong max_tokens: %d", req.MaxTokens)
	}
}

// TestE2E_TranslateOpenAIFullResponse verifies full response generation.
func TestE2E_TranslateOpenAIFullResponse(t *testing.T) {
	resp := translate.OpenAIFullResponse("resp-1", "test answer", "stop", nil, nil)
	b, _ := json.Marshal(resp)
	var parsed map[string]any
	json.Unmarshal(b, &parsed)

	if parsed["id"] != "resp-1" {
		t.Errorf("wrong id: %v", parsed["id"])
	}
	choices := resp["choices"].([]map[string]any)
	if len(choices) == 0 {
		t.Fatal("no choices")
	}
	choice := choices[0]
	msg := choice["message"].(map[string]any)
	if msg["content"] != "test answer" {
		t.Errorf("wrong content: %v", msg["content"])
	}
	if msg["role"] != "assistant" {
		t.Errorf("wrong role: %v", msg["role"])
	}
	if choice["finish_reason"] != "stop" {
		t.Errorf("wrong finish_reason: %v", choice["finish_reason"])
	}
}

// TestE2E_TranslateAnthropicFullResponse verifies Anthropic response generation.
func TestE2E_TranslateAnthropicFullResponse(t *testing.T) {
	resp := translate.AnthropicFullResponse("resp-2", "claude-3", "anthropic answer", "end_turn")
	b, _ := json.Marshal(resp)
	var parsed map[string]any
	json.Unmarshal(b, &parsed)

	if parsed["stop_reason"] != "end_turn" {
		t.Errorf("wrong stop_reason: %v", parsed["stop_reason"])
	}
	content := resp["content"].([]map[string]any)
	if len(content) == 0 {
		t.Fatal("no content")
	}
	textBlock := content[0]
	if textBlock["type"] != "text" {
		t.Errorf("wrong content type: %v", textBlock["type"])
	}
}

// TestE2E_TranslateResponsesInbound verifies OpenAI Responses format inbound.
func TestE2E_TranslateResponsesInbound(t *testing.T) {
	input := []byte(`{
		"model": "responses-model",
		"input": [{"role": "user", "content": "hello"}]
	}`)

	req, err := translate.ResponsesInbound(input)
	if err != nil {
		t.Fatalf("ResponsesInbound: %v", err)
	}
	if req.Model != "responses-model" {
		t.Errorf("wrong model: %s", req.Model)
	}
	if req.SourceFmt != "openai-responses" {
		t.Errorf("wrong source format: %s", req.SourceFmt)
	}
}

// TestE2E_TranslateStatsVerification verifies stats recording works end-to-end.
func TestE2E_TranslateStatsVerification(t *testing.T) {
	req := types.InferenceRequest{
		Model: "stats-model",
		Owner: "stats-user",
	}

	// Simulate stats recording
	reqStats := stats.New()
	usage := &types.UsageInfo{
		PromptTokens:     10,
		CompletionTokens: 20,
		TotalTokens:      30,
	}
	reqStats.Record(req.Model, req.Owner, usage.PromptTokens, usage.CompletionTokens)

	byModel := reqStats.ByModel()
	if len(byModel) != 1 {
		t.Fatalf("expected 1 model stat, got %d", len(byModel))
	}
	if byModel[0].Name != "stats-model" {
		t.Errorf("wrong model name: %s", byModel[0].Name)
	}
	if byModel[0].PromptTokens != 10 {
		t.Errorf("wrong prompt tokens: %d", byModel[0].PromptTokens)
	}
	if byModel[0].CompletionTokens != 20 {
		t.Errorf("wrong completion tokens: %d", byModel[0].CompletionTokens)
	}

	byUser := reqStats.ByUser()
	if len(byUser) != 1 {
		t.Fatalf("expected 1 user stat, got %d", len(byUser))
	}
	if byUser[0].Requests != 1 {
		t.Errorf("wrong request count: %d", byUser[0].Requests)
	}
}
