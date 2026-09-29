package admin

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"llmesh/router/internal/authz"
)

// zitadelSyncPolicy maps roles, groups, and a claim, with revalidation on.
func zitadelSyncPolicy() OIDCConfig {
	return OIDCConfig{
		RolesClaim:        "urn:zitadel:iam:org:project:roles",
		MemberRole:        "llmesh-user",
		RoleMap:           map[string]string{"llmesh-ops": authz.RoleOperator, "llmesh-audit": authz.RoleAuditor},
		Provision:         true,
		GroupsClaim:       "groups",
		TeamMap:           map[string]string{"research-staff": "research", "ops-staff": "ops"},
		AttrMap:           map[string]string{"department": "department", "employment_type": "employment"},
		RevalidateMinutes: 60,
	}
}

func syncDoc(sub string, roles, groups []string, attrs map[string]string) map[string]any {
	doc := zitadelDoc(sub, "alice", roles...)
	doc["groups"] = groups
	for k, v := range attrs {
		doc[k] = v
	}
	return doc
}

func TestOIDCSyncsRolesTeamsAndAttrs(t *testing.T) {
	a, f := oidcPolicyAdmin(t, syncDoc("s1", []string{"llmesh-user", "llmesh-ops"}, []string{"research-staff"},
		map[string]string{"department": "finance", "employment_type": "contractor"}), OIDCConfig{})
	addTestUser(t, a, "root", "admin")
	a.state.CreateTeam("research", "", "", "root")
	a.state.CreateTeam("ops", "", "", "root")
	a.state.CreateTeam("social", "", "", "root") // not mapped: never touched
	p := zitadelSyncPolicy()
	p.Issuer = testOIDCIssuer
	if err := a.state.SetOIDC(p); err != nil {
		t.Fatal(err)
	}

	if rr := oidcSignIn(t, a); rr.Code != http.StatusFound {
		t.Fatalf("sign-in: %d %s", rr.Code, rr.Body.String())
	}
	if got := a.globalRoles("alice"); !sameRoles(got, authz.RoleMember, authz.RoleOperator) {
		t.Errorf("roles: %v", got)
	}
	if teams, _ := a.state.TeamsOf("alice"); strings.Join(teams, ",") != "research" {
		t.Errorf("teams: %v", teams)
	}
	a.state.AddTeamMember("social", "alice", false)
	a.state.SetUserAttrs("alice", map[string]string{"department": "finance", "employment": "contractor", "desk": "7"})
	if at := a.state.UserAttrs("alice"); at["department"] != "finance" || at["employment"] != "contractor" {
		t.Errorf("attrs: %v", at)
	}
	if a.state.oidcRefreshToken("alice") != "rt-1" {
		t.Error("the refresh token was not kept for revalidation")
	}

	// The provider changes its mind: ops role gone, audit added, moved team,
	// department changed, employment type removed.
	f.account.doc = syncDoc("s1", []string{"llmesh-user", "llmesh-audit"}, []string{"ops-staff"},
		map[string]string{"department": "ops"})
	oidcSignIn(t, a)
	if got := a.globalRoles("alice"); !sameRoles(got, authz.RoleAuditor, authz.RoleMember) {
		t.Errorf("roles after change: %v", got)
	}
	teams, _ := a.state.TeamsOf("alice")
	if strings.Join(teams, ",") != "ops,social" {
		t.Errorf("teams after change: %v (an unmapped team must be left alone)", teams)
	}
	at := a.state.UserAttrs("alice")
	if at["department"] != "ops" || at["employment"] != "" || at["desk"] != "7" {
		t.Errorf("attrs after change: %v (unmapped attributes must be kept)", at)
	}
}

func TestOIDCRequestsOfflineAccess(t *testing.T) {
	a, _ := oidcPolicyAdmin(t, zitadelDoc("s1", "alice", "llmesh-user"), OIDCConfig{})
	p := zitadelSyncPolicy()
	p.Issuer = testOIDCIssuer
	a.state.SetOIDC(p)
	pr, _ := a.providerFor(providerOIDC)
	if !strings.Contains(pr.scope, "offline_access") {
		t.Errorf("scope %q lacks offline_access with revalidation on", pr.scope)
	}
	p.RevalidateMinutes = 0
	a.state.SetOIDC(p)
	if pr, _ := a.providerFor(providerOIDC); strings.Contains(pr.scope, "offline_access") {
		t.Errorf("scope %q asks for offline_access with revalidation off", pr.scope)
	}
}

func TestOIDCRevalidation(t *testing.T) {
	setup := func(t *testing.T) (*Admin, *fakeProvider) {
		a, f := oidcPolicyAdmin(t, zitadelDoc("s1", "alice", "llmesh-user"), OIDCConfig{})
		addTestUser(t, a, "root", "admin")
		p := zitadelSyncPolicy()
		p.Issuer = testOIDCIssuer
		p.GroupsClaim, p.TeamMap, p.AttrMap = "", nil, nil
		a.state.SetOIDC(p)
		if rr := oidcSignIn(t, a); rr.Code != http.StatusFound {
			t.Fatalf("sign-in: %d", rr.Code)
		}
		a.state.AddAPIKey(testAPIKey("k", "alice", "sk-alice-rv", "normal"))
		return a, f
	}
	disabled := func(a *Admin) bool { u, _ := a.state.LookupUser("alice"); return u.Disabled }

	t.Run("still permitted", func(t *testing.T) {
		a, f := setup(t)
		f.account.doc = zitadelDoc("s1", "alice", "llmesh-user", "llmesh-ops")
		a.revalidateOIDC(context.Background())
		if disabled(a) || f.refreshes != 1 {
			t.Fatalf("disabled=%v refreshes=%d", disabled(a), f.refreshes)
		}
		if got := a.globalRoles("alice"); !sameRoles(got, authz.RoleMember, authz.RoleOperator) {
			t.Errorf("roles were not re-synced: %v", got)
		}
		if a.state.oidcRefreshToken("alice") != "rt-rotated" {
			t.Error("a rotated refresh token was not stored")
		}
	})
	t.Run("deactivated at the provider", func(t *testing.T) {
		a, f := setup(t)
		session := a.sessions.create("alice")
		f.refreshErr = "invalid_grant"
		a.revalidateOIDC(context.Background())
		if !disabled(a) || a.state.DisabledBy("alice") != providerOIDC {
			t.Fatal("a refused refresh did not disable the account")
		}
		if _, ok := a.sessions.lookup(session); ok {
			t.Error("the disabled account kept a session")
		}
		if a.state.ValidAPIKey("sk-alice-rv") {
			t.Error("the disabled account's API key still works")
		}
		if a.state.oidcRefreshToken("alice") != "" {
			t.Error("a disabled account kept its refresh token")
		}
		// When the provider vouches for her again, her next sign-in restores her.
		f.refreshErr = ""
		if rr := oidcSignIn(t, a); rr.Code != http.StatusFound || disabled(a) {
			t.Fatalf("sign-in after reinstatement: %d disabled=%v", rr.Code, disabled(a))
		}
	})
	t.Run("lost every role", func(t *testing.T) {
		a, f := setup(t)
		f.account.doc = zitadelDoc("s1", "alice")
		a.revalidateOIDC(context.Background())
		if !disabled(a) {
			t.Fatal("an account with no granting role stayed enabled")
		}
	})
	t.Run("different account behind the token", func(t *testing.T) {
		a, f := setup(t)
		f.account.doc = zitadelDoc("s2", "mallory", "llmesh-user")
		a.revalidateOIDC(context.Background())
		if !disabled(a) {
			t.Fatal("a refresh that returned a different subject did not disable the account")
		}
	})
	t.Run("provider unreachable", func(t *testing.T) {
		a, f := setup(t)
		f.refreshStatus = http.StatusBadGateway
		a.revalidateOIDC(context.Background())
		if disabled(a) {
			t.Fatal("a provider outage disabled an account")
		}
	})
	t.Run("expired client secret is not a refusal", func(t *testing.T) {
		a, f := setup(t)
		f.refreshErr = "invalid_client"
		a.revalidateOIDC(context.Background())
		if disabled(a) {
			t.Fatal("a configuration error (invalid_client) disabled an account")
		}
	})
	t.Run("last admin is never disabled", func(t *testing.T) {
		a, f := setup(t)
		a.state.UpdateUser("root", func(u *User) { u.Disabled = true })
		a.state.Bind(RoleBinding{Principal: "user:alice", Role: authz.RoleAdmin})
		f.refreshErr = "invalid_grant"
		a.revalidateOIDC(context.Background())
		if disabled(a) {
			t.Fatal("revalidation disabled the last active admin")
		}
	})
	t.Run("admin-disabled stays disabled", func(t *testing.T) {
		a, _ := setup(t)
		a.state.UpdateUser("alice", func(u *User) { u.Disabled = true })
		if rr := oidcSignIn(t, a); rr.Code == http.StatusFound || !disabled(a) {
			t.Fatal("signing in re-enabled an account an admin disabled")
		}
	})
}

func TestOIDCSyncSettingsValidation(t *testing.T) {
	a := newTestAdmin(t)
	addTestUser(t, a, "admin", "admin")
	issuer := startFakeIssuer(t, a)
	base := func() url.Values {
		return url.Values{"client_id": {"cid"}, "client_secret": {"shh"}, "issuer": {issuer.URL},
			"roles_claim": {"roles"}, "member_role": {"u"}}
	}
	for _, tc := range []struct{ field, value, want string }{
		{"role_map", "x=wizard", "unknown role"},
		{"role_map", "x=team-member", "team roles come from the team mapping"},
		{"role_map", "no-equals", "not name=value"},
		{"team_map", "g=ghost", "unknown team"},
		{"attr_map", "claim=Bad Name", "not a valid attribute name"},
		{"revalidate_minutes", "2", "at most every 5 minutes"},
		{"revalidate_minutes", "soon", "number of minutes"},
	} {
		form := base()
		form.Set(tc.field, tc.value)
		rr := postAs(t, a, "admin", "/portal/settings/auth/oidc", form, a.handleOAuthSettingsUpdate(providerOIDC))
		if !strings.Contains(rr.Body.String(), tc.want) {
			t.Errorf("%s=%q: expected %q", tc.field, tc.value, tc.want)
		}
	}
	form := base()
	form.Set("role_map", "ops=operator\n\naudit=auditor")
	form.Set("revalidate_minutes", "30")
	postAs(t, a, "admin", "/portal/settings/auth/oidc", form, a.handleOAuthSettingsUpdate(providerOIDC))
	got := a.state.OIDC()
	if got.RoleMap["ops"] != "operator" || got.RoleMap["audit"] != "auditor" || got.RevalidateMinutes != 30 {
		t.Fatalf("stored: %+v", got)
	}
}

// If the provider seems to refuse most accounts at once, the run disables
// no one: that is a misconfiguration, not a mass deactivation.
func TestOIDCRevalidationCircuitBreaker(t *testing.T) {
	a, f := oidcPolicyAdmin(t, zitadelDoc("s1", "alice", "llmesh-user"), OIDCConfig{})
	addTestUser(t, a, "root", "admin")
	p := zitadelSyncPolicy()
	p.Issuer = testOIDCIssuer
	p.GroupsClaim, p.TeamMap, p.AttrMap = "", nil, nil
	a.state.SetOIDC(p)
	for _, sub := range []string{"s1", "s2", "s3"} {
		f.account.doc = zitadelDoc(sub, "user-"+sub, "llmesh-user")
		f.account.id = oidcSubjectID(testOIDCIssuer, sub)
		if rr := oidcSignIn(t, a); rr.Code != http.StatusFound {
			t.Fatalf("sign-in %s: %d", sub, rr.Code)
		}
	}
	f.refreshErr = "invalid_grant"
	a.revalidateOIDC(context.Background())
	for _, u := range a.state.Users() {
		if u.ManagedBy == providerOIDC && u.Disabled {
			t.Errorf("%s was disabled by a run that refused every account", u.Username)
		}
	}
}
