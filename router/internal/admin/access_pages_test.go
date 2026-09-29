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
	// Owners can, and the legacy column follows.
	postAs(t, a, "root", "/portal/settings/users/roles/add", url.Values{"username": {"bob"}, "role": {authz.RoleOwner}}, a.handleUserRoleAdd)
	if u, _ := a.state.LookupUser("bob"); !a.state.IsOwner("bob") || u.Role != "admin" {
		t.Errorf("owner grant: owner=%v role=%q", a.state.IsOwner("bob"), u.Role)
	}
	postAs(t, a, "root", "/portal/settings/users/roles/remove", url.Values{"username": {"bob"}, "role": {authz.RoleOwner}}, a.handleUserRoleRemove)
	if u, _ := a.state.LookupUser("bob"); a.state.IsOwner("bob") || u.Role != "member" {
		t.Errorf("owner removal: owner=%v role=%q", a.state.IsOwner("bob"), u.Role)
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

	rr := postAs(t, a, "mia", "/portal/teams", url.Values{"id": {"research"}, "name": {"Research"}}, a.handleTeamCreate)
	if !strings.Contains(rr.Body.String(), "Team Research created") {
		t.Fatalf("create: %.300s", rr.Body.String())
	}
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
	// Promote bob, then he can manage.
	postAs(t, a, "mia", "/portal/teams/members/add", url.Values{"team": {"research"}, "username": {"bob"}, "maintainer": {"1"}}, a.handleTeamMemberAdd)
	rr = postAs(t, a, "bob", "/portal/teams/members/add", url.Values{"team": {"research"}, "username": {"eve"}}, a.handleTeamMemberAdd)
	if rr.Code == http.StatusForbidden {
		t.Error("a promoted maintainer still cannot manage the team")
	}
	// Disable and delete.
	postAs(t, a, "mia", "/portal/teams/state", url.Values{"team": {"research"}, "action": {"disable"}}, a.handleTeamState)
	if tm, _ := a.state.LookupTeam("research"); !tm.Disabled {
		t.Error("team was not disabled")
	}
	postAs(t, a, "mia", "/portal/teams/state", url.Values{"team": {"research"}, "action": {"delete"}}, a.handleTeamState)
	if _, ok := a.state.LookupTeam("research"); ok {
		t.Error("team was not deleted")
	}
	// Viewers cannot create teams (the route checks team.create).
	if routeStatus(t, a, "vic", "team.create") != http.StatusForbidden {
		t.Error("a viewer may create teams")
	}
}
