package admin

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"llmesh/router/internal/authz"
)

func TestPolicyEditor(t *testing.T) {
	a := newTestAdmin(t)
	withRole(t, a, "root", authz.RoleOwner)
	withRole(t, a, "aud", authz.RoleAuditor)

	for _, tc := range []struct{ body, want string }{
		{`{not json`, "not valid JSON"},
		{`{"id":"x","effect":"deny","actions":["model.use"],"colour":"red"}`, "not valid JSON"}, // unknown field
		{`{"id":"x","effect":"deny","actions":["model.fly"]}`, "Policy not saved"},
		{`{"id":"x","effect":"deny","actions":["model.use"],"condition":{"eq":["subject.nope","a"]}}`, "Policy not saved"},
	} {
		rr := postAs(t, a, "root", "/portal/settings/policies", url.Values{"policy": {tc.body}}, a.handlePolicySave)
		if !strings.Contains(rr.Body.String(), tc.want) {
			t.Errorf("%s: expected %q", tc.body, tc.want)
		}
	}
	rr := postAs(t, a, "root", "/portal/settings/policies", url.Values{"policy": {policyTemplate}}, a.handlePolicySave)
	if !strings.Contains(rr.Body.String(), "Policy contractors-local-only saved") {
		t.Fatalf("the template did not save: %.300s", rr.Body.String())
	}
	if routeStatus(t, a, "aud", "policy.manage") != 403 || routeStatus(t, a, "aud", "policy.simulate") != 200 {
		t.Error("an auditor should simulate but not edit")
	}
	var found bool
	for _, p := range a.policyRows() {
		if p.ID == "contractors-local-only" && strings.Contains(p.Summary, "condition") && strings.Contains(p.JSON, "time_between") {
			found = true
		}
	}
	if !found {
		t.Error("the saved policy is not listed with its JSON")
	}
}

func TestSimulator(t *testing.T) {
	a := newTestAdmin(t)
	withRole(t, a, "root", authz.RoleOwner)
	withRole(t, a, "bob", authz.RoleMember)
	a.state.AddAPIKey(testAPIKey("k", "bob", "sk-bob-sim", "normal"))

	res := a.simulate(SimForm{SubjectKind: "user", Subject: "bob", Action: "model.use", ResType: "model", ResID: "gpt-4o"}, false)
	if res.Error != "" || !res.Current.Allowed || res.Current.By != defaultModelPolicy.ID {
		t.Fatalf("live decision: %+v", res)
	}
	draft := `{"id":"no-gpt","effect":"deny","enabled":true,"actions":["model.use"],"resource":{"type":"model","ids":["gpt-*"]}}`
	res = a.simulate(SimForm{SubjectKind: "key", Subject: "sk-bob-sim", Action: "model.use", ResType: "model", ResID: "gpt-4o", Draft: draft}, false)
	if !res.Current.Allowed || res.WithDraft == nil || res.WithDraft.Allowed || res.WithDraft.By != "no-gpt" {
		t.Fatalf("draft decision: %+v %+v", res, res.WithDraft)
	}
	if ps, _ := a.state.Policies(); len(ps) != 1 {
		t.Fatal("simulating a draft stored it")
	}
	// Time-dependent drafts use the time given.
	night := `{"id":"night","effect":"deny","enabled":true,"actions":["model.use"],"condition":{"time_between":["context.time","22:00","06:00","UTC"]}}`
	day := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC).Local().Format("2006-01-02T15:04")
	res = a.simulate(SimForm{SubjectKind: "user", Subject: "bob", Action: "model.use", ResType: "model", ResID: "m", Time: day, Draft: night}, false)
	if res.WithDraft == nil || !res.WithDraft.Allowed {
		t.Errorf("a night-only rule applied at noon: %+v", res.WithDraft)
	}
	if res := a.simulate(SimForm{SubjectKind: "user", Subject: "bob", Action: "model.use", Draft: "{"}, false); !strings.Contains(res.Error, "not valid JSON") {
		t.Errorf("bad draft: %+v", res)
	}
	if res := a.simulate(SimForm{SubjectKind: "user", Subject: "ghost", Action: "model.use"}, false); res.Error == "" {
		t.Error("an unknown user was simulated")
	}

	// Replay: bob made 7 requests to gpt-4o and 3 to qwen3 yesterday.
	b := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour).Format(time.RFC3339)
	for _, q := range []string{
		`INSERT INTO usage_hourly (bucket, owner, key_label, model, requests, prompt_tokens, completion_tokens) VALUES ('` + b + `', 'bob', 'bob/k', 'gpt-4o', 7, 0, 0)`,
		`INSERT INTO usage_hourly (bucket, owner, key_label, model, requests, prompt_tokens, completion_tokens) VALUES ('` + b + `', 'bob', 'bob/k', 'qwen3', 3, 0, 0)`,
	} {
		if _, err := a.state.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	res = a.simulate(SimForm{Draft: draft}, true)
	if res.Replay == nil || res.Replay.Requests != 10 || res.Replay.NewlyDenied != 7 || len(res.Replay.Changes) != 1 ||
		res.Replay.Changes[0].Model != "gpt-4o" {
		t.Fatalf("replay: %+v", res.Replay)
	}
	if res := a.simulate(SimForm{}, true); !strings.Contains(res.Error, "enter a draft") {
		t.Errorf("replay without a draft: %+v", res)
	}
}

func TestDenialsAreRecorded(t *testing.T) {
	s := newTestState(t)
	s.AddUser(User{Username: "bob", Role: "member"})
	s.AddAPIKey(testAPIKey("k", "bob", "sk-bob-d", "normal"))
	s.SavePolicy(authz.Policy{ID: "no-gpt", Effect: authz.Deny, Enabled: true, Actions: []string{"model.use"},
		Resource: authz.ResourceMatcher{Type: "model", IDs: []string{"gpt-*"}}}, "t")
	s.AuthorizeModels("sk-bob-d", "/v1/chat/completions", "", []string{"gpt-4o"}, nil)
	s.AuthorizeModels("sk-bob-d", "/v1/chat/completions", "", []string{"qwen3"}, nil) // allowed: not recorded
	d := s.RecentDenials()
	if len(d) != 1 || d[0].Subject != "user:bob" || d[0].Resource != "gpt-4o" || !strings.Contains(d[0].Reason, "no-gpt") {
		t.Fatalf("denials: %+v", d)
	}
	// The ring keeps the newest.
	for i := 0; i < maxDenials+5; i++ {
		s.AuthorizeModels("sk-bob-d", "", "", []string{"gpt-4o"}, nil)
	}
	if n := len(s.RecentDenials()); n != maxDenials {
		t.Errorf("ring holds %d, want %d", n, maxDenials)
	}
}

// A rule on how a model is served works end to end, from the attributes the
// hub and pricing report through AuthorizeModels.
func TestAttributeRules(t *testing.T) {
	s := newTestState(t)
	s.AddUser(User{Username: "carl", Role: "member"})
	s.db.Exec(`UPDATE users SET attrs = '{"employment":"contractor"}' WHERE username = 'carl'`)
	s.AddAPIKey(testAPIKey("k", "carl", "sk-carl", "normal"))
	s.SavePolicy(authz.Policy{ID: "contractors-local", Effect: authz.Deny, Enabled: true,
		Subject: authz.SubjectMatcher{Attrs: map[string]string{"employment": "contractor"}},
		Actions: []string{"model.use"}, Resource: authz.ResourceMatcher{Type: "model"},
		Condition: mustCond(t, `{"ne": ["resource.served_by_kind", "llama.cpp"]}`)}, "t")
	attrs := map[string]map[string]any{
		"local": {"served_by_kind": "llama.cpp"},
		"paid":  {"served_by_kind": "shim"},
	}
	got, _ := s.AuthorizeModels("sk-carl", "", "", []string{"local", "paid", "unknown"}, attrs)
	if strings.Join(got, ",") != "local" {
		t.Errorf("contractor got %v, want only the local model (unknown kind stays denied)", got)
	}
}

func TestUserAttrsEditor(t *testing.T) {
	a := newTestAdmin(t)
	withRole(t, a, "root", authz.RoleOwner)
	withRole(t, a, "carl", authz.RoleMember)
	postAs(t, a, "root", "/portal/settings/users/attrs", url.Values{"username": {"carl"}, "attrs": {"employment = contractor\ndepartment=ops\n"}}, a.handleUserAttrs)
	subj, _ := a.state.SubjectFor("carl")
	if subj.Attrs["employment"] != "contractor" || subj.Attrs["department"] != "ops" {
		t.Fatalf("attrs: %v", subj.Attrs)
	}
	rr := postAs(t, a, "root", "/portal/settings/users/attrs", url.Values{"username": {"carl"}, "attrs": {"Bad Key=x"}}, a.handleUserAttrs)
	if !strings.Contains(rr.Body.String(), "attribute name") {
		t.Error("an invalid attribute name was accepted")
	}
	// The route requires user.manage, so members cannot rewrite their own
	// attributes (and with them, which policies apply to them).
	if routeStatus(t, a, "carl", "user.manage") != 403 {
		t.Error("a member may edit attributes")
	}
}
