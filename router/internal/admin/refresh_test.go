package admin

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Without the header the portal must behave exactly as it always has. This is
// what keeps every action working with scripting disabled, so it is the case
// worth pinning: the enhancement is allowed to change, the fallback is not.
func TestRedirectOrRefresh_PlainPostStillRedirects(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/portal/clients/revoke", nil)

	redirectOrRefresh(w, r, "/portal/clients")

	if w.Code != http.StatusFound {
		t.Errorf("status: got %d, want %d", w.Code, http.StatusFound)
	}
	if loc := w.Header().Get("Location"); loc != "/portal/clients" {
		t.Errorf("Location: got %q, want /portal/clients", loc)
	}
}

// fetch() follows redirects transparently, so answering an expired session with
// a 302 hands the page a 200 holding the login form. The action would look like
// it succeeded while doing nothing. A script-issued request has to get a status
// it cannot mistake for success; a browser form post still gets the redirect.
func TestRequireAuth_ExpiredSession(t *testing.T) {
	a := newTestAdmin(t)
	protected := a.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler ran without a session")
	})

	t.Run("form post redirects", func(t *testing.T) {
		w := httptest.NewRecorder()
		protected(w, httptest.NewRequest(http.MethodPost, "/portal/clients/revoke", nil))
		if w.Code != http.StatusFound {
			t.Errorf("status: got %d, want %d", w.Code, http.StatusFound)
		}
		if got := w.Header().Get("Location"); got != "/portal/login" {
			t.Errorf("Location: got %q, want /portal/login", got)
		}
	})

	t.Run("script post is refused", func(t *testing.T) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/portal/clients/revoke", nil)
		r.Header.Set(portalFetchHeader, "1")
		protected(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("status: got %d, want %d", w.Code, http.StatusUnauthorized)
		}
		if got := w.Header().Get("X-Portal-Location"); got != "/portal/login" {
			t.Errorf("X-Portal-Location: got %q, want /portal/login", got)
		}
	})
}

// A script-submitted action gets no body and no redirect to follow, just where
// the page should pull its content from.
func TestRedirectOrRefresh_FetchPostReturnsDestination(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/portal/clients/revoke", nil)
	r.Header.Set(portalFetchHeader, "1")

	redirectOrRefresh(w, r, "/portal/clients")

	if w.Code != http.StatusNoContent {
		t.Errorf("status: got %d, want %d", w.Code, http.StatusNoContent)
	}
	if got := w.Header().Get("X-Portal-Location"); got != "/portal/clients" {
		t.Errorf("X-Portal-Location: got %q, want /portal/clients", got)
	}
	if w.Header().Get("Location") != "" {
		t.Error("a 204 must not also carry a Location to follow")
	}
	if w.Body.Len() != 0 {
		t.Errorf("body: got %q, want empty", w.Body.String())
	}
}

// Several actions finish somewhere other than where they started, and some
// carry a #tab the page has to re-select, so the destination travels verbatim.
func TestRedirectOrRefresh_PreservesDestinationAndFragment(t *testing.T) {
	for _, dest := range []string{
		"/portal/settings#tab-upstreams",
		"/portal/settings#tab-users",
		"/portal/",
		"/portal/api-keys",
	} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/portal/anything", nil)
		r.Header.Set(portalFetchHeader, "1")

		redirectOrRefresh(w, r, dest)

		if got := w.Header().Get("X-Portal-Location"); got != dest {
			t.Errorf("got %q, want %q", got, dest)
		}
	}
}
