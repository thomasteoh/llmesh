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

func TestGitHubAuthSettingsForm(t *testing.T) {
	a := newTestAdmin(t)
	addTestUser(t, a, "admin", "admin")

	rr := postAs(t, a, "admin", "/portal/settings/auth/github", url.Values{
		"enabled": {"on"}, "client_id": {"iv1.abc"}, "client_secret": {"shh"},
	}, a.handleGitHubAuthUpdate)
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rr.Code, rr.Body.String())
	}
	if !a.state.GitHubAuth().Configured() {
		t.Fatal("saving an enabled config did not configure GitHub sign-in")
	}
	// The secret must not come back out in the rendered page.
	if strings.Contains(rr.Body.String(), "shh") {
		t.Fatal("the client secret was rendered back to the browser")
	}

	// Editing the client ID with the secret field left blank — what the form
	// submits every time after the first — must not blank the secret.
	postAs(t, a, "admin", "/portal/settings/auth/github", url.Values{
		"enabled": {"on"}, "client_id": {"iv1.xyz"},
	}, a.handleGitHubAuthUpdate)
	if got := a.state.GitHubAuth(); got.ClientSecret != "shh" || got.ClientID != "iv1.xyz" {
		t.Fatalf("re-save mangled the config: %+v", got)
	}

	// Unchecking the box switches it off without losing the credentials.
	postAs(t, a, "admin", "/portal/settings/auth/github", url.Values{
		"client_id": {"iv1.xyz"},
	}, a.handleGitHubAuthUpdate)
	if got := a.state.GitHubAuth(); got.Configured() || got.ClientSecret == "" {
		t.Fatalf("disabling should keep credentials and stop offering sign-in: %+v", got)
	}
}

func TestGitHubClearSecretDisablesSignIn(t *testing.T) {
	a := newTestAdmin(t)
	addTestUser(t, a, "admin", "admin")
	if err := a.state.SetGitHubAuth(GitHubAuthConfig{Enabled: true, ClientID: "id", ClientSecret: "shh"}); err != nil {
		t.Fatal(err)
	}
	postAs(t, a, "admin", "/portal/settings/auth/github", url.Values{"clear_secret": {"1"}}, a.handleGitHubAuthUpdate)

	got := a.state.GitHubAuth()
	if got.ClientSecret != "" {
		t.Fatal("secret survived")
	}
	// Leaving it switched on would put a button on the login page that can only
	// ever fail.
	if got.Enabled {
		t.Fatal("GitHub sign-in is still on with no secret to complete it")
	}
}

func TestGitHubAuthSettingsRejectEnableWithoutCredentials(t *testing.T) {
	a := newTestAdmin(t)
	addTestUser(t, a, "admin", "admin")
	rr := postAs(t, a, "admin", "/portal/settings/auth/github", url.Values{"enabled": {"on"}}, a.handleGitHubAuthUpdate)
	if !strings.Contains(rr.Body.String(), "client ID is required") {
		t.Fatalf("expected a validation error, got:\n%s", rr.Body.String())
	}
	if a.state.GitHubAuth().Configured() {
		t.Fatal("an invalid save still enabled sign-in")
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

	for _, tc := range []struct {
		path    string
		handler http.HandlerFunc
	}{
		{"/portal/settings/auth/github", a.handleGitHubAuthUpdate},
		{"/portal/settings/auth/smtp", a.handleSMTPUpdate},
		{"/portal/settings/auth/smtp/test", a.handleSMTPTest},
	} {
		req := httptest.NewRequest("POST", tc.path, strings.NewReader(""))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(signIn(a, "bob"))
		rr := httptest.NewRecorder()
		a.requireAdmin(tc.handler)(rr, req)
		if rr.Code != http.StatusForbidden {
			t.Errorf("%s: a member got %d, want 403", tc.path, rr.Code)
		}
	}
	if a.state.SMTP().Configured() || a.state.GitHubAuth().Configured() {
		t.Fatal("a member changed the router's sign-in configuration")
	}
}

// TestSignInRoutesAreRegistered pins the routes to their paths, since the
// templates and the emailed links hard-code them.
func TestSignInRoutesAreRegistered(t *testing.T) {
	a := newTestAdmin(t)
	addTestUser(t, a, "admin", "admin")
	a.registerRoutes()

	for _, path := range []string{
		"/portal/login/magic",
		"/portal/login/magic/verify",
		"/portal/auth/github",
		"/portal/auth/github/callback",
		"/portal/settings/email",
		"/portal/settings/email/resend",
		"/portal/settings/email/verify",
		"/portal/settings/github/link",
		"/portal/settings/github/unlink",
		"/portal/settings/auth/github",
		"/portal/settings/auth/smtp",
		"/portal/settings/auth/smtp/test",
	} {
		if _, pattern := a.mux.Handler(httptest.NewRequest("GET", path, nil)); pattern != path {
			t.Errorf("%s resolves to %q, so it is not registered", path, pattern)
		}
	}
}
