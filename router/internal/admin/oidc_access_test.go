package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func rawClaims(t *testing.T, doc string) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(doc), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestClaimRoles(t *testing.T) {
	cases := []struct {
		name, doc, claim string
		want             []string
	}{
		{"list", `{"roles":["a","b"]}`, "roles", []string{"a", "b"}},
		{"string", `{"role":"a"}`, "role", []string{"a"}},
		{"zitadel object", `{"urn:zitadel:iam:org:project:roles":{"llmesh-admin":{"123":"acme.zitadel.cloud"}}}`,
			"urn:zitadel:iam:org:project:roles", []string{"llmesh-admin"}},
		{"keycloak nested", `{"realm_access":{"roles":["a"]}}`, "realm_access.roles", []string{"a"}},
		{"top-level wins over path", `{"a.b":["top"],"a":{"b":["nested"]}}`, "a.b", []string{"top"}},
		{"missing", `{"sub":"1"}`, "roles", nil},
		{"wrong type", `{"roles":42}`, "roles", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := claimRoles(rawClaims(t, tc.doc), tc.claim)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for _, r := range tc.want {
				if !got[r] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestRoleFor(t *testing.T) {
	policy := OIDCConfig{RolesClaim: "roles", MemberRole: "user", AdminRole: "admin"}
	cases := []struct {
		doc         string
		wantRole    string
		wantAllowed bool
	}{
		{`{"roles":["admin","user"]}`, "admin", true},
		{`{"roles":["user"]}`, "member", true},
		{`{"roles":["other"]}`, "", false},
		{`{}`, "", false},
	}
	for _, tc := range cases {
		role, ok := policy.roleFor(rawClaims(t, tc.doc))
		if role != tc.wantRole || ok != tc.wantAllowed {
			t.Errorf("%s: got (%q, %v), want (%q, %v)", tc.doc, role, ok, tc.wantRole, tc.wantAllowed)
		}
	}
	// With the policy off the provider has no say.
	if role, ok := (OIDCConfig{}).roleFor(rawClaims(t, `{}`)); role != "" || !ok {
		t.Errorf("policy off: got (%q, %v)", role, ok)
	}
}

func TestValidateAccess(t *testing.T) {
	if err := (OIDCConfig{RolesClaim: "roles"}).validateAccess(); err == nil {
		t.Error("a roles claim with no role to require was accepted")
	}
	if err := (OIDCConfig{Provision: true}).validateAccess(); err == nil {
		t.Error("provisioning without a role requirement was accepted")
	}
	if err := (OIDCConfig{RolesClaim: "roles", MemberRole: "u", Provision: true}).validateAccess(); err != nil {
		t.Errorf("a valid policy was refused: %v", err)
	}
}

func TestProvisionedUsername(t *testing.T) {
	for _, tc := range []struct {
		ident oauthIdentity
		want  string
	}{
		{oauthIdentity{Username: "alice@acme.zitadel.cloud"}, "alice"},
		{oauthIdentity{Username: "Bob Smith"}, "bob-smith"},
		{oauthIdentity{Label: "carol@example.com"}, "carol"},
		{oauthIdentity{Label: "not-an-address"}, "user"},
		{oauthIdentity{Username: "../../etc"}, "etc"},
		{oauthIdentity{Username: strings.Repeat("x", 50)}, strings.Repeat("x", 32)},
	} {
		if got := provisionedUsername(tc.ident); got != tc.want {
			t.Errorf("provisionedUsername(%+v) = %q, want %q", tc.ident, got, tc.want)
		}
	}
}

// oidcPolicyAdmin returns an Admin whose OIDC provider reports doc and has the
// given access policy.
func oidcPolicyAdmin(t *testing.T, doc map[string]any, policy OIDCConfig) (*Admin, *fakeProvider) {
	t.Helper()
	a := newTestAdmin(t)
	sub, _ := doc["sub"].(string)
	f := configureFakeProvider(t, a, providerOIDC, providerAccount{doc: doc, id: oidcSubjectID(testOIDCIssuer, sub)})
	policy.Issuer = testOIDCIssuer
	if err := a.state.SetOIDC(policy); err != nil {
		t.Fatal(err)
	}
	return a, f
}

func oidcSignIn(t *testing.T, a *Admin) *httptest.ResponseRecorder {
	t.Helper()
	stateCookie, nonce := startAuthorization(t, a, providerOIDC, oauthModeLogin, nil)
	return callback(a, providerOIDC, stateCookie, nonce, nil)
}

var zitadelPolicy = OIDCConfig{
	RolesClaim: "urn:zitadel:iam:org:project:roles",
	MemberRole: "llmesh-user",
	AdminRole:  "llmesh-admin",
	Provision:  true,
}

func zitadelDoc(sub, username string, roles ...string) map[string]any {
	granted := map[string]any{}
	for _, r := range roles {
		granted[r] = map[string]string{"123": "acme.zitadel.cloud"}
	}
	return map[string]any{
		"sub": sub, "preferred_username": username,
		"email": username + "@example.com", "email_verified": true,
		"urn:zitadel:iam:org:project:roles": granted,
	}
}

func TestOIDCProvisionsManagedAccount(t *testing.T) {
	a, _ := oidcPolicyAdmin(t, zitadelDoc("s1", "alice@acme.zitadel.cloud", "llmesh-admin"), zitadelPolicy)
	addTestUser(t, a, "root", "admin")

	rr := oidcSignIn(t, a)
	if rr.Code != http.StatusFound || sessionCookieFrom(rr) == nil {
		t.Fatalf("first sign-in did not succeed: %d %s", rr.Code, rr.Body.String())
	}
	u, ok := a.state.LookupUser("alice")
	if !ok {
		t.Fatal("no account was created")
	}
	if u.Role != "admin" || u.ManagedBy != providerOIDC || u.OIDCSubject != oidcSubjectID(testOIDCIssuer, "s1") {
		t.Fatalf("created account is %+v", u)
	}
	if u.PasswordHash != "" {
		t.Fatal("a provisioned account was given a password")
	}

	// A second sign-in reuses the account.
	if rr := oidcSignIn(t, a); rr.Code != http.StatusFound {
		t.Fatalf("second sign-in failed: %d", rr.Code)
	}
	if n := len(a.state.Users()); n != 2 {
		t.Fatalf("expected 2 users, got %d", n)
	}
}

func TestOIDCProvisionUsernameCollision(t *testing.T) {
	a, _ := oidcPolicyAdmin(t, zitadelDoc("s1", "alice", "llmesh-user"), zitadelPolicy)
	addTestUser(t, a, "alice", "member")
	if rr := oidcSignIn(t, a); rr.Code != http.StatusFound {
		t.Fatalf("sign-in failed: %d %s", rr.Code, rr.Body.String())
	}
	u, ok := a.state.LookupUser("alice-2")
	if !ok || u.ManagedBy != providerOIDC {
		t.Fatalf("expected a managed alice-2, got %+v", u)
	}
	if local, _ := a.state.LookupUser("alice"); local.OIDCSubject != "" {
		t.Fatal("the existing local alice was linked to someone else's identity")
	}
}

func TestOIDCRefusedWithoutRole(t *testing.T) {
	a, f := oidcPolicyAdmin(t, zitadelDoc("s1", "mallory"), zitadelPolicy)
	rr := oidcSignIn(t, a)
	if rr.Code == http.StatusFound || !strings.Contains(rr.Body.String(), "has not been granted access") {
		t.Fatalf("a sign-in with no role was not refused: %d %s", rr.Code, rr.Body.String())
	}
	if len(a.state.Users()) != 0 {
		t.Fatal("an account was created for an identity with no role")
	}

	// Being linked does not get round the policy.
	addTestUser(t, a, "mallory", "member")
	linkIdentity(t, a, "mallory", providerOIDC, oauthIdentity{ID: f.account.id})
	if rr := oidcSignIn(t, a); rr.Code == http.StatusFound {
		t.Fatal("a linked identity with no role signed in")
	}
}

func TestOIDCManagedRoleFollowsProvider(t *testing.T) {
	a, f := oidcPolicyAdmin(t, zitadelDoc("s1", "alice", "llmesh-user"), zitadelPolicy)
	addTestUser(t, a, "root", "admin")
	oidcSignIn(t, a)
	if u, _ := a.state.LookupUser("alice"); u.Role != "member" {
		t.Fatalf("expected member, got %s", u.Role)
	}

	f.account.doc = zitadelDoc("s1", "alice", "llmesh-user", "llmesh-admin")
	oidcSignIn(t, a)
	if u, _ := a.state.LookupUser("alice"); u.Role != "admin" {
		t.Fatalf("promotion at the provider did not apply: %s", u.Role)
	}

	f.account.doc = zitadelDoc("s1", "alice", "llmesh-user")
	oidcSignIn(t, a)
	if u, _ := a.state.LookupUser("alice"); u.Role != "member" {
		t.Fatalf("demotion at the provider did not apply: %s", u.Role)
	}
}

func TestOIDCNeverDemotesLastAdmin(t *testing.T) {
	a, f := oidcPolicyAdmin(t, zitadelDoc("s1", "alice", "llmesh-admin"), zitadelPolicy)
	oidcSignIn(t, a)
	f.account.doc = zitadelDoc("s1", "alice", "llmesh-user")
	if rr := oidcSignIn(t, a); rr.Code != http.StatusFound {
		t.Fatalf("sign-in failed: %d", rr.Code)
	}
	if u, _ := a.state.LookupUser("alice"); u.Role != "admin" {
		t.Fatal("the last active admin was demoted by the provider")
	}
}

func TestOIDCLeavesLocalAccountRoleAlone(t *testing.T) {
	a, f := oidcPolicyAdmin(t, zitadelDoc("s1", "root", "llmesh-user"), zitadelPolicy)
	addTestUser(t, a, "root", "admin")
	addTestUser(t, a, "other", "admin")
	linkIdentity(t, a, "root", providerOIDC, oauthIdentity{ID: f.account.id})
	if rr := oidcSignIn(t, a); rr.Code != http.StatusFound {
		t.Fatalf("sign-in failed: %d", rr.Code)
	}
	if u, _ := a.state.LookupUser("root"); u.Role != "admin" || u.ManagedBy != "" {
		t.Fatalf("a local account's role was changed by the provider: %+v", u)
	}
}

func TestManagedAccountSignsInOnlyThroughProvider(t *testing.T) {
	a, _ := oidcPolicyAdmin(t, zitadelDoc("s1", "alice", "llmesh-user"), zitadelPolicy)
	addTestUser(t, a, "root", "admin")
	oidcSignIn(t, a)

	// Give the managed account a password behind the portal's back, as a
	// stale database or a future bug might, and check it still cannot be used.
	hash, _ := HashPassword("pw-alice")
	a.state.UpdateUser("alice", func(u *User) { u.PasswordHash = hash })
	req := httptest.NewRequest("POST", "/portal/login",
		strings.NewReader(url.Values{"username": {"alice"}, "password": {"pw-alice"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	a.handleLogin(rr, req)
	if sessionCookieFrom(rr) != nil || !strings.Contains(rr.Body.String(), "managed by your organisation") {
		t.Fatalf("a managed account signed in with a password: %d %s", rr.Code, rr.Body.String())
	}
}

func TestManagedAccountPortalRules(t *testing.T) {
	a, _ := oidcPolicyAdmin(t, zitadelDoc("s1", "alice", "llmesh-user"), zitadelPolicy)
	addTestUser(t, a, "root", "admin")
	oidcSignIn(t, a)

	rr := postAs(t, a, "root", "/portal/settings/users/promote", url.Values{"username": {"alice"}}, a.handleUserPromote)
	if !strings.Contains(rr.Body.String(), "managed by single sign-on") {
		t.Fatalf("promoting a managed account was not refused:\n%s", rr.Body.String())
	}
	if u, _ := a.state.LookupUser("alice"); u.Role != "member" {
		t.Fatal("a managed account's role was changed in the portal")
	}

	// The user cannot unlink the identity that is their only way in.
	rr = postAs(t, a, "alice", "/portal/settings/oidc/unlink", nil, a.handleOAuthUnlink(providerOIDC))
	if u, _ := a.state.LookupUser("alice"); u.OIDCSubject == "" {
		t.Fatal("a managed account unlinked its provider identity")
	}

	// Setting a password hands the account back to this router.
	postAs(t, a, "root", "/portal/settings/users/reset-password", url.Values{"username": {"alice"}}, a.handleUserResetPassword)
	u, _ := a.state.LookupUser("alice")
	if u.ManagedBy != "" || u.PasswordHash == "" {
		t.Fatalf("take-over did not release the account: %+v", u)
	}
	rr = postAs(t, a, "root", "/portal/settings/users/promote", url.Values{"username": {"alice"}}, a.handleUserPromote)
	if u, _ := a.state.LookupUser("alice"); u.Role != "admin" {
		t.Fatalf("a released account's role could not be changed: %s", rr.Body.String())
	}
}

func TestOIDCSettingsAccessPolicy(t *testing.T) {
	a := newTestAdmin(t)
	addTestUser(t, a, "admin", "admin")
	issuer := startFakeIssuer(t, a)
	path := "/portal/settings/auth/" + providerOIDC
	base := url.Values{
		"enabled": {"on"}, "client_id": {"cid"}, "client_secret": {"shh"}, "issuer": {issuer.URL},
	}

	form := url.Values{}
	for k, v := range base {
		form[k] = v
	}
	form.Set("provision", "on")
	rr := postAs(t, a, "admin", path, form, a.handleOAuthSettingsUpdate(providerOIDC))
	if !strings.Contains(rr.Body.String(), "requires a roles claim") {
		t.Fatalf("provisioning without a policy was accepted:\n%s", rr.Body.String())
	}

	form.Set("roles_claim", "urn:zitadel:iam:org:project:roles")
	form.Set("member_role", "llmesh-user")
	form.Set("extra_scopes", "  urn:zitadel:iam:org:projects:roles  ")
	rr = postAs(t, a, "admin", path, form, a.handleOAuthSettingsUpdate(providerOIDC))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "sign-in is on") {
		t.Fatalf("a valid policy was refused:\n%s", rr.Body.String())
	}
	got := a.state.OIDC()
	if !got.Provision || got.MemberRole != "llmesh-user" || got.ExtraScopes != "urn:zitadel:iam:org:projects:roles" {
		t.Fatalf("stored policy is %+v", got)
	}
	p, _ := a.providerFor(providerOIDC)
	if p.scope != "openid email profile urn:zitadel:iam:org:projects:roles" {
		t.Fatalf("scope is %q", p.scope)
	}
}
