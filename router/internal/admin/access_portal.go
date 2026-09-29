package admin

import (
	"context"
	"net/http"
	"strings"

	"llmesh/router/internal/authz"
)

// Portal enforcement for access management v2 (design §11). Every portal
// decision goes through can: route guards, list filters, and the capability
// flags templates use to decide which controls to draw.

const ctxSubject contextKey = 2

// subject returns the authz subject for the signed-in user, built once per
// request by requireAuth.
func (a *Admin) subject(r *http.Request) authz.Subject {
	if s, ok := r.Context().Value(ctxSubject).(authz.Subject); ok {
		return s
	}
	// Outside requireAuth: build it on demand, or return a subject that
	// nothing grants.
	if u, ok := r.Context().Value(ctxUser).(User); ok {
		if s, err := a.state.SubjectFor(u.Username); err == nil {
			return s
		}
	}
	return authz.Subject{}
}

// withSubject attaches the user's subject to the request context.
func (a *Admin) withSubject(r *http.Request, u User) *http.Request {
	s, err := a.state.SubjectFor(u.Username)
	if err != nil {
		s = authz.Subject{ID: userPrincipal(u.Username), Kind: authz.KindUser}
	}
	return r.WithContext(context.WithValue(r.Context(), ctxSubject, s))
}

// ownedResource describes a resource by its owner column value, filling in
// the team for team-owned rows.
func ownedResource(typ, id, owner string) authz.Resource {
	res := authz.Resource{Type: typ, ID: id, Owner: ownerPrincipal(owner)}
	if t, ok := strings.CutPrefix(res.Owner, "team:"); ok {
		res.Team = t
	}
	return res
}

// can asks the engine whether the signed-in user may act on a resource.
func (a *Admin) can(r *http.Request, action string, res authz.Resource) bool {
	e := a.state.Authz()
	if e == nil {
		return false
	}
	return e.Can(authz.Request{Subject: a.subject(r), Action: action, Resource: res,
		Context: authz.Context{CredentialKind: "session"}})
}

// canDo asks about a router-wide action with no particular resource: the
// resource type is the action's first segment.
func (a *Admin) canDo(r *http.Request, action string) bool {
	typ, _, _ := strings.Cut(action, ".")
	return a.can(r, action, authz.Resource{Type: typ})
}

// canAny reports whether a scoped action reaches every resource of its type:
// the ".any" scope, or a policy granting it without regard to owner. It is
// asked with an owner nobody has, so neither own nor team scope can match.
func (a *Admin) canAny(r *http.Request, action string) bool {
	typ, _, _ := strings.Cut(action, ".")
	return a.can(r, action, authz.Resource{Type: typ, Owner: "\x00none"})
}

// requirePerm guards a route with a router-wide permission.
func (a *Admin) requirePerm(action string, next http.HandlerFunc) http.HandlerFunc {
	return a.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		if !a.canDo(r, action) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next(w, r)
	})
}

// ownerScope returns the owners whose data a scoped view action lets the user
// see: nil for everyone (the .any scope), otherwise the user plus each team
// the action reaches. Usage and perf rows record the owner column value, so
// these are column values, not principal ids.
func (a *Admin) ownerScope(r *http.Request, action string) []string {
	if a.canAny(r, action) {
		return nil
	}
	s := a.subject(r)
	typ, _, _ := strings.Cut(action, ".")
	var out []string
	username := strings.TrimPrefix(s.ID, "user:")
	if a.can(r, action, authz.Resource{Type: typ, Owner: s.ID}) {
		out = append(out, username)
	}
	for _, t := range s.Teams {
		if a.can(r, action, authz.Resource{Type: typ, Owner: teamPrincipal(t), Team: t}) {
			out = append(out, teamPrincipal(t))
		}
	}
	if out == nil {
		out = []string{}
	}
	return out
}

// capabilityActions are the router-wide permissions templates branch on. Keys
// in the resulting map replace '.' with '_' so templates can write
// {{if .Can.alias_manage}}.
var capabilityActions = []string{
	"alias.manage", "queue.cancel", "settings.view", "settings.manage",
	"user.view", "user.manage", "role.manage", "owner.manage",
	"pricing.manage", "upstream.manage", "policy.view", "policy.manage", "policy.simulate",
	"audit.view", "fleet.view", "team.create",
}

// capabilities computes the flags for the signed-in user.
func (a *Admin) capabilities(r *http.Request) map[string]bool {
	out := make(map[string]bool, len(capabilityActions)+4)
	for _, action := range capabilityActions {
		out[strings.ReplaceAll(action, ".", "_")] = a.canDo(r, action)
	}
	out["key_limits_any"] = a.canAny(r, "key.limits")
	out["key_create_any"] = a.canAny(r, "key.create")
	out["client_create_any"] = a.canAny(r, "client.create")
	out["job_cancel_any"] = a.canAny(r, "job.cancel")
	return out
}

// topRole names the most senior router-wide role a user holds, for the badge
// beside their name.
func topRole(s authz.Subject) string {
	rank := map[string]int{
		authz.RoleOwner: 6, authz.RoleAdmin: 5, authz.RoleOperator: 4,
		authz.RoleAuditor: 3, authz.RoleMember: 2, authz.RoleViewer: 1,
	}
	best, bestRank := "", 0
	for _, b := range s.Bindings {
		if b.Team != "" {
			continue
		}
		r, known := rank[b.Role]
		if !known {
			r = 1 // a custom role: show it, below the built-ins
		}
		if r > bestRank {
			best, bestRank = b.Role, r
		}
	}
	return best
}

// jobAccess decides whether the user may see and cancel a job. Besides job
// permissions on the requester's side, whoever manages the client serving the
// job may see and stop it: it is their hardware.
func (a *Admin) jobAccess(r *http.Request, jobID, reqOwner, clientOwner string) (view, cancel bool) {
	job := ownedResource("job", jobID, reqOwner)
	onMyHardware := clientOwner != "" && a.can(r, "client.manage", ownedResource("client", "", clientOwner))
	view = onMyHardware || a.can(r, "job.view", job)
	cancel = onMyHardware || a.can(r, "job.cancel", job)
	return view, cancel
}

// visibleClientTokens returns the client tokens the user may see.
func (a *Admin) visibleClientTokens(r *http.Request) []ClientToken {
	var out []ClientToken
	for _, t := range a.state.ClientTokensFor("", true) {
		if a.can(r, "client.view", ownedResource("client", t.TokenHash, t.Owner)) {
			out = append(out, t)
		}
	}
	return out
}

// visibleAPIKeys returns the API keys the user may see.
func (a *Admin) visibleAPIKeys(r *http.Request) []APIKey {
	var out []APIKey
	for _, k := range a.state.APIKeysFor("", true) {
		if a.can(r, "key.view", ownedResource("key", k.KeyHash, k.Owner)) {
			out = append(out, k)
		}
	}
	return out
}

// credentialOwner resolves who a new key or client token belongs to — the
// user themselves, another user ("bob"), or a team ("team:research") — and
// checks the user may create one there. action is "key.create" or
// "client.create". The returned message is fit to show the user.
func (a *Admin) credentialOwner(r *http.Request, action, requested string) (owner, errMsg string) {
	u := ctxGetUser(r)
	requested = strings.TrimSpace(requested)
	switch {
	case requested == "" || requested == u.Username:
		owner = u.Username
	case strings.HasPrefix(requested, "team:"):
		if _, ok := a.state.LookupTeam(strings.TrimPrefix(requested, "team:")); !ok {
			return "", "Team " + strings.TrimPrefix(requested, "team:") + " not found."
		}
		owner = requested
	default:
		if _, ok := a.state.LookupUser(requested); !ok {
			return "", "User " + requested + " not found."
		}
		owner = requested
	}
	typ := strings.TrimSuffix(action, ".create")
	if !a.can(r, action, ownedResource(typ, "", owner)) {
		if owner == u.Username {
			return "", "You do not have permission to create this."
		}
		return "", "You do not have permission to create this for " + owner + "."
	}
	return owner, ""
}

// ownerChoices lists the owners, besides the user, that they may create a
// key or token for, for the form's owner field.
func (a *Admin) ownerChoices(r *http.Request, action string) []string {
	typ := strings.TrimSuffix(action, ".create")
	u := ctxGetUser(r)
	var out []string
	if a.canAny(r, action) {
		for _, us := range a.state.Users() {
			if us.Username != u.Username {
				out = append(out, us.Username)
			}
		}
	}
	teams, _ := a.state.Teams()
	for _, t := range teams {
		if !t.Disabled && a.can(r, action, authz.Resource{Type: typ, Owner: teamPrincipal(t.ID), Team: t.ID}) {
			out = append(out, teamPrincipal(t.ID))
		}
	}
	return out
}

// secretOwnerPart is the owner as it appears inside a generated key or token
// ("sk-<owner>-<random>"): a team's ':' becomes '-', so the value stays one
// token for shells and headers.
func secretOwnerPart(owner string) string { return strings.ReplaceAll(owner, ":", "-") }

// userChangeRefused returns why the signed-in user may not change target's
// account, or "" if they may. Changing an owner's account — disabling,
// deleting, resetting, demoting — takes owner.manage, which admins do not
// have; otherwise an admin could remove the owners above them.
func (a *Admin) userChangeRefused(r *http.Request, target string) string {
	if a.state.IsOwner(target) && !a.canDo(r, "owner.manage") {
		return "Only an owner can change another owner's account."
	}
	return ""
}
