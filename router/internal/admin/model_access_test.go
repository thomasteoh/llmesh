package admin

import (
	"net/url"
	"strings"
	"testing"

	"llmesh/router/internal/authz"
)

func TestAuthorizeModels(t *testing.T) {
	s := newTestState(t)
	s.AddUser(User{Username: "ann", Role: "member"})
	s.AddUser(User{Username: "vic", Role: "member"})
	s.Unbind(RoleBinding{Principal: "user:vic", Role: authz.RoleMember})
	s.Bind(RoleBinding{Principal: "user:vic", Role: authz.RoleViewer})
	s.AddAPIKey(testAPIKey("k", "ann", "sk-ann-1", "normal"))
	s.AddAPIKey(testAPIKey("v", "vic", "sk-vic-1", "normal"))

	models := []string{"gpt-4o", "qwen3-30b", "llama3"}
	got, _ := s.AuthorizeModels("sk-ann-1", "/v1/chat/completions", "10.0.0.1", models, nil)
	if len(got) != 3 {
		t.Fatalf("default grant: %v", got)
	}
	// A viewer holds no model.use at all.
	if got, reason := s.AuthorizeModels("sk-vic-1", "", "", models, nil); len(got) != 0 || reason == "" {
		t.Errorf("viewer: %v (%q)", got, reason)
	}

	// A deny rule narrows everyone.
	s.SavePolicy(authz.Policy{ID: "no-gpt", Effect: authz.Deny, Enabled: true, Actions: []string{"model.use"},
		Resource: authz.ResourceMatcher{Type: "model", IDs: []string{"gpt-*"}}}, "root")
	got, _ = s.AuthorizeModels("sk-ann-1", "", "", models, nil)
	if strings.Join(got, ",") != "qwen3-30b,llama3" {
		t.Errorf("after deny: %v", got)
	}
	got, reason := s.AuthorizeModels("sk-ann-1", "", "", []string{"gpt-4o"}, nil)
	if len(got) != 0 || !strings.Contains(reason, "denied by policy") {
		t.Errorf("single denied model: %v (%q)", got, reason)
	}

	// A key scope narrows further, and never widens.
	k := testAPIKey("scoped", "ann", "sk-ann-2", "normal")
	k.Scope = KeyScope{Models: []string{"qwen*", "gpt-*"}, Endpoints: []string{"/v1/messages"}}
	s.AddAPIKey(k)
	got, _ = s.AuthorizeModels("sk-ann-2", "/v1/messages", "", models, nil)
	if strings.Join(got, ",") != "qwen3-30b" {
		t.Errorf("scoped key: %v (the scope's gpt-* must not undo the deny)", got)
	}
	if got, reason := s.AuthorizeModels("sk-ann-2", "/v1/chat/completions", "", models, nil); len(got) != 0 || !strings.Contains(reason, "may not use") {
		t.Errorf("scoped key, wrong endpoint: %v (%q)", got, reason)
	}
	stored, _ := s.LookupAPIKey("sk-ann-2")
	if len(stored.Scope.Models) != 2 || len(stored.Scope.Endpoints) != 1 {
		t.Errorf("scope did not round-trip: %+v", stored.Scope)
	}
}

func TestTeamKeyActsAsTeam(t *testing.T) {
	s := newTestState(t)
	s.AddUser(User{Username: "mia", Role: "member"})
	s.CreateTeam("research", "", "", "mia")
	s.AddAPIKey(testAPIKey("t", "team:research", "sk-team-1", "normal"))
	s.SavePolicy(authz.Policy{ID: "research-only", Effect: authz.Allow, Enabled: true, Actions: []string{"model.use"},
		Subject:  authz.SubjectMatcher{Teams: []string{"research"}},
		Resource: authz.ResourceMatcher{Type: "model", IDs: []string{"big-*"}}}, "root")
	s.SavePolicy(authz.Policy{ID: "big-nobody-else", Effect: authz.Deny, Enabled: true, Actions: []string{"model.use"},
		Resource:  authz.ResourceMatcher{Type: "model", IDs: []string{"big-*"}},
		Condition: mustCond(t, `{"not": {"has": ["subject.teams", "research"]}}`)}, "root")
	if got, _ := s.AuthorizeModels("sk-team-1", "", "", []string{"big-model"}, nil); len(got) != 1 {
		t.Errorf("a team key could not use the team's model: %v", got)
	}
	s.AddAPIKey(testAPIKey("m", "mia", "sk-mia-1", "normal"))
	s.AddUser(User{Username: "eve", Role: "member"})
	s.AddAPIKey(testAPIKey("e", "eve", "sk-eve-1", "normal"))
	if got, _ := s.AuthorizeModels("sk-eve-1", "", "", []string{"big-model"}, nil); len(got) != 0 {
		t.Errorf("an outsider used the team's model: %v", got)
	}
	if got, _ := s.AuthorizeModels("sk-mia-1", "", "", []string{"big-model"}, nil); len(got) != 1 {
		t.Errorf("a team member's own key could not use the team's model: %v", got)
	}
}

func mustCond(t *testing.T, s string) *authz.Cond {
	t.Helper()
	var c authz.Cond
	if err := c.UnmarshalJSON([]byte(s)); err != nil {
		t.Fatal(err)
	}
	return &c
}

func TestModelAccessTab(t *testing.T) {
	a := newTestAdmin(t)
	withRole(t, a, "root", authz.RoleOwner)
	withRole(t, a, "op", authz.RoleOperator)

	rr := postAs(t, a, "root", "/portal/settings/model-access", url.Values{
		"effect": {"deny"}, "models": {"gpt-*, claude-*"}, "teams": {"interns"}, "users": {"bob"},
	}, a.handleModelRuleSave)
	if !strings.Contains(rr.Body.String(), "Model access rule saved") {
		t.Fatalf("save: %.300s", rr.Body.String())
	}
	rules := a.modelRules()
	if len(rules) != 2 {
		t.Fatalf("rules: %+v", rules)
	}
	var id string
	for _, r := range rules {
		if r.Effect == "deny" {
			id = r.ID
			if !strings.Contains(r.Who, "user:bob") || !strings.Contains(r.Who, "teams interns") || len(r.Models) != 2 {
				t.Errorf("saved rule: %+v", r)
			}
		}
	}
	postAs(t, a, "root", "/portal/settings/model-access/toggle", url.Values{"id": {id}}, a.handleModelRuleToggle)
	for _, r := range a.modelRules() {
		if r.ID == id && r.Enabled {
			t.Error("toggle did not disable the rule")
		}
	}
	postAs(t, a, "root", "/portal/settings/model-access/delete", url.Values{"id": {id}}, a.handleModelRuleDelete)
	if len(a.modelRules()) != 1 {
		t.Error("delete did not remove the rule")
	}
	// Operators cannot manage policies.
	if routeStatus(t, a, "op", "policy.manage") != 403 {
		t.Error("an operator may manage model access")
	}
}

func TestKeyFormSavesScope(t *testing.T) {
	a := newTestAdmin(t)
	addTestUser(t, a, "ann", "member")
	postAs(t, a, "ann", "/portal/api-keys", url.Values{"label": {"narrow"}, "scope_models": {"qwen*, llama3"},
		"scope_endpoints": {"/v1/messages", "/etc/passwd"}}, a.handleAPIKeyCreate)
	keys := a.state.APIKeysFor("ann", false)
	if len(keys) != 1 || strings.Join(keys[0].Scope.Models, ",") != "qwen*,llama3" ||
		strings.Join(keys[0].Scope.Endpoints, ",") != "/v1/messages" {
		t.Fatalf("scope: %+v", keys)
	}
}
