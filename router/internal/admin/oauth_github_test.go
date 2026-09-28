package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// fakeGitHub stands in for GitHub's OAuth and API endpoints.
type fakeGitHub struct {
	srv *httptest.Server
	// identity returned by /user.
	id    int64
	login string
	// tokenErr, when set, is returned by the token endpoint as an OAuth error.
	tokenErr string
	// lastTokenForm records what the router posted to the token endpoint.
	lastTokenForm url.Values
}

func startFakeGitHub(t *testing.T, id int64, login string) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{id: id, login: login}
	mux := http.NewServeMux()
	mux.HandleFunc("/login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		f.lastTokenForm = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		if f.tokenErr != "" {
			json.NewEncoder(w).Encode(map[string]string{"error": f.tokenErr})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"access_token": "gho_test", "token_type": "bearer"})
	})
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer gho_test" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": f.id, "login": f.login})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// newGitHubTestAdmin returns an Admin with GitHub sign-in configured and its
// endpoints pointed at a fake.
func newGitHubTestAdmin(t *testing.T, id int64, login string) (*Admin, *fakeGitHub) {
	t.Helper()
	a := newTestAdmin(t)
	f := startFakeGitHub(t, id, login)
	a.ghAuthorizeURL = f.srv.URL + "/login/oauth/authorize"
	a.ghTokenURL = f.srv.URL + "/login/oauth/access_token"
	a.ghAPIBaseURL = f.srv.URL
	a.httpClient = f.srv.Client()
	if err := a.state.SetGitHubAuth(GitHubAuthConfig{Enabled: true, ClientID: "iv1.test", ClientSecret: "shh"}); err != nil {
		t.Fatal(err)
	}
	return a, f
}

// addTestUser creates a user with a known password.
func addTestUser(t *testing.T, a *Admin, username, role string) User {
	t.Helper()
	hash, err := HashPassword("pw-" + username)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.state.AddUser(User{Username: username, PasswordHash: hash, Role: role}); err != nil {
		t.Fatal(err)
	}
	u, _ := a.state.LookupUser(username)
	return u
}

// signIn returns a session cookie for username, as a completed sign-in would.
func signIn(a *Admin, username string) *http.Cookie {
	sid := a.sessions.create(username)
	token, _ := generateCSRFToken()
	a.sessions.setCSRF(sid, token)
	return &http.Cookie{Name: sessionCookie, Value: sid}
}

// startAuthorization runs the redirect-to-GitHub step and returns the state
// cookie and the nonce GitHub was asked to echo back.
func startAuthorization(t *testing.T, a *Admin, mode string, session *http.Cookie) (*http.Cookie, string) {
	t.Helper()
	req := httptest.NewRequest("GET", "/portal/auth/github", nil)
	if session != nil {
		req.AddCookie(session)
	}
	rr := httptest.NewRecorder()
	if mode == oauthModeLink {
		req.Method = "POST"
		a.handleGitHubLink(rr, req)
	} else {
		a.handleGitHubLogin(rr, req)
	}
	if rr.Code != http.StatusFound {
		t.Fatalf("expected a redirect to GitHub, got %d: %s", rr.Code, rr.Body.String())
	}
	loc, err := url.Parse(rr.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	nonce := loc.Query().Get("state")
	if nonce == "" {
		t.Fatal("no state parameter sent to GitHub")
	}
	var stateCookie *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == oauthStateCookie {
			stateCookie = c
		}
	}
	if stateCookie == nil {
		t.Fatal("no state cookie set")
	}
	return stateCookie, nonce
}

// callback replays GitHub's redirect back to the router.
func callback(a *Admin, stateCookie *http.Cookie, nonce string, session *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "/portal/auth/github/callback?code=abc123&state="+url.QueryEscape(nonce), nil)
	if stateCookie != nil {
		req.AddCookie(stateCookie)
	}
	if session != nil {
		req.AddCookie(session)
	}
	rr := httptest.NewRecorder()
	a.handleGitHubCallback(rr, req)
	return rr
}

func TestGitHubLoginRefusesUnlinkedAccount(t *testing.T) {
	a, _ := newGitHubTestAdmin(t, 4242, "octocat")
	addTestUser(t, a, "alice", "member")

	stateCookie, nonce := startAuthorization(t, a, oauthModeLogin, nil)
	rr := callback(a, stateCookie, nonce, nil)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected the login page, got %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "not linked to an llmesh account") {
		t.Fatalf("expected a refusal explaining how to link, got:\n%s", rr.Body.String())
	}
	// Crucially, no account was created for the GitHub identity.
	if got := len(a.state.Users()); got != 1 {
		t.Fatalf("an unlinked GitHub sign-in provisioned an account: %d users", got)
	}
	for _, c := range rr.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			t.Fatal("a session was issued for an unlinked GitHub account")
		}
	}
}

func TestGitHubLinkThenLogin(t *testing.T) {
	a, _ := newGitHubTestAdmin(t, 4242, "octocat")
	addTestUser(t, a, "alice", "member")
	session := signIn(a, "alice")

	// Link, as the user would from their own settings page.
	stateCookie, nonce := startAuthorization(t, a, oauthModeLink, session)
	rr := callback(a, stateCookie, nonce, session)
	if rr.Code != http.StatusOK {
		t.Fatalf("link callback: got %d: %s", rr.Code, rr.Body.String())
	}
	u, _ := a.state.LookupUser("alice")
	if u.GitHubUserID != "4242" || u.GitHubLogin != "octocat" {
		t.Fatalf("link did not record the identity: %+v", u)
	}

	// Now sign in with it, from no session at all.
	stateCookie, nonce = startAuthorization(t, a, oauthModeLogin, nil)
	rr = callback(a, stateCookie, nonce, nil)
	if rr.Code != http.StatusFound || rr.Header().Get("Location") != "/portal/" {
		t.Fatalf("expected a redirect into the portal, got %d %q", rr.Code, rr.Header().Get("Location"))
	}
	var got *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == sessionCookie {
			got = c
		}
	}
	if got == nil {
		t.Fatal("no session cookie issued")
	}
	if name, ok := a.sessions.lookup(got.Value); !ok || name != "alice" {
		t.Fatalf("session is for %q (ok=%v), want alice", name, ok)
	}
}

func TestGitHubLoginRefusesDisabledAccount(t *testing.T) {
	a, _ := newGitHubTestAdmin(t, 4242, "octocat")
	addTestUser(t, a, "alice", "member")
	if err := a.state.UpdateUser("alice", func(u *User) {
		u.GitHubUserID = "4242"
		u.GitHubLogin = "octocat"
		u.Disabled = true
	}); err != nil {
		t.Fatal(err)
	}

	stateCookie, nonce := startAuthorization(t, a, oauthModeLogin, nil)
	rr := callback(a, stateCookie, nonce, nil)
	if !strings.Contains(rr.Body.String(), "Account disabled") {
		t.Fatalf("a disabled account signed in via GitHub:\n%s", rr.Body.String())
	}
}

func TestGitHubCallbackRejectsBadState(t *testing.T) {
	a, _ := newGitHubTestAdmin(t, 4242, "octocat")
	addTestUser(t, a, "alice", "member")
	if err := a.state.UpdateUser("alice", func(u *User) { u.GitHubUserID = "4242" }); err != nil {
		t.Fatal(err)
	}

	stateCookie, _ := startAuthorization(t, a, oauthModeLogin, nil)

	// An attacker-chosen code with a state that does not match the cookie is
	// the classic login-CSRF shape, and must not produce a session.
	rr := callback(a, stateCookie, "not-the-nonce", nil)
	if !strings.Contains(rr.Body.String(), "could not be verified") {
		t.Fatalf("mismatched state was accepted:\n%s", rr.Body.String())
	}
	for _, c := range rr.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			t.Fatal("a session was issued despite a bad state")
		}
	}

	// With no cookie at all it must also fail.
	rr = callback(a, nil, "anything", nil)
	if !strings.Contains(rr.Body.String(), "could not be verified") {
		t.Fatalf("a missing state cookie was accepted:\n%s", rr.Body.String())
	}
}

func TestGitHubStateCookieIsSpent(t *testing.T) {
	a, _ := newGitHubTestAdmin(t, 4242, "octocat")
	addTestUser(t, a, "alice", "member")
	if err := a.state.UpdateUser("alice", func(u *User) { u.GitHubUserID = "4242" }); err != nil {
		t.Fatal(err)
	}
	stateCookie, nonce := startAuthorization(t, a, oauthModeLogin, nil)
	rr := callback(a, stateCookie, nonce, nil)

	cleared := false
	for _, c := range rr.Result().Cookies() {
		if c.Name == oauthStateCookie && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("the state cookie was not cleared, so it could be replayed")
	}
}

func TestGitHubFlowRefusedWhenNotConfigured(t *testing.T) {
	a := newTestAdmin(t)
	req := httptest.NewRequest("GET", "/portal/auth/github", nil)
	rr := httptest.NewRecorder()
	a.handleGitHubLogin(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 with GitHub sign-in unconfigured, got %d", rr.Code)
	}

	rr = httptest.NewRecorder()
	a.handleGitHubCallback(rr, httptest.NewRequest("GET", "/portal/auth/github/callback?code=x&state=y", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected the callback to 404 too, got %d", rr.Code)
	}
}

func TestGitHubTokenErrorDoesNotLeakToBrowser(t *testing.T) {
	a, f := newGitHubTestAdmin(t, 4242, "octocat")
	f.tokenErr = "bad_verification_code"
	addTestUser(t, a, "alice", "member")

	stateCookie, nonce := startAuthorization(t, a, oauthModeLogin, nil)
	rr := callback(a, stateCookie, nonce, nil)
	body := rr.Body.String()
	if strings.Contains(body, "bad_verification_code") {
		t.Fatalf("GitHub's error text was shown to the browser:\n%s", body)
	}
	if !strings.Contains(body, "Could not complete GitHub sign-in") {
		t.Fatalf("expected a generic failure message, got:\n%s", body)
	}
}

func TestGitHubLoginRefreshesChangedHandle(t *testing.T) {
	// Matching is on the numeric id, so renaming on GitHub must not lock the
	// user out — and the portal should stop showing the stale handle.
	a, _ := newGitHubTestAdmin(t, 4242, "new-handle")
	addTestUser(t, a, "alice", "member")
	if err := a.state.UpdateUser("alice", func(u *User) {
		u.GitHubUserID = "4242"
		u.GitHubLogin = "old-handle"
	}); err != nil {
		t.Fatal(err)
	}
	stateCookie, nonce := startAuthorization(t, a, oauthModeLogin, nil)
	if rr := callback(a, stateCookie, nonce, nil); rr.Code != http.StatusFound {
		t.Fatalf("expected sign-in to succeed, got %d: %s", rr.Code, rr.Body.String())
	}
	if u, _ := a.state.LookupUser("alice"); u.GitHubLogin != "new-handle" {
		t.Fatalf("stale handle kept: %q", u.GitHubLogin)
	}
}

func TestGitHubIdentityCannotBeLinkedTwice(t *testing.T) {
	a, _ := newGitHubTestAdmin(t, 4242, "octocat")
	addTestUser(t, a, "alice", "member")
	addTestUser(t, a, "bob", "member")
	if err := a.state.UpdateUser("alice", func(u *User) {
		u.GitHubUserID = "4242"
		u.GitHubLogin = "octocat"
	}); err != nil {
		t.Fatal(err)
	}

	bobSession := signIn(a, "bob")
	stateCookie, nonce := startAuthorization(t, a, oauthModeLink, bobSession)
	rr := callback(a, stateCookie, nonce, bobSession)
	if !strings.Contains(rr.Body.String(), "already linked to another user") {
		t.Fatalf("expected the second claim to be refused, got:\n%s", rr.Body.String())
	}
	if u, _ := a.state.LookupUser("bob"); u.GitHubUserID != "" {
		t.Fatalf("bob took alice's GitHub identity: %+v", u)
	}
	if u, _ := a.state.LookupUser("alice"); u.GitHubUserID != "4242" {
		t.Fatal("alice lost her linked identity")
	}
}

func TestGitHubUnlink(t *testing.T) {
	a, _ := newGitHubTestAdmin(t, 4242, "octocat")
	addTestUser(t, a, "alice", "member")
	if err := a.state.UpdateUser("alice", func(u *User) {
		u.GitHubUserID = "4242"
		u.GitHubLogin = "octocat"
	}); err != nil {
		t.Fatal(err)
	}
	session := signIn(a, "alice")
	req := httptest.NewRequest("POST", "/portal/settings/github/unlink", nil)
	req.AddCookie(session)
	rr := httptest.NewRecorder()
	a.requireAuth(a.handleGitHubUnlink)(rr, req)

	u, _ := a.state.LookupUser("alice")
	if u.GitHubUserID != "" || u.GitHubLogin != "" {
		t.Fatalf("unlink left the identity behind: %+v", u)
	}
	// And the identity is free for someone else to claim.
	if _, ok := a.state.LookupUserByGitHubID("4242"); ok {
		t.Fatal("the unlinked id still resolves to an account")
	}
}

func TestGitHubCallbackURLUsesEffectiveHost(t *testing.T) {
	a, _ := newGitHubTestAdmin(t, 1, "x")
	if err := a.state.SetPortalHost("llm.example.com"); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/portal/settings", nil)
	req.TLS = nil
	if got := a.GitHubCallbackURL(req); got != "http://llm.example.com/portal/auth/github/callback" {
		t.Fatalf("got %q", got)
	}
	req.Header.Set("X-Forwarded-Proto", "https")
	a.SetTrustProxy(true)
	if got := a.GitHubCallbackURL(req); got != "https://llm.example.com/portal/auth/github/callback" {
		t.Fatalf("behind a TLS-terminating proxy, got %q", got)
	}
}
