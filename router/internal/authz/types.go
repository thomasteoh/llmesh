// Package authz decides who may do what in llmesh.
//
// It is the single decision point described in
// docs/superpowers/specs/2026-09-29-access-management-v2.md: roles (named
// permission sets), per-resource grants such as model access, attribute-based
// policies, and capacity sharing all evaluate here. The package is pure — no
// database, no network, no clock except what the caller passes in — so the
// portal, the API ingress, and the scheduler can all depend on it and test it
// with plain values.
package authz

import (
	"net/netip"
	"time"
)

// PrincipalKind is the kind of thing that acts.
type PrincipalKind string

const (
	KindUser   PrincipalKind = "user"
	KindTeam   PrincipalKind = "team"
	KindRouter PrincipalKind = "router"
)

// Binding grants a role to a subject, either everywhere or within one team.
type Binding struct {
	Role string
	// Team, when set, confines the role to resources belonging to that team.
	Team string
}

// Subject is the principal a request is made as, with everything a decision
// may consult about it. Callers build it once per request from the store.
type Subject struct {
	// ID is the principal id, e.g. "user:alice", "team:research".
	ID   string
	Kind PrincipalKind
	// Bindings are the roles held, directly or through teams.
	Bindings []Binding
	// Teams the subject belongs to, by team id.
	Teams []string
	// ManagedBy names the identity provider that owns the account, if any.
	ManagedBy string
	// Attrs are free-form attributes set by an admin or mapped from an
	// identity provider, e.g. {"department": "finance"}.
	Attrs map[string]string
	// Rev identifies this exact subject state for decision caching. Two
	// subjects with the same ID and Rev must be identical. Empty disables
	// caching for the subject.
	Rev string
}

// HasTeam reports whether the subject belongs to team.
func (s Subject) HasTeam(team string) bool {
	for _, t := range s.Teams {
		if t == team {
			return true
		}
	}
	return false
}

// Resource is the thing acted on.
type Resource struct {
	// Type is the resource type: "model", "client", "key", "team", "user",
	// "usage", "job", "audit", "settings", "alias", "policy".
	Type string
	// ID identifies the resource within its type (a model name, a token hash).
	ID string
	// Owner is the owning principal id, if the resource has one.
	Owner string
	// Team is the team the resource belongs to, if any. A team-owned resource
	// has Owner "team:<id>" and Team "<id>".
	Team string
	// Attrs are type-specific attributes, e.g. a model's "modality" or a
	// client's "tags". Values are strings, numbers, bools, or string lists.
	Attrs map[string]any
	// Sharing is a client's sharing setting; only meaningful for Type "client".
	Sharing *Sharing
}

// Context is what the request carries besides who and what.
type Context struct {
	Time           time.Time
	SourceIP       netip.Addr
	Endpoint       string
	Priority       string
	CredentialKind string // "session", "key", "token"
	ViaUpstream    bool
	PromptTokens   int
	// Extra holds additional context attributes a caller wants rules to see.
	Extra map[string]any
}

// Request is one access question.
type Request struct {
	Subject  Subject
	Action   string
	Resource Resource
	Context  Context
}

// Decision is the answer, with what decided it.
type Decision struct {
	Allowed bool
	// By names what decided: a policy id, "role:<id>", or "default".
	By string
	// Reason is a short explanation fit for a log line or an error body. It
	// never names another principal.
	Reason string
}

// Effect is what a policy does when it applies.
type Effect string

const (
	Allow Effect = "allow"
	Deny  Effect = "deny"
)
