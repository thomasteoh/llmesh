package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"llmesh/router/internal/authz"
)

// TestHandleTokenLogin provisions an admin for a machine user and returns a
// session cookie + CSRF token, so a token-based client can post to the
// CSRF-protected provisioning endpoints.
func TestHandleTokenLogin(t *testing.T) {
	a := newTestAdmin(t)
	f := startFakeProvider(t, providerAccount{
		doc:   map[string]any{"sub": "machine-1", "preferred_username": "llmesh-admin"},
		id:    oidcSubjectID(testOIDCIssuer, "machine-1"),
		label: "llmesh-admin",
	})
	if a.oauthOverrides == nil {
		a.oauthOverrides = map[string]oauthEndpoints{}
	}
	a.oauthOverrides[providerOIDC] = oauthEndpoints{
		authorizeURL: f.srv.URL + "/authorize",
		tokenURL:     f.srv.URL + "/token",
		userInfoURL:  f.srv.URL + "/userinfo",
	}
	a.httpClient = f.srv.Client()
	if err := a.state.SetOIDC(OIDCConfig{
		Issuer:       testOIDCIssuer,
		AuthorizeURL: f.srv.URL + "/authorize",
		TokenURL:     f.srv.URL + "/token",
		UserInfoURL:  f.srv.URL + "/userinfo",
	}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/portal/api/token-login", nil)
	req.Header.Set("Authorization", "Bearer at_test")
	rec := httptest.NewRecorder()
	a.handleTokenLogin(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	// Session cookie must be set.
	resp := rec.Result()
	if c := resp.Cookies(); len(c) == 0 {
		t.Fatal("no session cookie set")
	}
	// CSRF token must be returned for the provisioning endpoints.
	var body struct {
		Username  string `json:"username"`
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Username != "llmesh-admin" {
		t.Fatalf("username = %q, want llmesh-admin", body.Username)
	}
	if body.CSRFToken == "" {
		t.Fatal("no csrf_token returned")
	}
	// The machine account must have been provisioned and bound as admin.
	u, ok := a.state.LookupUserByOAuth(providerOIDC, "machine-1")
	if !ok {
		t.Fatal("machine account not provisioned")
	}
	if u.Username != "llmesh-admin" {
		t.Fatalf("username = %q, want llmesh-admin", u.Username)
	}
	var role string
	if err := a.state.db.QueryRow(`SELECT role FROM role_bindings WHERE principal = ?`, userPrincipal(u.Username)).Scan(&role); err != nil {
		t.Fatalf("read role binding: %v", err)
	}
	if role != authz.RoleAdmin {
		t.Fatalf("binding role = %q, want %q", role, authz.RoleAdmin)
	}
}

// TestHandleTokenLoginRefusesBadToken verifies the handler rejects a token the
// provider does not vouch for.
func TestHandleTokenLoginRefusesBadToken(t *testing.T) {
	a := newTestAdmin(t)
	f := startFakeProvider(t, providerAccount{
		doc:   map[string]any{"sub": "machine-1", "preferred_username": "llmesh-admin"},
		id:    oidcSubjectID(testOIDCIssuer, "machine-1"),
		label: "llmesh-admin",
	})
	if a.oauthOverrides == nil {
		a.oauthOverrides = map[string]oauthEndpoints{}
	}
	a.oauthOverrides[providerOIDC] = oauthEndpoints{
		authorizeURL: f.srv.URL + "/authorize",
		tokenURL:     f.srv.URL + "/token",
		userInfoURL:  f.srv.URL + "/userinfo",
	}
	a.httpClient = f.srv.Client()
	if err := a.state.SetOIDC(OIDCConfig{
		Issuer:       testOIDCIssuer,
		AuthorizeURL: f.srv.URL + "/authorize",
		TokenURL:     f.srv.URL + "/token",
		UserInfoURL:  f.srv.URL + "/userinfo",
	}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/portal/api/token-login", nil)
	req.Header.Set("Authorization", "Bearer")
	rec := httptest.NewRecorder()
	a.handleTokenLogin(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// TestHandleTokenLoginMissingHeader verifies a request with no bearer token is
// refused.
func TestHandleTokenLoginMissingHeader(t *testing.T) {
	a := newTestAdmin(t)
	req := httptest.NewRequest("POST", "/portal/api/token-login", nil)
	rec := httptest.NewRecorder()
	a.handleTokenLogin(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// TestHandleTokenLoginMethodNotAllowed verifies GET is refused.
func TestHandleTokenLoginMethodNotAllowed(t *testing.T) {
	a := newTestAdmin(t)
	req := httptest.NewRequest("GET", "/portal/api/token-login", nil)
	rec := httptest.NewRecorder()
	a.handleTokenLogin(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

// Ensure strings import is used (the bearer prefix check).
var _ = strings.HasPrefix
