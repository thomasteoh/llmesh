package admin

import (
	"net/url"
	"path/filepath"
	"testing"

	"llmesh/router/internal/authz"
)

func TestMigrateSharing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, _ := LoadState(path)
	s.AddUser(User{Username: "alice", Role: "member"})
	s.AddUser(User{Username: "bob", Role: "member"})
	s.AddUser(User{Username: "carol", Role: "member"})
	// Recreate the pre-v2 shape: slots and flags on the old columns only.
	for _, q := range []string{
		`INSERT INTO client_tokens (token_hash, token_prefix, name, owner, created_at, owner_slots) VALUES ('h1', 'p', 'box', 'alice', '2026-01-01T00:00:00Z', '{"llama3":2,"any":1}')`,
		`INSERT INTO client_tokens (token_hash, token_prefix, name, owner, created_at, owner_slots) VALUES ('h2', 'p', 'mac', 'bob', '2026-01-01T00:00:00Z', '{}')`,
		`UPDATE users SET send_isolation = 1 WHERE username = 'bob'`,
		`UPDATE users SET receive_isolation = 1 WHERE username = 'carol'`,
		`DELETE FROM settings WHERE key = 'authz.migrated_sharing'`,
	} {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	s.db.Close()
	s, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}

	sh := s.ClientSharing("h1")
	if sh == nil || sh.Mode != authz.ShareOpen || sh.ReservedSlots["llama3"] != 2 || sh.ReservedSlots["*"] != 1 {
		t.Fatalf("h1 sharing: %+v", sh)
	}
	if s.ClientSharing("h2") != nil {
		t.Error("a token with no reservations got a sharing setting")
	}
	// Isolation became policies that behave as the flags did.
	s.AddClientToken(ClientToken{Name: "rig", Owner: "carol", TokenHash: "h3"})
	for _, tc := range []struct {
		req, owner, token string
		want              bool
	}{
		{"bob", "alice", "h1", false},   // send-isolated bob stays on his own clients
		{"bob", "bob", "h2", true},      // including his own
		{"alice", "carol", "h3", false}, // receive-isolated carol's client serves only her
		{"carol", "carol", "h3", true},
		{"alice", "bob", "h2", true}, // nothing else changes
	} {
		if got := s.PairClient(tc.req, tc.owner, tc.token).Allowed; got != tc.want {
			t.Errorf("%s on %s's client: %v, want %v", tc.req, tc.owner, got, tc.want)
		}
	}
	// Turning a flag off removes its policy.
	if err := s.SetUserIsolation("bob", false, false); err != nil {
		t.Fatal(err)
	}
	if !s.PairClient("bob", "alice", "h1").Allowed {
		t.Error("clearing send isolation did not take effect")
	}
}

func TestTeamClientSharing(t *testing.T) {
	s := newTestState(t)
	s.AddUser(User{Username: "mia", Role: "member"})
	s.AddUser(User{Username: "eve", Role: "member"})
	s.CreateTeam("research", "", "", "mia")
	s.AddClientToken(ClientToken{Name: "rig", Owner: "team:research", TokenHash: "th"})
	if err := s.SetClientSharing("th", authz.Sharing{Mode: authz.SharePrivate}); err != nil {
		t.Fatal(err)
	}
	if p := s.PairClient("mia", "team:research", "th"); !p.Allowed || !p.OwnerSide {
		t.Errorf("a team member on the team's private client: %+v", p)
	}
	if p := s.PairClient("team:research", "team:research", "th"); !p.Allowed || !p.OwnerSide {
		t.Errorf("the team's own key on its client: %+v", p)
	}
	if s.PairClient("eve", "team:research", "th").Allowed {
		t.Error("an outsider used a private team client")
	}
	s.SetClientSharing("th", authz.Sharing{Mode: authz.ShareIdle})
	if p := s.PairClient("eve", "team:research", "th"); !p.Allowed || !p.IdleOnly {
		t.Errorf("an outsider on a share-when-idle team client: %+v", p)
	}
}

func TestUpstreamJobsKeepWorking(t *testing.T) {
	s := newTestState(t)
	s.AddUser(User{Username: "alice", Role: "member"})
	s.AddClientToken(ClientToken{Name: "box", Owner: "alice", TokenHash: "h"})
	if !s.PairClient("upstream:hq", "alice", "h").Allowed {
		t.Error("a job from an upstream router can no longer run on a shared client")
	}
	s.SetClientSharing("h", authz.Sharing{Mode: authz.SharePrivate})
	if s.PairClient("upstream:hq", "alice", "h").Allowed {
		t.Error("an upstream job ran on a private client")
	}
}

func TestSharingCacheInvalidates(t *testing.T) {
	s := newTestState(t)
	s.AddUser(User{Username: "alice", Role: "member"})
	s.AddUser(User{Username: "bob", Role: "member"})
	s.AddClientToken(ClientToken{Name: "box", Owner: "alice", TokenHash: "h"})
	if !s.PairClient("bob", "alice", "h").Allowed {
		t.Fatal("bob refused on a shared client")
	}
	// A change to access reaches the next decision, not one after a TTL.
	s.SavePolicy(authz.Policy{ID: "no-bob", Effect: authz.Deny, Enabled: true, Actions: []string{"client.use"},
		Subject: authz.SubjectMatcher{IDs: []string{"user:bob"}}}, "t")
	if s.PairClient("bob", "alice", "h").Allowed {
		t.Error("a new deny policy did not reach a cached pairing")
	}
}

func TestClientSharingForm(t *testing.T) {
	a := newTestAdmin(t)
	withRole(t, a, "alice", authz.RoleMember)
	withRole(t, a, "bob", authz.RoleMember)
	a.state.AddClientToken(ClientToken{Name: "box", Owner: "alice", TokenHash: "h"})

	rr := postAs(t, a, "bob", "/portal/clients/sharing", url.Values{"token_hash": {"h"}, "mode": {"private"}}, a.handleClientSharing)
	if rr.Code != 403 {
		t.Errorf("bob changed alice's sharing: %d", rr.Code)
	}
	postAs(t, a, "alice", "/portal/clients/sharing", url.Values{"token_hash": {"h"}, "mode": {"idle"}}, a.handleClientSharing)
	if sh := a.state.ClientSharing("h"); sh == nil || sh.Mode != authz.ShareIdle {
		t.Fatalf("preset not saved: %+v", sh)
	}
	postAs(t, a, "alice", "/portal/clients/sharing", url.Values{"token_hash": {"h"}, "mode": {"idle"},
		"with": {"bob, team:research"}, "per_requester_max": {"2"}, "reserved_default": {"1"}}, a.handleClientSharing)
	sh := a.state.ClientSharing("h")
	if len(sh.With) != 2 || sh.With[0] != "user:bob" || sh.PerRequesterMax != 2 || sh.ReservedSlots["*"] != 1 {
		t.Fatalf("advanced not saved: %+v", sh)
	}
	// A preset click keeps the Advanced fields.
	postAs(t, a, "alice", "/portal/clients/sharing", url.Values{"token_hash": {"h"}, "mode": {"shared"}}, a.handleClientSharing)
	if sh := a.state.ClientSharing("h"); sh.Mode != authz.ShareOpen || len(sh.With) != 2 || sh.PerRequesterMax != 2 {
		t.Fatalf("a preset click lost the Advanced fields: %+v", sh)
	}
	rr = postAs(t, a, "alice", "/portal/clients/sharing", url.Values{"token_hash": {"h"}, "mode": {"sometimes"}}, a.handleClientSharing)
	if a.state.ClientSharing("h").Mode != authz.ShareOpen {
		t.Error("an invalid mode was saved")
	}
}
