package admin

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"llmesh/router/internal/authz"
)

// Portal pages and actions for access management v2 (phase 3): role
// assignment on the Users tab, custom roles, teams, and sessions.

// RoleOption is a role an admin can assign, for the Users tab's picker and
// the Roles tab.
type RoleOption struct {
	ID          string
	Name        string
	Description string
	Builtin     bool
	Permissions []string
	// TeamRole marks roles meant for team-scoped bindings; they are not
	// offered router-wide.
	TeamRole bool
}

// PermissionGroup is one resource type's permissions, for the custom role
// editor's checkbox grid.
type PermissionGroup struct {
	Resource    string
	Permissions []string
}

// roleOptions lists every role, built-in first.
func (a *Admin) roleOptions() []RoleOption {
	var out []RoleOption
	for _, r := range authz.BuiltinRoles() {
		out = append(out, RoleOption{ID: r.ID, Name: r.Name, Description: r.Description, Builtin: true,
			Permissions: r.Permissions,
			TeamRole:    r.ID == authz.RoleTeamMaintainer || r.ID == authz.RoleTeamMember})
	}
	custom, _ := a.state.CustomRoles()
	for _, r := range custom {
		name := r.Name
		if name == "" {
			name = r.ID
		}
		out = append(out, RoleOption{ID: r.ID, Name: name, Description: r.Description, Permissions: r.Permissions})
	}
	return out
}

// permissionGroups lists every grantable permission, grouped by resource.
func permissionGroups() []PermissionGroup {
	byRes := map[string][]string{}
	for _, action := range authz.Actions() {
		res, _, _ := strings.Cut(action, ".")
		if _, err := authz.ParsePermission(action); err == nil {
			byRes[res] = append(byRes[res], action)
			continue
		}
		for _, scope := range []string{"own", "team", "any"} {
			byRes[res] = append(byRes[res], action+"."+scope)
		}
	}
	var out []PermissionGroup
	for res, perms := range byRes {
		sort.Strings(perms)
		out = append(out, PermissionGroup{Resource: res, Permissions: perms})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Resource < out[j].Resource })
	return out
}

// globalRoles returns a user's router-wide role ids.
func (a *Admin) globalRoles(username string) []string {
	bs, _ := a.state.Bindings(userPrincipal(username))
	var out []string
	for _, b := range bs {
		if b.Team == "" {
			out = append(out, b.Role)
		}
	}
	return out
}

// roleChangeRefused returns why the user may not grant or remove role on
// target, or "" if they may. Granting or removing owner takes owner.manage;
// changing an owner's roles at all takes it too.
func (a *Admin) roleChangeRefused(r *http.Request, target, role string) string {
	if msg := a.userChangeRefused(r, target); msg != "" {
		return msg
	}
	if role == authz.RoleOwner && !a.canDo(r, "owner.manage") {
		return "Only an owner can grant or remove the owner role."
	}
	if !a.canGrantRole(r, role) {
		return "You cannot grant or remove the " + role + " role: it carries permissions you do not hold."
	}
	if msg := a.roleManagedElsewhere(target); msg != "" {
		return msg
	}
	return ""
}

func (a *Admin) handleUserRoleAdd(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	target, role := r.FormValue("username"), r.FormValue("role")
	if msg := a.roleChangeRefused(r, target, role); msg != "" {
		a.renderSettings(w, r, u, "", msg)
		return
	}
	if role == authz.RoleTeamMaintainer || role == authz.RoleTeamMember {
		a.renderSettings(w, r, u, "", "Team roles are granted on the Teams page.")
		return
	}
	if err := a.state.Bind(RoleBinding{Principal: userPrincipal(target), Role: role}); err != nil {
		a.renderSettings(w, r, u, "", err.Error())
		return
	}
	a.state.RecordAudit(u.Username, "role.bind", target+" "+role, a.clientIP(r))
	a.renderSettings(w, r, u, fmt.Sprintf("%s now has the %s role.", target, role), "")
}

func (a *Admin) handleUserRoleRemove(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	target, role := r.FormValue("username"), r.FormValue("role")
	if msg := a.roleChangeRefused(r, target, role); msg != "" {
		a.renderSettings(w, r, u, "", msg)
		return
	}
	if err := a.state.Unbind(RoleBinding{Principal: userPrincipal(target), Role: role}); err != nil {
		a.renderSettings(w, r, u, "", err.Error())
		return
	}
	a.state.RecordAudit(u.Username, "role.unbind", target+" "+role, a.clientIP(r))
	a.renderSettings(w, r, u, fmt.Sprintf("%s no longer has the %s role.", target, role), "")
}

// handleUserSignOut ends every session another user has.
func (a *Admin) handleUserSignOut(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	target := r.FormValue("username")
	if msg := a.userChangeRefused(r, target); msg != "" {
		a.renderSettings(w, r, u, "", msg)
		return
	}
	if err := a.state.RevokeSessions(target, ""); err != nil {
		a.renderSettings(w, r, u, "", err.Error())
		return
	}
	a.state.RecordAudit(u.Username, "user.sign_out_everywhere", target, a.clientIP(r))
	a.renderSettings(w, r, u, target+" has been signed out everywhere.", "")
}

// handleSessionRevoke ends one of the user's own sessions, or all but the
// current one.
func (a *Admin) handleSessionRevoke(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	keep := ""
	if c, err := r.Cookie(sessionCookie); err == nil {
		keep = c.Value
	}
	if r.FormValue("all_others") != "" {
		if err := a.state.RevokeSessions(u.Username, keep); err != nil {
			a.renderSettings(w, r, u, "", err.Error())
			return
		}
		a.state.RecordAudit(u.Username, "session.revoke_others", u.Username, a.clientIP(r))
		a.renderSettings(w, r, u, "Your other sessions have been signed out.", "")
		return
	}
	idHash := r.FormValue("session")
	// Scoped to the user's own sessions: a hash belonging to anyone else
	// matches no row.
	if _, err := a.state.db.Exec(`DELETE FROM sessions WHERE id_hash = ? AND username = ?`, idHash, u.Username); err != nil {
		a.renderSettings(w, r, u, "", err.Error())
		return
	}
	a.state.RecordAudit(u.Username, "session.revoke", u.Username, a.clientIP(r))
	a.renderSettings(w, r, u, "Session signed out.", "")
}

// handleRoleSave creates or replaces a custom role.
func (a *Admin) handleRoleSave(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	role := authz.Role{
		ID:          strings.ToLower(strings.TrimSpace(r.FormValue("id"))),
		Name:        strings.TrimSpace(r.FormValue("name")),
		Description: strings.TrimSpace(r.FormValue("description")),
		Permissions: r.Form["perm"],
	}
	// Granting a permission you do not hold would be an escalation by proxy:
	// define a role with owner.manage, bind it to yourself.
	for _, p := range role.Permissions {
		if perm, err := authz.ParsePermission(p); err == nil && !a.holdsPermission(r, perm) {
			a.renderSettings(w, r, u, "", "You cannot grant "+p+", which you do not hold yourself.")
			return
		}
	}
	if err := a.state.SaveRole(role); err != nil {
		a.renderSettings(w, r, u, "", err.Error())
		return
	}
	a.state.RecordAudit(u.Username, "role.save", role.ID, a.clientIP(r))
	a.renderSettings(w, r, u, "Role "+role.ID+" saved.", "")
}

// holdsPermission reports whether the user holds a permission at least as
// broad as perm.
func (a *Admin) holdsPermission(r *http.Request, perm authz.Permission) bool {
	typ, _, _ := strings.Cut(perm.Action, ".")
	s := a.subject(r)
	switch perm.Scope {
	case authz.ScopeOwn:
		return a.can(r, perm.Action, authz.Resource{Type: typ, Owner: s.ID})
	case authz.ScopeTeam:
		// Team scope reaches every team the eventual holder joins, so it
		// takes the any scope to hand out.
		return a.canAny(r, perm.Action)
	}
	if strings.HasSuffix(perm.String(), ".any") {
		return a.canAny(r, perm.Action)
	}
	return a.canDo(r, perm.Action)
}

func (a *Admin) handleRoleDelete(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	id := r.FormValue("id")
	if err := a.state.DeleteRole(id); err != nil {
		a.renderSettings(w, r, u, "", err.Error())
		return
	}
	a.state.RecordAudit(u.Username, "role.delete", id, a.clientIP(r))
	a.renderSettings(w, r, u, "Role "+id+" deleted.", "")
}

// --- Teams page ---

// TeamRow is one team as the Teams page shows it.
type TeamRow struct {
	Team
	Members   []TeamMember
	CanManage bool
	IsMember  bool
	// CanControl covers disabling, enabling, and deleting, which only
	// router-wide team managers may do.
	CanControl bool
}

// TeamsPage is the Teams page's data.
type TeamsPage struct {
	basePage
	Teams []TeamRow
	// Users is offered when adding a member.
	Users     []string
	CanCreate bool
}

func (a *Admin) handleTeams(w http.ResponseWriter, r *http.Request) {
	a.renderTeams(w, r, ctxGetUser(r), "", "")
}

func (a *Admin) renderTeams(w http.ResponseWriter, r *http.Request, u User, flash, errMsg string) {
	bp := a.newBasePage("teams", u, r)
	bp.Flash, bp.Error = flash, errMsg
	page := TeamsPage{basePage: bp, CanCreate: a.canDo(r, "team.create")}
	teams, _ := a.state.Teams()
	subj := a.subject(r)
	for _, t := range teams {
		res := authz.Resource{Type: "team", ID: t.ID, Owner: teamPrincipal(t.ID), Team: t.ID}
		if !a.can(r, "team.view", res) {
			continue
		}
		members, _ := a.state.TeamMembers(t.ID)
		page.Teams = append(page.Teams, TeamRow{Team: t, Members: members,
			CanManage: a.can(r, "team.manage", res), IsMember: subj.HasTeam(t.ID),
			CanControl: a.canAny(r, "team.manage")})
	}
	for _, us := range a.state.Users() {
		page.Users = append(page.Users, us.Username)
	}
	a.render(w, "teams", page)
}

// teamFromForm loads the team a team action targets and checks the user may
// manage it.
func (a *Admin) teamFromForm(w http.ResponseWriter, r *http.Request) (Team, bool) {
	t, ok := a.state.LookupTeam(r.FormValue("team"))
	if !ok || !a.can(r, "team.manage", authz.Resource{Type: "team", ID: t.ID, Owner: teamPrincipal(t.ID), Team: t.ID}) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return Team{}, false
	}
	return t, true
}

func (a *Admin) handleTeamCreate(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	t, err := a.state.CreateTeam(r.FormValue("id"), r.FormValue("name"), r.FormValue("description"), u.Username)
	if err != nil {
		a.renderTeams(w, r, u, "", err.Error())
		return
	}
	a.state.RecordAudit(u.Username, "team.create", t.ID, a.clientIP(r))
	a.renderTeams(w, r, u, "Team "+t.Name+" created. You maintain it.", "")
}

func (a *Admin) handleTeamMemberAdd(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	t, ok := a.teamFromForm(w, r)
	if !ok {
		return
	}
	username := strings.TrimSpace(r.FormValue("username"))
	maintainer := r.FormValue("maintainer") != ""
	// A maintainer can create team keys, which act as the team rather than
	// as the person, so appointing one is an admin's decision: otherwise a
	// maintainer could hand a restricted user a way around their
	// restrictions. Maintainers may add plain members.
	if maintainer && !a.canAny(r, "team.manage") {
		a.renderTeams(w, r, u, "", "Only an admin can appoint team maintainers.")
		return
	}
	if err := a.state.AddTeamMember(t.ID, username, maintainer); err != nil {
		a.renderTeams(w, r, u, "", err.Error())
		return
	}
	a.state.RecordAudit(u.Username, "team.member.add", t.ID+" "+username, a.clientIP(r))
	a.renderTeams(w, r, u, username+" added to "+t.Name+".", "")
}

func (a *Admin) handleTeamMemberRemove(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	t, ok := a.teamFromForm(w, r)
	if !ok {
		return
	}
	username := r.FormValue("username")
	if err := a.state.RemoveTeamMember(t.ID, username); err != nil {
		a.renderTeams(w, r, u, "", err.Error())
		return
	}
	a.state.RecordAudit(u.Username, "team.member.remove", t.ID+" "+username, a.clientIP(r))
	a.renderTeams(w, r, u, username+" removed from "+t.Name+".", "")
}

func (a *Admin) handleTeamState(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	t, ok := a.teamFromForm(w, r)
	if !ok {
		return
	}
	// Disabling is how an admin stops a team, so maintainers — who manage
	// members — must not be able to undo it, nor delete and escape it.
	if !a.canAny(r, "team.manage") {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	switch r.FormValue("action") {
	case "disable", "enable":
		disable := r.FormValue("action") == "disable"
		if err := a.state.SetTeamDisabled(t.ID, disable); err != nil {
			a.renderTeams(w, r, u, "", err.Error())
			return
		}
		if disable {
			// The team's tokens stop authenticating; drop live connections too.
			for _, tok := range a.state.ClientTokensFor(teamPrincipal(t.ID), false) {
				a.hub.CloseByToken(tok.TokenHash)
			}
		}
		a.state.RecordAudit(u.Username, "team."+r.FormValue("action"), t.ID, a.clientIP(r))
		a.renderTeams(w, r, u, "Team "+t.Name+" "+r.FormValue("action")+"d.", "")
	case "delete":
		for _, tok := range a.state.ClientTokensFor(teamPrincipal(t.ID), false) {
			a.hub.CloseByToken(tok.TokenHash)
		}
		if err := a.state.DeleteTeam(t.ID); err != nil {
			a.renderTeams(w, r, u, "", err.Error())
			return
		}
		a.state.RecordAudit(u.Username, "team.delete", t.ID, a.clientIP(r))
		a.renderTeams(w, r, u, "Team "+t.Name+" deleted, with its keys and client tokens.", "")
	default:
		http.Error(w, "bad request", http.StatusBadRequest)
	}
}

// handleUserAttrs sets a user's attributes from "key=value" lines.
func (a *Admin) handleUserAttrs(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	target := r.FormValue("username")
	if msg := a.userChangeRefused(r, target); msg != "" {
		a.renderSettings(w, r, u, "", msg)
		return
	}
	attrs := map[string]string{}
	for _, line := range strings.Split(r.FormValue("attrs"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			a.renderSettings(w, r, u, "", fmt.Sprintf("%q is not key=value.", line))
			return
		}
		attrs[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	if err := a.state.SetUserAttrs(target, attrs); err != nil {
		a.renderSettings(w, r, u, "", err.Error())
		return
	}
	a.state.RecordAudit(u.Username, "user.attrs", target, a.clientIP(r))
	a.renderSettings(w, r, u, "Attributes for "+target+" saved.", "")
}
