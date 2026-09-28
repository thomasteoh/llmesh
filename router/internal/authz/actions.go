package authz

import (
	"fmt"
	"strings"
)

// Scope limits which resources a role permission reaches.
type Scope string

const (
	// ScopeOwn reaches resources the subject owns.
	ScopeOwn Scope = "own"
	// ScopeTeam reaches resources of teams the subject belongs to.
	ScopeTeam Scope = "team"
	// ScopeAny reaches every resource of the type.
	ScopeAny Scope = "any"
)

// actionInfo describes one action in the catalogue.
type actionInfo struct {
	// scoped actions take an own/team/any suffix in role permissions.
	scoped bool
	// needsGrant actions are allowed only when, besides a role permitting
	// the action at all, some allow policy grants it on the specific
	// resource. model.use is one: a role says "may use models", the model's
	// access list says which. See Engine.Decide.
	needsGrant bool
}

// catalogue lists every action the router checks. Role permissions must name
// one of these; policies may also use globs over them ("model.*").
var catalogue = map[string]actionInfo{
	"model.use":       {needsGrant: true},
	"client.use":      {needsGrant: true},
	"key.create":      {scoped: true},
	"key.manage":      {scoped: true},
	"key.view":        {scoped: true},
	"client.create":   {scoped: true},
	"client.manage":   {scoped: true},
	"client.view":     {scoped: true},
	"client.share":    {scoped: true},
	"usage.view":      {scoped: true},
	"job.view":        {scoped: true},
	"job.cancel":      {scoped: true},
	"team.create":     {},
	"team.manage":     {scoped: true},
	"team.view":       {scoped: true},
	"fleet.view":      {},
	"user.view":       {},
	"user.manage":     {},
	"role.manage":     {},
	"alias.manage":    {},
	"pricing.manage":  {},
	"settings.view":   {},
	"settings.manage": {},
	"upstream.manage": {},
	"queue.cancel":    {},
	"policy.view":     {},
	"policy.manage":   {},
	"policy.simulate": {},
	"audit.view":      {},
	"owner.manage":    {},
}

// KnownAction reports whether action is in the catalogue.
func KnownAction(action string) bool {
	_, ok := catalogue[action]
	return ok
}

// Actions returns every catalogued action.
func Actions() []string {
	out := make([]string, 0, len(catalogue))
	for a := range catalogue {
		out = append(out, a)
	}
	return out
}

// Permission is one parsed role permission: an action, and for scoped actions
// how far it reaches.
type Permission struct {
	Action string
	Scope  Scope
}

// ParsePermission reads "key.manage.team" or "fleet.view". A scoped action
// without a suffix is rejected rather than guessed, since guessing "any" would
// silently grant more than an admin wrote.
func ParsePermission(s string) (Permission, error) {
	if info, ok := catalogue[s]; ok {
		if info.scoped {
			return Permission{}, fmt.Errorf("permission %q needs a scope: %s.own, %s.team, or %s.any", s, s, s, s)
		}
		return Permission{Action: s, Scope: ScopeAny}, nil
	}
	i := strings.LastIndexByte(s, '.')
	if i < 0 {
		return Permission{}, fmt.Errorf("unknown permission %q", s)
	}
	action, scope := s[:i], Scope(s[i+1:])
	info, ok := catalogue[action]
	if !ok {
		return Permission{}, fmt.Errorf("unknown permission %q", s)
	}
	if !info.scoped {
		return Permission{}, fmt.Errorf("permission %q does not take a scope", action)
	}
	switch scope {
	case ScopeOwn, ScopeTeam, ScopeAny:
		return Permission{Action: action, Scope: scope}, nil
	}
	return Permission{}, fmt.Errorf("permission %q has unknown scope %q", s, scope)
}

func (p Permission) String() string {
	if catalogue[p.Action].scoped {
		return p.Action + "." + string(p.Scope)
	}
	return p.Action
}
