package authz

import "fmt"

// Glob reports whether name matches pattern, where '*' matches any run of
// characters (including '/', since model names contain it) and '?' matches
// exactly one. There are no character classes or escapes: patterns are
// written by admins for model names and ids, and a smaller language is one
// they can predict.
func Glob(pattern, name string) bool {
	// Iterative matcher with single-star backtracking: linear in practice,
	// never exponential.
	p, n := 0, 0
	starP, starN := -1, 0
	for n < len(name) {
		switch {
		case p < len(pattern) && (pattern[p] == '?' || pattern[p] == name[n]):
			p++
			n++
		case p < len(pattern) && pattern[p] == '*':
			starP, starN = p, n
			p++
		case starP >= 0:
			starN++
			p, n = starP+1, starN
		default:
			return false
		}
	}
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}

func globAny(patterns []string, name string) bool {
	for _, p := range patterns {
		if Glob(p, name) {
			return true
		}
	}
	return false
}

// SubjectMatcher selects the subjects a policy applies to. Every non-empty
// field must match; an empty matcher selects every subject. Within a field,
// any listed value matching is enough.
//
// Matchers select, they do not protect: a subject missing an attribute a
// matcher names is simply not selected. A restriction that must hold even when
// an attribute is absent belongs in a condition, where absence counts against
// the subject (see Cond).
type SubjectMatcher struct {
	// IDs are globs over principal ids ("user:*", "team:research").
	IDs   []string          `json:"ids,omitempty"`
	Kinds []PrincipalKind   `json:"kinds,omitempty"`
	Roles []string          `json:"roles,omitempty"`
	Teams []string          `json:"teams,omitempty"`
	Attrs map[string]string `json:"attrs,omitempty"` // value is a glob
}

func (m SubjectMatcher) matches(s Subject) bool {
	if len(m.IDs) > 0 && !globAny(m.IDs, s.ID) {
		return false
	}
	if len(m.Kinds) > 0 {
		ok := false
		for _, k := range m.Kinds {
			if k == s.Kind {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	if len(m.Roles) > 0 {
		ok := false
		for _, b := range s.Bindings {
			for _, r := range m.Roles {
				if b.Role == r {
					ok = true
				}
			}
		}
		if !ok {
			return false
		}
	}
	if len(m.Teams) > 0 {
		ok := false
		for _, t := range m.Teams {
			if s.HasTeam(t) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	for k, pattern := range m.Attrs {
		v, present := s.Attrs[k]
		if !present || !Glob(pattern, v) {
			return false
		}
	}
	return true
}

// ResourceMatcher selects the resources a policy applies to.
type ResourceMatcher struct {
	// Type is the resource type; empty matches any.
	Type string `json:"type,omitempty"`
	// IDs are globs over resource ids (model names for Type "model").
	IDs    []string `json:"ids,omitempty"`
	Owners []string `json:"owners,omitempty"` // globs over owner principal ids
	Teams  []string `json:"teams,omitempty"`
}

func (m ResourceMatcher) matches(r Resource) bool {
	if m.Type != "" && m.Type != r.Type {
		return false
	}
	if len(m.IDs) > 0 && !globAny(m.IDs, r.ID) {
		return false
	}
	if len(m.Owners) > 0 && !globAny(m.Owners, r.Owner) {
		return false
	}
	if len(m.Teams) > 0 {
		ok := false
		for _, t := range m.Teams {
			if t == r.Team {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

func (m ResourceMatcher) validate() error {
	for _, g := range append(append([]string{}, m.IDs...), m.Owners...) {
		if g == "" {
			return fmt.Errorf("empty pattern in resource matcher")
		}
	}
	return nil
}
