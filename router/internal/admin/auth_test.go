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
	t.Skip("GET handler render tested in Task 5 when templates are wired")
	a := newTestAdmin(t)
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

func TestHandleLogin_DisabledAccount(t *testing.T) {
	a := newTestAdmin(t)
	hash, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	a.state.AddUser(User{Username: "dave", PasswordHash: string(hash), Role: "member", Disabled: true})

	form := url.Values{"username": {"dave"}, "password": {"pw"}}
	req := httptest.NewRequest("POST", "/admin/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	a.handleLogin(rr, req)
	// No session cookie should be set
	for _, c := range rr.Result().Cookies() {
		if c.Name == sessionCookie {
			t.Fatal("session cookie should not be set for disabled account")
		}
	}
}

func TestHandleLogout_MethodNotAllowed(t *testing.T) {
	a := newTestAdmin(t)
	req := httptest.NewRequest("GET", "/admin/logout", nil)
	rr := httptest.NewRecorder()
	a.handleLogout(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rr.Code)
	}
}

func TestRequireAdmin_Forbidden(t *testing.T) {
	a := newTestAdmin(t)
	hash, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	a.state.AddUser(User{Username: "carol", PasswordHash: string(hash), Role: "member"})
	sid := a.sessions.create("carol")

	protected := a.requireAdmin(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})
	req := httptest.NewRequest("GET", "/admin/settings", nil)
	req.AddCookie(&http.Cookie{Name: "admin_session", Value: sid})
	rr := httptest.NewRecorder()
	protected(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rr.Code)
	}
}

func TestGenerateTempPassword(t *testing.T) {
	a, err := generateTempPassword()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(a) < 16 {
		t.Fatalf("temp password too short: %q", a)
	}
	// It must hash and verify like any other password.
	hash, err := HashPassword(a)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(a)) != nil {
		t.Fatal("generated password does not verify against its own hash")
	}
	// Successive calls must differ.
	b, _ := generateTempPassword()
	if a == b {
		t.Fatal("temp passwords are not unique")
	}
}
