package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// The federated sign-in flow is provider-agnostic, so almost everything worth
// asserting about it is asserted against every provider. A test that pins one
// provider is either about that provider's wire format or about two providers
// not being confusable, and says so.

// providerAccount is the account a fake provider reports: the document it puts
// on the wire, and the (id, label) this router should end up storing for it.
type providerAccount struct {
	doc   map[string]any
	id    string
	label string
}

func githubAccount(id int64, login string) providerAccount {
	return providerAccount{
		doc:   map[string]any{"id": id, "login": login},
		id:    strconv.FormatInt(id, 10),
		label: login,
	}
}

func googleAccount(sub, email string) providerAccount {
	return providerAccount{
		doc:   map[string]any{"sub": sub, "email": email, "email_verified": true},
		id:    sub,
		label: email,
	}
}

// testOIDCIssuer is the issuer the OIDC provider is configured with in tests.
// Its endpoints are overridden to point at a fake, so it is never fetched.
const testOIDCIssuer = "https://idp.test"

func oidcAccount(sub, email string) providerAccount {
	return providerAccount{
		doc:   map[string]any{"sub": sub, "email": email, "email_verified": true},
		id:    oidcSubjectID(testOIDCIssuer, sub),
		label: email,
	}
}

// testAccounts is the account each fake provider reports by default.
var testAccounts = map[string]providerAccount{
	providerGitHub: githubAccount(4242, "octocat"),
	providerGoogle: googleAccount("sub-4242", "alice@example.com"),
	providerOIDC:   oidcAccount("sub-4242", "alice@idp.test"),
}

// forEachProvider runs a subtest per provider.
func forEachProvider(t *testing.T, fn func(t *testing.T, key string)) {
	t.Helper()
	for _, key := range oauthProviderOrder {
		t.Run(key, func(t *testing.T) { fn(t, key) })
	}
}

// fakeProvider stands in for one provider's OAuth and userinfo endpoints.
type fakeProvider struct {
	srv     *httptest.Server
	account providerAccount
	// tokenErr, when set, is returned by the token endpoint as an OAuth error.
	tokenErr string
	// lastTokenForm and lastTokenAuth record what the router posted to the
	// token endpoint, and the Authorization header it posted it with.
	lastTokenForm url.Values
	lastTokenAuth string
}

func startFakeProvider(t *testing.T, account providerAccount) *fakeProvider {
	t.Helper()
	f := &fakeProvider{account: account}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		f.lastTokenForm = r.PostForm
		f.lastTokenAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		if f.tokenErr != "" {
			json.NewEncoder(w).Encode(map[string]string{"error": f.tokenErr})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"access_token": "at_test", "token_type": "bearer"})
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer at_test" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(f.account.doc)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// newOAuthTestAdmin returns an Admin with one provider configured and pointed
// at a fake.
func newOAuthTestAdmin(t *testing.T, provider string) (*Admin, *fakeProvider) {
	t.Helper()
	a := newTestAdmin(t)
	f := configureFakeProvider(t, a, provider, testAccounts[provider])
	return a, f
}

// configureFakeProvider points one provider at a fake and enables it, leaving
// any other provider on the same Admin untouched.
func configureFakeProvider(t *testing.T, a *Admin, provider string, account providerAccount) *fakeProvider {
	t.Helper()
	f := startFakeProvider(t, account)
	if a.oauthOverrides == nil {
		a.oauthOverrides = map[string]oauthEndpoints{}
	}
	a.oauthOverrides[provider] = oauthEndpoints{
		authorizeURL: f.srv.URL + "/authorize",
		tokenURL:     f.srv.URL + "/token",
		userInfoURL:  f.srv.URL + "/userinfo",
	}
	a.httpClient = f.srv.Client()
	if provider == providerOIDC {
		if err := a.state.SetOIDC(OIDCConfig{Issuer: testOIDCIssuer}); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.state.SetOAuth(provider, oauthProviders[provider].name,
		OAuthConfig{Enabled: true, ClientID: "cid-" + provider, ClientSecret: "shh"}); err != nil {
		t.Fatal(err)
	}
	return f
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

// linkIdentity attaches a provider account to a user directly, for tests whose
// subject is the login half of the flow.
func linkIdentity(t *testing.T, a *Admin, username, provider string, ident oauthIdentity) {
	t.Helper()
	p := oauthProviders[provider]
	if err := a.state.UpdateUser(username, func(u *User) { p.set(u, ident) }); err != nil {
		t.Fatal(err)
	}
}

// signIn returns a session cookie for username, as a completed sign-in would.
func signIn(a *Admin, username string) *http.Cookie {
	sid := a.sessions.create(username)
	token, _ := generateCSRFToken()
	a.sessions.setCSRF(sid, token)
	return &http.Cookie{Name: sessionCookie, Value: sid}
}

// startAuthorization runs the redirect-to-provider step and returns the state
// cookie and the nonce the provider was asked to echo back.
func startAuthorization(t *testing.T, a *Admin, provider, mode string, session *http.Cookie) (*http.Cookie, string) {
	t.Helper()
	req := httptest.NewRequest("GET", oauthStartPath(provider), nil)
	if session != nil {
		req.AddCookie(session)
	}
	rr := httptest.NewRecorder()
	if mode == oauthModeLink {
		req.Method = "POST"
		a.handleOAuthLink(provider)(rr, req)
	} else {
		a.handleOAuthLogin(provider)(rr, req)
	}
	if rr.Code != http.StatusFound {
		t.Fatalf("expected a redirect to the provider, got %d: %s", rr.Code, rr.Body.String())
	}
	loc, err := url.Parse(rr.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	nonce := loc.Query().Get("state")
	if nonce == "" {
		t.Fatal("no state parameter sent to the provider")
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

// callback replays the provider's redirect back to the router.
func callback(a *Admin, provider string, stateCookie *http.Cookie, nonce string, session *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET",
		oauthCallbackPath(provider)+"?code=abc123&state="+url.QueryEscape(nonce), nil)
	if stateCookie != nil {
		req.AddCookie(stateCookie)
	}
	if session != nil {
		req.AddCookie(session)
	}
	rr := httptest.NewRecorder()
	a.handleOAuthCallback(provider)(rr, req)
	return rr
}

func sessionCookieFrom(rr *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rr.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			return c
		}
	}
	return nil
}

func TestOAuthLoginRefusesUnlinkedAccount(t *testing.T) {
	forEachProvider(t, func(t *testing.T, key string) {
		a, _ := newOAuthTestAdmin(t, key)
		addTestUser(t, a, "alice", "member")

		stateCookie, nonce := startAuthorization(t, a, key, oauthModeLogin, nil)
		rr := callback(a, key, stateCookie, nonce, nil)

		if rr.Code != http.StatusOK {
			t.Fatalf("expected the login page, got %d", rr.Code)
		}
		if !strings.Contains(rr.Body.String(), "not linked to an llmesh account") {
			t.Fatalf("expected a refusal explaining how to link, got:\n%s", rr.Body.String())
		}
		// Crucially, no account was created for the provider identity.
		if got := len(a.state.Users()); got != 1 {
			t.Fatalf("an unlinked sign-in provisioned an account: %d users", got)
		}
		if sessionCookieFrom(rr) != nil {
			t.Fatal("a session was issued for an unlinked account")
		}
	})
}

func TestOAuthLinkThenLogin(t *testing.T) {
	forEachProvider(t, func(t *testing.T, key string) {
		a, f := newOAuthTestAdmin(t, key)
		addTestUser(t, a, "alice", "member")
		session := signIn(a, "alice")

		// Link, as the user would from their own settings page.
		stateCookie, nonce := startAuthorization(t, a, key, oauthModeLink, session)
		rr := callback(a, key, stateCookie, nonce, session)
		if rr.Code != http.StatusOK {
			t.Fatalf("link callback: got %d: %s", rr.Code, rr.Body.String())
		}
		u, _ := a.state.LookupUser("alice")
		got := oauthProviders[key].get(u)
		if got.ID != f.account.id || got.Label != f.account.label {
			t.Fatalf("link did not record the identity: %+v", got)
		}

		// Now sign in with it, from no session at all.
		stateCookie, nonce = startAuthorization(t, a, key, oauthModeLogin, nil)
		rr = callback(a, key, stateCookie, nonce, nil)
		if rr.Code != http.StatusFound || rr.Header().Get("Location") != "/portal/" {
			t.Fatalf("expected a redirect into the portal, got %d %q", rr.Code, rr.Header().Get("Location"))
		}
		c := sessionCookieFrom(rr)
		if c == nil {
			t.Fatal("no session cookie issued")
		}
		if name, ok := a.sessions.lookup(c.Value); !ok || name != "alice" {
			t.Fatalf("session is for %q (ok=%v), want alice", name, ok)
		}
	})
}

func TestOAuthLoginRefusesDisabledAccount(t *testing.T) {
	forEachProvider(t, func(t *testing.T, key string) {
		a, f := newOAuthTestAdmin(t, key)
		addTestUser(t, a, "alice", "member")
		linkIdentity(t, a, "alice", key, oauthIdentity{ID: f.account.id, Label: f.account.label})
		if err := a.state.UpdateUser("alice", func(u *User) { u.Disabled = true }); err != nil {
			t.Fatal(err)
		}

		stateCookie, nonce := startAuthorization(t, a, key, oauthModeLogin, nil)
		rr := callback(a, key, stateCookie, nonce, nil)
		if !strings.Contains(rr.Body.String(), "Account disabled") {
			t.Fatalf("a disabled account signed in:\n%s", rr.Body.String())
		}
		if sessionCookieFrom(rr) != nil {
			t.Fatal("a session was issued for a disabled account")
		}
	})
}

func TestOAuthCallbackRejectsBadState(t *testing.T) {
	forEachProvider(t, func(t *testing.T, key string) {
		a, f := newOAuthTestAdmin(t, key)
		addTestUser(t, a, "alice", "member")
		linkIdentity(t, a, "alice", key, oauthIdentity{ID: f.account.id})

		stateCookie, _ := startAuthorization(t, a, key, oauthModeLogin, nil)

		// An attacker-chosen code with a state that does not match the cookie
		// is the classic login-CSRF shape, and must not produce a session.
		rr := callback(a, key, stateCookie, "not-the-nonce", nil)
		if !strings.Contains(rr.Body.String(), "could not be verified") {
			t.Fatalf("mismatched state was accepted:\n%s", rr.Body.String())
		}
		if sessionCookieFrom(rr) != nil {
			t.Fatal("a session was issued despite a bad state")
		}

		// With no cookie at all it must also fail.
		rr = callback(a, key, nil, "anything", nil)
		if !strings.Contains(rr.Body.String(), "could not be verified") {
			t.Fatalf("a missing state cookie was accepted:\n%s", rr.Body.String())
		}
	})
}

// TestOAuthStateIsBoundToItsProvider is the test that only exists because there
// is more than one provider: a state minted for one must not be spendable at
// another's callback, or the weaker of the two would set the bar for both.
func TestOAuthStateIsBoundToItsProvider(t *testing.T) {
	a, _ := newOAuthTestAdmin(t, providerGitHub)
	ghAccount := testAccounts[providerGitHub]
	googleFake := configureFakeProvider(t, a, providerGoogle, googleAccount("sub-evil", "evil@example.com"))
	_ = googleFake

	addTestUser(t, a, "alice", "member")
	linkIdentity(t, a, "alice", providerGitHub, oauthIdentity{ID: ghAccount.id, Label: ghAccount.label})

	// Start a GitHub authorization, then present its state at Google's callback.
	stateCookie, nonce := startAuthorization(t, a, providerGitHub, oauthModeLogin, nil)
	rr := callback(a, providerGoogle, stateCookie, nonce, nil)

	if !strings.Contains(rr.Body.String(), "could not be verified") {
		t.Fatalf("a state minted for GitHub was spent at Google's callback:\n%s", rr.Body.String())
	}
	if sessionCookieFrom(rr) != nil {
		t.Fatal("a session was issued across providers")
	}
}

func TestOAuthStateCookieIsSpent(t *testing.T) {
	forEachProvider(t, func(t *testing.T, key string) {
		a, f := newOAuthTestAdmin(t, key)
		addTestUser(t, a, "alice", "member")
		linkIdentity(t, a, "alice", key, oauthIdentity{ID: f.account.id})

		stateCookie, nonce := startAuthorization(t, a, key, oauthModeLogin, nil)
		rr := callback(a, key, stateCookie, nonce, nil)

		cleared := false
		for _, c := range rr.Result().Cookies() {
			if c.Name == oauthStateCookie && c.MaxAge < 0 {
				cleared = true
			}
		}
		if !cleared {
			t.Fatal("the state cookie was not cleared, so it could be replayed")
		}
	})
}

func TestOAuthFlowRefusedWhenNotConfigured(t *testing.T) {
	forEachProvider(t, func(t *testing.T, key string) {
		a := newTestAdmin(t)
		rr := httptest.NewRecorder()
		a.handleOAuthLogin(key)(rr, httptest.NewRequest("GET", oauthStartPath(key), nil))
		if rr.Code != http.StatusNotFound {
			t.Fatalf("expected 404 with the provider unconfigured, got %d", rr.Code)
		}

		rr = httptest.NewRecorder()
		a.handleOAuthCallback(key)(rr, httptest.NewRequest("GET", oauthCallbackPath(key)+"?code=x&state=y", nil))
		if rr.Code != http.StatusNotFound {
			t.Fatalf("expected the callback to 404 too, got %d", rr.Code)
		}
	})
}

func TestOAuthTokenErrorDoesNotLeakToBrowser(t *testing.T) {
	forEachProvider(t, func(t *testing.T, key string) {
		a, f := newOAuthTestAdmin(t, key)
		f.tokenErr = "bad_verification_code"
		addTestUser(t, a, "alice", "member")

		stateCookie, nonce := startAuthorization(t, a, key, oauthModeLogin, nil)
		rr := callback(a, key, stateCookie, nonce, nil)
		body := rr.Body.String()
		if strings.Contains(body, "bad_verification_code") {
			t.Fatalf("the provider's error text was shown to the browser:\n%s", body)
		}
		if !strings.Contains(body, "Could not complete") {
			t.Fatalf("expected a generic failure message, got:\n%s", body)
		}
	})
}

func TestOAuthLoginRefreshesChangedLabel(t *testing.T) {
	// Matching is on the account id, so renaming at the provider must not lock
	// the user out — and the portal should stop showing the stale name.
	forEachProvider(t, func(t *testing.T, key string) {
		a, f := newOAuthTestAdmin(t, key)
		addTestUser(t, a, "alice", "member")
		linkIdentity(t, a, "alice", key, oauthIdentity{ID: f.account.id, Label: "stale-name"})

		stateCookie, nonce := startAuthorization(t, a, key, oauthModeLogin, nil)
		if rr := callback(a, key, stateCookie, nonce, nil); rr.Code != http.StatusFound {
			t.Fatalf("expected sign-in to succeed, got %d: %s", rr.Code, rr.Body.String())
		}
		u, _ := a.state.LookupUser("alice")
		if got := oauthProviders[key].get(u); got.Label != f.account.label {
			t.Fatalf("stale label kept: %q", got.Label)
		}
	})
}

func TestOAuthIdentityCannotBeLinkedTwice(t *testing.T) {
	forEachProvider(t, func(t *testing.T, key string) {
		a, f := newOAuthTestAdmin(t, key)
		addTestUser(t, a, "alice", "member")
		addTestUser(t, a, "bob", "member")
		linkIdentity(t, a, "alice", key, oauthIdentity{ID: f.account.id, Label: f.account.label})

		bobSession := signIn(a, "bob")
		stateCookie, nonce := startAuthorization(t, a, key, oauthModeLink, bobSession)
		rr := callback(a, key, stateCookie, nonce, bobSession)
		if !strings.Contains(rr.Body.String(), "already linked to another user") {
			t.Fatalf("expected the second claim to be refused, got:\n%s", rr.Body.String())
		}
		bob, _ := a.state.LookupUser("bob")
		if oauthProviders[key].get(bob).ID != "" {
			t.Fatalf("bob took alice's identity: %+v", bob)
		}
		alice, _ := a.state.LookupUser("alice")
		if oauthProviders[key].get(alice).ID != f.account.id {
			t.Fatal("alice lost her linked identity")
		}
	})
}

func TestOAuthUnlink(t *testing.T) {
	forEachProvider(t, func(t *testing.T, key string) {
		a, f := newOAuthTestAdmin(t, key)
		addTestUser(t, a, "alice", "member")
		linkIdentity(t, a, "alice", key, oauthIdentity{ID: f.account.id, Label: f.account.label})

		req := httptest.NewRequest("POST", "/portal/settings/"+key+"/unlink", nil)
		req.AddCookie(signIn(a, "alice"))
		rr := httptest.NewRecorder()
		a.requireAuth(a.handleOAuthUnlink(key))(rr, req)

		u, _ := a.state.LookupUser("alice")
		if got := oauthProviders[key].get(u); got.ID != "" || got.Label != "" {
			t.Fatalf("unlink left the identity behind: %+v", got)
		}
		// And the identity is free for someone else to claim.
		if _, ok := a.state.LookupUserByOAuth(key, f.account.id); ok {
			t.Fatal("the unlinked id still resolves to an account")
		}
	})
}

// TestOAuthIdentitiesAreIndependentAcrossProviders pins that linking or
// unlinking one provider leaves the other alone — the failure a single set of
// shared columns would produce.
func TestOAuthIdentitiesAreIndependentAcrossProviders(t *testing.T) {
	a, ghFake := newOAuthTestAdmin(t, providerGitHub)
	googleFake := configureFakeProvider(t, a, providerGoogle, testAccounts[providerGoogle])
	addTestUser(t, a, "alice", "member")
	session := signIn(a, "alice")

	for key := range map[string]bool{providerGitHub: true, providerGoogle: true} {
		stateCookie, nonce := startAuthorization(t, a, key, oauthModeLink, session)
		if rr := callback(a, key, stateCookie, nonce, session); rr.Code != http.StatusOK {
			t.Fatalf("%s link: got %d: %s", key, rr.Code, rr.Body.String())
		}
	}

	u, _ := a.state.LookupUser("alice")
	if u.GitHubUserID != ghFake.account.id || u.GoogleUserID != googleFake.account.id {
		t.Fatalf("both links should be present: %+v", u)
	}

	// Either identity now signs her in.
	for _, key := range []string{providerGitHub, providerGoogle} {
		stateCookie, nonce := startAuthorization(t, a, key, oauthModeLogin, nil)
		if rr := callback(a, key, stateCookie, nonce, nil); rr.Code != http.StatusFound {
			t.Fatalf("%s sign-in failed: %d %s", key, rr.Code, rr.Body.String())
		}
	}

	// Unlinking one leaves the other working.
	req := httptest.NewRequest("POST", "/portal/settings/github/unlink", nil)
	req.AddCookie(session)
	a.requireAuth(a.handleOAuthUnlink(providerGitHub))(httptest.NewRecorder(), req)

	u, _ = a.state.LookupUser("alice")
	if u.GitHubUserID != "" {
		t.Fatal("github unlink did not take")
	}
	if u.GoogleUserID != googleFake.account.id {
		t.Fatalf("unlinking github took the google link with it: %+v", u)
	}
	stateCookie, nonce := startAuthorization(t, a, providerGoogle, oauthModeLogin, nil)
	if rr := callback(a, providerGoogle, stateCookie, nonce, nil); rr.Code != http.StatusFound {
		t.Fatalf("google sign-in broke after unlinking github: %d %s", rr.Code, rr.Body.String())
	}
}

func TestOAuthCallbackURLUsesEffectiveHost(t *testing.T) {
	forEachProvider(t, func(t *testing.T, key string) {
		a := newTestAdmin(t)
		if err := a.state.SetPortalHost("llm.example.com"); err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest("GET", "/portal/settings", nil)
		want := "http://llm.example.com/portal/auth/" + key + "/callback"
		if got := a.OAuthCallbackURL(req, key); got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
		req.Header.Set("X-Forwarded-Proto", "https")
		a.SetTrustProxy(true)
		want = "https://llm.example.com/portal/auth/" + key + "/callback"
		if got := a.OAuthCallbackURL(req, key); got != want {
			t.Fatalf("behind a TLS-terminating proxy, got %q, want %q", got, want)
		}
	})
}

// --- Provider wire formats ---

func TestGitHubIdentityParsing(t *testing.T) {
	ident, err := githubIdentity([]byte(`{"id":4242,"login":"octocat","name":"Mona"}`))
	if err != nil {
		t.Fatal(err)
	}
	if ident.ID != "4242" || ident.Label != "octocat" {
		t.Fatalf("got %+v", ident)
	}
	// A document with no id yields no identity, which the caller turns into an
	// error rather than an account match on "".
	if ident, err := githubIdentity([]byte(`{"login":"octocat"}`)); err != nil || ident.ID != "" {
		t.Fatalf("got %+v, %v", ident, err)
	}
	if _, err := githubIdentity([]byte(`not json`)); err == nil {
		t.Fatal("expected a decode error")
	}
}

func TestGoogleIdentityParsing(t *testing.T) {
	ident, err := googleIdentity([]byte(`{"sub":"11223344","email":"Alice@Example.COM","email_verified":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if ident.ID != "11223344" {
		t.Fatalf("the subject must be the identity, got %q", ident.ID)
	}
	if ident.Label != "alice@example.com" {
		t.Fatalf("the displayed address should be normalised, got %q", ident.Label)
	}

	// An unverified address is not displayed: showing it would read as a claim
	// this router had checked, and it has not.
	ident, err = googleIdentity([]byte(`{"sub":"11223344","email":"alice@example.com","email_verified":false}`))
	if err != nil {
		t.Fatal(err)
	}
	if ident.Label == "alice@example.com" {
		t.Fatal("an unverified address was displayed as if confirmed")
	}
	if ident.ID != "11223344" || ident.Label != "11223344" {
		t.Fatalf("expected a fallback to the subject, got %+v", ident)
	}

	if _, err := googleIdentity([]byte(`not json`)); err == nil {
		t.Fatal("expected a decode error")
	}
}

// TestGoogleTokenExchangeSendsGrantType pins the parameter Google requires and
// GitHub does not, since the flow that omits it fails only against the real
// endpoint.
func TestGoogleTokenExchangeSendsGrantType(t *testing.T) {
	a, f := newOAuthTestAdmin(t, providerGoogle)
	addTestUser(t, a, "alice", "member")
	linkIdentity(t, a, "alice", providerGoogle, oauthIdentity{ID: f.account.id})

	stateCookie, nonce := startAuthorization(t, a, providerGoogle, oauthModeLogin, nil)
	if rr := callback(a, providerGoogle, stateCookie, nonce, nil); rr.Code != http.StatusFound {
		t.Fatalf("sign-in failed: %d %s", rr.Code, rr.Body.String())
	}
	if got := f.lastTokenForm.Get("grant_type"); got != "authorization_code" {
		t.Fatalf("grant_type sent as %q", got)
	}
	if got := f.lastTokenForm.Get("client_secret"); got != "shh" {
		t.Fatalf("client_secret sent as %q", got)
	}
}

// TestAuthorizationRequestParameters checks what each provider is actually
// asked for, since an over-broad scope is invisible until someone reads the
// consent screen.
func TestAuthorizationRequestParameters(t *testing.T) {
	wantScopes := map[string]string{
		providerGitHub: "read:user",
		providerGoogle: "openid email",
		providerOIDC:   "openid email profile",
	}
	forEachProvider(t, func(t *testing.T, key string) {
		a, _ := newOAuthTestAdmin(t, key)
		req := httptest.NewRequest("GET", oauthStartPath(key), nil)
		rr := httptest.NewRecorder()
		a.handleOAuthLogin(key)(rr, req)

		loc, err := url.Parse(rr.Header().Get("Location"))
		if err != nil {
			t.Fatal(err)
		}
		q := loc.Query()
		if got := q.Get("scope"); got != wantScopes[key] {
			t.Errorf("scope is %q, want %q", got, wantScopes[key])
		}
		if got := q.Get("response_type"); got != "code" {
			t.Errorf("response_type is %q, want code", got)
		}
		if got := q.Get("client_id"); got != "cid-"+key {
			t.Errorf("client_id is %q", got)
		}
		if !strings.HasSuffix(q.Get("redirect_uri"), oauthCallbackPath(key)) {
			t.Errorf("redirect_uri is %q", q.Get("redirect_uri"))
		}
	})

	// Provider-specific parameters.
	a, _ := newOAuthTestAdmin(t, providerGoogle)
	rr := httptest.NewRecorder()
	a.handleOAuthLogin(providerGoogle)(rr, httptest.NewRequest("GET", oauthStartPath(providerGoogle), nil))
	loc, _ := url.Parse(rr.Header().Get("Location"))
	if got := loc.Query().Get("prompt"); got != "select_account" {
		t.Errorf("google prompt is %q, want select_account", got)
	}

	a, _ = newOAuthTestAdmin(t, providerGitHub)
	rr = httptest.NewRecorder()
	a.handleOAuthLogin(providerGitHub)(rr, httptest.NewRequest("GET", oauthStartPath(providerGitHub), nil))
	loc, _ = url.Parse(rr.Header().Get("Location"))
	if got := loc.Query().Get("allow_signup"); got != "false" {
		t.Errorf("github allow_signup is %q, want false", got)
	}
}
