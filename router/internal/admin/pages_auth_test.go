package admin

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// postAs submits a form to handler as username, through requireAuth so the
// handler sees a real session user.
func postAs(t *testing.T, a *Admin, username, path string, form url.Values, handler http.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(signIn(a, username))
	rr := httptest.NewRecorder()
	a.requireAuth(handler)(rr, req)
	return rr
}

func TestOAuthSettingsForm(t *testing.T) {
	forEachProvider(t, func(t *testing.T, key string) {
		a := newTestAdmin(t)
		addTestUser(t, a, "admin", "admin")
		path := "/portal/settings/auth/" + key

		rr := postAs(t, a, "admin", path, url.Values{
			"enabled": {"on"}, "client_id": {"cid-1"}, "client_secret": {"shh"},
		}, a.handleOAuthSettingsUpdate(key))
		if rr.Code != http.StatusOK {
			t.Fatalf("got %d: %s", rr.Code, rr.Body.String())
		}
		if !a.state.OAuth(key).Configured() {
			t.Fatal("saving an enabled config did not configure this sign-in")
		}
		// The secret must not come back out in the rendered page.
		if strings.Contains(rr.Body.String(), "shh") {
			t.Fatal("the client secret was rendered back to the browser")
		}

		// Editing the client ID with the secret field left blank — what the
		// form submits every time after the first — must not blank the secret.
		postAs(t, a, "admin", path, url.Values{
			"enabled": {"on"}, "client_id": {"cid-2"},
		}, a.handleOAuthSettingsUpdate(key))
		if got := a.state.OAuth(key); got.ClientSecret != "shh" || got.ClientID != "cid-2" {
			t.Fatalf("re-save mangled the config: %+v", got)
		}

		// Unchecking the box switches it off without losing the credentials.
		postAs(t, a, "admin", path, url.Values{
			"client_id": {"cid-2"},
		}, a.handleOAuthSettingsUpdate(key))
		if got := a.state.OAuth(key); got.Configured() || got.ClientSecret == "" {
			t.Fatalf("disabling should keep credentials and stop offering sign-in: %+v", got)
		}
	})
}

func TestOAuthClearSecretDisablesSignIn(t *testing.T) {
	forEachProvider(t, func(t *testing.T, key string) {
		a := newTestAdmin(t)
		addTestUser(t, a, "admin", "admin")
		if err := a.state.SetOAuth(key, oauthProviders[key].name,
			OAuthConfig{Enabled: true, ClientID: "id", ClientSecret: "shh"}); err != nil {
			t.Fatal(err)
		}
		postAs(t, a, "admin", "/portal/settings/auth/"+key,
			url.Values{"clear_secret": {"1"}}, a.handleOAuthSettingsUpdate(key))

		got := a.state.OAuth(key)
		if got.ClientSecret != "" {
			t.Fatal("secret survived")
		}
		// Leaving it switched on would put a button on the login page that can
		// only ever fail.
		if got.Enabled {
			t.Fatal("sign-in is still on with no secret to complete it")
		}
	})
}

func TestOAuthSettingsRejectEnableWithoutCredentials(t *testing.T) {
	forEachProvider(t, func(t *testing.T, key string) {
		a := newTestAdmin(t)
		addTestUser(t, a, "admin", "admin")
		rr := postAs(t, a, "admin", "/portal/settings/auth/"+key,
			url.Values{"enabled": {"on"}}, a.handleOAuthSettingsUpdate(key))
		if !strings.Contains(rr.Body.String(), "client ID is required") {
			t.Fatalf("expected a validation error, got:\n%s", rr.Body.String())
		}
		// The error should name the provider the admin is looking at.
		if !strings.Contains(rr.Body.String(), oauthProviders[key].name) {
			t.Fatalf("the error does not name the provider:\n%s", rr.Body.String())
		}
		if a.state.OAuth(key).Configured() {
			t.Fatal("an invalid save still enabled sign-in")
		}
	})
}

// TestOAuthSettingsFormsDoNotCollide pins that saving one provider's card
// leaves the other's credentials alone — the failure a shared handler makes
// easiest to introduce.
func TestOAuthSettingsFormsDoNotCollide(t *testing.T) {
	a := newTestAdmin(t)
	addTestUser(t, a, "admin", "admin")

	postAs(t, a, "admin", "/portal/settings/auth/github", url.Values{
		"enabled": {"on"}, "client_id": {"gh-id"}, "client_secret": {"gh-secret"},
	}, a.handleOAuthSettingsUpdate(providerGitHub))
	postAs(t, a, "admin", "/portal/settings/auth/google", url.Values{
		"enabled": {"on"}, "client_id": {"goog-id"}, "client_secret": {"goog-secret"},
	}, a.handleOAuthSettingsUpdate(providerGoogle))

	if got := a.state.OAuth(providerGitHub); got.ClientID != "gh-id" || got.ClientSecret != "gh-secret" {
		t.Fatalf("saving google overwrote github: %+v", got)
	}
	if got := a.state.OAuth(providerGoogle); got.ClientID != "goog-id" || got.ClientSecret != "goog-secret" {
		t.Fatalf("google did not save independently: %+v", got)
	}

	// Clearing one secret switches that provider off and leaves the other on.
	postAs(t, a, "admin", "/portal/settings/auth/github",
		url.Values{"clear_secret": {"1"}}, a.handleOAuthSettingsUpdate(providerGitHub))
	if a.state.OAuth(providerGitHub).Configured() {
		t.Fatal("github should be off after clearing its secret")
	}
	if !a.state.OAuth(providerGoogle).Configured() {
		t.Fatal("clearing github's secret switched google off too")
	}
}

func TestSMTPSettingsForm(t *testing.T) {
	a := newTestAdmin(t)
	addTestUser(t, a, "admin", "admin")

	rr := postAs(t, a, "admin", "/portal/settings/auth/smtp", url.Values{
		"enabled": {"on"}, "host": {"smtp.example.com"}, "port": {""},
		"security": {"tls"}, "from": {"llmesh@example.com"},
		"username": {"llmesh"}, "password": {"hunter2"},
	}, a.handleSMTPUpdate)
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "hunter2") {
		t.Fatal("the SMTP password was rendered back to the browser")
	}
	got := a.state.SMTP()
	if !got.Configured() || got.Port != 465 || got.Security != "tls" {
		t.Fatalf("unexpected saved config: %+v", got)
	}

	// A non-numeric port is read as "use the default", not as an error that
	// loses the rest of the form.
	postAs(t, a, "admin", "/portal/settings/auth/smtp", url.Values{
		"enabled": {"on"}, "host": {"smtp.example.com"}, "port": {"abc"},
		"security": {"starttls"}, "from": {"llmesh@example.com"},
	}, a.handleSMTPUpdate)
	if got := a.state.SMTP(); got.Port != 587 {
		t.Fatalf("expected the STARTTLS default port, got %d", got.Port)
	}
	if got := a.state.SMTP(); got.Password != "hunter2" {
		t.Fatal("blank password field discarded the stored password")
	}
}

func TestSMTPSettingsRejectEnableWithoutHost(t *testing.T) {
	a := newTestAdmin(t)
	addTestUser(t, a, "admin", "admin")
	rr := postAs(t, a, "admin", "/portal/settings/auth/smtp", url.Values{
		"enabled": {"on"}, "from": {"a@b.com"},
	}, a.handleSMTPUpdate)
	if !strings.Contains(rr.Body.String(), "SMTP host is required") {
		t.Fatalf("expected a validation error, got:\n%s", rr.Body.String())
	}
	if a.state.SMTP().Configured() {
		t.Fatal("an invalid save still enabled email sign-in")
	}
}

func TestSMTPTestMessage(t *testing.T) {
	a, f := newMailTestAdmin(t)
	addTestUser(t, a, "admin", "admin")

	rr := postAs(t, a, "admin", "/portal/settings/auth/smtp/test",
		url.Values{"to": {"someone@example.com"}}, a.handleSMTPTest)
	if !strings.Contains(rr.Body.String(), "Test message sent to someone@example.com") {
		t.Fatalf("expected a success flash, got:\n%s", rr.Body.String())
	}
	msgs := f.messages()
	if len(msgs) != 1 || !strings.Contains(msgs[0], "To: someone@example.com") {
		t.Fatalf("test message not delivered: %v", msgs)
	}
}

func TestSMTPTestReportsRelayFailure(t *testing.T) {
	// An admin is watching this page, so the relay's own error has to reach
	// them rather than only the log.
	a := newTestAdmin(t)
	addTestUser(t, a, "admin", "admin")
	if err := a.state.SetSMTP(SMTPConfig{
		Enabled: true, Host: "127.0.0.1", Port: 1, From: "a@b.com", Security: "none",
	}); err != nil {
		t.Fatal(err)
	}
	rr := postAs(t, a, "admin", "/portal/settings/auth/smtp/test",
		url.Values{"to": {"someone@example.com"}}, a.handleSMTPTest)
	if !strings.Contains(rr.Body.String(), "Test message failed") {
		t.Fatalf("expected the failure to be reported, got:\n%s", rr.Body.String())
	}
}

func TestSignInConfigurationIsAdminOnly(t *testing.T) {
	a := newTestAdmin(t)
	addTestUser(t, a, "admin", "admin")
	addTestUser(t, a, "bob", "member")

	cases := []struct {
		path    string
		handler http.HandlerFunc
	}{
		{"/portal/settings/auth/smtp", a.handleSMTPUpdate},
		{"/portal/settings/auth/smtp/test", a.handleSMTPTest},
	}
	for _, key := range oauthProviderOrder {
		cases = append(cases, struct {
			path    string
			handler http.HandlerFunc
		}{"/portal/settings/auth/" + key, a.handleOAuthSettingsUpdate(key)})
	}
	for _, tc := range cases {
		req := httptest.NewRequest("POST", tc.path, strings.NewReader(""))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(signIn(a, "bob"))
		rr := httptest.NewRecorder()
		a.requireAdmin(tc.handler)(rr, req)
		if rr.Code != http.StatusForbidden {
			t.Errorf("%s: a member got %d, want 403", tc.path, rr.Code)
		}
	}
	if a.state.SMTP().Configured() {
		t.Fatal("a member changed the router's email sign-in configuration")
	}
	for _, key := range oauthProviderOrder {
		if a.state.OAuth(key).Configured() {
			t.Fatalf("a member configured %s sign-in", key)
		}
	}
}

// TestSignInRoutesAreRegistered pins the routes to their paths, since the
// templates and the emailed links hard-code them.
func TestSignInRoutesAreRegistered(t *testing.T) {
	a := newTestAdmin(t)
	addTestUser(t, a, "admin", "admin")
	a.registerRoutes()

	paths := []string{
		"/portal/login/magic",
		"/portal/login/magic/verify",
		"/portal/settings/email",
		"/portal/settings/email/resend",
		"/portal/settings/email/verify",
		"/portal/settings/auth/smtp",
		"/portal/settings/auth/smtp/test",
	}
	for _, key := range oauthProviderOrder {
		paths = append(paths,
			"/portal/auth/"+key,
			"/portal/auth/"+key+"/callback",
			"/portal/settings/"+key+"/link",
			"/portal/settings/"+key+"/unlink",
			"/portal/settings/auth/"+key,
		)
	}
	for _, path := range paths {
		if _, pattern := a.mux.Handler(httptest.NewRequest("GET", path, nil)); pattern != path {
			t.Errorf("%s resolves to %q, so it is not registered", path, pattern)
		}
	}
}
