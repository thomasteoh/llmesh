package admin

import (
	"fmt"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"llmesh/router/internal/authz"
)

// Inference-side enforcement (design §4, §5, §11): which models an API key may
// reach. The API ingress calls AuthorizeModels on every request and for model
// listings; the answer is the subset of candidate models the key's owner may
// use, narrowed by the key's own scope.

// AuthorizeModels returns the candidates the key may use at endpoint, and
// when none survive, why. attrs carries model attributes (modalities, context
// size, pricing basis) for policies that read them; it may be nil.
//
// A key owned by a team acts as the team. A key whose owner is not a user or
// team row predates accounts and acts as a plain member, which is what every
// key could do before.
func (s *State) AuthorizeModels(key, endpoint, sourceIP string, candidates []string, attrs map[string]map[string]any) ([]string, string) {
	k, ok := s.LookupAPIKey(key)
	if !ok {
		return nil, "invalid API key"
	}
	if len(k.Scope.Endpoints) > 0 && endpoint != "" && !containsString(k.Scope.Endpoints, endpoint) {
		return nil, "this API key may not use " + endpoint
	}
	subj := s.keySubject(k.Owner)
	e := s.Authz()
	if e == nil {
		return nil, "access policies are not loaded"
	}
	ctx := authz.Context{
		Time:           time.Now(),
		Endpoint:       endpoint,
		Priority:       k.Priority,
		CredentialKind: "key",
	}
	if ip, err := netip.ParseAddr(sourceIP); err == nil {
		ctx.SourceIP = ip
	}
	var allowed []string
	reason := ""
	for _, m := range candidates {
		if len(k.Scope.Models) > 0 && !globAnyString(k.Scope.Models, m) {
			if reason == "" {
				reason = "this API key is not scoped to model " + m
			}
			continue
		}
		d := e.Decide(authz.Request{Subject: subj, Action: "model.use",
			Resource: authz.Resource{Type: "model", ID: m, Attrs: attrs[m]}, Context: ctx})
		if d.Allowed {
			allowed = append(allowed, m)
		} else if reason == "" {
			reason = d.Reason
		}
	}
	if len(allowed) > 0 {
		reason = ""
	} else {
		if reason == "" {
			reason = "no model available to this API key"
		}
		s.denials.add(Denial{At: time.Now(), Subject: subj.ID, Action: "model.use",
			Resource: strings.Join(candidates, ", "), Reason: reason})
	}
	return allowed, reason
}

// keySubject builds the subject a key acts as.
func (s *State) keySubject(owner string) authz.Subject {
	if t, ok := strings.CutPrefix(owner, "team:"); ok {
		if subj, err := s.SubjectForTeam(t); err == nil {
			return subj
		}
		return authz.Subject{ID: owner, Kind: authz.KindTeam}
	}
	if subj, err := s.SubjectFor(owner); err == nil {
		return subj
	}
	return authz.Subject{ID: ownerPrincipal(owner), Kind: authz.KindUser,
		Bindings: []authz.Binding{{Role: authz.RoleMember}}}
}

func containsString(list []string, s string) bool {
	for _, e := range list {
		if e == s {
			return true
		}
	}
	return false
}

func globAnyString(patterns []string, s string) bool {
	for _, p := range patterns {
		if authz.Glob(p, s) {
			return true
		}
	}
	return false
}

// --- Model access rules (the Settings tab) ---

// ModelRuleRow is one model-access policy as the Model access tab shows it.
type ModelRuleRow struct {
	ID      string
	Name    string
	Effect  string
	Models  []string
	Who     string // human summary of the subject matcher
	Enabled bool
	// Advanced marks a policy with a condition or other actions, which this
	// tab lists but leaves to the policy editor.
	Advanced bool
}

// modelRules lists the policies that govern model.use.
func (a *Admin) modelRules() []ModelRuleRow {
	ps, _ := a.state.Policies()
	var out []ModelRuleRow
	for _, p := range ps {
		if !containsString(p.Actions, "model.use") {
			continue
		}
		out = append(out, ModelRuleRow{
			ID: p.ID, Name: p.Name, Effect: string(p.Effect), Models: p.Resource.IDs,
			Who: subjectSummary(p.Subject), Enabled: p.Enabled,
			Advanced: p.Condition != nil || len(p.Actions) > 1 || p.Resource.Type != "model",
		})
	}
	return out
}

func subjectSummary(m authz.SubjectMatcher) string {
	var parts []string
	if len(m.IDs) > 0 {
		parts = append(parts, strings.Join(m.IDs, ", "))
	}
	if len(m.Teams) > 0 {
		parts = append(parts, "teams "+strings.Join(m.Teams, ", "))
	}
	if len(m.Roles) > 0 {
		parts = append(parts, "roles "+strings.Join(m.Roles, ", "))
	}
	for k, v := range m.Attrs {
		parts = append(parts, k+"="+v)
	}
	if len(parts) == 0 {
		return "everyone"
	}
	return strings.Join(parts, "; ")
}

func splitList(s string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '\n' }) {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

func (a *Admin) handleModelRuleSave(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	models := splitList(r.FormValue("models"))
	if len(models) == 0 {
		a.renderSettings(w, r, u, "", "Name at least one model or pattern.")
		return
	}
	effect := authz.Effect(r.FormValue("effect"))
	p := authz.Policy{
		ID:       strings.TrimSpace(r.FormValue("id")),
		Name:     strings.TrimSpace(r.FormValue("name")),
		Effect:   effect,
		Enabled:  true,
		Actions:  []string{"model.use"},
		Resource: authz.ResourceMatcher{Type: "model", IDs: models},
	}
	for _, s := range splitList(r.FormValue("users")) {
		if !strings.Contains(s, ":") {
			s = userPrincipal(s)
		}
		p.Subject.IDs = append(p.Subject.IDs, s)
	}
	p.Subject.Teams = splitList(r.FormValue("teams"))
	p.Subject.Roles = splitList(r.FormValue("roles"))
	if p.ID == "" {
		p.ID = "model-" + strings.ToLower(strconv.FormatInt(time.Now().UnixNano(), 36))
	}
	if p.Name == "" {
		p.Name = fmt.Sprintf("%s %s for %s", effect, strings.Join(models, ", "), subjectSummary(p.Subject))
	}
	if err := a.state.SavePolicy(p, u.Username); err != nil {
		a.renderSettings(w, r, u, "", err.Error())
		return
	}
	a.state.RecordAudit(u.Username, "policy.save", p.ID, a.clientIP(r))
	a.renderSettings(w, r, u, "Model access rule saved.", "")
}

func (a *Admin) handleModelRuleToggle(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	id := r.FormValue("id")
	ps, _ := a.state.Policies()
	for _, p := range ps {
		if p.ID != id {
			continue
		}
		p.Enabled = !p.Enabled
		if msg := a.policyRefused(r, p); msg != "" {
			a.renderSettings(w, r, u, "", msg)
			return
		}
		if err := a.state.SavePolicy(p, u.Username); err != nil {
			a.renderSettings(w, r, u, "", err.Error())
			return
		}
		a.state.RecordAudit(u.Username, "policy.toggle", fmt.Sprintf("%s enabled=%t", id, p.Enabled), a.clientIP(r))
		a.renderSettings(w, r, u, "Rule "+map[bool]string{true: "enabled", false: "disabled"}[p.Enabled]+".", "")
		return
	}
	a.renderSettings(w, r, u, "", "Rule not found.")
}

func (a *Admin) handleModelRuleDelete(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	id := r.FormValue("id")
	if err := a.state.DeletePolicy(id); err != nil {
		a.renderSettings(w, r, u, "", err.Error())
		return
	}
	a.state.RecordAudit(u.Username, "policy.delete", id, a.clientIP(r))
	a.renderSettings(w, r, u, "Rule deleted.", "")
}

// ModelAttrs describes a model for access policies from what the router
// stores about it: pricing_basis is "actual" when an admin recorded that a
// provider bills for it (a paid API) and "estimated" otherwise. A model with
// no pricing row has no pricing_basis.
func (s *State) ModelAttrs(model string) map[string]any {
	var basis string
	if err := s.db.QueryRow(`SELECT basis FROM model_pricing WHERE model = ?`, model).Scan(&basis); err != nil || basis == "" {
		return nil
	}
	return map[string]any{"pricing_basis": basis}
}

// AuthorizeUpstreamJob decides which of candidates a job arriving from an
// upstream router may run on here, as that router's principal. It is the
// inbound counterpart of AuthorizeModels, which checks API keys.
func (s *State) AuthorizeUpstreamJob(owner string, candidates []string, attrs map[string]map[string]any) ([]string, string) {
	e := s.Authz()
	if e == nil {
		return nil, "access policies are not loaded"
	}
	subj := s.requesterSubject(owner)
	var allowed []string
	reason := ""
	for _, m := range candidates {
		d := e.Decide(authz.Request{Subject: subj, Action: "model.use",
			Resource: authz.Resource{Type: "model", ID: m, Attrs: attrs[m]},
			Context:  authz.Context{Time: time.Now(), CredentialKind: "token", ViaUpstream: true}})
		if d.Allowed {
			allowed = append(allowed, m)
		} else if reason == "" {
			reason = d.Reason
		}
	}
	if len(allowed) == 0 {
		if reason == "" {
			reason = "no model available"
		}
		s.denials.add(Denial{At: time.Now(), Subject: subj.ID, Action: "model.use",
			Resource: strings.Join(candidates, ", "), Reason: reason})
	}
	return allowed, reason
}
