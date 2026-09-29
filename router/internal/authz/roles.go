package authz

import (
	"fmt"
	"sort"
)

// Role is a named set of permissions.
type Role struct {
	ID          string
	Name        string
	Description string
	// Builtin roles ship with the router and cannot be edited, only cloned.
	Builtin     bool
	Permissions []string
}

// Built-in role ids.
const (
	RoleOwner    = "owner"
	RoleAdmin    = "admin"
	RoleOperator = "operator"
	RoleAuditor  = "auditor"
	RoleMember   = "member"
	RoleViewer   = "viewer"
	// Team roles, meaningful in a binding scoped to a team.
	RoleTeamMaintainer = "team-maintainer"
	RoleTeamMember     = "team-member"
)

func allPermissions(except ...string) []string {
	skip := map[string]bool{}
	for _, e := range except {
		skip[e] = true
	}
	var out []string
	for action, info := range catalogue {
		if skip[action] {
			continue
		}
		if info.scoped {
			out = append(out, action+".any")
		} else {
			out = append(out, action)
		}
	}
	sort.Strings(out)
	return out
}

// BuiltinRoles returns the roles every router has.
func BuiltinRoles() []Role {
	return []Role{
		{
			ID: RoleOwner, Name: "Owner", Builtin: true,
			Description: "Everything, including managing other owners. The last owner cannot be removed.",
			Permissions: allPermissions(),
		},
		{
			ID: RoleAdmin, Name: "Admin", Builtin: true,
			Description: "Everything except adding or removing owners.",
			Permissions: allPermissions("owner.manage"),
		},
		{
			ID: RoleOperator, Name: "Operator", Builtin: true,
			Description: "Runs the fleet: clients, aliases, queue, pricing, upstreams. No users, roles, or policies.",
			Permissions: []string{
				"model.use", "client.use",
				"key.create.own", "key.manage.own", "key.view.own", "key.limits.any",
				"client.create.any", "client.manage.any", "client.view.any", "client.share.any",
				"usage.view.any", "job.view.any", "job.cancel.any", "queue.cancel",
				"fleet.view", "alias.manage", "pricing.manage", "upstream.manage",
				"settings.view", "team.create", "team.view.any",
			},
		},
		{
			ID: RoleAuditor, Name: "Auditor", Builtin: true,
			Description: "Reads everything, including usage and the audit log. Changes nothing.",
			Permissions: []string{
				"key.view.any", "client.view.any", "usage.view.any", "job.view.any",
				"team.view.any", "fleet.view", "user.view", "settings.view",
				"policy.view", "policy.simulate", "audit.view",
			},
		},
		{
			ID: RoleMember, Name: "Member", Builtin: true,
			Description: "Uses models it is granted, and manages its own keys, clients, usage, and jobs.",
			Permissions: []string{
				"model.use", "client.use",
				"key.create.own", "key.manage.own", "key.view.own",
				"client.create.own", "client.manage.own", "client.view.own", "client.share.own",
				"usage.view.own", "job.view.own", "job.cancel.own",
				"team.create", "team.view.own",
			},
		},
		{
			ID: RoleViewer, Name: "Viewer", Builtin: true,
			Description: "Sees its own usage. Cannot create keys or use models.",
			Permissions: []string{"usage.view.own", "job.view.own", "team.view.own"},
		},
		{
			ID: RoleTeamMaintainer, Name: "Team maintainer", Builtin: true,
			Description: "Within a team: manages its keys, clients, sharing, and members, and sees its usage.",
			Permissions: []string{
				"model.use", "client.use",
				"key.create.team", "key.manage.team", "key.view.team",
				"client.create.team", "client.manage.team", "client.view.team", "client.share.team",
				"usage.view.team", "job.view.team", "job.cancel.team",
				"team.manage.team", "team.view.team",
			},
		},
		{
			ID: RoleTeamMember, Name: "Team member", Builtin: true,
			Description: "Within a team: uses its keys and clients and sees its usage.",
			Permissions: []string{
				"model.use", "client.use",
				"key.view.team", "client.view.team",
				"usage.view.team", "job.view.team", "job.cancel.own",
				"team.view.team",
			},
		},
	}
}

// compiledRole is a role with its permissions parsed and indexed by action.
type compiledRole struct {
	Role
	grants map[string][]Scope
}

func compileRole(r Role) (compiledRole, error) {
	if r.ID == "" {
		return compiledRole{}, fmt.Errorf("role has no id")
	}
	cr := compiledRole{Role: r, grants: map[string][]Scope{}}
	for _, s := range r.Permissions {
		p, err := ParsePermission(s)
		if err != nil {
			return compiledRole{}, fmt.Errorf("role %q: %w", r.ID, err)
		}
		cr.grants[p.Action] = append(cr.grants[p.Action], p.Scope)
	}
	return cr, nil
}
