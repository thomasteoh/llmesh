package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"llmesh/router/internal/hub"
	"llmesh/router/internal/logring"
)

// connTestAdmin builds an Admin backed by a real hub and real templates, which
// the connections API needs: it renders through the Clients page's own partials
// and reads live state straight out of the hub.
func connTestAdmin(t *testing.T) (*Admin, *hub.Hub) {
	t.Helper()
	h := hub.New(logring.NewLogger(logring.New(16), "test", 0))
	a, err := New(
		filepath.Join(t.TempDir(), "state.db"), h, nil,
		func() int64 { return 0 }, nil, "v0", "llmesh", "example.com", logring.New(16),
	)
	if err != nil {
		t.Fatal(err)
	}
	return a, h
}

// dialClient connects a client to the hub and registers it, so it shows up as a
// live connection rather than an unregistered socket.
func dialClient(t *testing.T, h *hub.Hub, name, owner, tokenHash string) *websocket.Conn {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeWS(w, r, name, owner, tokenHash, nil)
	}))
	t.Cleanup(srv.Close)
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial %s: %v", name, err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := conn.WriteJSON(map[string]any{
		"type":           "register",
		"models":         []map[string]any{{"name": "llama3"}},
		"max_concurrent": 2,
		"version":        "1.0.0",
	}); err != nil {
		t.Fatalf("register %s: %v", name, err)
	}
	return conn
}

// getConnections calls the connections endpoint as username, with the given
// `known` set, and decodes the payload.
func getConnections(t *testing.T, a *Admin, username, known string) connectionsJSON {
	t.Helper()
	sid := a.sessions.create(username)
	url := "/portal/api/connections"
	if known != "" {
		url += "?known=" + known
	}
	r := httptest.NewRequest(http.MethodGet, url, nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: sid})
	w := httptest.NewRecorder()

	a.requireAuth(a.handleConnectionsJSON)(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (%s)", w.Code, w.Body.String())
	}
	var out connectionsJSON
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v — body %s", err, w.Body.String())
	}
	return out
}

// Two owners may each have a client token called the same thing: client_tokens
// is UNIQUE(owner, name), not UNIQUE(name). An admin sees both at once, so the
// page must be able to tell them apart. Keying the markup on the name meant the
// second machine's row was suppressed as already-known and never appeared.
func TestConnectionsJSON_SameNameDifferentOwners(t *testing.T) {
	a, h := connTestAdmin(t)
	for _, u := range []User{
		{Username: "root", Role: "admin"},
		{Username: "alice", Role: "member"},
		{Username: "bob", Role: "member"},
	} {
		if err := a.state.AddUser(u); err != nil {
			t.Fatal(err)
		}
	}
	// Same name, different owners — permitted by the schema.
	for _, ct := range []ClientToken{
		{Name: "gpu-box", Owner: "alice", TokenHash: "hash-alice", CreatedAt: time.Now()},
		{Name: "gpu-box", Owner: "bob", TokenHash: "hash-bob", CreatedAt: time.Now()},
	} {
		if err := a.state.AddClientToken(ct); err != nil {
			t.Fatal(err)
		}
	}
	dialClient(t, h, "gpu-box", "alice", "hash-alice")
	dialClient(t, h, "gpu-box", "bob", "hash-bob")
	waitFor(t, func() bool { return len(h.ConnectionsLoad()) == 2 })

	out := getConnections(t, a, "root", "")

	if len(out.New) != 2 {
		t.Fatalf("markup for new connections: got %d, want 2 (one per owner)", len(out.New))
	}
	if out.New[0].ID == out.New[1].ID {
		t.Errorf("both connections share ID %q; the page cannot address them separately", out.New[0].ID)
	}
	byToken := map[string]newConnJSON{}
	for _, n := range out.New {
		byToken[n.TokenHash] = n
	}
	for _, want := range []string{"hash-alice", "hash-bob"} {
		n, ok := byToken[want]
		if !ok {
			t.Errorf("no markup returned for token %s", want)
			continue
		}
		if !strings.Contains(n.HTML, `data-conn-row="`+n.ID+`"`) {
			t.Errorf("token %s: markup is not keyed on its ID\n%s", want, n.HTML)
		}
		if strings.Contains(n.HTML, `data-conn-row="gpu-box"`) {
			t.Errorf("token %s: markup still keyed on the shared name", want)
		}
	}
}

// Once the page holds a connection, the server should stop re-rendering it —
// but only that one. Suppression keyed on the shared name hid the other owner's
// machine too, which is the bug this pins.
func TestConnectionsJSON_KnownSuppressesOnlyThatConnection(t *testing.T) {
	a, h := connTestAdmin(t)
	if err := a.state.AddUser(User{Username: "root", Role: "admin"}); err != nil {
		t.Fatal(err)
	}
	for _, ct := range []ClientToken{
		{Name: "gpu-box", Owner: "alice", TokenHash: "hash-alice", CreatedAt: time.Now()},
		{Name: "gpu-box", Owner: "bob", TokenHash: "hash-bob", CreatedAt: time.Now()},
	} {
		if err := a.state.AddClientToken(ct); err != nil {
			t.Fatal(err)
		}
	}
	dialClient(t, h, "gpu-box", "alice", "hash-alice")
	dialClient(t, h, "gpu-box", "bob", "hash-bob")
	waitFor(t, func() bool { return len(h.ConnectionsLoad()) == 2 })

	first := getConnections(t, a, "root", "")
	if len(first.New) != 2 {
		t.Fatalf("setup: got %d new, want 2", len(first.New))
	}

	// The page now shows one of them and says so.
	held := first.New[0]
	second := getConnections(t, a, "root", held.ID)

	if len(second.New) != 1 {
		t.Fatalf("new markup with one connection known: got %d, want 1", len(second.New))
	}
	if second.New[0].ID == held.ID {
		t.Error("re-rendered the connection the page already holds")
	}
	// Both are still reported as alive, or the page would drop the one it holds.
	var live []string
	for _, tk := range second.Tokens {
		live = append(live, tk.Conns...)
	}
	if len(live) != 2 {
		t.Errorf("live connection IDs: got %v, want 2", live)
	}
}

// A member must not learn about another member's machines, same name or not.
func TestConnectionsJSON_MemberSeesOnlyOwnTokens(t *testing.T) {
	a, h := connTestAdmin(t)
	if err := a.state.AddUser(User{Username: "alice", Role: "member"}); err != nil {
		t.Fatal(err)
	}
	for _, ct := range []ClientToken{
		{Name: "gpu-box", Owner: "alice", TokenHash: "hash-alice", CreatedAt: time.Now()},
		{Name: "gpu-box", Owner: "bob", TokenHash: "hash-bob", CreatedAt: time.Now()},
	} {
		if err := a.state.AddClientToken(ct); err != nil {
			t.Fatal(err)
		}
	}
	dialClient(t, h, "gpu-box", "alice", "hash-alice")
	dialClient(t, h, "gpu-box", "bob", "hash-bob")
	waitFor(t, func() bool { return len(h.ConnectionsLoad()) == 2 })

	out := getConnections(t, a, "alice", "")

	for _, tk := range out.Tokens {
		if tk.TokenHash != "hash-alice" {
			t.Errorf("member saw token %s", tk.TokenHash)
		}
	}
	for _, n := range out.New {
		if n.TokenHash != "hash-alice" {
			t.Errorf("member got markup for token %s", n.TokenHash)
		}
	}
}

// The connections container has to be empty in the CSS sense when nothing is
// connected, or the rule that collapses it never matches and every disconnected
// token carries a blank band. A newline inside the div is enough to break it.
func TestConnSubrow_EmptyContainerHasNoTextNode(t *testing.T) {
	a, _ := connTestAdmin(t)
	row := ClientTokenRow{ClientToken: ClientToken{TokenHash: "hash-alice", Name: "gpu-box", Owner: "alice"}}

	var sb strings.Builder
	if err := a.tmpls["clients"].ExecuteTemplate(&sb, "conn-subrow", row); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sb.String(), `data-token-conns="hash-alice"></div>`) {
		t.Errorf("container is not empty, so :empty will not match it:\n%s", sb.String())
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for hub state")
}
