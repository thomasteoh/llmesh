package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"

	"llmesh/router/internal/authz"
)

// Phase 6: the policy editor, the simulator (with a replay of recent traffic
// against a draft), and a log of recent denials.

// --- Recent denials ---

// Denial is one refused access, for the Policies tab.
type Denial struct {
	At       time.Time
	Subject  string
	Action   string
	Resource string
	By       string
	Reason   string
}

const maxDenials = 200

type denialLog struct {
	mu   sync.Mutex
	ring []Denial
	next int
	full bool
}

func (d *denialLog) add(x Denial) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ring == nil {
		d.ring = make([]Denial, maxDenials)
	}
	d.ring[d.next] = x
	d.next = (d.next + 1) % maxDenials
	if d.next == 0 {
		d.full = true
	}
}

// recent returns denials newest first.
func (d *denialLog) recent() []Denial {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := d.next
	if d.full {
		n = maxDenials
	}
	out := make([]Denial, 0, n)
	for i := 0; i < n; i++ {
		idx := (d.next - 1 - i + maxDenials) % maxDenials
		out = append(out, d.ring[idx])
	}
	return out
}

// RecentDenials returns refused accesses, newest first. It is in memory:
// a restart clears it, and the router log keeps the full record.
func (s *State) RecentDenials() []Denial { return s.denials.recent() }

// --- Policy editor ---

// PolicyRow is one policy as the Policies tab lists it.
type PolicyRow struct {
	ID      string
	Name    string
	Effect  string
	Actions string
	Summary string
	Enabled bool
	JSON    string // pretty-printed, for the edit box
}

func (a *Admin) policyRows() []PolicyRow {
	ps, _ := a.state.Policies()
	out := make([]PolicyRow, 0, len(ps))
	for _, p := range ps {
		b, _ := json.MarshalIndent(p, "", "  ")
		summary := "who: " + subjectSummary(p.Subject)
		if p.Resource.Type != "" || len(p.Resource.IDs) > 0 {
			summary += "; what: " + p.Resource.Type
			if len(p.Resource.IDs) > 0 {
				summary += " " + strings.Join(p.Resource.IDs, ", ")
			}
		}
		if p.Condition != nil {
			summary += "; with a condition"
		}
		out = append(out, PolicyRow{ID: p.ID, Name: p.Name, Effect: string(p.Effect),
			Actions: strings.Join(p.Actions, ", "), Summary: summary, Enabled: p.Enabled, JSON: string(b)})
	}
	return out
}

// policyTemplate is the starting point offered for a new policy.
const policyTemplate = `{
  "id": "contractors-local-only",
  "name": "Contractors: local models only, business hours",
  "effect": "deny",
  "enabled": true,
  "subject": {"attrs": {"employment": "contractor"}},
  "actions": ["model.use"],
  "resource": {"type": "model"},
  "condition": {"any": [
    {"ne": ["resource.served_by_kind", "llama.cpp"]},
    {"not": {"time_between": ["context.time", "08:00", "18:00", "Australia/Melbourne"]}}
  ]}
}`

func (a *Admin) handlePolicySave(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	var p authz.Policy
	dec := json.NewDecoder(strings.NewReader(r.FormValue("policy")))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		a.renderSettings(w, r, u, "", "Policy is not valid JSON: "+err.Error())
		return
	}
	if err := a.state.SavePolicy(p, u.Username); err != nil {
		a.renderSettings(w, r, u, "", "Policy not saved: "+err.Error())
		return
	}
	a.state.RecordAudit(u.Username, "policy.save", p.ID, a.clientIP(r))
	a.renderSettings(w, r, u, "Policy "+p.ID+" saved.", "")
}

// --- Simulator ---

// SimForm holds the simulator's inputs, echoed back into the form.
type SimForm struct {
	SubjectKind string // "user", "team", or "key"
	Subject     string
	Action      string
	ResType     string
	ResID       string
	ResOwner    string
	Time        string // datetime-local
	SourceIP    string
	Endpoint    string
	Draft       string
}

// SimResult is what the simulator found.
type SimResult struct {
	Current   authz.Decision
	WithDraft *authz.Decision
	// Replay compares the draft with the live set over recent traffic.
	Replay *ReplayResult
	Error  string
}

// ReplayResult summarises a draft's effect on recent requests.
type ReplayResult struct {
	Window       string
	Requests     int64
	NewlyDenied  int64
	NewlyAllowed int64
	Changes      []ReplayChange
}

// ReplayChange is one (owner, model) pair whose outcome the draft changes.
type ReplayChange struct {
	Owner    string
	Model    string
	Requests int64
	Before   bool
	After    bool
	By       string
}

// simSubject resolves the simulator's subject field.
func (a *Admin) simSubject(kind, name string) (authz.Subject, error) {
	switch kind {
	case "team":
		return a.state.SubjectForTeam(strings.TrimPrefix(name, "team:"))
	case "key":
		k, ok := a.state.LookupAPIKey(name)
		if !ok {
			return authz.Subject{}, fmt.Errorf("no such API key")
		}
		return a.state.keySubject(k.Owner), nil
	default:
		return a.state.SubjectFor(strings.TrimPrefix(name, "user:"))
	}
}

func (a *Admin) handleSimulate(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	f := SimForm{
		SubjectKind: r.FormValue("subject_kind"), Subject: strings.TrimSpace(r.FormValue("subject")),
		Action: r.FormValue("action"), ResType: strings.TrimSpace(r.FormValue("res_type")),
		ResID: strings.TrimSpace(r.FormValue("res_id")), ResOwner: strings.TrimSpace(r.FormValue("res_owner")),
		Time: r.FormValue("time"), SourceIP: strings.TrimSpace(r.FormValue("source_ip")),
		Endpoint: strings.TrimSpace(r.FormValue("endpoint")), Draft: strings.TrimSpace(r.FormValue("draft")),
	}
	res := a.simulate(f, r.FormValue("replay") != "")
	a.renderSettingsWith(w, r, u, "", "", func(p *SettingsPage) { p.Sim, p.SimForm = res, f })
}

func (a *Admin) simulate(f SimForm, replay bool) *SimResult {
	out := &SimResult{}
	live := a.state.Authz()
	var draft *authz.Engine
	if f.Draft != "" {
		var p authz.Policy
		dec := json.NewDecoder(strings.NewReader(f.Draft))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&p); err != nil {
			out.Error = "Draft is not valid JSON: " + err.Error()
			return out
		}
		e, err := a.state.compileWith(p)
		if err != nil {
			out.Error = "Draft does not compile: " + err.Error()
			return out
		}
		draft = e
	}
	if replay {
		if draft == nil {
			out.Error = "Replay compares a draft with the live policies; enter a draft."
			return out
		}
		out.Replay = a.replay(live, draft, 24*time.Hour)
		return out
	}
	subj, err := a.simSubject(f.SubjectKind, f.Subject)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	if !authz.KnownAction(f.Action) {
		out.Error = "Choose an action."
		return out
	}
	req := authz.Request{Subject: subj, Action: f.Action, Resource: ownedResource(f.ResType, f.ResID, f.ResOwner)}
	if req.Resource.Type == "model" {
		req.Resource.Attrs = a.state.ModelAttrs(f.ResID)
		if a.hub != nil {
			if req.Resource.Attrs == nil {
				req.Resource.Attrs = map[string]any{}
			}
			for k, v := range a.hub.ModelAttrs(f.ResID) {
				req.Resource.Attrs[k] = v
			}
		}
	}
	if req.Resource.Type == "client" && f.ResID != "" {
		req.Resource.Sharing = a.state.ClientSharing(f.ResID)
	}
	req.Context.Endpoint = f.Endpoint
	req.Context.Time = time.Now()
	if f.Time != "" {
		if t, err := time.ParseInLocation("2006-01-02T15:04", f.Time, time.Local); err == nil {
			req.Context.Time = t
		}
	}
	if ip, err := netip.ParseAddr(f.SourceIP); err == nil {
		req.Context.SourceIP = ip
	}
	out.Current = live.Decide(req)
	if draft != nil {
		d := draft.Decide(req)
		out.WithDraft = &d
	}
	return out
}

// compileWith compiles the live roles and policies with p added or replacing
// the policy of the same id, without storing anything.
func (s *State) compileWith(p authz.Policy) (*authz.Engine, error) {
	existing, err := s.Policies()
	if err != nil {
		return nil, err
	}
	next := make([]authz.Policy, 0, len(existing)+1)
	for _, e := range existing {
		if e.ID != p.ID {
			next = append(next, e)
		}
	}
	next = append(next, p)
	roles, err := s.CustomRoles()
	if err != nil {
		return nil, err
	}
	return authz.Compile(append(authz.BuiltinRoles(), roles...), next)
}

// replay decides model.use for every (owner, model) pair in recent usage
// under the live policies and the draft, and reports what would change.
// Usage rows carry neither request context nor key scope, so rules that
// depend on those are evaluated as for a request with no context.
func (a *Admin) replay(live, draft *authz.Engine, window time.Duration) *ReplayResult {
	out := &ReplayResult{Window: window.String()}
	rows, err := a.state.db.Query(`SELECT owner, model, SUM(requests) FROM usage_hourly
		WHERE bucket >= ? GROUP BY owner, model`, time.Now().Add(-window).UTC().Format(time.RFC3339))
	if err != nil {
		return out
	}
	type pair struct {
		owner, model string
		n            int64
	}
	var pairs []pair
	for rows.Next() {
		var p pair
		if rows.Scan(&p.owner, &p.model, &p.n) == nil {
			pairs = append(pairs, p)
		}
	}
	rows.Close()
	for _, p := range pairs {
		out.Requests += p.n
		subj := a.state.keySubject(p.owner)
		res := authz.Resource{Type: "model", ID: p.model, Attrs: a.state.ModelAttrs(p.model)}
		before := live.Decide(authz.Request{Subject: subj, Action: "model.use", Resource: res})
		after := draft.Decide(authz.Request{Subject: subj, Action: "model.use", Resource: res})
		if before.Allowed == after.Allowed {
			continue
		}
		if after.Allowed {
			out.NewlyAllowed += p.n
		} else {
			out.NewlyDenied += p.n
		}
		out.Changes = append(out.Changes, ReplayChange{Owner: p.owner, Model: p.model, Requests: p.n,
			Before: before.Allowed, After: after.Allowed, By: after.By})
	}
	sort.Slice(out.Changes, func(i, j int) bool { return out.Changes[i].Requests > out.Changes[j].Requests })
	if len(out.Changes) > 50 {
		out.Changes = out.Changes[:50]
	}
	return out
}
