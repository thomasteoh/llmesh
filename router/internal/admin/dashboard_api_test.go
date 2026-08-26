package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"llmesh/router/internal/hub"
)

// getDashboard calls the dashboard endpoint as username and decodes the payload.
func getDashboard(t *testing.T, a *Admin, username string) dashboardJSON {
	t.Helper()
	sid := a.sessions.create(username)
	r := httptest.NewRequest(http.MethodGet, "/portal/api/dashboard", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: sid})
	w := httptest.NewRecorder()

	a.requireAuth(a.handleDashboardJSON)(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (%s)", w.Code, w.Body.String())
	}
	var out dashboardJSON
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v — body %s", err, w.Body.String())
	}
	return out
}

// dashboardFixture stands up an admin and a member, a client token, and one
// connected worker serving llama3 under an alias.
func dashboardFixture(t *testing.T) (*Admin, *hub.Hub) {
	t.Helper()
	a, h := connTestAdmin(t)
	for _, u := range []User{
		{Username: "root", Role: "admin"},
		{Username: "alice", Role: "member"},
	} {
		if err := a.state.AddUser(u); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.state.AddClientToken(ClientToken{
		Name: "gpu-box", Owner: "alice", TokenHash: "hash-alice", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.state.AddAlias("fast", "llama3"); err != nil {
		t.Fatal(err)
	}
	dialClient(t, h, "gpu-box", "alice", "hash-alice")
	waitFor(t, func() bool { return len(h.ActiveModels()) == 1 })
	return a, h
}

// The two model cards used to be sent as raw data nothing read, so they never
// refreshed. They are now rendered markup, and it has to actually carry the
// live state the poll exists to deliver.
func TestDashboardJSON_CarriesRenderedModelCards(t *testing.T) {
	a, _ := dashboardFixture(t)

	d := getDashboard(t, a, "root")

	if !strings.Contains(d.ActiveModelsHTML, "llama3") {
		t.Errorf("active models card does not mention the live model:\n%s", d.ActiveModelsHTML)
	}
	if !strings.Contains(d.AliasChainsHTML, "fast") {
		t.Errorf("alias chain card does not mention the alias:\n%s", d.AliasChainsHTML)
	}
	// The reachability badge is the part that changes as workers come and go.
	if !strings.Contains(d.AliasChainsHTML, "reachable") {
		t.Errorf("alias chain card does not show target reachability:\n%s", d.AliasChainsHTML)
	}
}

// The cards carry admin-only controls, and the poll injects them into the page
// verbatim. Rendering them for a member would hand that member working
// alias-editing markup, so the fragment has to be built with the caller's role.
func TestDashboardJSON_OmitsAdminControlsForMembers(t *testing.T) {
	a, _ := dashboardFixture(t)

	adminHTML := getDashboard(t, a, "root").ActiveModelsHTML
	if !strings.Contains(adminHTML, "/portal/model-aliases") {
		t.Fatalf("admin did not get the alias editor:\n%s", adminHTML)
	}

	memberHTML := getDashboard(t, a, "alice").ActiveModelsHTML
	if strings.Contains(memberHTML, "/portal/model-aliases") {
		t.Errorf("member was served the alias editor:\n%s", memberHTML)
	}
	if !strings.Contains(memberHTML, "llama3") {
		t.Errorf("member did not get the model list itself:\n%s", memberHTML)
	}

	memberChains := getDashboard(t, a, "alice").AliasChainsHTML
	if strings.Contains(memberChains, "/portal/model-aliases") {
		t.Errorf("member was served alias reorder controls:\n%s", memberChains)
	}
}

// The forms are posted straight back, so they must carry the caller's own CSRF
// token rather than a blank or a token minted for another session.
func TestDashboardJSON_CardsCarryTheCallersCSRFToken(t *testing.T) {
	a, _ := dashboardFixture(t)

	// Login mints the token; a session made directly has none, so set one.
	sid := a.sessions.create("root")
	const csrf = "csrf-for-this-session"
	a.sessions.setCSRF(sid, csrf)

	r := httptest.NewRequest(http.MethodGet, "/portal/api/dashboard", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: sid})
	w := httptest.NewRecorder()
	a.requireAuth(a.handleDashboardJSON)(w, r)

	var d dashboardJSON
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(d.ActiveModelsHTML, csrf) {
		t.Errorf("card markup does not carry the session's CSRF token:\n%s", d.ActiveModelsHTML)
	}
}

// With nothing to show the fragment must come back empty rather than absent, so
// the page can hide the card instead of leaving a stale one on screen.
func TestDashboardJSON_EmptyCardsWhenNothingIsLive(t *testing.T) {
	a, _ := connTestAdmin(t)
	if err := a.state.AddUser(User{Username: "root", Role: "admin"}); err != nil {
		t.Fatal(err)
	}

	d := getDashboard(t, a, "root")

	if strings.TrimSpace(d.ActiveModelsHTML) != "" {
		t.Errorf("expected no model rows, got:\n%s", d.ActiveModelsHTML)
	}
	if strings.TrimSpace(d.AliasChainsHTML) != "" {
		t.Errorf("expected no alias rows, got:\n%s", d.AliasChainsHTML)
	}
}

// The client table is rendered server-side and swapped in, so the poll's markup
// has to be the same markup the page was built from — including the status
// badge and the models summary, both of which the script used to spell out for
// itself and could therefore get wrong on its own.
func TestDashboardJSON_CarriesRenderedClientRows(t *testing.T) {
	a, h := connTestAdmin(t)
	if err := a.state.AddUser(User{Username: "root", Role: "admin"}); err != nil {
		t.Fatal(err)
	}
	for _, ct := range []ClientToken{
		{Name: "full", Owner: "alice", TokenHash: "hash-full", CreatedAt: time.Now()},
		{Name: "partial", Owner: "alice", TokenHash: "hash-partial", CreatedAt: time.Now()},
	} {
		if err := a.state.AddClientToken(ct); err != nil {
			t.Fatal(err)
		}
	}
	dialClientWithModels(t, h, "full", "alice", "hash-full", "gemma", "medium", "qwen")
	dialClientWithModels(t, h, "partial", "alice", "hash-partial", "gemma", "qwen")
	// Both clients must be registered, not merely connected. Waiting on the
	// model count returned as soon as "full" registered, since it alone serves
	// all three; "partial" was still mid-handshake and its row came back empty.
	waitFor(t, func() bool { return len(h.ConnectionsLoad()) == 2 })

	html := getDashboard(t, a, "root").ClientRowsHTML

	for _, want := range []string{
		"alice/full", "alice/partial",
		"badge connected", // the status badge, previously rebuilt in JS
		"all except medium",
		`title="gemma, qwen"`, // the summary's full list stays reachable
	} {
		if !strings.Contains(html, want) {
			t.Errorf("client rows missing %q:\n%s", want, html)
		}
	}
}

// With no tokens at all the rows still have to say so, or the table is silently
// blank after the first poll.
func TestDashboardJSON_ClientRowsRenderTheEmptyState(t *testing.T) {
	a, _ := connTestAdmin(t)
	if err := a.state.AddUser(User{Username: "root", Role: "admin"}); err != nil {
		t.Fatal(err)
	}

	html := getDashboard(t, a, "root").ClientRowsHTML

	if !strings.Contains(html, "No client tokens registered.") {
		t.Errorf("empty fleet did not render the empty row:\n%s", html)
	}
}
