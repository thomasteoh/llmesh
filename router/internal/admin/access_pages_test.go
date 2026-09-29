package admin

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"llmesh/router/internal/authz"
)

func TestUserRoleAssignment(t *testing.T) {
	a := newTestAdmin(t)
	withRole(t, a, "root", authz.RoleOwner)
	withRole(t, a, "adm", authz.RoleAdmin)
	withRole(t, a, "bob", authz.RoleMember)

	rr := postAs(t, a, "adm", "/portal/settings/users/roles/add", url.Values{"username": {"bob"}, "role": {authz.RoleOperator}}, a.handleUserRoleAdd)
	if !strings.Contains(rr.Body.String(), "now has the operator role") {
		t.Fatalf("admin could not grant operator: %.300s", rr.Body.String())
	}
	if got := a.globalRoles("bob"); !sameRoles(got, authz.RoleMember, authz.RoleOperator) {
		t.Errorf("bob's roles: %v", got)
	}
	// Admins cannot mint owners.
	rr = postAs(t, a, "adm", "/portal/settings/users/roles/add", url.Values{"username": {"bob"}, "role": {authz.RoleOwner}}, a.handleUserRoleAdd)
	if !strings.Contains(rr.Body.String(), "Only an owner can grant") || a.state.IsOwner("bob") {
		t.Error("an admin granted the owner role")
	}
	// Team roles belong on the Teams page.
	rr = postAs(t, a, "adm", "/portal/settings/users/roles/add", url.Values{"username": {"bob"}, "role": {authz.RoleTeamMaintainer}}, a.handleUserRoleAdd)
	if !strings.Contains(rr.Body.String(), "Team roles are granted on the Teams page") {
		t.Error("a team role was bound router-wide")
	}
	// Owners can.
	postAs(t, a, "root", "/portal/settings/users/roles/add", url.Values{"username": {"bob"}, "role": {authz.RoleOwner}}, a.handleUserRoleAdd)
	if !a.state.IsOwner("bob") {
		t.Error("owner grant did not apply")
	}
	postAs(t, a, "root", "/portal/settings/users/roles/remove", url.Values{"username": {"bob"}, "role": {authz.RoleOwner}}, a.handleUserRoleRemove)
	if a.state.IsOwner("bob") {
		t.Error("owner removal did not apply")
	}
}

func TestLastAdminRoleCannotBeRemoved(t *testing.T) {
	a := newTestAdmin(t)
	withRole(t, a, "root", authz.RoleOwner)
	rr := postAs(t, a, "root", "/portal/settings/users/roles/remove", url.Values{"username": {"root"}, "role": {authz.RoleOwner}}, a.handleUserRoleRemove)
	if !a.state.IsOwner("root") {
		t.Fatalf("the last owner lost the role: %.300s", rr.Body.String())
	}
	// An admin who is the last privileged user cannot lose admin either.
	s := newTestState(t)
	s.AddUser(User{Username: "solo", Role: "admin"})
	if err := s.Unbind(RoleBinding{Principal: "user:solo", Role: authz.RoleAdmin}); err == nil {
		t.Fatal("the last admin lost the role")
	}
}

func TestSignOutEverywhere(t *testing.T) {
	a := newTestAdmin(t)
	withRole(t, a, "root", authz.RoleOwner)
	withRole(t, a, "bob", authz.RoleMember)
	bobSession := a.sessions.create("bob")
	postAs(t, a, "root", "/portal/settings/users/sign-out", url.Values{"username": {"bob"}}, a.handleUserSignOut)
	if _, ok := a.sessions.lookup(bobSession); ok {
		t.Fatal("bob's session survived sign out everywhere")
	}
}

func TestOwnSessionRevocation(t *testing.T) {
	a := newTestAdmin(t)
	withRole(t, a, "ann", authz.RoleMember)
	withRole(t, a, "bob", authz.RoleMember)
	annOther := a.sessions.create("ann")
	bobs := a.sessions.create("bob")

	// A user cannot end someone else's session by naming its hash.
	postAs(t, a, "ann", "/portal/settings/sessions/revoke", url.Values{"session": {hashToken(bobs)}}, a.handleSessionRevoke)
	if _, ok := a.sessions.lookup(bobs); !ok {
		t.Fatal("ann ended bob's session")
	}
	postAs(t, a, "ann", "/portal/settings/sessions/revoke", url.Values{"session": {hashToken(annOther)}}, a.handleSessionRevoke)
	if _, ok := a.sessions.lookup(annOther); ok {
		t.Fatal("ann could not end her own session")
	}
}

func TestCustomRoleCannotEscalate(t *testing.T) {
	a := newTestAdmin(t)
	withRole(t, a, "root", authz.RoleOwner)
	if err := a.state.SaveRole(authz.Role{ID: "rolemgr", Permissions: []string{"role.manage", "user.view", "usage.view.own"}}); err != nil {
		t.Fatal(err)
	}
	withRole(t, a, "rm", "rolemgr")

	rr := postAs(t, a, "rm", "/portal/settings/roles", url.Values{"id": {"sneaky"}, "perm": {"owner.manage"}}, a.handleRoleSave)
	if !strings.Contains(rr.Body.String(), "which you do not hold") {
		t.Fatalf("a role manager defined a role with a permission they lack: %.300s", rr.Body.String())
	}
	rr = postAs(t, a, "rm", "/portal/settings/roles", url.Values{"id": {"sneaky"}, "perm": {"usage.view.team"}}, a.handleRoleSave)
	if !strings.Contains(rr.Body.String(), "which you do not hold") {
		t.Error("team scope was granted by someone holding only own scope")
	}
	rr = postAs(t, a, "rm", "/portal/settings/roles", url.Values{"id": {"viewers"}, "name": {"Viewers"}, "perm": {"usage.view.own", "user.view"}}, a.handleRoleSave)
	if !strings.Contains(rr.Body.String(), "Role viewers saved") {
		t.Fatalf("a role within the manager's own permissions was refused: %.300s", rr.Body.String())
	}
	// The owner can grant anything.
	rr = postAs(t, a, "root", "/portal/settings/roles", url.Values{"id": {"auditplus"}, "perm": {"audit.view", "usage.view.any"}}, a.handleRoleSave)
	if !strings.Contains(rr.Body.String(), "saved") {
		t.Fatalf("owner could not save a role: %.300s", rr.Body.String())
	}
	postAs(t, a, "root", "/portal/settings/roles/delete", url.Values{"id": {"auditplus"}}, a.handleRoleDelete)
	if roles, _ := a.state.CustomRoles(); len(roles) != 2 {
		t.Fatalf("custom roles after delete: %+v", roles)
	}
}

func TestTeamsPage(t *testing.T) {
	a := newTestAdmin(t)
	withRole(t, a, "mia", authz.RoleMember)
	withRole(t, a, "bob", authz.RoleMember)
	withRole(t, a, "eve", authz.RoleMember)
	withRole(t, a, "vic", authz.RoleViewer)
	withRole(t, a, "root", authz.RoleOwner)

	// Members cannot create teams; an owner creates one and makes mia its
	// maintainer.
	if routeStatus(t, a, "mia", "team.create") != http.StatusForbidden {
		t.Error("a member may create teams")
	}
	rr := postAs(t, a, "root", "/portal/teams", url.Values{"id": {"research"}, "name": {"Research"}}, a.handleTeamCreate)
	if !strings.Contains(rr.Body.String(), "Team Research created") {
		t.Fatalf("create: %.300s", rr.Body.String())
	}
	a.state.RemoveTeamMember("research", "root")
	a.state.AddTeamMember("research", "mia", true)
	postAs(t, a, "mia", "/portal/teams/members/add", url.Values{"team": {"research"}, "username": {"bob"}}, a.handleTeamMemberAdd)
	if m, _ := a.state.TeamMembers("research"); len(m) != 2 {
		t.Fatalf("members: %+v", m)
	}
	// A plain member cannot manage the team; an outsider cannot see it.
	rr = postAs(t, a, "bob", "/portal/teams/members/add", url.Values{"team": {"research"}, "username": {"eve"}}, a.handleTeamMemberAdd)
	if rr.Code != http.StatusForbidden {
		t.Errorf("a plain member added someone: %d", rr.Code)
	}
	rr = postAs(t, a, "eve", "/portal/teams", nil, func(w http.ResponseWriter, r *http.Request) { a.handleTeams(w, r) })
	if strings.Contains(rr.Body.String(), "team:research") {
		t.Error("an outsider can see the team")
	}
	rr = postAs(t, a, "bob", "/portal/teams", nil, func(w http.ResponseWriter, r *http.Request) { a.handleTeams(w, r) })
	if !strings.Contains(rr.Body.String(), "team:research") || strings.Contains(rr.Body.String(), "Add member") {
		t.Error("a member should see the team without management controls")
	}
	// A maintainer cannot appoint maintainers; an owner can.
	rr = postAs(t, a, "mia", "/portal/teams/members/add", url.Values{"team": {"research"}, "username": {"bob"}, "maintainer": {"1"}}, a.handleTeamMemberAdd)
	if !strings.Contains(rr.Body.String(), "Only an admin can appoint") {
		t.Error("a maintainer appointed another maintainer")
	}
	postAs(t, a, "root", "/portal/teams/members/add", url.Values{"team": {"research"}, "username": {"bob"}, "maintainer": {"1"}}, a.handleTeamMemberAdd)
	rr = postAs(t, a, "bob", "/portal/teams/members/add", url.Values{"team": {"research"}, "username": {"eve"}}, a.handleTeamMemberAdd)
	if rr.Code == http.StatusForbidden {
		t.Error("a promoted maintainer still cannot manage the team")
	}
	// Only a router-wide team manager may disable, enable, or delete: a
	// maintainer must not be able to undo an admin's disable.
	rr = postAs(t, a, "mia", "/portal/teams/state", url.Values{"team": {"research"}, "action": {"disable"}}, a.handleTeamState)
	if rr.Code != http.StatusForbidden {
		t.Errorf("a maintainer disabled the team: %d", rr.Code)
	}
	postAs(t, a, "root", "/portal/teams/state", url.Values{"team": {"research"}, "action": {"disable"}}, a.handleTeamState)
	if tm, _ := a.state.LookupTeam("research"); !tm.Disabled {
		t.Error("team was not disabled")
	}
	if rr := postAs(t, a, "mia", "/portal/teams/state", url.Values{"team": {"research"}, "action": {"enable"}}, a.handleTeamState); rr.Code != http.StatusForbidden {
		t.Errorf("a maintainer re-enabled a team an owner disabled: %d", rr.Code)
	}
	postAs(t, a, "root", "/portal/teams/state", url.Values{"team": {"research"}, "action": {"delete"}}, a.handleTeamState)
	if _, ok := a.state.LookupTeam("research"); ok {
		t.Error("team was not deleted")
	}
	// Viewers cannot create teams (the route checks team.create).
	if routeStatus(t, a, "vic", "team.create") != http.StatusForbidden {
		t.Error("a viewer may create teams")
	}
}

func TestPolicyManageCannotEscalate(t *testing.T) {
	a := newTestAdmin(t)
	withRole(t, a, "root", authz.RoleOwner)
	if err := a.state.SaveRole(authz.Role{ID: "policies", Permissions: []string{"policy.manage", "policy.view"}}); err != nil {
		t.Fatal(err)
	}
	withRole(t, a, "pm", "policies")

	grabAll := `{"id":"grab","effect":"allow","enabled":true,"actions":["*"],"subject":{"ids":["user:pm"]}}`
	rr := postAs(t, a, "pm", "/portal/settings/policies", url.Values{"policy": {grabAll}}, a.handlePolicySave)
	if !strings.Contains(rr.Body.String(), "which you do not hold yourself") {
		t.Fatalf("a policy manager granted themselves everything: %.300s", rr.Body.String())
	}
	// Model grants are what policy managers are for.
	grant := `{"id":"finance-gpt","effect":"allow","enabled":true,"actions":["model.use"],"resource":{"type":"model","ids":["gpt-*"]}}`
	if rr := postAs(t, a, "pm", "/portal/settings/policies", url.Values{"policy": {grant}}, a.handlePolicySave); !strings.Contains(rr.Body.String(), "saved") {
		t.Errorf("a model grant was refused: %.300s", rr.Body.String())
	}
	// A disabled allow policy saved by an owner cannot be switched on by
	// someone who lacks its actions.
	off := authz.Policy{ID: "dormant", Effect: authz.Allow, Enabled: false, Actions: []string{"user.manage"},
		Subject: authz.SubjectMatcher{IDs: []string{"user:pm"}}}
	a.state.SavePolicy(off, "root")
	postAs(t, a, "pm", "/portal/settings/policies/toggle", url.Values{"id": {"dormant"}}, a.handleModelRuleToggle)
	for _, p := range a.policyRows() {
		if p.ID == "dormant" && p.Enabled {
			t.Error("a policy manager switched on a grant they do not hold")
		}
	}
}

func TestPoliciesCannotLockOutOwners(t *testing.T) {
	a := newTestAdmin(t)
	withRole(t, a, "root", authz.RoleOwner)
	lockout := `{"id":"lock","effect":"deny","enabled":true,"actions":["policy.manage"]}`
	rr := postAs(t, a, "root", "/portal/settings/policies", url.Values{"policy": {lockout}}, a.handlePolicySave)
	if !strings.Contains(rr.Body.String(), "no owner able to manage") {
		t.Fatalf("a policy locking out every owner was saved: %.300s", rr.Body.String())
	}
	// Denying it to everyone but one owner is fine.
	scoped := `{"id":"lock","effect":"deny","enabled":true,"actions":["policy.manage"],"subject":{"ids":["user:someone"]}}`
	if rr := postAs(t, a, "root", "/portal/settings/policies", url.Values{"policy": {scoped}}, a.handlePolicySave); !strings.Contains(rr.Body.String(), "saved") {
		t.Errorf("a targeted deny was refused: %.300s", rr.Body.String())
	}
}
