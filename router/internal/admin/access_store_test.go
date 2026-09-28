package admin

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"llmesh/router/internal/authz"
)

func bindingRoles(t *testing.T, s *State, principal string) []string {
	t.Helper()
	bs, err := s.Bindings(principal)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, b := range bs {
		r := b.Role
		if b.Team != "" {
			r += "@" + b.Team
		}
		out = append(out, r)
	}
	return out
}

func sameRoles(got []string, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// A pre-v2 database — users with only a role column — upgrades to bindings
// with no change in what anyone can do.
func TestMigrateAccessFromLegacyRoles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	// Recreate the pre-migration shape: users with roles and nothing else.
	for _, q := range []string{
		`INSERT INTO users (username, role) VALUES ('root', 'admin'), ('ann', 'admin'), ('bob', 'member')`,
		`DELETE FROM role_bindings`,
		`DELETE FROM policies`,
		`DELETE FROM settings WHERE key IN ('authz.migrated_v2', 'authz.version')`,
	} {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	s.db.Close()

	s, err = LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := bindingRoles(t, s, "user:root"); !sameRoles(got, authz.RoleOwner) {
		t.Errorf("root: %v", got)
	}
	if got := bindingRoles(t, s, "user:ann"); !sameRoles(got, authz.RoleOwner) {
		t.Errorf("every existing admin should become an owner, ann: %v", got)
	}
	if got := bindingRoles(t, s, "user:bob"); !sameRoles(got, authz.RoleMember) {
		t.Errorf("bob: %v", got)
	}
	ps, _ := s.Policies()
	if len(ps) != 1 || ps[0].ID != defaultModelPolicy.ID {
		t.Fatalf("expected the default model grant, got %+v", ps)
	}

	// Members can still use every model, and admins still do admin things.
	bob, _ := s.SubjectFor("bob")
	if !s.Authz().Can(authz.Request{Subject: bob, Action: "model.use", Resource: authz.Resource{Type: "model", ID: "anything"}}) {
		t.Error("a member lost model access in the upgrade")
	}
	root, _ := s.SubjectFor("root")
	if !s.Authz().Can(authz.Request{Subject: root, Action: "user.manage", Resource: authz.Resource{Type: "user"}}) {
		t.Error("an admin lost user management in the upgrade")
	}

	// Running again changes nothing.
	s.db.Close()
	s, _ = LoadState(path)
	if got := bindingRoles(t, s, "user:root"); !sameRoles(got, authz.RoleOwner) {
		t.Errorf("second run changed root: %v", got)
	}
}

func TestLegacyRoleStaysInStep(t *testing.T) {
	s := newTestState(t)
	if err := s.AddFirstAdmin(User{Username: "root", Role: "admin"}); err != nil {
		t.Fatal(err)
	}
	if got := bindingRoles(t, s, "user:root"); !sameRoles(got, authz.RoleOwner) {
		t.Errorf("first admin: %v", got)
	}
	s.AddUser(User{Username: "bob", Role: "member"})
	if got := bindingRoles(t, s, "user:bob"); !sameRoles(got, authz.RoleMember) {
		t.Errorf("new member: %v", got)
	}
	s.UpdateUser("bob", func(u *User) { u.Role = "admin" })
	if got := bindingRoles(t, s, "user:bob"); !sameRoles(got, authz.RoleAdmin) {
		t.Errorf("promotion should grant admin, not owner: %v", got)
	}
	if err := s.DemoteUser("root", "bob"); err != nil {
		t.Fatal(err)
	}
	if got := bindingRoles(t, s, "user:bob"); !sameRoles(got, authz.RoleMember) {
		t.Errorf("demotion: %v", got)
	}
}

func TestValidUsername(t *testing.T) {
	for name, ok := range map[string]bool{
		"alice": true, "a.b-c_d": true,
		"": false, "team:x": false, "a/b": false, "a b": false, strings.Repeat("x", 65): false,
	} {
		if err := ValidUsername(name); (err == nil) != ok {
			t.Errorf("ValidUsername(%q) = %v, want ok=%v", name, err, ok)
		}
	}
	s := newTestState(t)
	if err := s.AddUser(User{Username: "team:evil", Role: "member"}); err == nil {
		t.Error("a username shaped like a principal id was accepted")
	}
}

func TestTeams(t *testing.T) {
	s := newTestState(t)
	s.AddUser(User{Username: "mia", Role: "member"})
	s.AddUser(User{Username: "bob", Role: "member"})

	if _, err := s.CreateTeam("Bad ID!", "", "", "mia"); err == nil {
		t.Error("an invalid team id was accepted")
	}
	team, err := s.CreateTeam("research", "Research", "", "mia")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTeam("research", "Again", "", "mia"); err == nil {
		t.Error("a duplicate team id was accepted")
	}
	if got := bindingRoles(t, s, "user:mia"); !sameRoles(got, authz.RoleMember, authz.RoleTeamMaintainer+"@research") {
		t.Errorf("creator should maintain the team: %v", got)
	}
	if err := s.AddTeamMember(team.ID, "bob", false); err != nil {
		t.Fatal(err)
	}
	members, _ := s.TeamMembers(team.ID)
	if len(members) != 2 || !members[1].Maintainer || members[0].Maintainer {
		t.Errorf("members: %+v", members) // bob, mia (sorted)
	}

	// Membership reaches team resources through the engine.
	bob, _ := s.SubjectFor("bob")
	if len(bob.Teams) != 1 || bob.Teams[0] != "research" {
		t.Fatalf("bob's teams: %v", bob.Teams)
	}
	teamUsage := authz.Resource{Type: "usage", Team: "research"}
	if !s.Authz().Can(authz.Request{Subject: bob, Action: "usage.view", Resource: teamUsage}) {
		t.Error("a team member cannot see the team's usage")
	}
	teamKey := authz.Resource{Type: "key", Owner: "team:research", Team: "research"}
	if s.Authz().Can(authz.Request{Subject: bob, Action: "key.manage", Resource: teamKey}) {
		t.Error("a plain team member can manage team keys")
	}
	mia, _ := s.SubjectFor("mia")
	if !s.Authz().Can(authz.Request{Subject: mia, Action: "key.manage", Resource: teamKey}) {
		t.Error("the maintainer cannot manage team keys")
	}

	// A team-owned key works until the team is disabled.
	s.AddAPIKey(testAPIKey("shared", "team:research", "sk-team-abc123", "normal"))
	if !s.ValidAPIKey("sk-team-abc123") {
		t.Fatal("a team key was refused")
	}
	s.SetTeamDisabled(team.ID, true)
	if s.ValidAPIKey("sk-team-abc123") {
		t.Error("a disabled team's key still authenticates")
	}
	if bob, _ = s.SubjectFor("bob"); len(bob.Teams) != 0 {
		t.Error("membership of a disabled team still counts")
	}
	s.SetTeamDisabled(team.ID, false)

	// Leaving removes the team's bindings too.
	s.RemoveTeamMember(team.ID, "mia")
	if got := bindingRoles(t, s, "user:mia"); !sameRoles(got, authz.RoleMember) {
		t.Errorf("leaving kept team bindings: %v", got)
	}

	// Deleting the team takes its credentials with it.
	if err := s.DeleteTeam(team.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.LookupAPIKey("sk-team-abc123"); ok {
		t.Error("a deleted team's key survived")
	}
	if got := bindingRoles(t, s, "user:bob"); !sameRoles(got, authz.RoleMember) {
		t.Errorf("deleting the team kept its bindings: %v", got)
	}
}

func TestPrincipalActive(t *testing.T) {
	s := newTestState(t)
	s.AddUser(User{Username: "ann", Role: "member"})
	s.AddUser(User{Username: "off", Role: "member", Disabled: true})
	s.CreateTeam("t1", "", "", "")
	for p, want := range map[string]bool{
		"":             true, // legacy ownerless credential
		"user:ann":     true,
		"user:off":     false,
		"user:ghost":   true, // an owner that was never a user row
		"team:t1":      true,
		"team:ghost":   false, // a deleted team
		"router:x":     false,
		"no-separator": false,
	} {
		if got := s.PrincipalActive(p); got != want {
			t.Errorf("PrincipalActive(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestLastOwnerCannotBeUnbound(t *testing.T) {
	s := newTestState(t)
	s.AddFirstAdmin(User{Username: "root", Role: "admin"})
	if err := s.Unbind(RoleBinding{Principal: "user:root", Role: authz.RoleOwner}); err == nil {
		t.Fatal("the last owner was unbound")
	}
	s.AddUser(User{Username: "ann", Role: "member"})
	if err := s.Bind(RoleBinding{Principal: "user:ann", Role: authz.RoleOwner}); err != nil {
		t.Fatal(err)
	}
	if err := s.Unbind(RoleBinding{Principal: "user:root", Role: authz.RoleOwner}); err != nil {
		t.Fatalf("with a second owner, unbinding failed: %v", err)
	}
}

func TestBindValidates(t *testing.T) {
	s := newTestState(t)
	s.AddUser(User{Username: "ann", Role: "member"})
	for name, b := range map[string]RoleBinding{
		"unknown role":   {Principal: "user:ann", Role: "wizard"},
		"unknown user":   {Principal: "user:ghost", Role: authz.RoleMember},
		"unknown team":   {Principal: "user:ann", Role: authz.RoleTeamMember, Team: "ghost"},
		"bad principal":  {Principal: "ann", Role: authz.RoleMember},
		"router binding": {Principal: "router:up", Role: authz.RoleMember},
	} {
		if err := s.Bind(b); err == nil {
			t.Errorf("%s: bound", name)
		}
	}
}

func TestCustomRoles(t *testing.T) {
	s := newTestState(t)
	s.AddUser(User{Username: "ann", Role: "member"})
	if err := s.SaveRole(authz.Role{ID: authz.RoleAdmin, Permissions: []string{"audit.view"}}); err == nil {
		t.Error("a built-in role was overwritten")
	}
	if err := s.SaveRole(authz.Role{ID: "bad", Permissions: []string{"key.manage"}}); err == nil {
		t.Error("a role with an invalid permission was saved")
	}
	if err := s.SaveRole(authz.Role{ID: "billing", Name: "Billing", Permissions: []string{"usage.view.any", "pricing.manage"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Bind(RoleBinding{Principal: "user:ann", Role: "billing"}); err != nil {
		t.Fatal(err)
	}
	ann, _ := s.SubjectFor("ann")
	if !s.Authz().Can(authz.Request{Subject: ann, Action: "pricing.manage", Resource: authz.Resource{Type: "settings"}}) {
		t.Error("a custom role's permission did not take effect")
	}
	if err := s.DeleteRole("billing"); err != nil {
		t.Fatal(err)
	}
	ann, _ = s.SubjectFor("ann")
	if s.Authz().Can(authz.Request{Subject: ann, Action: "pricing.manage", Resource: authz.Resource{Type: "settings"}}) {
		t.Error("a deleted role's permission still applies")
	}
}

func TestPolicies(t *testing.T) {
	s := newTestState(t)
	s.AddUser(User{Username: "ann", Role: "member"})
	gpt := authz.Resource{Type: "model", ID: "gpt-4o"}
	ann, _ := s.SubjectFor("ann")
	if !s.Authz().Can(authz.Request{Subject: ann, Action: "model.use", Resource: gpt}) {
		t.Fatal("the default grant is missing")
	}

	bad := authz.Policy{ID: "broken", Effect: authz.Deny, Enabled: true, Actions: []string{"model.fly"}}
	if err := s.SavePolicy(bad, "root"); err == nil {
		t.Fatal("an invalid policy was saved")
	}
	if ps, _ := s.Policies(); len(ps) != 1 {
		t.Fatal("a refused policy was stored")
	}

	noGPT := authz.Policy{ID: "no-gpt", Effect: authz.Deny, Enabled: true, Actions: []string{"model.use"},
		Resource: authz.ResourceMatcher{Type: "model", IDs: []string{"gpt-*"}}}
	if err := s.SavePolicy(noGPT, "root"); err != nil {
		t.Fatal(err)
	}
	if s.Authz().Can(authz.Request{Subject: ann, Action: "model.use", Resource: gpt}) {
		t.Error("a saved deny policy did not take effect")
	}
	if err := s.DeletePolicy("no-gpt"); err != nil {
		t.Fatal(err)
	}
	if !s.Authz().Can(authz.Request{Subject: ann, Action: "model.use", Resource: gpt}) {
		t.Error("a deleted policy still applies")
	}
}

func TestSessionsPersistAndRevoke(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, _ := LoadState(path)
	s.AddUser(User{Username: "ann", Role: "member"})
	ss := newSessionStore(s)
	a, b := ss.create("ann"), ss.create("ann")

	// The id itself is never stored.
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE id_hash = ?`, a).Scan(&n)
	if n != 0 {
		t.Fatal("a session id is stored in plaintext")
	}

	// Sessions survive a restart.
	s.db.Close()
	s, _ = LoadState(path)
	ss = newSessionStore(s)
	if u, ok := ss.lookup(a); !ok || u != "ann" {
		t.Fatal("a session did not survive a restart")
	}

	// A password change keeps the session that made it.
	s.RevokeSessions("ann", a)
	if _, ok := ss.lookup(a); !ok {
		t.Error("the kept session was revoked")
	}
	if _, ok := ss.lookup(b); ok {
		t.Error("another session survived revocation")
	}

	// Disabling ends every session.
	s.UpdateUser("ann", func(u *User) { u.Disabled = true })
	if _, ok := ss.lookup(a); ok {
		t.Error("disabling the user left a session alive")
	}

	// An expired session is refused and removed.
	c := ss.create("ann")
	s.db.Exec(`UPDATE sessions SET expires_at = ? WHERE id_hash = ?`, time.Now().Add(-time.Minute).UTC().Format(time.RFC3339), hashToken(c))
	if _, ok := ss.lookup(c); ok {
		t.Error("an expired session was accepted")
	}
}

func TestPasswordChangeSignsOutOtherSessions(t *testing.T) {
	a := newTestAdmin(t)
	addTestUser(t, a, "ann", "member")
	other := a.sessions.create("ann")
	current := signIn(a, "ann")
	postAs2 := func(form url.Values) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/portal/settings/password", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(current)
		rr := httptest.NewRecorder()
		a.requireAuth(a.handleChangePassword)(rr, req)
		return rr
	}
	rr := postAs2(url.Values{"current": {"pw-ann"}, "new": {"new-password-1"}, "confirm": {"new-password-1"}})
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "other sessions have been signed out") {
		t.Fatalf("password change failed: %d %.400s", rr.Code, rr.Body.String())
	}
	if _, ok := a.sessions.lookup(other); ok {
		t.Error("another session survived a password change")
	}
	if _, ok := a.sessions.lookup(current.Value); !ok {
		t.Error("the session that changed the password was signed out")
	}
}

func TestAPIKeyExpiryAndLastUse(t *testing.T) {
	s := newTestState(t)
	s.AddUser(User{Username: "ann", Role: "member"})
	k := testAPIKey("short", "ann", "sk-ann-short1", "normal")
	k.ExpiresAt = time.Now().Add(-time.Second)
	s.AddAPIKey(k)
	if s.ValidAPIKey("sk-ann-short1") {
		t.Error("an expired key authenticated")
	}
	k = testAPIKey("long", "ann", "sk-ann-long1", "normal")
	k.ExpiresAt = time.Now().Add(time.Hour)
	s.AddAPIKey(k)
	if !s.ValidAPIKey("sk-ann-long1") {
		t.Fatal("an unexpired key was refused")
	}
	got, _ := s.LookupAPIKey("sk-ann-long1")
	if got.LastUsedAt.IsZero() || got.ExpiresAt.IsZero() {
		t.Errorf("last use or expiry not recorded: %+v", got)
	}
	if expired("garbage") != true {
		t.Error("an unreadable expiry was treated as valid")
	}
}

func TestAPIKeyFormSetsExpiry(t *testing.T) {
	a := newTestAdmin(t)
	addTestUser(t, a, "ann", "member")
	rr := postAs(t, a, "ann", "/portal/api-keys", url.Values{"label": {"tmp"}, "expires_days": {"7"}}, a.handleAPIKeyCreate)
	if rr.Code != http.StatusOK {
		t.Fatalf("create failed: %d", rr.Code)
	}
	keys := a.state.APIKeysFor("ann", false)
	if len(keys) != 1 {
		t.Fatalf("expected one key, got %d", len(keys))
	}
	if d := time.Until(keys[0].ExpiresAt); d < 6*24*time.Hour || d > 8*24*time.Hour {
		t.Errorf("expiry is %v away, want about 7 days", d)
	}
	rr = postAs(t, a, "ann", "/portal/api-keys", url.Values{"label": {"bad"}, "expires_days": {"-1"}}, a.handleAPIKeyCreate)
	if !strings.Contains(rr.Body.String(), "Expiry must be between") {
		t.Error("a negative expiry was accepted")
	}
}
