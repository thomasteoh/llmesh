package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"llmesh/pkg/types"
)

// requestAs returns a request carrying username's user and subject, as
// requireAuth would build it.
func requestAs(t *testing.T, a *Admin, username string) *http.Request {
	t.Helper()
	u, ok := a.state.LookupUser(username)
	if !ok {
		t.Fatalf("no user %s", username)
	}
	r := httptest.NewRequest("GET", "/portal/", nil)
	r = r.WithContext(context.WithValue(r.Context(), ctxUser, u))
	return a.withSubject(r, u)
}

func TestFilterQueue(t *testing.T) {
	a := newTestAdmin(t)
	addTestUser(t, a, "root", "admin")
	addTestUser(t, a, "alice", "member")
	addTestUser(t, a, "carol", "member")
	items := []types.InferenceRequest{
		{ID: "req-alice", Owner: "alice"},
		{ID: "req-bob", Owner: "bob"},
	}

	// Admin sees all.
	if got := a.filterQueue(requestAs(t, a, "root"), items); len(got) != 2 {
		t.Errorf("admin: expected 2, got %d", len(got))
	}
	// Member sees only own.
	got := a.filterQueue(requestAs(t, a, "alice"), items)
	if len(got) != 1 || got[0].Owner != "alice" {
		t.Fatalf("alice: got %+v", got)
	}
	// Member with no items sees nothing.
	if got := a.filterQueue(requestAs(t, a, "carol"), items); len(got) != 0 {
		t.Errorf("carol: expected 0, got %d", len(got))
	}
}

// A client that has never connected is not live: "never_connected" contains
// "connected", which once marked its owner live and sorted them first.
func TestClientGroupsLiveOnlyWhenConnected(t *testing.T) {
	groups := buildClientGroups([]ClientTokenRow{
		{ClientToken: ClientToken{Owner: "alice"}, Status: "never_connected"},
		{ClientToken: ClientToken{Owner: "bob"}, Status: "offline"},
		{ClientToken: ClientToken{Owner: "carol"}, Status: "connected"},
	})
	live := map[string]bool{}
	for _, g := range groups {
		live[g.Username] = g.HasLive
	}
	if live["alice"] || live["bob"] || !live["carol"] {
		t.Errorf("live owners: %v, want only carol", live)
	}
	if groups[0].Username != "carol" {
		t.Errorf("live owner not sorted first: %s", groups[0].Username)
	}
}
