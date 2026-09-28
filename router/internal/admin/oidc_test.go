package admin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// fakeIssuer serves an OpenID Connect discovery document. doc overrides the
// fields it would otherwise fill in from its own URL.
type fakeIssuer struct {
	*httptest.Server
	doc map[string]any
}

func startFakeIssuerWith(t *testing.T, a *Admin, edit func(doc map[string]any, base string)) *fakeIssuer {
	t.Helper()
	f := &fakeIssuer{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(f.doc)
	}))
	t.Cleanup(f.Close)
	f.doc = map[string]any{
		"issuer":                 f.URL,
		"authorization_endpoint": f.URL + "/oauth/v2/authorize",
		"token_endpoint":         f.URL + "/oauth/v2/token",
		"userinfo_endpoint":      f.URL + "/oidc/v1/userinfo",
	}
	if edit != nil {
		edit(f.doc, f.URL)
	}
	if a != nil && a.httpClient == nil {
		a.httpClient = f.Client()
	}
	return f
}

func startFakeIssuer(t *testing.T, a *Admin) *fakeIssuer {
	return startFakeIssuerWith(t, a, nil)
}

// setDiscoveredOIDC stores an OIDC configuration as if discovery had run.
func setDiscoveredOIDC(t *testing.T, s *State) {
	t.Helper()
	if err := s.SetOIDC(OIDCConfig{
		Issuer:       testOIDCIssuer,
		AuthorizeURL: testOIDCIssuer + "/authorize",
		TokenURL:     testOIDCIssuer + "/token",
		UserInfoURL:  testOIDCIssuer + "/userinfo",
	}); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverOIDC(t *testing.T) {
	a := newTestAdmin(t)
	f := startFakeIssuer(t, a)

	// A trailing slash is what an admin pasting from a browser bar will often
	// enter; it must still match the document's issuer.
	got, err := a.discoverOIDC(context.Background(), f.URL+"/")
	if err != nil {
		t.Fatal(err)
	}
	if got.Issuer != f.URL || got.AuthorizeURL != f.URL+"/oauth/v2/authorize" ||
		got.TokenURL != f.URL+"/oauth/v2/token" || got.UserInfoURL != f.URL+"/oidc/v1/userinfo" {
		t.Fatalf("unexpected discovery result: %+v", got)
	}
}

func TestDiscoverOIDCRejects(t *testing.T) {
	cases := []struct {
		name string
		edit func(doc map[string]any, base string)
		want string
	}{
		{
			// The document must describe the issuer that was asked for, or a
			// redirect could substitute another provider's endpoints.
			name: "issuer mismatch",
			edit: func(doc map[string]any, _ string) { doc["issuer"] = "https://other.example" },
			want: "names its issuer",
		},
		{
			name: "missing token endpoint",
			edit: func(doc map[string]any, _ string) { delete(doc, "token_endpoint") },
			want: "no token endpoint",
		},
		{
			name: "plain-http endpoint off loopback",
			edit: func(doc map[string]any, _ string) { doc["userinfo_endpoint"] = "http://idp.example/userinfo" },
			want: "must use https",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newTestAdmin(t)
			f := startFakeIssuerWith(t, a, tc.edit)
			_, err := a.discoverOIDC(context.Background(), f.URL)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got error %v, want one containing %q", err, tc.want)
			}
		})
	}

	a := newTestAdmin(t)
	if _, err := a.discoverOIDC(context.Background(), "http://idp.example"); err == nil ||
		!strings.Contains(err.Error(), "must use https") {
		t.Fatalf("a plain-http issuer off loopback was accepted: %v", err)
	}
}

func TestRequireSecureURL(t *testing.T) {
	for raw, ok := range map[string]bool{
		"https://idp.example":     true,
		"http://127.0.0.1:8080":   true,
		"http://localhost:8080/x": true,
		"http://[::1]:8080":       true,
		"http://idp.example":      false,
		"http://127.0.0.1.nip.io": false,
		"ftp://idp.example":       false,
		"/relative/path":          false,
		"https://":                false,
	} {
		if err := requireSecureURL("url", raw); (err == nil) != ok {
			t.Errorf("requireSecureURL(%q) error = %v, want ok=%v", raw, err, ok)
		}
	}
}

func TestOIDCIdentityParsing(t *testing.T) {
	cases := []struct {
		name      string
		doc       string
		wantID    string
		wantLabel string
	}{
		{"verified email", `{"sub":"123","email":"Alice@Example.com","email_verified":true}`, "123", "alice@example.com"},
		{"verified as string", `{"sub":"123","email":"alice@example.com","email_verified":"true"}`, "123", "alice@example.com"},
		// An unverified address would read as a claim the router had checked.
		{"unverified email", `{"sub":"123","email":"alice@example.com","email_verified":false,"preferred_username":"alice"}`, "123", "alice"},
		{"subject only", `{"sub":"123"}`, "123", "123"},
		{"no subject", `{"email":"alice@example.com","email_verified":true}`, "", "alice@example.com"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := oidcIdentity([]byte(tc.doc))
			if err != nil {
				t.Fatal(err)
			}
			if got.ID != tc.wantID || got.Label != tc.wantLabel {
				t.Fatalf("got %+v, want id %q label %q", got, tc.wantID, tc.wantLabel)
			}
		})
	}
}

// TestOIDCUsesPKCE checks that the challenge sent to the provider is the S256
// hash of the verifier later sent with the code.
func TestOIDCUsesPKCE(t *testing.T) {
	a, f := newOAuthTestAdmin(t, providerOIDC)
	addTestUser(t, a, "alice", "member")
	linkIdentity(t, a, "alice", providerOIDC, oauthIdentity{ID: f.account.id})

	rr := httptest.NewRecorder()
	a.handleOAuthLogin(providerOIDC)(rr, httptest.NewRequest("GET", oauthStartPath(providerOIDC), nil))
	loc, _ := url.Parse(rr.Header().Get("Location"))
	q := loc.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
		t.Fatalf("no S256 challenge in the authorization request: %s", loc)
	}
	var stateCookie *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == oauthStateCookie {
			stateCookie = c
		}
	}

	if rr := callback(a, providerOIDC, stateCookie, q.Get("state"), nil); rr.Code != http.StatusFound {
		t.Fatalf("sign-in failed: %d %s", rr.Code, rr.Body.String())
	}
	verifier := f.lastTokenForm.Get("code_verifier")
	if verifier == "" {
		t.Fatal("no code_verifier sent with the code")
	}
	if pkceChallenge(verifier) != q.Get("code_challenge") {
		t.Fatal("the verifier does not match the challenge")
	}

	// A provider without PKCE sends neither half.
	a, f = newOAuthTestAdmin(t, providerGitHub)
	rr = httptest.NewRecorder()
	a.handleOAuthLogin(providerGitHub)(rr, httptest.NewRequest("GET", oauthStartPath(providerGitHub), nil))
	loc, _ = url.Parse(rr.Header().Get("Location"))
	if loc.Query().Get("code_challenge") != "" {
		t.Fatal("github was sent a PKCE challenge")
	}
}

// TestOIDCRefusesStateWithoutVerifier guards the PKCE half of the state
// cookie: a cookie that carries no verifier for a PKCE provider was not
// written by this router for this flow.
func TestOIDCRefusesStateWithoutVerifier(t *testing.T) {
	a, f := newOAuthTestAdmin(t, providerOIDC)
	addTestUser(t, a, "alice", "member")
	linkIdentity(t, a, "alice", providerOIDC, oauthIdentity{ID: f.account.id})

	forged := &http.Cookie{Name: oauthStateCookie, Value: providerOIDC + ":" + oauthModeLogin + ":nonce123:"}
	rr := callback(a, providerOIDC, forged, "nonce123", nil)
	if rr.Code == http.StatusFound || sessionCookieFrom(rr) != nil {
		t.Fatal("a state cookie with no verifier completed a PKCE sign-in")
	}
	if f.lastTokenForm != nil {
		t.Fatal("the code was exchanged despite the missing verifier")
	}
}

func TestOIDCTokenAuthMethod(t *testing.T) {
	signInOnce := func(t *testing.T, a *Admin) {
		t.Helper()
		stateCookie, nonce := startAuthorization(t, a, providerOIDC, oauthModeLogin, nil)
		if rr := callback(a, providerOIDC, stateCookie, nonce, nil); rr.Code != http.StatusFound {
			t.Fatalf("sign-in failed: %d %s", rr.Code, rr.Body.String())
		}
	}

	t.Run("basic", func(t *testing.T) {
		a, f := newOAuthTestAdmin(t, providerOIDC)
		// A secret with characters that must be form-encoded before the pair
		// is base64-encoded (RFC 6749 §2.3.1).
		if err := a.state.SetOAuth(providerOIDC, "x", OAuthConfig{Enabled: true, ClientID: "cid-oidc", ClientSecret: "s:e%c ret"}); err != nil {
			t.Fatal(err)
		}
		addTestUser(t, a, "alice", "member")
		linkIdentity(t, a, "alice", providerOIDC, oauthIdentity{ID: f.account.id})
		signInOnce(t, a)

		want := "Basic " + base64.StdEncoding.EncodeToString([]byte("cid-oidc:s%3Ae%25c+ret"))
		if f.lastTokenAuth != want {
			t.Fatalf("Authorization header is %q, want %q", f.lastTokenAuth, want)
		}
		if f.lastTokenForm.Get("client_secret") != "" || f.lastTokenForm.Get("client_id") != "" {
			t.Fatal("credentials sent in the body as well as the header")
		}
	})

	t.Run("post", func(t *testing.T) {
		a, f := newOAuthTestAdmin(t, providerOIDC)
		if err := a.state.SetOIDC(OIDCConfig{Issuer: testOIDCIssuer, AuthMethod: oidcAuthPost}); err != nil {
			t.Fatal(err)
		}
		addTestUser(t, a, "alice", "member")
		linkIdentity(t, a, "alice", providerOIDC, oauthIdentity{ID: f.account.id})
		signInOnce(t, a)

		if f.lastTokenAuth != "" {
			t.Fatalf("Authorization header sent with client_secret_post: %q", f.lastTokenAuth)
		}
		if f.lastTokenForm.Get("client_secret") != "shh" || f.lastTokenForm.Get("client_id") != "cid-oidc" {
			t.Fatalf("credentials missing from the body: %v", f.lastTokenForm)
		}
	})
}

// TestOIDCSubjectIsScopedToIssuer checks that a link made against one issuer
// does not sign anyone in once the router points at another, even when the new
// issuer reports the same subject.
func TestOIDCSubjectIsScopedToIssuer(t *testing.T) {
	a, _ := newOAuthTestAdmin(t, providerOIDC)
	addTestUser(t, a, "alice", "member")
	session := signIn(a, "alice")

	stateCookie, nonce := startAuthorization(t, a, providerOIDC, oauthModeLink, session)
	if rr := callback(a, providerOIDC, stateCookie, nonce, session); rr.Code != http.StatusOK {
		t.Fatalf("link failed: %d %s", rr.Code, rr.Body.String())
	}
	u, _ := a.state.LookupUser("alice")
	if u.OIDCSubject != testOIDCIssuer+"#sub-4242" {
		t.Fatalf("stored subject is %q, want it prefixed with the issuer", u.OIDCSubject)
	}

	if err := a.state.SetOIDC(OIDCConfig{Issuer: "https://other-idp.test"}); err != nil {
		t.Fatal(err)
	}
	stateCookie, nonce = startAuthorization(t, a, providerOIDC, oauthModeLogin, nil)
	rr := callback(a, providerOIDC, stateCookie, nonce, nil)
	if rr.Code == http.StatusFound || sessionCookieFrom(rr) != nil {
		t.Fatal("the same subject at a different issuer signed in to the old link")
	}
}

func TestOIDCSettings(t *testing.T) {
	a := newTestAdmin(t)
	addTestUser(t, a, "admin", "admin")
	path := "/portal/settings/auth/" + providerOIDC
	issuer := startFakeIssuer(t, a)

	// Enabling without an issuer is refused, and names the provider.
	rr := postAs(t, a, "admin", path, url.Values{
		"enabled": {"on"}, "client_id": {"cid"}, "client_secret": {"shh"}, "name": {"Zitadel"},
	}, a.handleOAuthSettingsUpdate(providerOIDC))
	if !strings.Contains(rr.Body.String(), "issuer URL is required to enable Zitadel sign-in") {
		t.Fatalf("expected an issuer error, got:\n%s", rr.Body.String())
	}
	if a.state.OAuth(providerOIDC).Enabled {
		t.Fatal("a refused save still switched sign-in on")
	}

	// A good save discovers the endpoints and offers the button by name.
	rr = postAs(t, a, "admin", path, url.Values{
		"enabled": {"on"}, "client_id": {"cid"}, "client_secret": {"shh"},
		"name": {"Zitadel"}, "issuer": {issuer.URL}, "auth_method": {oidcAuthPost},
	}, a.handleOAuthSettingsUpdate(providerOIDC))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "Zitadel sign-in is on") {
		t.Fatalf("save failed: %d\n%s", rr.Code, rr.Body.String())
	}
	got := a.state.OIDC()
	if got.Issuer != issuer.URL || got.TokenURL != issuer.URL+"/oauth/v2/token" || got.AuthMethod != oidcAuthPost {
		t.Fatalf("stored config is %+v", got)
	}
	login := httptest.NewRecorder()
	a.handleLogin(login, httptest.NewRequest("GET", "/portal/login", nil))
	if !strings.Contains(login.Body.String(), "Continue with Zitadel") {
		t.Fatal("the login page does not offer the provider by its display name")
	}

	// A save whose discovery fails leaves the working configuration alone.
	broken := startFakeIssuerWith(t, a, func(doc map[string]any, _ string) { doc["issuer"] = "https://wrong.example" })
	rr = postAs(t, a, "admin", path, url.Values{
		"enabled": {"on"}, "client_id": {"cid"}, "name": {"Zitadel"}, "issuer": {broken.URL},
	}, a.handleOAuthSettingsUpdate(providerOIDC))
	if !strings.Contains(rr.Body.String(), "could not use issuer") {
		t.Fatalf("expected a discovery error, got:\n%s", rr.Body.String())
	}
	if a.state.OIDC().Issuer != issuer.URL {
		t.Fatal("a failed discovery overwrote the working issuer")
	}

	// Moving to another issuer warns that existing links are detached.
	other := startFakeIssuer(t, a)
	rr = postAs(t, a, "admin", path, url.Values{
		"enabled": {"on"}, "client_id": {"cid"}, "name": {"Zitadel"}, "issuer": {other.URL},
	}, a.handleOAuthSettingsUpdate(providerOIDC))
	if !strings.Contains(rr.Body.String(), "must be linked again") {
		t.Fatalf("no warning about detached links:\n%s", rr.Body.String())
	}
}
