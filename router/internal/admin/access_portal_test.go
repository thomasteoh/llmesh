package admin

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"llmesh/router/internal/authz"
)

// withRole creates a user holding exactly one router-wide role.
func withRole(t *testing.T, a *Admin, username, role string) {
	t.Helper()
	addTestUser(t, a, username, "member")
	if role == authz.RoleMember {
		return
	}
	if err := a.state.Bind(RoleBinding{Principal: userPrincipal(username), Role: role}); err != nil {
		t.Fatal(err)
	}
	if err := a.state.Unbind(RoleBinding{Principal: userPrincipal(username), Role: authz.RoleMember}); err != nil {
		t.Fatal(err)
	}
}

func routeStatus(t *testing.T, a *Admin, username, action string) int {
	t.Helper()
	req := httptest.NewRequest("GET", "/x", nil)
	req.AddCookie(signIn(a, username))
	rr := httptest.NewRecorder()
	a.requirePerm(action, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })(rr, req)
	return rr.Code
}

func TestRolesGateRoutes(t *testing.T) {
	a := newTestAdmin(t)
	withRole(t, a, "root", authz.RoleOwner)
	withRole(t, a, "op", authz.RoleOperator)
	withRole(t, a, "aud", authz.RoleAuditor)
	withRole(t, a, "mem", authz.RoleMember)

	for _, tc := range []struct {
		user, action string
		want         int
	}{
		{"root", "user.manage", 200},
		{"op", "alias.manage", 200},
		{"op", "pricing.manage", 200},
		{"op", "user.manage", 403},
		{"op", "settings.manage", 403},
		{"aud", "audit.view", 200},
		{"aud", "settings.view", 200},
		{"aud", "alias.manage", 403},
		{"aud", "user.manage", 403},
		{"mem", "alias.manage", 403},
		{"mem", "audit.view", 403},
	} {
		if got := routeStatus(t, a, tc.user, tc.action); got != tc.want {
			t.Errorf("%s → %s: %d, want %d", tc.user, tc.action, got, tc.want)
		}
	}
}

func TestAdminCannotChangeOwner(t *testing.T) {
	a := newTestAdmin(t)
	withRole(t, a, "root", authz.RoleOwner)
	withRole(t, a, "boss", authz.RoleOwner)
	withRole(t, a, "adm", authz.RoleAdmin)

	for path, h := range map[string]http.HandlerFunc{
		"/portal/settings/users/disable":        a.handleUserDisable,
		"/portal/settings/users/reset-password": a.handleUserResetPassword,
	} {
		rr := postAs(t, a, "adm", path, url.Values{"username": {"boss"}}, h)
		if !strings.Contains(rr.Body.String(), "Only an owner can change another owner") {
			t.Errorf("%s: an admin changed an owner: %d", path, rr.Code)
		}
	}
	if u, _ := a.state.LookupUser("boss"); u.Disabled || !a.state.IsOwner("boss") {
		t.Fatal("the owner's account was changed")
	}
	// An owner may.
	postAs(t, a, "root", "/portal/settings/users/disable", url.Values{"username": {"boss"}}, a.handleUserDisable)
	if u, _ := a.state.LookupUser("boss"); !u.Disabled {
		t.Fatal("an owner could not disable another owner")
	}
}

func TestLastAdminCannotBeDisabled(t *testing.T) {
	a := newTestAdmin(t)
	// A disabled owner does not count as an active admin.
	withRole(t, a, "old", authz.RoleOwner)
	a.state.UpdateUser("old", func(u *User) { u.Disabled = true })
	withRole(t, a, "adm", authz.RoleAdmin)
	// Someone holding every admin permission under a custom role, so they may
	// act on admins without counting as one themselves.
	var adminPerms []string
	for _, r := range authz.BuiltinRoles() {
		if r.ID == authz.RoleAdmin {
			adminPerms = r.Permissions
		}
	}
	if err := a.state.SaveRole(authz.Role{ID: "usermgr", Permissions: adminPerms}); err != nil {
		t.Fatal(err)
	}
	withRole(t, a, "hr", "usermgr")

	rr := postAs(t, a, "hr", "/portal/settings/users/disable", url.Values{"username": {"adm"}}, a.handleUserDisable)
	if !strings.Contains(rr.Body.String(), "Cannot disable the last admin account") {
		t.Fatalf("expected the last-admin guard, got %d: %.300s", rr.Code, rr.Body.String())
	}
	if u, _ := a.state.LookupUser("adm"); u.Disabled {
		t.Fatal("the last active admin was disabled")
	}
}

func TestTeamOwnedKeys(t *testing.T) {
	a := newTestAdmin(t)
	withRole(t, a, "mia", authz.RoleMember)
	withRole(t, a, "bob", authz.RoleMember)
	withRole(t, a, "eve", authz.RoleMember)
	if _, err := a.state.CreateTeam("research", "Research", "", "mia"); err != nil {
		t.Fatal(err)
	}
	a.state.AddTeamMember("research", "bob", false)

	// The maintainer can create a team key; the key names the team.
	rr := postAs(t, a, "mia", "/portal/api-keys", url.Values{"label": {"shared"}, "for_user": {"team:research"}}, a.handleAPIKeyCreate)
	if !strings.Contains(rr.Body.String(), "sk-team-research-") {
		t.Fatalf("maintainer could not create a team key:\n%.500s", rr.Body.String())
	}
	keys := a.state.APIKeysFor("team:research", false)
	if len(keys) != 1 {
		t.Fatalf("team keys: %+v", keys)
	}
	// A plain member cannot, and an outsider cannot see it.
	rr = postAs(t, a, "bob", "/portal/api-keys", url.Values{"label": {"mine"}, "for_user": {"team:research"}}, a.handleAPIKeyCreate)
	if !strings.Contains(rr.Body.String(), "do not have permission") {
		t.Error("a plain team member created a team key")
	}
	if got := a.visibleAPIKeys(requestAs(t, a, "bob")); len(got) != 1 {
		t.Errorf("a team member should see the team's key, saw %d", len(got))
	}
	if got := a.visibleAPIKeys(requestAs(t, a, "eve")); len(got) != 0 {
		t.Errorf("an outsider saw %d team keys", len(got))
	}
	// Only the maintainer can revoke it.
	postAs(t, a, "bob", "/portal/api-keys/revoke", url.Values{"key_hash": {keys[0].KeyHash}}, a.handleAPIKeyRevoke)
	if len(a.state.APIKeysFor("team:research", false)) != 1 {
		t.Fatal("a plain team member revoked the team key")
	}
	postAs(t, a, "mia", "/portal/api-keys/revoke", url.Values{"key_hash": {keys[0].KeyHash}}, a.handleAPIKeyRevoke)
	if len(a.state.APIKeysFor("team:research", false)) != 0 {
		t.Fatal("the maintainer could not revoke the team key")
	}
}

func TestMembersCannotActForOthers(t *testing.T) {
	a := newTestAdmin(t)
	withRole(t, a, "ann", authz.RoleMember)
	withRole(t, a, "bob", authz.RoleMember)
	withRole(t, a, "vic", authz.RoleViewer)

	rr := postAs(t, a, "ann", "/portal/api-keys", url.Values{"label": {"x"}, "for_user": {"bob"}}, a.handleAPIKeyCreate)
	if !strings.Contains(rr.Body.String(), "do not have permission") || len(a.state.APIKeysFor("bob", false)) != 0 {
		t.Error("a member created a key for someone else")
	}
	rr = postAs(t, a, "vic", "/portal/api-keys", url.Values{"label": {"x"}}, a.handleAPIKeyCreate)
	if !strings.Contains(rr.Body.String(), "do not have permission") || len(a.state.APIKeysFor("vic", false)) != 0 {
		t.Error("a viewer created a key")
	}
	a.state.AddAPIKey(testAPIKey("b", "bob", "sk-bob-zzz", "normal"))
	k, _ := a.state.LookupAPIKey("sk-bob-zzz")
	rr = postAs(t, a, "ann", "/portal/api-keys/revoke", url.Values{"key_hash": {k.KeyHash}}, a.handleAPIKeyRevoke)
	if rr.Code != http.StatusForbidden {
		t.Errorf("revoking someone else's key: %d, want 403", rr.Code)
	}
	rr = postAs(t, a, "ann", "/portal/api-keys/priority", url.Values{"key_hash": {k.KeyHash}, "priority": {"high"}}, a.handleAPIKeyPriority)
	if rr.Code != http.StatusForbidden {
		t.Errorf("a member changed a key's priority: %d", rr.Code)
	}
}

func TestUsageScopeIncludesTeams(t *testing.T) {
	a := newTestAdmin(t)
	withRole(t, a, "root", authz.RoleOwner)
	withRole(t, a, "mia", authz.RoleMember)
	a.state.CreateTeam("research", "", "", "mia")
	a.state.CreateTeam("ops", "", "", "root")

	if got := a.ownerScope(requestAs(t, a, "root"), "usage.view"); got != nil {
		t.Errorf("an owner's usage scope should be everyone, got %v", got)
	}
	got := a.ownerScope(requestAs(t, a, "mia"), "usage.view")
	if len(got) != 2 || got[0] != "mia" || got[1] != "team:research" {
		t.Errorf("mia's usage scope: %v", got)
	}
}

func TestCapabilitiesAndBadge(t *testing.T) {
	a := newTestAdmin(t)
	withRole(t, a, "op", authz.RoleOperator)
	withRole(t, a, "mem", authz.RoleMember)
	opReq := requestAs(t, a, "op")
	caps := a.capabilities(opReq)
	if !caps["alias_manage"] || caps["user_manage"] || !caps["key_limits_any"] {
		t.Errorf("operator capabilities: %v", caps)
	}
	if bp := a.newBasePage("dashboard", User{Username: "op"}, opReq); bp.RoleBadge != authz.RoleOperator {
		t.Errorf("operator badge: %q", bp.RoleBadge)
	}
	if bp := a.newBasePage("dashboard", User{Username: "mem"}, requestAs(t, a, "mem")); bp.RoleBadge != "" {
		t.Errorf("a member should have no badge, got %q", bp.RoleBadge)
	}
}

// user.manage alone cannot reach an admin: not by granting itself admin, not
// by promoting, and not by resetting an admin's password.
func TestUserManageCannotEscalate(t *testing.T) {
	a := newTestAdmin(t)
	withRole(t, a, "root", authz.RoleOwner)
	withRole(t, a, "adm", authz.RoleAdmin)
	if err := a.state.SaveRole(authz.Role{ID: "helpdesk", Permissions: []string{"user.manage", "user.view"}}); err != nil {
		t.Fatal(err)
	}
	withRole(t, a, "hd", "helpdesk")
	withRole(t, a, "bob", authz.RoleMember)

	postAs(t, a, "hd", "/portal/settings/users/roles/add", url.Values{"username": {"hd"}, "role": {authz.RoleAdmin}}, a.handleUserRoleAdd)
	if a.state.isPrivileged("hd") {
		t.Fatal("user.manage granted itself admin")
	}
	postAs(t, a, "hd", "/portal/settings/users/roles/add", url.Values{"username": {"bob"}, "role": {authz.RoleOperator}}, a.handleUserRoleAdd)
	if sameRoles(a.globalRoles("bob"), authz.RoleMember, authz.RoleOperator) {
		t.Error("helpdesk granted a role carrying permissions it lacks")
	}
	before, _ := a.state.LookupUser("adm")
	rr := postAs(t, a, "hd", "/portal/settings/users/reset-password", url.Values{"username": {"adm"}}, a.handleUserResetPassword)
	if after, _ := a.state.LookupUser("adm"); after.PasswordHash != before.PasswordHash {
		t.Fatalf("helpdesk reset an admin's password: %.200s", rr.Body.String())
	}
	// It can still manage a plain member.
	before, _ = a.state.LookupUser("bob")
	postAs(t, a, "hd", "/portal/settings/users/reset-password", url.Values{"username": {"bob"}}, a.handleUserResetPassword)
	if after, _ := a.state.LookupUser("bob"); after.PasswordHash == before.PasswordHash {
		t.Error("helpdesk could not reset a member's password")
	}
}
