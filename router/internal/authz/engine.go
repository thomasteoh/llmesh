package authz

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Policy is an attribute-based rule. See the package design document §6.
type Policy struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Effect      Effect         `json:"effect"`
	Enabled     bool           `json:"enabled"`
	Subject     SubjectMatcher `json:"subject"`
	// Actions are action names or globs over them ("model.*").
	Actions   []string        `json:"actions"`
	Resource  ResourceMatcher `json:"resource"`
	Condition *Cond           `json:"condition,omitempty"`
}

type compiledPolicy struct {
	Policy
	contextDependent bool
}

func (p *compiledPolicy) applies(req *Request) tri {
	if !p.Subject.matches(req.Subject) || !p.Resource.matches(req.Resource) {
		return triFalse
	}
	if p.Condition == nil {
		return triTrue
	}
	return p.Condition.eval(req)
}

// Engine answers access questions against one compiled set of roles and
// policies. It is immutable: a change to roles or policies compiles a new
// Engine, which callers swap in atomically. That keeps decisions consistent
// within a request and makes the cache trivially correct.
type Engine struct {
	roles map[string]compiledRole
	// deny and allow hold enabled policies per catalogued action, with globs
	// already expanded, so a decision only visits policies that can apply.
	deny  map[string][]*compiledPolicy
	allow map[string][]*compiledPolicy
	// contextDependent marks actions whose outcome can depend on the request
	// context, which disables caching for them.
	contextDependent map[string]bool

	cacheMu sync.Mutex
	cache   map[string]Decision
}

const maxCacheEntries = 65536

// Compile validates roles and policies and builds an Engine. It fails on the
// first invalid role or policy rather than skipping it, because a policy that
// silently fails to load is a restriction that silently does not apply.
func Compile(roles []Role, policies []Policy) (*Engine, error) {
	e := &Engine{
		roles:            map[string]compiledRole{},
		deny:             map[string][]*compiledPolicy{},
		allow:            map[string][]*compiledPolicy{},
		contextDependent: map[string]bool{},
		cache:            map[string]Decision{},
	}
	for _, r := range roles {
		if _, dup := e.roles[r.ID]; dup {
			return nil, fmt.Errorf("duplicate role %q", r.ID)
		}
		cr, err := compileRole(r)
		if err != nil {
			return nil, err
		}
		e.roles[r.ID] = cr
	}
	seen := map[string]bool{}
	for _, p := range policies {
		if err := validatePolicy(p); err != nil {
			return nil, fmt.Errorf("policy %q: %w", p.ID, err)
		}
		if seen[p.ID] {
			return nil, fmt.Errorf("duplicate policy %q", p.ID)
		}
		seen[p.ID] = true
		if !p.Enabled {
			continue
		}
		if p.Condition != nil {
			p.Condition = p.Condition.clone()
			prepare(p.Condition)
		}
		cp := &compiledPolicy{Policy: p, contextDependent: p.Condition != nil && p.Condition.contextDependent()}
		for _, action := range expandActions(p.Actions) {
			if p.Effect == Deny {
				e.deny[action] = append(e.deny[action], cp)
			} else {
				e.allow[action] = append(e.allow[action], cp)
			}
			if cp.contextDependent {
				e.contextDependent[action] = true
			}
		}
	}
	// Deterministic order, so "By" names the same policy every time.
	for _, m := range []map[string][]*compiledPolicy{e.deny, e.allow} {
		for a := range m {
			sort.Slice(m[a], func(i, j int) bool { return m[a][i].ID < m[a][j].ID })
		}
	}
	return e, nil
}

func validatePolicy(p Policy) error {
	if p.ID == "" {
		return fmt.Errorf("policy has no id")
	}
	if p.Effect != Allow && p.Effect != Deny {
		return fmt.Errorf("effect must be %q or %q", Allow, Deny)
	}
	if len(p.Actions) == 0 {
		return fmt.Errorf("policy names no actions")
	}
	for _, a := range p.Actions {
		if len(expandActions([]string{a})) == 0 {
			return fmt.Errorf("action %q matches nothing in the catalogue", a)
		}
	}
	if err := p.Resource.validate(); err != nil {
		return err
	}
	if p.Condition != nil {
		if err := p.Condition.validate(0); err != nil {
			return err
		}
	}
	return nil
}

func expandActions(patterns []string) []string {
	var out []string
	for action := range catalogue {
		if globAny(patterns, action) {
			out = append(out, action)
		}
	}
	sort.Strings(out)
	return out
}

// Decide answers one access question.
//
// The order is fixed and is the whole of the model:
//
//  1. An unknown action is denied.
//  2. Any enabled deny policy that applies — its condition true or unknown —
//     denies. Deny overrides everything.
//  3. For actions that need a grant (model.use, client.use), a role must
//     permit the action AND something must grant it on this resource: an
//     allow policy whose condition is true, or for client.use the client's
//     own sharing setting.
//  4. For every other action, a role permission or an applicable allow
//     policy permits it.
//  5. Otherwise the answer is no.
func (e *Engine) Decide(req Request) Decision {
	info, known := catalogue[req.Action]
	if !known {
		return Decision{By: "default", Reason: "unknown action " + req.Action}
	}
	key, cacheable := e.cacheKey(&req)
	if cacheable {
		e.cacheMu.Lock()
		d, hit := e.cache[key]
		e.cacheMu.Unlock()
		if hit {
			return d
		}
	}
	d := e.decide(&req, info)
	if cacheable {
		e.cacheMu.Lock()
		if len(e.cache) >= maxCacheEntries {
			e.cache = map[string]Decision{}
		}
		e.cache[key] = d
		e.cacheMu.Unlock()
	}
	return d
}

// Can is Decide reduced to a yes or no.
func (e *Engine) Can(req Request) bool { return e.Decide(req).Allowed }

func (e *Engine) decide(req *Request, info actionInfo) Decision {
	for _, p := range e.deny[req.Action] {
		if p.applies(req) != triFalse {
			return Decision{By: p.ID, Reason: "denied by policy " + policyLabel(p)}
		}
	}
	roleOK, roleBy := e.roleAllows(req)
	var grantBy string
	for _, p := range e.allow[req.Action] {
		if p.applies(req) == triTrue {
			grantBy = p.ID
			break
		}
	}
	if info.needsGrant {
		if !roleOK {
			return Decision{By: "default", Reason: "no role permits " + req.Action}
		}
		if grantBy == "" && req.Action == "client.use" && sharingAdmits(req.Subject, req.Resource) {
			grantBy = "sharing"
		}
		if grantBy == "" {
			return Decision{By: "default", Reason: "no grant for " + req.Action + " on " + req.Resource.Type + " " + req.Resource.ID}
		}
		return Decision{Allowed: true, By: grantBy}
	}
	if roleOK {
		return Decision{Allowed: true, By: roleBy}
	}
	if grantBy != "" {
		return Decision{Allowed: true, By: grantBy}
	}
	return Decision{By: "default", Reason: "no role or policy permits " + req.Action}
}

func policyLabel(p *compiledPolicy) string {
	if p.Name != "" {
		return fmt.Sprintf("%q", p.Name)
	}
	return p.ID
}

// roleAllows reports whether any of the subject's bindings permits the action
// on the resource.
//
// A binding confined to a team applies its scoped permissions only to that
// team's resources, whatever scope the permission names. Of its unscoped
// permissions, it honours only those that still need a resource grant
// (model.use, client.use); anything else — fleet.view, audit.view — would
// otherwise leak router-wide from a team-level role.
func (e *Engine) roleAllows(req *Request) (bool, string) {
	info := catalogue[req.Action]
	for _, b := range req.Subject.Bindings {
		r, ok := e.roles[b.Role]
		if !ok {
			continue
		}
		for _, scope := range r.grants[req.Action] {
			if b.Team != "" {
				if !info.scoped {
					if info.needsGrant {
						return true, "role:" + r.ID
					}
					continue
				}
				if req.Resource.Team == b.Team {
					return true, "role:" + r.ID
				}
				continue
			}
			switch scope {
			case ScopeAny:
				return true, "role:" + r.ID
			case ScopeOwn:
				if req.Resource.Owner != "" && req.Resource.Owner == req.Subject.ID {
					return true, "role:" + r.ID
				}
			case ScopeTeam:
				if req.Resource.Team != "" && req.Subject.HasTeam(req.Resource.Team) {
					return true, "role:" + r.ID
				}
			}
		}
	}
	return false, ""
}

// cacheKey builds the memoisation key, or reports the decision uncacheable:
// when the caller has not identified the subject's state (Rev), when the
// resource carries attributes or sharing that are not part of the key, or
// when some policy for the action reads the request context.
func (e *Engine) cacheKey(req *Request) (string, bool) {
	if req.Subject.Rev == "" || e.contextDependent[req.Action] {
		return "", false
	}
	if len(req.Resource.Attrs) > 0 || req.Resource.Sharing != nil {
		return "", false
	}
	var b strings.Builder
	for _, s := range []string{req.Subject.ID, req.Subject.Rev, req.Action,
		req.Resource.Type, req.Resource.ID, req.Resource.Owner, req.Resource.Team} {
		b.WriteString(s)
		b.WriteByte(0)
	}
	return b.String(), true
}
