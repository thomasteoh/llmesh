# llmesh Admin UI Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a web management console to the llmesh router for API key, client token, and user management — all behind a login wall, embedded in the router binary.

**Architecture:** A new `router/internal/admin` package serves the `/admin/` path prefix. Mutable state (users, API keys, client tokens) moves from `config.yaml` to a `state.json` file managed through the UI. The hub is extended to track per-client token/name/owner and last-seen time.

**Tech Stack:** Go `html/template` with `embed.FS`, bcrypt (`golang.org/x/crypto/bcrypt`), vanilla JS, no build step.

---

## File Map

**New files:**
- `router/internal/admin/state.go` — State struct, load/save, CRUD, token generation
- `router/internal/admin/state_test.go` — unit tests for state
- `router/internal/admin/auth.go` — session store, login/logout/setup handlers, requireAuth middleware
- `router/internal/admin/auth_test.go` — unit tests for auth
- `router/internal/admin/pages.go` — page handlers (dashboard, api-keys, client-tokens, docs, settings)
- `router/internal/admin/api.go` — JSON endpoint for dashboard auto-refresh
- `router/internal/admin/handler.go` — Admin struct, New(), ServeHTTP, template setup
- `router/internal/admin/templates/layout.html` — base layout with top nav
- `router/internal/admin/templates/login.html` — standalone login form
- `router/internal/admin/templates/setup.html` — standalone first-run setup form
- `router/internal/admin/templates/dashboard.html` — dashboard content block
- `router/internal/admin/templates/api-keys.html` — API keys content block
- `router/internal/admin/templates/client-tokens.html` — client tokens content block
- `router/internal/admin/templates/docs.html` — docs content block
- `router/internal/admin/templates/settings.html` — settings content block
- `router/internal/admin/static/admin.css` — dark theme CSS

**Modified files:**
- `router/internal/hub/hub.go` — add Name/Owner/Token/lastSeen, update ServeWS signature, add CloseByToken/IsConnected/LastSeenTime/ConnectedModels/ActiveClientCount
- `router/internal/api/handler.go` — introduce APIKeyStore interface, replace Config-based key lookup, add RequestCount
- `router/cmd/router/main.go` — load state.json, create admin handler, update /ws/client auth, register /admin/
- `router/config.go` — remove APIKeys/APIKeyConfig/related methods, remove ClientToken from Server
- `router/config.yaml` — remove api_keys section and server.client_token

---

### Task 1: Add bcrypt dependency

**Files:**
- Modify: `go.mod`, `go.sum`

- [ ] **Step 1: Add the dependency**

```bash
cd /home/tteoh/llmesh && go get golang.org/x/crypto@latest
```

Expected: `go.mod` gains `golang.org/x/crypto` line.

- [ ] **Step 2: Verify build still passes**

```bash
cd /home/tteoh/llmesh && go build ./...
```

Expected: no errors.

- [ ] **Step 3: Commit**

```bash
git add go.mod go.sum
git commit -m "chore: add golang.org/x/crypto for bcrypt"
```

---

### Task 2: Admin state package

**Files:**
- Create: `router/internal/admin/state.go`
- Create: `router/internal/admin/state_test.go`

- [ ] **Step 1: Write the failing tests**

Create `router/internal/admin/state_test.go`:

```go
package admin

import (
	"os"
	"path/filepath"
	"testing"

	"llmesh/pkg/types"
)

func TestNeedsSetup_Empty(t *testing.T) {
	s, err := LoadState(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !s.NeedsSetup() {
		t.Fatal("expected NeedsSetup=true for fresh state")
	}
}

func TestAddUser_LookupUser(t *testing.T) {
	f := filepath.Join(t.TempDir(), "state.json")
	s, _ := LoadState(f)
	if err := s.AddUser(User{Username: "alice", PasswordHash: "h", Role: "admin"}); err != nil {
		t.Fatal(err)
	}
	if s.NeedsSetup() {
		t.Fatal("expected NeedsSetup=false after AddUser")
	}
	u, ok := s.LookupUser("alice")
	if !ok || u.Role != "admin" {
		t.Fatalf("got %+v ok=%v", u, ok)
	}
	// persists across reload
	s2, _ := LoadState(f)
	if _, ok := s2.LookupUser("alice"); !ok {
		t.Fatal("user not persisted")
	}
}

func TestLookupUser_NotFound(t *testing.T) {
	s, _ := LoadState(filepath.Join(t.TempDir(), "state.json"))
	_, ok := s.LookupUser("nobody")
	if ok {
		t.Fatal("expected not found")
	}
}

func TestActiveAdminCount(t *testing.T) {
	s, _ := LoadState(filepath.Join(t.TempDir(), "state.json"))
	s.AddUser(User{Username: "a", Role: "admin", Disabled: false})
	s.AddUser(User{Username: "b", Role: "admin", Disabled: true})
	s.AddUser(User{Username: "c", Role: "member", Disabled: false})
	if n := s.ActiveAdminCount(); n != 1 {
		t.Fatalf("want 1, got %d", n)
	}
}

func TestAPIKey_AddRevoke(t *testing.T) {
	s, _ := LoadState(filepath.Join(t.TempDir(), "state.json"))
	k := APIKey{Label: "prod", Owner: "alice", Key: "sk-alice-abc123", Priority: "high"}
	if err := s.AddAPIKey(k); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.LookupAPIKey("sk-alice-abc123"); !ok {
		t.Fatal("key not found after add")
	}
	if err := s.RevokeAPIKey("alice", "sk-alice-abc123", false); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.LookupAPIKey("sk-alice-abc123"); ok {
		t.Fatal("key found after revoke")
	}
}

func TestAPIKey_LabelUniqueness(t *testing.T) {
	s, _ := LoadState(filepath.Join(t.TempDir(), "state.json"))
	s.AddAPIKey(APIKey{Label: "dev", Owner: "alice", Key: "sk-alice-1"})
	if err := s.AddAPIKey(APIKey{Label: "dev", Owner: "alice", Key: "sk-alice-2"}); err == nil {
		t.Fatal("expected error for duplicate label")
	}
	// different owner OK
	if err := s.AddAPIKey(APIKey{Label: "dev", Owner: "bob", Key: "sk-bob-1"}); err != nil {
		t.Fatalf("different owner should be allowed: %v", err)
	}
}

func TestClientToken_AddRevoke(t *testing.T) {
	s, _ := LoadState(filepath.Join(t.TempDir(), "state.json"))
	tok := ClientToken{Name: "mac", Owner: "alice", Token: "ct-alice-abc123"}
	s.AddClientToken(tok)
	if _, ok := s.LookupClientToken("ct-alice-abc123"); !ok {
		t.Fatal("token not found after add")
	}
	s.RevokeClientToken("alice", "ct-alice-abc123", false)
	if _, ok := s.LookupClientToken("ct-alice-abc123"); ok {
		t.Fatal("token found after revoke")
	}
}

func TestValidAPIKey(t *testing.T) {
	s, _ := LoadState(filepath.Join(t.TempDir(), "state.json"))
	s.AddAPIKey(APIKey{Label: "x", Owner: "alice", Key: "sk-alice-xyz"})
	if !s.ValidAPIKey("sk-alice-xyz") {
		t.Fatal("expected valid")
	}
	if s.ValidAPIKey("sk-unknown") {
		t.Fatal("expected invalid")
	}
}

func TestPriorityFor(t *testing.T) {
	s, _ := LoadState(filepath.Join(t.TempDir(), "state.json"))
	s.AddAPIKey(APIKey{Label: "hi", Owner: "a", Key: "sk-a-hi", Priority: "high"})
	if p := s.PriorityFor("sk-a-hi"); p != types.PriorityHigh {
		t.Fatalf("want PriorityHigh, got %v", p)
	}
	if p := s.PriorityFor("sk-unknown"); p != types.PriorityNormal {
		t.Fatalf("want PriorityNormal for unknown, got %v", p)
	}
}

func TestGenAPIKeyValue(t *testing.T) {
	k, err := GenAPIKeyValue("alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(k) < 10 || k[:9] != "sk-alice-" {
		t.Fatalf("unexpected key format: %s", k)
	}
}
```

- [ ] **Step 2: Run tests to confirm they fail**

```bash
cd /home/tteoh/llmesh && go test ./router/internal/admin/... 2>&1 | head -5
```

Expected: `cannot find package` or compile error (package doesn't exist yet).

- [ ] **Step 3: Implement state.go**

Create `router/internal/admin/state.go`:

```go
package admin

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"llmesh/pkg/types"
)

type User struct {
	Username     string `json:"username"`
	PasswordHash string `json:"password_hash"`
	Role         string `json:"role"`     // "admin" | "member"
	Disabled     bool   `json:"disabled"`
}

type APIKey struct {
	Label     string    `json:"label"`
	Owner     string    `json:"owner"`
	Key       string    `json:"key"`
	Priority  string    `json:"priority"` // "high" | "normal" | "low"
	CreatedAt time.Time `json:"created_at"`
}

type ClientToken struct {
	Name      string    `json:"name"`
	Owner     string    `json:"owner"`
	Token     string    `json:"token"`
	CreatedAt time.Time `json:"created_at"`
}

type stateData struct {
	Users        []User        `json:"users"`
	APIKeys      []APIKey      `json:"api_keys"`
	ClientTokens []ClientToken `json:"client_tokens"`
}

// State is the mutable runtime state, persisted to state.json.
type State struct {
	mu   sync.RWMutex
	path string
	data stateData
}

// LoadState loads state from path. Returns empty state if file does not exist.
func LoadState(path string) (*State, error) {
	s := &State{path: path}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state: %w", err)
	}
	if err := json.Unmarshal(data, &s.data); err != nil {
		return nil, fmt.Errorf("parse state: %w", err)
	}
	return s, nil
}

// save writes state to disk. Caller must hold write lock.
func (s *State) save() error {
	data, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0600)
}

func (s *State) NeedsSetup() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.data.Users) == 0
}

// --- Users ---

func (s *State) LookupUser(username string) (User, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, u := range s.data.Users {
		if u.Username == username {
			return u, true
		}
	}
	return User{}, false
}

func (s *State) AddUser(u User) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Users = append(s.data.Users, u)
	return s.save()
}

// UpdateUser applies fn to the named user and saves. Returns error if not found.
func (s *State) UpdateUser(username string, fn func(*User)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.Users {
		if s.data.Users[i].Username == username {
			fn(&s.data.Users[i])
			return s.save()
		}
	}
	return fmt.Errorf("user not found: %s", username)
}

func (s *State) Users() []User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]User, len(s.data.Users))
	copy(out, s.data.Users)
	return out
}

func (s *State) ActiveAdminCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, u := range s.data.Users {
		if u.Role == "admin" && !u.Disabled {
			n++
		}
	}
	return n
}

// --- API Keys ---

func (s *State) LookupAPIKey(key string) (APIKey, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, k := range s.data.APIKeys {
		if k.Key == key {
			return k, true
		}
	}
	return APIKey{}, false
}

// APIKeysFor returns keys visible to owner. Admins (isAdmin=true) see all keys.
func (s *State) APIKeysFor(owner string, isAdmin bool) []APIKey {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []APIKey
	for _, k := range s.data.APIKeys {
		if isAdmin || k.Owner == owner {
			out = append(out, k)
		}
	}
	return out
}

func (s *State) AddAPIKey(k APIKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.data.APIKeys {
		if existing.Owner == k.Owner && existing.Label == k.Label {
			return fmt.Errorf("label %q already exists for this user", k.Label)
		}
	}
	s.data.APIKeys = append(s.data.APIKeys, k)
	return s.save()
}

// RevokeAPIKey removes the key. Non-admins can only revoke their own keys.
func (s *State) RevokeAPIKey(owner, key string, isAdmin bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, k := range s.data.APIKeys {
		if k.Key == key && (isAdmin || k.Owner == owner) {
			s.data.APIKeys = append(s.data.APIKeys[:i], s.data.APIKeys[i+1:]...)
			return s.save()
		}
	}
	return fmt.Errorf("key not found")
}

func (s *State) APIKeyCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.data.APIKeys)
}

// ValidAPIKey satisfies the api.APIKeyStore interface.
func (s *State) ValidAPIKey(key string) bool {
	_, ok := s.LookupAPIKey(key)
	return ok
}

// PriorityFor satisfies the api.APIKeyStore interface.
func (s *State) PriorityFor(key string) types.Priority {
	k, ok := s.LookupAPIKey(key)
	if !ok {
		return types.PriorityNormal
	}
	return types.PriorityFromString(k.Priority)
}

// --- Client Tokens ---

func (s *State) LookupClientToken(token string) (ClientToken, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, t := range s.data.ClientTokens {
		if t.Token == token {
			return t, true
		}
	}
	return ClientToken{}, false
}

// ClientTokensFor returns tokens visible to owner. Admins see all.
func (s *State) ClientTokensFor(owner string, isAdmin bool) []ClientToken {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []ClientToken
	for _, t := range s.data.ClientTokens {
		if isAdmin || t.Owner == owner {
			out = append(out, t)
		}
	}
	return out
}

func (s *State) AddClientToken(t ClientToken) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.data.ClientTokens {
		if existing.Owner == t.Owner && existing.Name == t.Name {
			return fmt.Errorf("name %q already exists for this user", t.Name)
		}
	}
	s.data.ClientTokens = append(s.data.ClientTokens, t)
	return s.save()
}

// RevokeClientToken removes the token. Non-admins can only revoke their own tokens.
func (s *State) RevokeClientToken(owner, token string, isAdmin bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, t := range s.data.ClientTokens {
		if t.Token == token && (isAdmin || t.Owner == owner) {
			s.data.ClientTokens = append(s.data.ClientTokens[:i], s.data.ClientTokens[i+1:]...)
			return s.save()
		}
	}
	return fmt.Errorf("token not found")
}

func (s *State) ClientTokenCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.data.ClientTokens)
}

// --- Token generation ---

func genRandom(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// GenAPIKeyValue returns "sk-{owner}-{16 hex chars}".
func GenAPIKeyValue(owner string) (string, error) {
	r, err := genRandom(8)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("sk-%s-%s", owner, r), nil
}

// GenClientTokenValue returns "ct-{owner}-{16 hex chars}".
func GenClientTokenValue(owner string) (string, error) {
	r, err := genRandom(8)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("ct-%s-%s", owner, r), nil
}
```

- [ ] **Step 4: Run tests**

```bash
cd /home/tteoh/llmesh && go test ./router/internal/admin/... -v 2>&1 | tail -20
```

Expected: all tests PASS.

- [ ] **Step 5: Commit**

```bash
git add router/internal/admin/state.go router/internal/admin/state_test.go
git commit -m "feat(admin): state package — users, API keys, client tokens"
```

---

### Task 3: Hub augmentation

**Files:**
- Modify: `router/internal/hub/hub.go`
- Modify: `router/cmd/router/main.go` (call-site fix for new ServeWS signature)

- [ ] **Step 1: Write tests for new hub methods**

Create `router/internal/hub/hub_test.go`:

```go
package hub

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func dialHub(t *testing.T, h *Hub, name, owner, token string) *websocket.Conn {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeWS(w, r, name, owner, token)
	}))
	t.Cleanup(srv.Close)
	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return conn
}

func TestIsConnected(t *testing.T) {
	h := New()
	conn := dialHub(t, h, "mac", "alice", "ct-alice-abc")
	defer conn.Close()
	time.Sleep(20 * time.Millisecond)
	if !h.IsConnected("ct-alice-abc") {
		t.Fatal("expected connected")
	}
}

func TestLastSeenTime_AfterDisconnect(t *testing.T) {
	h := New()
	conn := dialHub(t, h, "mac", "alice", "ct-alice-def")
	time.Sleep(20 * time.Millisecond)
	conn.Close()
	time.Sleep(50 * time.Millisecond)
	if h.IsConnected("ct-alice-def") {
		t.Fatal("expected disconnected")
	}
	if h.LastSeenTime("ct-alice-def").IsZero() {
		t.Fatal("expected non-zero LastSeen after disconnect")
	}
}

func TestLastSeenTime_NeverConnected(t *testing.T) {
	h := New()
	if !h.LastSeenTime("ct-nobody").IsZero() {
		t.Fatal("expected zero for never-connected token")
	}
}

func TestCloseByToken(t *testing.T) {
	h := New()
	conn := dialHub(t, h, "mac", "alice", "ct-alice-xyz")
	defer conn.Close()
	time.Sleep(20 * time.Millisecond)
	h.CloseByToken("ct-alice-xyz")
	time.Sleep(50 * time.Millisecond)
	if h.IsConnected("ct-alice-xyz") {
		t.Fatal("expected disconnected after CloseByToken")
	}
}

func TestActiveClientCount(t *testing.T) {
	h := New()
	if h.ActiveClientCount() != 0 {
		t.Fatal("expected 0 initially")
	}
	conn := dialHub(t, h, "mac", "alice", "ct-alice-1")
	defer conn.Close()
	time.Sleep(20 * time.Millisecond)
	if h.ActiveClientCount() != 1 {
		t.Fatalf("expected 1, got %d", h.ActiveClientCount())
	}
}
```

- [ ] **Step 2: Run tests to confirm they fail**

```bash
cd /home/tteoh/llmesh && go test ./router/internal/hub/... 2>&1 | head -5
```

Expected: compile error — `ServeWS` has wrong number of arguments, new methods don't exist.

- [ ] **Step 3: Update hub.go**

In `router/internal/hub/hub.go`, make the following changes:

**Add fields to `Client`** (after `inFlight atomic.Int32`):
```go
Name  string
Owner string
Token string
```

**Add `lastSeen` map to `Hub`** (after `clients map[string]*Client`):
```go
lastSeen map[string]time.Time // token → last disconnect time
```

**Update `New()`**:
```go
func New() *Hub {
	return &Hub{
		clients:  make(map[string]*Client),
		lastSeen: make(map[string]time.Time),
	}
}
```

**Change `ServeWS` signature and set fields**:
```go
func (h *Hub) ServeWS(w http.ResponseWriter, r *http.Request, name, owner, token string) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("hub: ws upgrade error: %v", err)
		return
	}

	client := &Client{
		ID:    uuid.New().String(),
		conn:  conn,
		send:  make(chan []byte, 64),
		Name:  name,
		Owner: owner,
		Token: token,
	}

	h.mu.Lock()
	h.clients[client.ID] = client
	h.mu.Unlock()

	log.Printf("hub: client connected: %s name=%s owner=%s", client.ID, name, owner)

	go h.writeLoop(client)
	h.readLoop(client)

	h.mu.Lock()
	delete(h.clients, client.ID)
	if token != "" {
		h.lastSeen[token] = time.Now()
	}
	h.mu.Unlock()
	close(client.send)
	log.Printf("hub: client disconnected: %s", client.ID)
	if h.OnAvailable != nil {
		h.OnAvailable()
	}
}
```

Add `time` to imports in hub.go.

**Add new methods** (at the end of hub.go):
```go
// IsConnected reports whether a client with the given token is currently connected.
func (h *Hub) IsConnected(token string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, c := range h.clients {
		if c.Token == token {
			return true
		}
	}
	return false
}

// LastSeenTime returns the last disconnect time for token, or zero if never connected.
func (h *Hub) LastSeenTime(token string) time.Time {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.lastSeen[token]
}

// ConnectedModels returns the models advertised by the currently-connected client with token.
func (h *Hub) ConnectedModels(token string) []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, c := range h.clients {
		if c.Token == token {
			var out []string
			for m := range c.Models {
				out = append(out, m)
			}
			return out
		}
	}
	return nil
}

// CloseByToken closes the WebSocket connection for the client with the given token.
func (h *Hub) CloseByToken(token string) {
	h.mu.RLock()
	var target *Client
	for _, c := range h.clients {
		if c.Token == token {
			target = c
			break
		}
	}
	h.mu.RUnlock()
	if target != nil {
		target.conn.Close()
	}
}

// ActiveClientCount returns the number of currently connected clients.
func (h *Hub) ActiveClientCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}
```

- [ ] **Step 4: Fix the ServeWS call site in main.go**

In `router/cmd/router/main.go`, update the `/ws/client` handler to pass empty strings temporarily (full wiring happens in Task 9):

```go
mux.HandleFunc("/ws/client", func(w http.ResponseWriter, r *http.Request) {
	token := api.ExtractBearer(r)
	if token != cfg.Server.ClientToken {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	h.ServeWS(w, r, "", "", token)
})
```

- [ ] **Step 5: Run tests**

```bash
cd /home/tteoh/llmesh && go test ./router/internal/hub/... -v -timeout 10s 2>&1 | tail -20
```

Expected: all hub tests PASS.

- [ ] **Step 6: Build check**

```bash
cd /home/tteoh/llmesh && go build ./...
```

Expected: no errors.

- [ ] **Step 7: Commit**

```bash
git add router/internal/hub/hub.go router/internal/hub/hub_test.go router/cmd/router/main.go
git commit -m "feat(hub): track token/name/owner/lastSeen per client, add CloseByToken"
```

---

### Task 4: Admin auth

**Files:**
- Create: `router/internal/admin/auth.go`
- Create: `router/internal/admin/auth_test.go`

- [ ] **Step 1: Write failing tests**

Create `router/internal/admin/auth_test.go`:

```go
package admin

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func newTestAdmin(t *testing.T) *Admin {
	t.Helper()
	statePath := filepath.Join(t.TempDir(), "state.json")
	state, err := LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	a := &Admin{
		state:    state,
		sessions: newSessionStore(),
	}
	return a
}

func TestHandleSetup_GET(t *testing.T) {
	a := newTestAdmin(t)
	// GET /admin/setup should 200 when no users
	req := httptest.NewRequest("GET", "/admin/setup", nil)
	rr := httptest.NewRecorder()
	// need a minimal template; call the handler directly after wiring templates
	// For now just verify NeedsSetup is true
	if !a.state.NeedsSetup() {
		t.Fatal("expected NeedsSetup")
	}
}

func TestHandleSetup_POST(t *testing.T) {
	a := newTestAdmin(t)
	form := url.Values{"username": {"admin"}, "password": {"secret123"}, "confirm": {"secret123"}}
	req := httptest.NewRequest("POST", "/admin/setup", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	a.handleSetupPost(w, req)  // will be wired via handler
	// After POST setup, user should exist
	u, ok := a.state.LookupUser("admin")
	if !ok {
		t.Fatal("user not created")
	}
	if u.Role != "admin" {
		t.Fatalf("want admin role, got %s", u.Role)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte("secret123")); err != nil {
		t.Fatalf("password not hashed correctly: %v", err)
	}
}

func TestSessionStore(t *testing.T) {
	ss := newSessionStore()
	id := ss.create("alice")
	if id == "" {
		t.Fatal("empty session id")
	}
	username, ok := ss.lookup(id)
	if !ok || username != "alice" {
		t.Fatalf("got %q ok=%v", username, ok)
	}
	ss.delete(id)
	if _, ok := ss.lookup(id); ok {
		t.Fatal("session should be deleted")
	}
}

func TestRequireAuth_Redirect(t *testing.T) {
	a := newTestAdmin(t)
	protected := a.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})
	req := httptest.NewRequest("GET", "/admin/", nil)
	rr := httptest.NewRecorder()
	protected(rr, req)
	if rr.Code != http.StatusFound {
		t.Fatalf("expected redirect, got %d", rr.Code)
	}
}

func TestRequireAuth_Passes(t *testing.T) {
	a := newTestAdmin(t)
	hash, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	a.state.AddUser(User{Username: "bob", PasswordHash: string(hash), Role: "member"})
	sid := a.sessions.create("bob")

	protected := a.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})
	req := httptest.NewRequest("GET", "/admin/", nil)
	req.AddCookie(&http.Cookie{Name: "admin_session", Value: sid})
	rr := httptest.NewRecorder()
	protected(rr, req)
	if rr.Code != 200 {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
}
```

- [ ] **Step 2: Run tests — confirm compile failure**

```bash
cd /home/tteoh/llmesh && go test ./router/internal/admin/... 2>&1 | head -10
```

Expected: compile errors (Admin struct missing fields, functions don't exist).

- [ ] **Step 3: Implement auth.go**

Create `router/internal/admin/auth.go`:

```go
package admin

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const sessionCookie = "admin_session"
const sessionTTL = 24 * time.Hour
const bcryptCost = 12

// sessionStore is an in-memory store of active sessions.
type sessionStore struct {
	mu      sync.Mutex
	entries map[string]sessionEntry
}

type sessionEntry struct {
	Username string
	Expiry   time.Time
}

func newSessionStore() *sessionStore {
	return &sessionStore{entries: make(map[string]sessionEntry)}
}

func (s *sessionStore) create(username string) string {
	b := make([]byte, 32)
	rand.Read(b)
	id := hex.EncodeToString(b)
	s.mu.Lock()
	s.entries[id] = sessionEntry{Username: username, Expiry: time.Now().Add(sessionTTL)}
	s.mu.Unlock()
	return id
}

func (s *sessionStore) lookup(id string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[id]
	if !ok {
		return "", false
	}
	if time.Now().After(e.Expiry) {
		delete(s.entries, id)
		return "", false
	}
	return e.Username, true
}

func (s *sessionStore) delete(id string) {
	s.mu.Lock()
	delete(s.entries, id)
	s.mu.Unlock()
}

// sessionUser returns the authenticated User for this request, or User{} if not authenticated.
func (a *Admin) sessionUser(r *http.Request) (User, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return User{}, false
	}
	username, ok := a.sessions.lookup(c.Value)
	if !ok {
		return User{}, false
	}
	u, found := a.state.LookupUser(username)
	if !found || u.Disabled {
		return User{}, false
	}
	return u, true
}

// requireAuth wraps a handler, redirecting to /admin/login if no valid session.
func (a *Admin) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := a.sessionUser(r); !ok {
			http.Redirect(w, r, "/admin/login", http.StatusFound)
			return
		}
		next(w, r)
	}
}

// requireAdmin wraps a handler, returning 403 if the session user is not an admin.
func (a *Admin) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return a.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		u, _ := a.sessionUser(r)
		if u.Role != "admin" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next(w, r)
	})
}

func (a *Admin) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		a.renderStandalone(w, "login", map[string]string{"Error": ""})
		return
	}
	r.ParseForm()
	username := r.FormValue("username")
	password := r.FormValue("password")
	u, ok := a.state.LookupUser(username)
	if !ok || u.Disabled {
		a.renderStandalone(w, "login", map[string]string{"Error": "Invalid credentials."})
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)); err != nil {
		a.renderStandalone(w, "login", map[string]string{"Error": "Invalid credentials."})
		return
	}
	sid := a.sessions.create(username)
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    sid,
		Path:     "/admin",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
	http.Redirect(w, r, "/admin/", http.StatusFound)
}

func (a *Admin) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		a.sessions.delete(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/admin", MaxAge: -1})
	http.Redirect(w, r, "/admin/login", http.StatusFound)
}

func (a *Admin) handleSetup(w http.ResponseWriter, r *http.Request) {
	if !a.state.NeedsSetup() {
		http.Redirect(w, r, "/admin/login", http.StatusFound)
		return
	}
	if r.Method == http.MethodGet {
		a.renderStandalone(w, "setup", map[string]string{"Error": ""})
		return
	}
	a.handleSetupPost(w, r)
}

func (a *Admin) handleSetupPost(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	username := r.FormValue("username")
	password := r.FormValue("password")
	confirm := r.FormValue("confirm")
	if username == "" || password == "" {
		a.renderStandalone(w, "setup", map[string]string{"Error": "Username and password are required."})
		return
	}
	if password != confirm {
		a.renderStandalone(w, "setup", map[string]string{"Error": "Passwords do not match."})
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		a.renderStandalone(w, "setup", map[string]string{"Error": "Internal error."})
		return
	}
	if err := a.state.AddUser(User{
		Username:     username,
		PasswordHash: string(hash),
		Role:         "admin",
	}); err != nil {
		a.renderStandalone(w, "setup", map[string]string{"Error": err.Error()})
		return
	}
	http.Redirect(w, r, "/admin/login", http.StatusFound)
}

// HashPassword hashes a plaintext password using bcrypt.
func HashPassword(password string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	return string(b), err
}
```

- [ ] **Step 4: Fix test — remove the broken line**

In `auth_test.go`, the line `a.handleSetupPost(w, req)` references `w` which should be `rr`. Fix it:

```go
func TestHandleSetup_POST(t *testing.T) {
	a := newTestAdmin(t)
	form := url.Values{"username": {"admin"}, "password": {"secret123"}, "confirm": {"secret123"}}
	req := httptest.NewRequest("POST", "/admin/setup", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	a.handleSetupPost(rr, req)
	u, ok := a.state.LookupUser("admin")
	if !ok {
		t.Fatal("user not created")
	}
	if u.Role != "admin" {
		t.Fatalf("want admin role, got %s", u.Role)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte("secret123")); err != nil {
		t.Fatalf("password not hashed correctly: %v", err)
	}
}
```

Also: `newTestAdmin` references `Admin` struct which needs `hub` field or needs to be nilable. `Admin` struct will be defined in handler.go (Task 7). For now, define a minimal stub in auth.go to make tests compile:

At the top of `auth.go`, add:
```go
// Admin is defined in handler.go. Auth methods are defined here as methods on *Admin.
```

The `Admin` struct itself will be defined in `handler.go`. Since both files are in the same package, the tests will compile once `handler.go` exists. Since `handler.go` is created in Task 7, skip running these tests until then. Commit auth.go now and revisit after Task 7.

- [ ] **Step 5: Commit**

```bash
git add router/internal/admin/auth.go router/internal/admin/auth_test.go
git commit -m "feat(admin): session store, login/logout/setup handlers"
```

---

### Task 5: Admin CSS and templates

**Files:**
- Create: `router/internal/admin/static/admin.css`
- Create: all 9 template files under `router/internal/admin/templates/`

- [ ] **Step 1: Create the CSS file**

Create `router/internal/admin/static/admin.css`:

```css
*, *::before, *::after { box-sizing: border-box; margin: 0; padding: 0; }

:root {
  --bg: #0f172a;
  --surface-1: #1e293b;
  --surface-2: #1e293b;
  --surface-3: #0f172a;
  --border: #334155;
  --accent: #6366f1;
  --text: #f1f5f9;
  --text-muted: #94a3b8;
  --danger: #ef4444;
  --success: #22c55e;
  --warning: #f59e0b;
}

body { background: var(--bg); color: var(--text); font-family: system-ui, sans-serif; font-size: 14px; line-height: 1.5; }

/* Auth pages */
.auth-body { display: flex; align-items: center; justify-content: center; min-height: 100vh; }
.auth-card { background: var(--surface-1); border: 1px solid var(--border); border-radius: 8px; padding: 32px; width: 360px; }
.auth-card .brand { font-size: 20px; font-weight: 700; margin-bottom: 24px; }
.auth-card label { display: block; margin-bottom: 14px; font-size: 12px; font-weight: 600; color: var(--text-muted); text-transform: uppercase; letter-spacing: 0.05em; }
.auth-card input { display: block; width: 100%; margin-top: 5px; background: var(--surface-3); border: 1px solid var(--border); border-radius: 4px; padding: 8px 10px; color: var(--text); font-size: 13px; }
.auth-card button { width: 100%; margin-top: 8px; background: var(--accent); border: none; border-radius: 4px; padding: 9px; color: #fff; font-size: 13px; font-weight: 600; cursor: pointer; }

/* Top nav */
.topnav { background: var(--surface-2); border-bottom: 1px solid var(--border); display: flex; align-items: stretch; padding: 0 20px; gap: 0; }
.brand { font-weight: 700; font-size: 13px; padding: 12px 16px 12px 0; margin-right: 8px; color: var(--text); display: flex; align-items: center; }
.navlink { padding: 12px 14px; font-size: 12px; color: var(--text-muted); text-decoration: none; display: flex; align-items: center; border-bottom: 2px solid transparent; }
.navlink:hover { color: var(--text); }
.navlink.active { color: var(--accent); border-bottom-color: var(--accent); font-weight: 600; }
.navright { margin-left: auto; padding: 12px 0; font-size: 12px; color: var(--text-muted); display: flex; align-items: center; }
.navright a { color: var(--text-muted); text-decoration: none; }
.navright a:hover { color: var(--text); }

/* Flash messages */
.flash { padding: 10px 20px; font-size: 12px; }
.flash.success { background: #22c55e22; color: var(--success); border-bottom: 1px solid #22c55e44; }
.flash.error { background: #ef444422; color: var(--danger); border-bottom: 1px solid #ef444444; }

/* Main content */
main { padding: 24px; }

/* Stat grid */
.stat-grid { display: grid; grid-template-columns: repeat(4, 1fr); gap: 16px; margin-bottom: 24px; }
.stat-card { background: var(--surface-1); border: 1px solid var(--border); border-radius: 6px; padding: 16px; }
.stat-value { font-size: 28px; font-weight: 700; }
.stat-label { font-size: 11px; color: var(--text-muted); text-transform: uppercase; letter-spacing: 0.05em; margin-top: 4px; }

/* Data tables */
.data-table { width: 100%; border-collapse: collapse; font-size: 12px; }
.data-table th { text-align: left; padding: 6px 10px; color: var(--text-muted); font-size: 11px; font-weight: 600; text-transform: uppercase; letter-spacing: 0.05em; border-bottom: 1px solid var(--border); }
.data-table td { padding: 8px 10px; border-bottom: 1px solid var(--border); }
.muted { color: var(--text-muted); }
mono { font-family: monospace; font-size: 11px; }

/* Badges */
.badge { padding: 2px 8px; border-radius: 10px; font-size: 11px; font-weight: 600; }
.badge.connected { background: #22c55e22; color: var(--success); }
.badge.offline, .badge.never_connected { background: #6b728022; color: var(--text-muted); }
.badge.admin { background: #6366f122; color: #818cf8; }
.badge.member { background: #6b728022; color: var(--text-muted); }
.badge.active { background: #22c55e22; color: var(--success); }
.badge.disabled { background: #ef444422; color: var(--danger); }
.badge.high { background: #f59e0b22; color: var(--warning); }
.badge.normal { background: #6b728022; color: var(--text-muted); }
.badge.low { background: #6b728022; color: var(--text-muted); }

/* Cards / forms */
.card { background: var(--surface-1); border: 1px solid var(--border); border-radius: 6px; padding: 20px; margin-bottom: 20px; }
.card-title { font-size: 13px; font-weight: 600; margin-bottom: 14px; }
.form-row { display: flex; gap: 10px; align-items: flex-end; }
.form-group { flex: 1; }
.form-group label { display: block; font-size: 11px; font-weight: 600; color: var(--text-muted); text-transform: uppercase; letter-spacing: 0.05em; margin-bottom: 5px; }
.form-group input, .form-group select { width: 100%; background: var(--surface-3); border: 1px solid var(--border); border-radius: 4px; padding: 7px 10px; color: var(--text); font-size: 12px; }
.prefix-wrap { display: flex; align-items: center; }
.prefix { background: var(--surface-3); border: 1px solid var(--border); border-right: none; border-radius: 4px 0 0 4px; padding: 7px 10px; font-size: 12px; color: var(--text-muted); white-space: nowrap; }
.prefix-wrap input { border-radius: 0 4px 4px 0; }
.btn { background: var(--accent); border: none; border-radius: 4px; padding: 8px 14px; color: #fff; font-size: 12px; font-weight: 600; cursor: pointer; white-space: nowrap; }
.btn-danger { background: none; border: 1px solid #ef444444; color: var(--danger); padding: 3px 10px; border-radius: 4px; font-size: 11px; cursor: pointer; }
.btn-secondary { background: none; border: 1px solid var(--border); color: var(--text-muted); padding: 3px 10px; border-radius: 4px; font-size: 11px; cursor: pointer; }
.btn-success { background: none; border: 1px solid #22c55e44; color: var(--success); padding: 3px 10px; border-radius: 4px; font-size: 11px; cursor: pointer; }
.table-hint { font-size: 11px; color: var(--text-muted); margin-top: 10px; }

/* New key banner */
.new-key-banner { background: #22c55e11; border: 1px solid #22c55e44; border-radius: 6px; padding: 14px 16px; margin-bottom: 20px; }
.new-key-banner p { font-size: 12px; color: var(--success); margin-bottom: 6px; }
.new-key-banner code { font-family: monospace; font-size: 12px; color: var(--text); background: var(--surface-3); padding: 4px 8px; border-radius: 3px; word-break: break-all; }

/* Docs layout */
.docs-layout { display: flex; gap: 0; min-height: 480px; }
.docs-nav { width: 180px; border-right: 1px solid var(--border); padding: 16px; flex-shrink: 0; font-size: 12px; }
.docs-nav-section { font-size: 11px; font-weight: 600; text-transform: uppercase; letter-spacing: 0.06em; color: var(--text-muted); margin-bottom: 8px; margin-top: 12px; }
.docs-nav-section:first-child { margin-top: 0; }
.docs-link { display: block; padding: 5px 8px; border-radius: 4px; color: var(--text-muted); text-decoration: none; cursor: pointer; font-size: 12px; }
.docs-link:hover, .docs-link.active { background: var(--accent); color: #fff; }
.docs-content { flex: 1; padding: 24px; overflow: auto; font-size: 12px; line-height: 1.6; }
.docs-section { display: none; }
.docs-section.active { display: block; }
.docs-title { font-size: 16px; font-weight: 700; margin-bottom: 4px; }
.docs-subtitle { color: var(--text-muted); margin-bottom: 16px; }
.docs-code { background: var(--surface-1); border-radius: 5px; padding: 12px 14px; font-family: monospace; font-size: 11px; margin-bottom: 16px; line-height: 1.8; white-space: pre-wrap; border-left: 3px solid var(--accent); }
.docs-label { font-weight: 600; margin-bottom: 6px; }
```

- [ ] **Step 2: Create layout.html**

Create `router/internal/admin/templates/layout.html`:

```html
<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>llmesh</title>
  <link rel="stylesheet" href="/admin/static/admin.css">
</head>
<body>
<nav class="topnav">
  <span class="brand">llmesh</span>
  <a href="/admin/" class="navlink{{if eq .Page "dashboard"}} active{{end}}">Dashboard</a>
  <a href="/admin/api-keys" class="navlink{{if eq .Page "api-keys"}} active{{end}}">API Keys</a>
  <a href="/admin/client-tokens" class="navlink{{if eq .Page "client-tokens"}} active{{end}}">Client Tokens</a>
  <a href="/admin/docs" class="navlink{{if eq .Page "docs"}} active{{end}}">Docs</a>
  <a href="/admin/settings" class="navlink{{if eq .Page "settings"}} active{{end}}">Settings</a>
  <span class="navright">{{.Username}} &middot; <a href="#" onclick="document.getElementById('lf').submit();return false">Sign out</a></span>
</nav>
<form id="lf" method="POST" action="/admin/logout" style="display:none"></form>
{{if .Flash}}<div class="flash success">{{.Flash}}</div>{{end}}
{{if .Error}}<div class="flash error">{{.Error}}</div>{{end}}
<main>{{block "content" .}}{{end}}</main>
</body>
</html>
```

- [ ] **Step 3: Create login.html**

Create `router/internal/admin/templates/login.html`:

```html
<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>llmesh &mdash; Sign in</title>
  <link rel="stylesheet" href="/admin/static/admin.css">
</head>
<body class="auth-body">
<div class="auth-card">
  <div class="brand">llmesh</div>
  {{if .Error}}<div class="flash error" style="margin-bottom:14px;border-radius:4px;">{{.Error}}</div>{{end}}
  <form method="POST" action="/admin/login">
    <label>Username<input name="username" type="text" autocomplete="username" autofocus></label>
    <label>Password<input name="password" type="password" autocomplete="current-password"></label>
    <button type="submit">Sign in</button>
  </form>
</div>
</body>
</html>
```

- [ ] **Step 4: Create setup.html**

Create `router/internal/admin/templates/setup.html`:

```html
<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>llmesh &mdash; Setup</title>
  <link rel="stylesheet" href="/admin/static/admin.css">
</head>
<body class="auth-body">
<div class="auth-card">
  <div class="brand">llmesh &mdash; First-run setup</div>
  <p style="font-size:12px;color:var(--text-muted);margin-bottom:18px;">Create the initial admin account.</p>
  {{if .Error}}<div class="flash error" style="margin-bottom:14px;border-radius:4px;">{{.Error}}</div>{{end}}
  <form method="POST" action="/admin/setup">
    <label>Username<input name="username" type="text" autocomplete="username" autofocus></label>
    <label>Password<input name="password" type="password" autocomplete="new-password"></label>
    <label>Confirm password<input name="confirm" type="password" autocomplete="new-password"></label>
    <button type="submit">Create account</button>
  </form>
</div>
</body>
</html>
```

- [ ] **Step 5: Create dashboard.html**

Create `router/internal/admin/templates/dashboard.html`:

```html
{{define "content"}}
<div class="stat-grid">
  <div class="stat-card"><div class="stat-value" id="req-count">{{.TotalRequests}}</div><div class="stat-label">Total Requests</div></div>
  <div class="stat-card"><div class="stat-value" id="active-clients">{{.ActiveClients}}</div><div class="stat-label">Active Clients</div></div>
  <div class="stat-card"><div class="stat-value" id="api-key-count">{{.APIKeyCount}}</div><div class="stat-label">API Keys</div></div>
  <div class="stat-card"><div class="stat-value" id="token-count">{{.TokenCount}}</div><div class="stat-label">Client Tokens</div></div>
</div>
<table class="data-table">
  <thead><tr><th>Name</th><th>Status</th><th>Last seen</th><th>Models</th></tr></thead>
  <tbody id="client-tbody">
    {{range .Clients}}
    <tr>
      <td>{{.Name}}</td>
      <td><span class="badge {{.Status}}">{{if eq .Status "connected"}}&#9679; connected{{else if eq .Status "offline"}}&#9675; offline{{else}}&#9675; never connected{{end}}</span></td>
      <td class="muted">{{if .LastSeen}}{{.LastSeen}}{{else}}&mdash;{{end}}</td>
      <td class="muted">{{if .Models}}{{.Models}}{{else}}&mdash;{{end}}</td>
    </tr>
    {{else}}
    <tr><td colspan="4" class="muted" style="padding:16px 10px;">No client tokens registered.</td></tr>
    {{end}}
  </tbody>
</table>
<script>
setInterval(function(){
  fetch('/admin/api/dashboard').then(function(r){return r.json();}).then(function(d){
    document.getElementById('req-count').textContent = d.total_requests;
    document.getElementById('active-clients').textContent = d.active_clients;
    document.getElementById('api-key-count').textContent = d.api_key_count;
    document.getElementById('token-count').textContent = d.token_count;
    var rows = d.clients.map(function(c){
      var cls = c.status==='connected'?'connected':'offline';
      var lbl = c.status==='connected'?'\u25cf connected':c.status==='offline'?'\u25cb offline':'\u25cb never connected';
      return '<tr><td>'+c.name+'</td><td><span class="badge '+cls+'">'+lbl+'</span></td><td class="muted">'+(c.last_seen||'&mdash;')+'</td><td class="muted">'+(c.models||'&mdash;')+'</td></tr>';
    });
    document.getElementById('client-tbody').innerHTML = rows.length ? rows.join('') : '<tr><td colspan="4" class="muted" style="padding:16px 10px;">No client tokens registered.</td></tr>';
  }).catch(function(){});
}, 10000);
</script>
{{end}}
```

- [ ] **Step 6: Create api-keys.html**

Create `router/internal/admin/templates/api-keys.html`:

```html
{{define "content"}}
{{if .NewKey}}
<div class="new-key-banner">
  <p>&#10003; Key created. Copy it now &mdash; it will not be shown again.</p>
  <code id="new-key-val">{{.NewKey}}</code>
  <button class="btn-secondary" style="margin-top:8px;" onclick="navigator.clipboard.writeText(document.getElementById('new-key-val').textContent)">Copy</button>
</div>
{{end}}
<div class="card">
  <div class="card-title">Add API key</div>
  <form method="POST" action="/admin/api-keys">
    <div class="form-row">
      <div class="form-group" style="flex:2;">
        <label>Label</label>
        <div class="prefix-wrap"><span class="prefix">{{.Username}}/</span><input name="label" placeholder="prod-agents"></div>
      </div>
      <div class="form-group" style="flex:1;">
        <label>Priority</label>
        <select name="priority"><option value="normal">normal</option><option value="high">high</option><option value="low">low</option></select>
      </div>
      <button type="submit" class="btn">Generate key</button>
    </div>
    {{if .FormError}}<p style="font-size:11px;color:var(--danger);margin-top:8px;">{{.FormError}}</p>{{end}}
  </form>
</div>
<table class="data-table">
  <thead><tr><th>Label</th><th>Key</th><th>Priority</th><th>Created</th><th></th></tr></thead>
  <tbody>
    {{range .Keys}}
    <tr>
      <td><span class="muted">{{.Owner}}/</span>{{.Label}}</td>
      <td class="muted" style="font-family:monospace;font-size:11px;">{{truncate .Key 20}}&hellip; <span style="cursor:pointer;" onclick="navigator.clipboard.writeText('{{.Key}}')">&#10697;</span></td>
      <td><span class="badge {{.Priority}}">{{.Priority}}</span></td>
      <td class="muted">{{.CreatedAt.Format "2006-01-02"}}</td>
      <td style="text-align:right;">
        <form method="POST" action="/admin/api-keys/revoke" style="display:inline;">
          <input type="hidden" name="key" value="{{.Key}}">
          <button type="submit" class="btn-danger">Revoke</button>
        </form>
      </td>
    </tr>
    {{else}}
    <tr><td colspan="5" class="muted" style="padding:16px 10px;">No API keys yet.</td></tr>
    {{end}}
  </tbody>
</table>
<p class="table-hint">Keys shown truncated. Click &#10697; to copy full value. Keys cannot be recovered after leaving this page.</p>
{{end}}
```

- [ ] **Step 7: Create client-tokens.html**

Create `router/internal/admin/templates/client-tokens.html`:

```html
{{define "content"}}
{{if .NewToken}}
<div class="new-key-banner">
  <p>&#10003; Token created. Copy it now &mdash; it will not be shown again.</p>
  <code id="new-tok-val">{{.NewToken}}</code>
  <button class="btn-secondary" style="margin-top:8px;" onclick="navigator.clipboard.writeText(document.getElementById('new-tok-val').textContent)">Copy</button>
</div>
{{end}}
<div class="card">
  <div class="card-title">Add client token</div>
  <form method="POST" action="/admin/client-tokens">
    <div class="form-row">
      <div class="form-group" style="flex:2;">
        <label>Name</label>
        <div class="prefix-wrap"><span class="prefix">{{.Username}}/</span><input name="name" placeholder="macbook-pro"></div>
      </div>
      <button type="submit" class="btn">Generate token</button>
    </div>
    {{if .FormError}}<p style="font-size:11px;color:var(--danger);margin-top:8px;">{{.FormError}}</p>{{end}}
  </form>
</div>
<table class="data-table">
  <thead><tr><th>Name</th><th>Token</th><th>Status</th><th>Last seen</th><th>Created</th><th></th></tr></thead>
  <tbody>
    {{range .Tokens}}
    <tr>
      <td><span class="muted">{{.Owner}}/</span>{{.Name}}</td>
      <td class="muted" style="font-family:monospace;font-size:11px;">{{truncate .Token 20}}&hellip; <span style="cursor:pointer;" onclick="navigator.clipboard.writeText('{{.Token}}')">&#10697;</span></td>
      <td><span class="badge {{.Status}}">{{if eq .Status "connected"}}&#9679; connected{{else if eq .Status "offline"}}&#9675; offline{{else}}&#9675; never connected{{end}}</span></td>
      <td class="muted">{{if .LastSeen}}{{.LastSeen}}{{else}}&mdash;{{end}}</td>
      <td class="muted">{{.CreatedAt.Format "2006-01-02"}}</td>
      <td style="text-align:right;">
        <form method="POST" action="/admin/client-tokens/revoke" style="display:inline;">
          <input type="hidden" name="token" value="{{.Token}}">
          <button type="submit" class="btn-danger">Revoke</button>
        </form>
      </td>
    </tr>
    {{else}}
    <tr><td colspan="6" class="muted" style="padding:16px 10px;">No client tokens yet.</td></tr>
    {{end}}
  </tbody>
</table>
<p class="table-hint">Tokens shown truncated. Click &#10697; to copy. Revoking disconnects the client immediately.</p>
{{end}}
```

- [ ] **Step 8: Create docs.html**

Create `router/internal/admin/templates/docs.html`:

```html
{{define "content"}}
<div class="docs-layout">
  <div class="docs-nav">
    <div class="docs-nav-section">Endpoints</div>
    <a class="docs-link active" onclick="showDoc('openai',this)">OpenAI compatible</a>
    <a class="docs-link" onclick="showDoc('anthropic',this)">Anthropic compatible</a>
    <a class="docs-link" onclick="showDoc('responses',this)">Responses API</a>
    <div class="docs-nav-section">Setup</div>
    <a class="docs-link" onclick="showDoc('router-config',this)">Router config</a>
    <a class="docs-link" onclick="showDoc('client-setup',this)">Client setup</a>
    <a class="docs-link" onclick="showDoc('docker-deploy',this)">Docker deploy</a>
    <a class="docs-link" onclick="showDoc('priority-tiers',this)">Priority tiers</a>
  </div>
  <div class="docs-content">

    <div class="docs-section active" id="openai">
      <div class="docs-title">OpenAI-compatible endpoint</div>
      <div class="docs-subtitle">Drop-in replacement for <code>api.openai.com</code></div>
      <div class="docs-code">POST https://llm.teoh.co/v1/chat/completions</div>
      <div class="docs-label">Authentication</div>
      <div class="docs-code">Authorization: Bearer sk-yourkey</div>
      <div class="docs-label">Example request</div>
      <div class="docs-code">curl https://llm.teoh.co/v1/chat/completions \
  -H "Authorization: Bearer sk-yourkey" \
  -H "Content-Type: application/json" \
  -d '{"model":"llama3.2:3b","messages":[{"role":"user","content":"hi"}],"stream":true}'</div>
      <div class="docs-label">Python SDK</div>
      <div class="docs-code">from openai import OpenAI
client = OpenAI(base_url="https://llm.teoh.co/v1", api_key="sk-yourkey")</div>
    </div>

    <div class="docs-section" id="anthropic">
      <div class="docs-title">Anthropic-compatible endpoint</div>
      <div class="docs-subtitle">Drop-in replacement for <code>api.anthropic.com</code></div>
      <div class="docs-code">POST https://llm.teoh.co/v1/messages</div>
      <div class="docs-label">Authentication</div>
      <div class="docs-code">x-api-key: sk-yourkey</div>
      <div class="docs-label">Python SDK</div>
      <div class="docs-code">import anthropic
client = anthropic.Anthropic(base_url="https://llm.teoh.co", api_key="sk-yourkey")</div>
    </div>

    <div class="docs-section" id="responses">
      <div class="docs-title">Responses API endpoint</div>
      <div class="docs-subtitle">OpenAI Responses API format</div>
      <div class="docs-code">POST https://llm.teoh.co/v1/responses</div>
      <div class="docs-label">Authentication</div>
      <div class="docs-code">Authorization: Bearer sk-yourkey</div>
    </div>

    <div class="docs-section" id="router-config">
      <div class="docs-title">Router config</div>
      <div class="docs-subtitle"><code>config.yaml</code> — read-only at startup</div>
      <div class="docs-code">server:
  port: 53002</div>
      <p style="font-size:12px;color:var(--text-muted);">API keys and client tokens are managed through this UI and stored in <code>state.json</code>.</p>
    </div>

    <div class="docs-section" id="client-setup">
      <div class="docs-title">Client setup</div>
      <div class="docs-subtitle"><code>client/config.yaml</code></div>
      <div class="docs-code">router_url: "wss://llm.teoh.co/ws/client"
router_token: "ct-yourname-yourtoken"
max_concurrent: 4
models:
  - name: "llama3.2:3b"
    endpoint: "http://host.docker.internal:8080"</div>
    </div>

    <div class="docs-section" id="docker-deploy">
      <div class="docs-title">Docker deploy</div>
      <div class="docs-subtitle">Router (server) and client (local machine)</div>
      <div class="docs-code"># Router
docker compose up -d

# Client (local machine with llama.cpp running on port 8080)
docker compose -f docker-compose.client.yml up -d</div>
    </div>

    <div class="docs-section" id="priority-tiers">
      <div class="docs-title">Priority tiers</div>
      <div class="docs-subtitle">Requests are ordered by API key priority, then FIFO within each tier</div>
      <div class="docs-code">high   → processed first  (0)
normal → default          (1)
low    → processed last   (2)</div>
      <p style="font-size:12px;color:var(--text-muted);">Set priority per API key on the API Keys page.</p>
    </div>

  </div>
</div>
<script>
function showDoc(id, el) {
  document.querySelectorAll('.docs-section').forEach(function(s){ s.classList.remove('active'); });
  document.querySelectorAll('.docs-link').forEach(function(a){ a.classList.remove('active'); });
  document.getElementById(id).classList.add('active');
  if (el) el.classList.add('active');
}
</script>
{{end}}
```

- [ ] **Step 9: Create settings.html**

Create `router/internal/admin/templates/settings.html`:

```html
{{define "content"}}
<div style="max-width:480px;">
  <div class="card">
    <div class="card-title">Change password</div>
    <form method="POST" action="/admin/settings/password">
      <div class="form-group" style="margin-bottom:12px;"><label>Current password</label><input name="current" type="password"></div>
      <div class="form-group" style="margin-bottom:12px;"><label>New password</label><input name="new" type="password"></div>
      <div class="form-group" style="margin-bottom:14px;"><label>Confirm new password</label><input name="confirm" type="password"></div>
      <button type="submit" class="btn">Update password</button>
    </form>
  </div>
</div>

{{if .IsAdmin}}
<div style="margin-top:8px;">
  <div style="display:flex;align-items:center;justify-content:space-between;margin-bottom:12px;">
    <div style="font-size:13px;font-weight:600;">Users</div>
    <form method="POST" action="/admin/settings/users" style="display:flex;gap:8px;align-items:center;">
      <input name="username" placeholder="username" style="background:var(--surface-1);border:1px solid var(--border);border-radius:4px;padding:6px 10px;color:var(--text);font-size:12px;width:130px;">
      <input name="password" type="password" placeholder="password" style="background:var(--surface-1);border:1px solid var(--border);border-radius:4px;padding:6px 10px;color:var(--text);font-size:12px;width:130px;">
      <button type="submit" class="btn">Add user</button>
    </form>
  </div>
  <table class="data-table">
    <thead><tr><th>Username</th><th>Role</th><th>Status</th><th></th></tr></thead>
    <tbody>
      {{range .Users}}
      <tr{{if .Disabled}} style="opacity:0.6;"{{end}}>
        <td>{{.Username}}{{if .IsSelf}} <span class="muted">(you)</span>{{end}}</td>
        <td><span class="badge {{.Role}}">{{.Role}}</span></td>
        <td><span class="badge {{if .Disabled}}disabled{{else}}active{{end}}">{{if .Disabled}}disabled{{else}}active{{end}}</span></td>
        <td style="text-align:right;">
          {{if not .IsSelf}}
          <div style="display:flex;gap:6px;justify-content:flex-end;">
            {{if .Disabled}}
              <form method="POST" action="/admin/settings/users/enable" style="display:inline;"><input type="hidden" name="username" value="{{.Username}}"><button type="submit" class="btn-success">Enable</button></form>
            {{else}}
              {{if eq .Role "admin"}}<form method="POST" action="/admin/settings/users/demote" style="display:inline;"><input type="hidden" name="username" value="{{.Username}}"><button type="submit" class="btn-secondary">Demote</button></form>{{end}}
              {{if eq .Role "member"}}<form method="POST" action="/admin/settings/users/promote" style="display:inline;"><input type="hidden" name="username" value="{{.Username}}"><button type="submit" class="btn-secondary">Promote</button></form>{{end}}
              <form method="POST" action="/admin/settings/users/disable" style="display:inline;"><input type="hidden" name="username" value="{{.Username}}"><button type="submit" class="btn-danger">Disable</button></form>
            {{end}}
          </div>
          {{end}}
        </td>
      </tr>
      {{end}}
    </tbody>
  </table>
  <p class="table-hint">Cannot disable or demote yourself. At least one active admin must remain.</p>
</div>
{{end}}
{{end}}
```

- [ ] **Step 10: Commit**

```bash
git add router/internal/admin/static/ router/internal/admin/templates/
git commit -m "feat(admin): CSS and HTML templates"
```

---

### Task 6: Page handlers and JSON API

**Files:**
- Create: `router/internal/admin/pages.go`
- Create: `router/internal/admin/api.go`

- [ ] **Step 1: Create pages.go**

Create `router/internal/admin/pages.go`:

```go
package admin

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"llmesh/router/internal/hub"
	"golang.org/x/crypto/bcrypt"
)

// --- Shared page data types ---

type basePage struct {
	Page     string
	Username string
	IsAdmin  bool
	Flash    string
	Error    string
}

type DashboardPage struct {
	basePage
	TotalRequests int64
	ActiveClients int
	APIKeyCount   int
	TokenCount    int
	Clients       []ClientRow
}

type ClientRow struct {
	Name     string
	Token    string
	Status   string // "connected" | "offline" | "never_connected"
	LastSeen string
	Models   string
}

type APIKeysPage struct {
	basePage
	Keys      []APIKey
	NewKey    string
	FormError string
}

type ClientTokensPage struct {
	basePage
	Tokens    []ClientTokenRow
	NewToken  string
	FormError string
}

type ClientTokenRow struct {
	ClientToken
	Status   string
	LastSeen string
}

type SettingsPage struct {
	basePage
	Users []UserRow
}

type UserRow struct {
	User
	IsSelf bool
}

// --- Dashboard ---

func (a *Admin) handleDashboard(w http.ResponseWriter, r *http.Request) {
	u, _ := a.sessionUser(r)
	tokens := a.state.ClientTokensFor("", true)
	clients := make([]ClientRow, 0, len(tokens))
	for _, t := range tokens {
		row := ClientRow{
			Name:  t.Owner + "/" + t.Name,
			Token: t.Token,
		}
		if a.hub.IsConnected(t.Token) {
			row.Status = "connected"
			mods := a.hub.ConnectedModels(t.Token)
			sort.Strings(mods)
			row.Models = strings.Join(mods, ", ")
		} else if ls := a.hub.LastSeenTime(t.Token); !ls.IsZero() {
			row.Status = "offline"
			row.LastSeen = humanTime(ls)
		} else {
			row.Status = "never_connected"
		}
		clients = append(clients, row)
	}
	data := DashboardPage{
		basePage:      basePage{Page: "dashboard", Username: u.Username, IsAdmin: u.Role == "admin"},
		TotalRequests: a.reqCount(),
		ActiveClients: a.hub.ActiveClientCount(),
		APIKeyCount:   a.state.APIKeyCount(),
		TokenCount:    a.state.ClientTokenCount(),
		Clients:       clients,
	}
	a.render(w, "dashboard", data)
}

// --- API Keys ---

func (a *Admin) handleAPIKeys(w http.ResponseWriter, r *http.Request) {
	u, _ := a.sessionUser(r)
	a.renderAPIKeys(w, u, "", "")
}

func (a *Admin) renderAPIKeys(w http.ResponseWriter, u User, newKey, formErr string) {
	keys := a.state.APIKeysFor(u.Username, u.Role == "admin")
	a.render(w, "api-keys", APIKeysPage{
		basePage:  basePage{Page: "api-keys", Username: u.Username, IsAdmin: u.Role == "admin"},
		Keys:      keys,
		NewKey:    newKey,
		FormError: formErr,
	})
}

func (a *Admin) handleAPIKeyCreate(w http.ResponseWriter, r *http.Request) {
	u, _ := a.sessionUser(r)
	r.ParseForm()
	label := strings.TrimSpace(r.FormValue("label"))
	priority := r.FormValue("priority")
	if priority == "" {
		priority = "normal"
	}
	if label == "" {
		a.renderAPIKeys(w, u, "", "Label is required.")
		return
	}
	keyVal, err := GenAPIKeyValue(u.Username)
	if err != nil {
		a.renderAPIKeys(w, u, "", "Failed to generate key.")
		return
	}
	k := APIKey{
		Label:     label,
		Owner:     u.Username,
		Key:       keyVal,
		Priority:  priority,
		CreatedAt: time.Now().UTC(),
	}
	if err := a.state.AddAPIKey(k); err != nil {
		a.renderAPIKeys(w, u, "", err.Error())
		return
	}
	a.renderAPIKeys(w, u, keyVal, "")
}

func (a *Admin) handleAPIKeyRevoke(w http.ResponseWriter, r *http.Request) {
	u, _ := a.sessionUser(r)
	r.ParseForm()
	key := r.FormValue("key")
	a.state.RevokeAPIKey(u.Username, key, u.Role == "admin")
	http.Redirect(w, r, "/admin/api-keys", http.StatusFound)
}

// --- Client Tokens ---

func (a *Admin) handleClientTokens(w http.ResponseWriter, r *http.Request) {
	u, _ := a.sessionUser(r)
	a.renderClientTokens(w, u, "", "")
}

func (a *Admin) renderClientTokens(w http.ResponseWriter, u User, newToken, formErr string) {
	rawTokens := a.state.ClientTokensFor(u.Username, u.Role == "admin")
	rows := make([]ClientTokenRow, 0, len(rawTokens))
	for _, t := range rawTokens {
		row := ClientTokenRow{ClientToken: t}
		if a.hub.IsConnected(t.Token) {
			row.Status = "connected"
		} else if ls := a.hub.LastSeenTime(t.Token); !ls.IsZero() {
			row.Status = "offline"
			row.LastSeen = humanTime(ls)
		} else {
			row.Status = "never_connected"
		}
		rows = append(rows, row)
	}
	a.render(w, "client-tokens", ClientTokensPage{
		basePage:  basePage{Page: "client-tokens", Username: u.Username, IsAdmin: u.Role == "admin"},
		Tokens:    rows,
		NewToken:  newToken,
		FormError: formErr,
	})
}

func (a *Admin) handleClientTokenCreate(w http.ResponseWriter, r *http.Request) {
	u, _ := a.sessionUser(r)
	r.ParseForm()
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		a.renderClientTokens(w, u, "", "Name is required.")
		return
	}
	tokVal, err := GenClientTokenValue(u.Username)
	if err != nil {
		a.renderClientTokens(w, u, "", "Failed to generate token.")
		return
	}
	t := ClientToken{
		Name:      name,
		Owner:     u.Username,
		Token:     tokVal,
		CreatedAt: time.Now().UTC(),
	}
	if err := a.state.AddClientToken(t); err != nil {
		a.renderClientTokens(w, u, "", err.Error())
		return
	}
	a.renderClientTokens(w, u, tokVal, "")
}

func (a *Admin) handleClientTokenRevoke(w http.ResponseWriter, r *http.Request) {
	u, _ := a.sessionUser(r)
	r.ParseForm()
	token := r.FormValue("token")
	a.state.RevokeClientToken(u.Username, token, u.Role == "admin")
	a.hub.CloseByToken(token)
	http.Redirect(w, r, "/admin/client-tokens", http.StatusFound)
}

// --- Docs ---

func (a *Admin) handleDocs(w http.ResponseWriter, r *http.Request) {
	u, _ := a.sessionUser(r)
	a.render(w, "docs", basePage{Page: "docs", Username: u.Username, IsAdmin: u.Role == "admin"})
}

// --- Settings ---

func (a *Admin) handleSettings(w http.ResponseWriter, r *http.Request) {
	u, _ := a.sessionUser(r)
	a.renderSettings(w, u, "", "")
}

func (a *Admin) renderSettings(w http.ResponseWriter, u User, flash, errMsg string) {
	users := a.state.Users()
	rows := make([]UserRow, 0, len(users))
	for _, usr := range users {
		rows = append(rows, UserRow{User: usr, IsSelf: usr.Username == u.Username})
	}
	a.render(w, "settings", SettingsPage{
		basePage: basePage{Page: "settings", Username: u.Username, IsAdmin: u.Role == "admin", Flash: flash, Error: errMsg},
		Users:    rows,
	})
}

func (a *Admin) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	u, _ := a.sessionUser(r)
	r.ParseForm()
	current := r.FormValue("current")
	newPw := r.FormValue("new")
	confirm := r.FormValue("confirm")
	if newPw != confirm {
		a.renderSettings(w, u, "", "New passwords do not match.")
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(current)); err != nil {
		a.renderSettings(w, u, "", "Current password is incorrect.")
		return
	}
	hash, err := HashPassword(newPw)
	if err != nil {
		a.renderSettings(w, u, "", "Internal error.")
		return
	}
	a.state.UpdateUser(u.Username, func(user *User) { user.PasswordHash = hash })
	a.renderSettings(w, u, "Password updated.", "")
}

func (a *Admin) handleAddUser(w http.ResponseWriter, r *http.Request) {
	u, _ := a.sessionUser(r)
	r.ParseForm()
	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")
	if username == "" || password == "" {
		a.renderSettings(w, u, "", "Username and password are required.")
		return
	}
	if _, exists := a.state.LookupUser(username); exists {
		a.renderSettings(w, u, "", fmt.Sprintf("Username %q already exists.", username))
		return
	}
	hash, err := HashPassword(password)
	if err != nil {
		a.renderSettings(w, u, "", "Internal error.")
		return
	}
	a.state.AddUser(User{Username: username, PasswordHash: hash, Role: "member"})
	a.renderSettings(w, u, fmt.Sprintf("User %q created.", username), "")
}

func (a *Admin) handleUserDisable(w http.ResponseWriter, r *http.Request) {
	u, _ := a.sessionUser(r)
	r.ParseForm()
	target := r.FormValue("username")
	if target == u.Username {
		a.renderSettings(w, u, "", "Cannot disable yourself.")
		return
	}
	a.state.UpdateUser(target, func(user *User) { user.Disabled = true })
	http.Redirect(w, r, "/admin/settings", http.StatusFound)
}

func (a *Admin) handleUserEnable(w http.ResponseWriter, r *http.Request) {
	u, _ := a.sessionUser(r)
	r.ParseForm()
	target := r.FormValue("username")
	a.state.UpdateUser(target, func(user *User) { user.Disabled = false })
	http.Redirect(w, r, "/admin/settings", http.StatusFound)
}

func (a *Admin) handleUserPromote(w http.ResponseWriter, r *http.Request) {
	u, _ := a.sessionUser(r)
	r.ParseForm()
	target := r.FormValue("username")
	a.state.UpdateUser(target, func(user *User) { user.Role = "admin" })
	http.Redirect(w, r, "/admin/settings", http.StatusFound)
}

func (a *Admin) handleUserDemote(w http.ResponseWriter, r *http.Request) {
	u, _ := a.sessionUser(r)
	r.ParseForm()
	target := r.FormValue("username")
	if target == u.Username {
		a.renderSettings(w, u, "", "Cannot demote yourself.")
		return
	}
	if a.state.ActiveAdminCount() <= 1 {
		a.renderSettings(w, u, "", "Cannot demote: at least one active admin must remain.")
		return
	}
	a.state.UpdateUser(target, func(user *User) { user.Role = "member" })
	http.Redirect(w, r, "/admin/settings", http.StatusFound)
}

// humanTime formats a time as a human-readable relative string.
func humanTime(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return t.Format("2006-01-02")
	}
}
```

- [ ] **Step 2: Create api.go**

Create `router/internal/admin/api.go`:

```go
package admin

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
)

type dashboardJSON struct {
	TotalRequests int64        `json:"total_requests"`
	ActiveClients int          `json:"active_clients"`
	APIKeyCount   int          `json:"api_key_count"`
	TokenCount    int          `json:"token_count"`
	Clients       []clientJSON `json:"clients"`
}

type clientJSON struct {
	Name     string `json:"name"`
	Status   string `json:"status"`
	LastSeen string `json:"last_seen,omitempty"`
	Models   string `json:"models,omitempty"`
}

func (a *Admin) handleDashboardJSON(w http.ResponseWriter, r *http.Request) {
	tokens := a.state.ClientTokensFor("", true)
	clients := make([]clientJSON, 0, len(tokens))
	for _, t := range tokens {
		c := clientJSON{Name: t.Owner + "/" + t.Name}
		if a.hub.IsConnected(t.Token) {
			c.Status = "connected"
			mods := a.hub.ConnectedModels(t.Token)
			sort.Strings(mods)
			c.Models = strings.Join(mods, ", ")
		} else if ls := a.hub.LastSeenTime(t.Token); !ls.IsZero() {
			c.Status = "offline"
			c.LastSeen = humanTime(ls)
		} else {
			c.Status = "never_connected"
		}
		clients = append(clients, c)
	}
	resp := dashboardJSON{
		TotalRequests: a.reqCount(),
		ActiveClients: a.hub.ActiveClientCount(),
		APIKeyCount:   a.state.APIKeyCount(),
		TokenCount:    a.state.ClientTokenCount(),
		Clients:       clients,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
```

- [ ] **Step 3: Commit**

```bash
git add router/internal/admin/pages.go router/internal/admin/api.go
git commit -m "feat(admin): page handlers and dashboard JSON API"
```

---

### Task 7: Admin handler wiring

**Files:**
- Create: `router/internal/admin/handler.go`

- [ ] **Step 1: Create handler.go**

Create `router/internal/admin/handler.go`:

```go
package admin

import (
	"embed"
	"html/template"
	"log"
	"net/http"
	"strings"

	"llmesh/router/internal/hub"
)

//go:embed templates static
var adminFS embed.FS

// Admin is the management console HTTP handler.
type Admin struct {
	state    *State
	hub      *hub.Hub
	reqCount func() int64
	sessions *sessionStore
	tmpls    map[string]*template.Template
	mux      *http.ServeMux
}

// New creates an Admin handler. statePath is the path to state.json.
func New(statePath string, h *hub.Hub, reqCount func() int64) (*Admin, error) {
	state, err := LoadState(statePath)
	if err != nil {
		return nil, err
	}
	a := &Admin{
		state:    state,
		hub:      h,
		reqCount: reqCount,
		sessions: newSessionStore(),
	}
	if err := a.parseTemplates(); err != nil {
		return nil, err
	}
	a.registerRoutes()
	return a, nil
}

// State returns the loaded State, for use by the API handler.
func (a *Admin) State() *State {
	return a.state
}

func (a *Admin) parseTemplates() error {
	funcMap := template.FuncMap{
		"truncate": func(s string, n int) string {
			if len(s) <= n {
				return s
			}
			return s[:n]
		},
		"not": func(b bool) bool { return !b },
	}

	layoutPages := []string{"dashboard", "api-keys", "client-tokens", "docs", "settings"}
	a.tmpls = make(map[string]*template.Template)
	for _, name := range layoutPages {
		t, err := template.New("layout.html").Funcs(funcMap).ParseFS(
			adminFS,
			"templates/layout.html",
			"templates/"+name+".html",
		)
		if err != nil {
			return err
		}
		a.tmpls[name] = t
	}
	for _, name := range []string{"login", "setup"} {
		t, err := template.New(name+".html").Funcs(funcMap).ParseFS(adminFS, "templates/"+name+".html")
		if err != nil {
			return err
		}
		a.tmpls[name] = t
	}
	return nil
}

func (a *Admin) registerRoutes() {
	mux := http.NewServeMux()

	// Static assets
	mux.Handle("/admin/static/", http.FileServer(http.FS(adminFS)))

	// Auth (no session required)
	mux.HandleFunc("/admin/login", a.handleLogin)
	mux.HandleFunc("/admin/logout", a.handleLogout)
	mux.HandleFunc("/admin/setup", a.handleSetup)

	// Protected pages
	mux.HandleFunc("/admin/", a.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/" {
			http.NotFound(w, r)
			return
		}
		// Redirect to setup if no users yet
		if a.state.NeedsSetup() {
			http.Redirect(w, r, "/admin/setup", http.StatusFound)
			return
		}
		a.handleDashboard(w, r)
	}))

	mux.HandleFunc("/admin/api-keys", a.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			a.handleAPIKeyCreate(w, r)
		} else {
			a.handleAPIKeys(w, r)
		}
	}))
	mux.HandleFunc("/admin/api-keys/revoke", a.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		a.handleAPIKeyRevoke(w, r)
	}))

	mux.HandleFunc("/admin/client-tokens", a.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			a.handleClientTokenCreate(w, r)
		} else {
			a.handleClientTokens(w, r)
		}
	}))
	mux.HandleFunc("/admin/client-tokens/revoke", a.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		a.handleClientTokenRevoke(w, r)
	}))

	mux.HandleFunc("/admin/docs", a.requireAuth(a.handleDocs))

	mux.HandleFunc("/admin/settings", a.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		} else {
			a.handleSettings(w, r)
		}
	}))
	mux.HandleFunc("/admin/settings/password", a.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		a.handleChangePassword(w, r)
	}))
	mux.HandleFunc("/admin/settings/users", a.requireAdmin(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		a.handleAddUser(w, r)
	}))
	mux.HandleFunc("/admin/settings/users/disable", a.requireAdmin(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		a.handleUserDisable(w, r)
	}))
	mux.HandleFunc("/admin/settings/users/enable", a.requireAdmin(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		a.handleUserEnable(w, r)
	}))
	mux.HandleFunc("/admin/settings/users/promote", a.requireAdmin(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		a.handleUserPromote(w, r)
	}))
	mux.HandleFunc("/admin/settings/users/demote", a.requireAdmin(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		a.handleUserDemote(w, r)
	}))

	// Dashboard JSON API
	mux.HandleFunc("/admin/api/dashboard", a.requireAuth(a.handleDashboardJSON))

	a.mux = mux
}

func (a *Admin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Redirect bare /admin to /admin/
	if r.URL.Path == "/admin" {
		http.Redirect(w, r, "/admin/", http.StatusMovedPermanently)
		return
	}
	// First-run redirect
	if a.state.NeedsSetup() &&
		!strings.HasPrefix(r.URL.Path, "/admin/setup") &&
		!strings.HasPrefix(r.URL.Path, "/admin/static") {
		http.Redirect(w, r, "/admin/setup", http.StatusFound)
		return
	}
	a.mux.ServeHTTP(w, r)
}

func (a *Admin) render(w http.ResponseWriter, name string, data any) {
	t, ok := a.tmpls[name]
	if !ok {
		http.Error(w, "template not found: "+name, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.Execute(w, data); err != nil {
		log.Printf("admin: render %s: %v", name, err)
	}
}

func (a *Admin) renderStandalone(w http.ResponseWriter, name string, data any) {
	a.render(w, name, data)
}
```

- [ ] **Step 2: Build check**

```bash
cd /home/tteoh/llmesh && go build ./router/internal/admin/...
```

Expected: no errors. (auth_test.go will still compile now that `Admin` is defined.)

- [ ] **Step 3: Run all admin tests**

```bash
cd /home/tteoh/llmesh && go test ./router/internal/admin/... -v 2>&1 | tail -30
```

Expected: state and session tests PASS; setup/auth handler tests PASS.

- [ ] **Step 4: Commit**

```bash
git add router/internal/admin/handler.go
git commit -m "feat(admin): Admin handler, template wiring, route mux"
```

---

### Task 8: Update api/handler.go

**Files:**
- Modify: `router/internal/api/handler.go`

- [ ] **Step 1: Add APIKeyStore interface and update Handler**

Replace the top of `router/internal/api/handler.go`. Change:

```go
type Handler struct {
	Config      *routerPkg.Config
	Queue       *queue.Queue
	Correlation *correlation.Store
	Scheduler   *scheduler.Scheduler
}
```

To:

```go
// APIKeyStore is satisfied by *admin.State (duck typing — no import needed).
type APIKeyStore interface {
	ValidAPIKey(key string) bool
	PriorityFor(key string) types.Priority
}

type Handler struct {
	Keys         APIKeyStore
	Queue        *queue.Queue
	Correlation  *correlation.Store
	Scheduler    *scheduler.Scheduler
	requestCount atomic.Int64
}

// Count returns the number of requests handled since startup.
func (h *Handler) Count() int64 {
	return h.requestCount.Load()
}
```

Add `"sync/atomic"` to imports (or use `atomic.Int64` which is in `sync/atomic` — but in Go 1.19+ it's directly available as `sync/atomic.Int64`; since go.mod requires 1.25.9, this is fine as a struct field without import: `atomic.Int64` comes from `sync/atomic`).

Actually `atomic.Int64` as a struct field requires importing `sync/atomic`. Add it.

- [ ] **Step 2: Update enqueue to use Keys**

In the `enqueue` method, replace:

```go
key := ExtractBearer(r)
if key == "" || !h.validKey(key) {
    unauthorised(w)
    return
}
```

with:

```go
key := ExtractBearer(r)
if key == "" || !h.Keys.ValidAPIKey(key) {
    unauthorised(w)
    return
}
```

Replace:

```go
req.Priority = h.Config.PriorityFor(key)
```

with:

```go
req.Priority = h.Keys.PriorityFor(key)
h.requestCount.Add(1)
```

- [ ] **Step 3: Remove validKey method and unused imports**

Delete:
```go
func (h *Handler) validKey(key string) bool {
	for _, k := range h.Config.APIKeys {
		if k.Key == key {
			return true
		}
	}
	return false
}
```

Remove `routerPkg "llmesh/router"` from imports (no longer used in handler.go).

- [ ] **Step 4: Build check**

```bash
cd /home/tteoh/llmesh && go build ./router/internal/api/...
```

Expected: no errors (main.go will fail until Task 9 since it still passes `Config` to Handler — that's OK, build `./router/internal/api/...` not `./...`).

- [ ] **Step 5: Commit**

```bash
git add router/internal/api/handler.go
git commit -m "feat(api): replace Config key lookup with APIKeyStore interface, add request counter"
```

---

### Task 9: Wire main.go + config cleanup

**Files:**
- Modify: `router/cmd/router/main.go`
- Modify: `router/config.go`
- Modify: `router/config.yaml`

- [ ] **Step 1: Update main.go**

Replace `router/cmd/router/main.go` entirely:

```go
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"

	"llmesh/pkg/types"
	routerPkg "llmesh/router"
	"llmesh/router/internal/admin"
	"llmesh/router/internal/api"
	"llmesh/router/internal/correlation"
	"llmesh/router/internal/hub"
	"llmesh/router/internal/queue"
	"llmesh/router/internal/scheduler"
)

func main() {
	configPath := flag.String("config", "/config.yaml", "path to config file")
	statePath := flag.String("state", "/state.json", "path to state.json")
	flag.Parse()

	cfg, err := routerPkg.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	q := queue.New()
	store := correlation.New()
	h := hub.New()

	h.OnChunk = func(msg types.ChunkMsg) {
		store.Send(msg)
	}
	h.OnError = func(msg types.ErrorMsg) {
		log.Printf("client error for request %s: %s", msg.RequestID, msg.Message)
		store.Send(types.ChunkMsg{
			Type:         "chunk",
			RequestID:    msg.RequestID,
			Done:         true,
			FinishReason: "error",
		})
	}

	sched := scheduler.New(q, h)
	sched.Start()

	adminHandler, err := admin.New(*statePath, h, nil) // reqCount wired below
	if err != nil {
		log.Fatalf("admin: %v", err)
	}

	handler := &api.Handler{
		Keys:        adminHandler.State(),
		Queue:       q,
		Correlation: store,
		Scheduler:   sched,
	}
	// Wire request counter back into admin handler after handler is created.
	adminHandler2, err := admin.New(*statePath, h, handler.Count)
	if err != nil {
		log.Fatalf("admin: %v", err)
	}
	_ = adminHandler // replaced by adminHandler2
	adminHandler = adminHandler2

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", handler.OpenAI())
	mux.HandleFunc("/v1/messages", handler.Anthropic())
	mux.HandleFunc("/v1/responses", handler.Responses())
	mux.HandleFunc("/ws/client", func(w http.ResponseWriter, r *http.Request) {
		token := api.ExtractBearer(r)
		ct, ok := adminHandler.State().LookupClientToken(token)
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeWS(w, r, ct.Name, ct.Owner, token)
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, `{"status":"ok"}`)
	})
	mux.Handle("/admin/", adminHandler)
	mux.Handle("/admin", adminHandler)

	addr := fmt.Sprintf(":%d", cfg.Server.Port)
	log.Printf("llm-router listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}
```

Note: `admin.New` is called twice to wire the request counter after `handler` is created. An alternative is to pass a function closure that captures `handler`:

```go
var handler *api.Handler
adminHandler, err := admin.New(*statePath, h, func() int64 {
    if handler == nil {
        return 0
    }
    return handler.Count()
})
// ...
handler = &api.Handler{...}
```

Use this cleaner approach instead. Replace the double-New pattern with:

```go
var apiHandler *api.Handler

adminHandler, err := admin.New(*statePath, h, func() int64 {
    if apiHandler == nil {
        return 0
    }
    return apiHandler.Count()
})
if err != nil {
    log.Fatalf("admin: %v", err)
}

apiHandler = &api.Handler{
    Keys:        adminHandler.State(),
    Queue:       q,
    Correlation: store,
    Scheduler:   sched,
}

mux := http.NewServeMux()
mux.HandleFunc("/v1/chat/completions", apiHandler.OpenAI())
mux.HandleFunc("/v1/messages", apiHandler.Anthropic())
mux.HandleFunc("/v1/responses", apiHandler.Responses())
mux.HandleFunc("/ws/client", func(w http.ResponseWriter, r *http.Request) {
    token := api.ExtractBearer(r)
    ct, ok := adminHandler.State().LookupClientToken(token)
    if !ok {
        http.Error(w, "unauthorized", http.StatusUnauthorized)
        return
    }
    h.ServeWS(w, r, ct.Name, ct.Owner, token)
})
mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
    fmt.Fprintln(w, `{"status":"ok"}`)
})
mux.Handle("/admin/", adminHandler)
mux.Handle("/admin", adminHandler)
```

- [ ] **Step 2: Update config.go — remove APIKeys and ClientToken**

In `router/config.go`, replace the file content with:

```go
package router

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config holds the router's runtime configuration.
type Config struct {
	Server struct {
		Port int `yaml:"port"`
	} `yaml:"server"`
}

// LoadConfig reads a YAML config file at path.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if cfg.Server.Port == 0 {
		cfg.Server.Port = 53002
	}
	return &cfg, nil
}
```

- [ ] **Step 3: Update config.yaml**

Replace `router/config.yaml` with:

```yaml
server:
  port: 53002
```

- [ ] **Step 4: Full build**

```bash
cd /home/tteoh/llmesh && go build ./...
```

Expected: no errors.

- [ ] **Step 5: Run all tests**

```bash
cd /home/tteoh/llmesh && go test ./... -timeout 30s 2>&1 | tail -20
```

Expected: all tests PASS.

- [ ] **Step 6: Smoke test — start router**

```bash
cd /home/tteoh/llmesh && go run ./router/cmd/router -- -config router/config.yaml -state /tmp/test-state.json &
sleep 1
curl -s http://localhost:53002/health
curl -s http://localhost:53002/admin/setup | head -5
kill %1
```

Expected: `/health` returns `{"status":"ok"}`, `/admin/setup` returns HTML with setup form.

- [ ] **Step 7: Update docker-compose to mount state.json**

In `docker-compose.yml`, add the state.json volume:

```yaml
services:
  llm-router:
    image: llm-router
    build: ./router
    ports:
      - "53002:53002"
    volumes:
      - ./router/config.yaml:/config.yaml:ro
      - ./router/state.json:/state.json
    restart: unless-stopped
```

Note: `state.json` is read-write (no `:ro`). The file will be created on first-run setup. Add `router/state.json` to `.gitignore`.

- [ ] **Step 8: Update .gitignore**

```bash
echo "router/state.json" >> /home/tteoh/llmesh/.gitignore
```

- [ ] **Step 9: Commit**

```bash
git add router/cmd/router/main.go router/config.go router/config.yaml docker-compose.yml .gitignore
git commit -m "feat: wire admin UI into router, migrate key/token auth to state.json"
```

---

### Task 10: Deploy

**Files:**
- No code changes — rebuild and restart the running container.

- [ ] **Step 1: Copy updated project to teoh home**

```bash
rsync -av --exclude='.git' --exclude='state.json' /home/tteoh/llmesh/ /home/teoh/llmesh/
```

- [ ] **Step 2: Rebuild and restart**

```bash
runuser -u teoh -- bash -c 'cd /home/teoh/llmesh && docker compose build && docker compose up -d'
```

- [ ] **Step 3: Verify health**

```bash
curl -s https://llm.teoh.co/health
curl -s -o /dev/null -w "%{http_code}" https://llm.teoh.co/admin/setup
```

Expected: `{"status":"ok"}` and `200`.

- [ ] **Step 4: Complete first-run setup**

Open `https://llm.teoh.co/admin/setup` in a browser and create the admin account.

- [ ] **Step 5: Create a client token via the UI**

Log in, go to Client Tokens, create a token. Copy the token value and update `client/config.yaml` with `router_token: <token>`.

- [ ] **Step 6: Create an API key via the UI**

Go to API Keys, create a key. Use it in place of any hardcoded `sk-prod-abc123` values.

> **Migration note:** Any API keys previously defined in `config.yaml` under `api_keys:` are no longer read. They must be recreated through the admin UI. Existing client tokens set via `server.client_token` in `config.yaml` are also no longer valid — create named client tokens through the UI and update `client/config.yaml` with the new token.

- [ ] **Step 7: Commit deploy notes**

```bash
git add docker-compose.yml
git commit -m "deploy: rebuild with admin UI, migrate to state.json"
```
