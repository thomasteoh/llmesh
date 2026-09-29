package admin

import (
	"net/url"
	"strings"
	"testing"

	"llmesh/router/internal/authz"
)

// Regression tests for the findings of the access-v2 security review that
// are not covered alongside the code they changed.

func TestKeyPriorityAtCreationNeedsKeyLimits(t *testing.T) {
	a := newTestAdmin(t)
	withRole(t, a, "ann", authz.RoleMember)
	withRole(t, a, "op", authz.RoleOperator)
	rr := postAs(t, a, "ann", "/portal/api-keys", url.Values{"label": {"fast"}, "priority": {"high"}}, a.handleAPIKeyCreate)
	if !strings.Contains(rr.Body.String(), "permission to set a key") || len(a.state.APIKeysFor("ann", false)) != 0 {
		t.Error("a member created a high-priority key")
	}
	rr = postAs(t, a, "ann", "/portal/api-keys", url.Values{"label": {"odd"}, "priority": {"urgent"}}, a.handleAPIKeyCreate)
	if !strings.Contains(rr.Body.String(), "Priority must be") {
		t.Error("an invalid priority was accepted")
	}
	postAs(t, a, "op", "/portal/api-keys", url.Values{"label": {"fast"}, "priority": {"high"}}, a.handleAPIKeyCreate)
	if keys := a.state.APIKeysFor("op", false); len(keys) != 1 || keys[0].Priority != "high" {
		t.Errorf("an operator could not set priority: %+v", keys)
	}
}

func TestRoleMapNeedsGrantableRoles(t *testing.T) {
	a := newTestAdmin(t)
	withRole(t, a, "adm", authz.RoleAdmin)
	issuer := startFakeIssuer(t, a)
	form := url.Values{"client_id": {"cid"}, "client_secret": {"shh"}, "issuer": {issuer.URL},
		"roles_claim": {"roles"}, "member_role": {"u"}, "role_map": {"big=owner"}}
	rr := postAs(t, a, "adm", "/portal/settings/auth/oidc", form, a.handleOAuthSettingsUpdate(providerOIDC))
	if !strings.Contains(rr.Body.String(), "you cannot map to") {
		t.Fatalf("an admin mapped a provider role to owner: %.300s", rr.Body.String())
	}
}

func TestOIDCSyncKeepsOwner(t *testing.T) {
	a, _ := oidcPolicyAdmin(t, zitadelDoc("s1", "alice", "llmesh-user"), zitadelPolicy)
	addTestUser(t, a, "root", "admin")
	oidcSignIn(t, a)
	a.state.Bind(RoleBinding{Principal: "user:alice", Role: authz.RoleOwner})
	oidcSignIn(t, a)
	if !a.state.IsOwner("alice") {
		t.Error("provider sync stripped an owner")
	}
}

func TestPerModelReservationKeepsDefault(t *testing.T) {
	s := newTestState(t)
	s.AddUser(User{Username: "alice", Role: "member"})
	s.AddClientToken(ClientToken{Name: "box", Owner: "alice", TokenHash: "h"})
	s.SetClientSharing("h", authz.Sharing{Mode: authz.ShareIdle, ReservedSlots: map[string]int{"*": 2}})
	if err := s.SetClientTokenOwnerSlots("alice", "h", "llama3", 1, true); err != nil {
		t.Fatal(err)
	}
	sh := s.ClientSharing("h")
	if sh.ReservedSlots["*"] != 2 || sh.ReservedSlots["llama3"] != 1 || sh.Mode != authz.ShareIdle {
		t.Errorf("sharing after a per-model change: %+v", sh)
	}
}

func TestAdminDisableOverridesProviderDisable(t *testing.T) {
	a := newTestAdmin(t)
	withRole(t, a, "root", authz.RoleOwner)
	withRole(t, a, "alice", authz.RoleMember)
	a.state.SetDisabledBy("alice", true, providerOIDC)
	postAs(t, a, "root", "/portal/settings/users/disable", url.Values{"username": {"alice"}}, a.handleUserDisable)
	if by := a.state.DisabledBy("alice"); by != "" {
		t.Errorf("an admin's disable left disabled_by=%q, so the next sign-in would undo it", by)
	}
}

// Found in the re-review: a conditional deny on the repair actions could
// slip past the lockout check.
func TestConditionalDenyCannotLockOutOwners(t *testing.T) {
	a := newTestAdmin(t)
	withRole(t, a, "root", authz.RoleOwner)
	withRole(t, a, "adm", authz.RoleAdmin)
	sneaky := `{"id":"sneaky","effect":"deny","enabled":true,"actions":["policy.manage","user.manage","role.manage","owner.manage"],
		"subject":{"roles":["owner"]},"condition":{"exists":["context.credential_kind"]}}`
	rr := postAs(t, a, "adm", "/portal/settings/policies", url.Values{"policy": {sneaky}}, a.handlePolicySave)
	if !strings.Contains(rr.Body.String(), "Only an owner can write a deny policy") {
		t.Fatalf("an admin saved a deny on the repair actions: %.300s", rr.Body.String())
	}
	// The owner is stopped by the lockout check, now run with the portal's
	// own context.
	rr = postAs(t, a, "root", "/portal/settings/policies", url.Values{"policy": {sneaky}}, a.handlePolicySave)
	if !strings.Contains(rr.Body.String(), "no owner able to manage") {
		t.Fatalf("a conditional lockout passed the check: %.300s", rr.Body.String())
	}
}

// Found in the re-review: re-enabling and isolation skipped the account
// change rule.
func TestHelpdeskCannotReenableAdmin(t *testing.T) {
	a := newTestAdmin(t)
	withRole(t, a, "root", authz.RoleOwner)
	withRole(t, a, "adm", authz.RoleAdmin)
	a.state.SaveRole(authz.Role{ID: "helpdesk", Permissions: []string{"user.manage", "user.view"}})
	withRole(t, a, "hd", "helpdesk")
	postAs(t, a, "root", "/portal/settings/users/disable", url.Values{"username": {"adm"}}, a.handleUserDisable)
	postAs(t, a, "hd", "/portal/settings/users/enable", url.Values{"username": {"adm"}}, a.handleUserEnable)
	if u, _ := a.state.LookupUser("adm"); !u.Disabled {
		t.Error("helpdesk re-enabled an admin an owner disabled")
	}
	postAs(t, a, "hd", "/portal/settings/users/isolation", url.Values{"username": {"root"}, "field": {"receive"}, "value": {"1"}}, a.handleUserIsolation)
	if u, _ := a.state.LookupUser("root"); u.ReceiveIsolation {
		t.Error("helpdesk changed an owner's isolation")
	}
}
