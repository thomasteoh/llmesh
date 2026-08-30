package admin

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// Creating a key or token is the one action whose response cannot be
// reconstructed: the secret is rendered once and only the hash is stored, so a
// caller that discards the response body has destroyed it. These pin the shape
// the page's script depends on — a page, not a 204 — because the script used to
// throw that body away, re-fetch the page, and leave the user with a key they
// had no way to read.

func portalPost(t *testing.T, a *Admin, h http.HandlerFunc, username, path string, form url.Values, portalFetch bool) *httptest.ResponseRecorder {
	t.Helper()
	sid := a.sessions.create(username)
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if portalFetch {
		r.Header.Set(portalFetchHeader, "1")
	}
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: sid})
	w := httptest.NewRecorder()
	a.requireAuth(h)(w, r)
	return w
}

func portalGet(t *testing.T, a *Admin, h http.HandlerFunc, username, path string) *httptest.ResponseRecorder {
	t.Helper()
	sid := a.sessions.create(username)
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Header.Set(portalFetchHeader, "1")
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: sid})
	w := httptest.NewRecorder()
	a.requireAuth(h)(w, r)
	return w
}

func adminWithUser(t *testing.T, username, role string) *Admin {
	t.Helper()
	a, _ := connTestAdmin(t)
	if err := a.state.AddUser(User{Username: username, Role: role}); err != nil {
		t.Fatal(err)
	}
	return a
}

var secretRe = regexp.MustCompile(`<code id="new-(?:key|tok)-val">([^<]+)</code>`)

func secretFrom(t *testing.T, body string) string {
	t.Helper()
	m := secretRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no one-shot secret rendered in the response:\n%s", truncateForLog(body))
	}
	return m[1]
}

func truncateForLog(s string) string {
	if len(s) > 1500 {
		return s[:1500] + "\n…truncated"
	}
	return s
}

// The created key must come back in the body of the POST itself. Answering a
// script-issued create with 204-and-a-location, as the other actions do, would
// be answering with nothing: there is no later request that can produce the
// key again.
func TestAPIKeyCreate_ReturnsTheKeyInThePostResponse(t *testing.T) {
	a := adminWithUser(t, "alice", "member")

	w := portalPost(t, a, a.handleAPIKeyCreate, "alice", "/portal/api-keys",
		url.Values{"label": {"prod"}, "priority": {"normal"}}, true)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type: got %q, want text/html", ct)
	}
	// A location header would tell the script to go and fetch a page instead of
	// reading this one, which is exactly how the key got lost.
	if loc := w.Header().Get("X-Portal-Location"); loc != "" {
		t.Errorf("X-Portal-Location: got %q, want none for a create", loc)
	}
	if key := secretFrom(t, w.Body.String()); !strings.HasPrefix(key, "sk-alice-") {
		t.Errorf("rendered secret %q does not look like alice's API key", key)
	}
}

// The reason the body matters: re-fetching the page, which is what the script
// did instead, cannot show the key. Only the hash was ever stored.
func TestAPIKeyCreate_KeyIsNotRecoverableFromALaterGet(t *testing.T) {
	a := adminWithUser(t, "alice", "member")

	created := portalPost(t, a, a.handleAPIKeyCreate, "alice", "/portal/api-keys",
		url.Values{"label": {"prod"}, "priority": {"normal"}}, true)
	key := secretFrom(t, created.Body.String())

	page := portalGet(t, a, a.handleAPIKeys, "alice", "/portal/api-keys")

	if page.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", page.Code)
	}
	if strings.Contains(page.Body.String(), key) {
		t.Fatal("the key came back on a later page load; it is supposed to be shown once")
	}
	// The key should still exist — it is the secret that is gone, not the record.
	if !strings.Contains(page.Body.String(), "prod") {
		t.Error("the key's label is missing from the page, so nothing was created")
	}
}

// Client tokens are the same one-shot secret with a different name, and were
// lost the same way.
func TestClientTokenCreate_ReturnsTheTokenInThePostResponse(t *testing.T) {
	a := adminWithUser(t, "alice", "member")

	w := portalPost(t, a, a.handleClientTokenCreate, "alice", "/portal/clients",
		url.Values{"name": {"gpu-box"}}, true)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", w.Code)
	}
	if loc := w.Header().Get("X-Portal-Location"); loc != "" {
		t.Errorf("X-Portal-Location: got %q, want none for a create", loc)
	}
	if tok := secretFrom(t, w.Body.String()); !strings.HasPrefix(tok, "ct-alice-") {
		t.Errorf("rendered secret %q does not look like alice's client token", tok)
	}
}

// A rejected create answers with the form and the reason. That is also carried
// only by the response body, so discarding it turned "Label is required" into
// a button that appeared to do nothing at all.
func TestCreate_ValidationErrorsAreInThePostResponse(t *testing.T) {
	tests := []struct {
		name    string
		handler func(a *Admin) http.HandlerFunc
		path    string
		form    url.Values
		want    string
	}{
		{
			name:    "api key without a label",
			handler: func(a *Admin) http.HandlerFunc { return a.handleAPIKeyCreate },
			path:    "/portal/api-keys",
			form:    url.Values{"label": {"  "}},
			want:    "Label is required.",
		},
		{
			name:    "client token without a name",
			handler: func(a *Admin) http.HandlerFunc { return a.handleClientTokenCreate },
			path:    "/portal/clients",
			form:    url.Values{"name": {"  "}},
			want:    "Name is required.",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := adminWithUser(t, "alice", "member")

			w := portalPost(t, a, tc.handler(a), "alice", tc.path, tc.form, true)

			if w.Code != http.StatusOK {
				t.Fatalf("status: got %d, want 200", w.Code)
			}
			if loc := w.Header().Get("X-Portal-Location"); loc != "" {
				t.Errorf("X-Portal-Location: got %q; a rejected create stays put", loc)
			}
			if !strings.Contains(w.Body.String(), tc.want) {
				t.Errorf("response does not carry %q:\n%s", tc.want, truncateForLog(w.Body.String()))
			}
			if secretRe.MatchString(w.Body.String()) {
				t.Error("a rejected create rendered a secret")
			}
		})
	}
}

// With scripting off the same post is a plain form navigation, and it has
// always answered with the rendered page. The enhancement is allowed to change;
// this is not.
func TestAPIKeyCreate_UnenhancedPostStillRendersTheKey(t *testing.T) {
	a := adminWithUser(t, "alice", "member")

	w := portalPost(t, a, a.handleAPIKeyCreate, "alice", "/portal/api-keys",
		url.Values{"label": {"prod"}, "priority": {"normal"}}, false)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", w.Code)
	}
	if key := secretFrom(t, w.Body.String()); !strings.HasPrefix(key, "sk-alice-") {
		t.Errorf("rendered secret %q does not look like an API key", key)
	}
}
