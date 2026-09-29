package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Access management from an OpenID Connect provider.
//
// With a roles claim configured, the provider decides who may sign in and, for
// accounts it created, what role they hold here:
//
//   - A sign-in whose roles include neither the admin role nor the member role
//     is refused, whether or not the identity is linked.
//   - With provisioning on, a permitted identity seen for the first time gets
//     an account, marked as managed by the provider.
//   - A managed account takes its role from the provider on every sign-in and
//     can sign in no other way.
//
// Accounts made here and linked afterwards stay managed here: their role is
// never changed by the provider, which keeps a break-glass admin's rights out
// of the provider's hands. They are still refused at the OIDC button if the
// provider does not grant them a role, since that is the provider saying no.
//
// Everything else — models, priority, concurrency, isolation — stays with the
// accounts and keys here. Roles at a provider are too coarse to carry it.

// accessPolicyOn reports whether the provider's roles gate sign-in.
func (c OIDCConfig) accessPolicyOn() bool { return c.RolesClaim != "" }

// validateAccess rejects a policy that would let in more than the admin meant.
func (c OIDCConfig) validateAccess() error {
	if c.RolesClaim != "" && c.MemberRole == "" && c.AdminRole == "" {
		return fmt.Errorf("name a member role, an admin role, or both, for the roles claim to grant access")
	}
	// Without a role requirement, provisioning would hand an account to anyone
	// the provider can authenticate — and many providers let anyone register.
	if c.Provision && c.RolesClaim == "" {
		return fmt.Errorf("creating accounts on first sign-in requires a roles claim and a role to require")
	}
	return nil
}

// roleFor decides whether an identity may sign in and which role the provider
// grants it. With the policy off every identity is allowed and the role is
// empty, meaning "the provider has no say".
func (c OIDCConfig) roleFor(claims map[string]json.RawMessage) (role string, allowed bool) {
	if !c.accessPolicyOn() {
		return "", true
	}
	roles := claimRoles(claims, c.RolesClaim)
	if c.AdminRole != "" && roles[c.AdminRole] {
		return "admin", true
	}
	if c.MemberRole != "" && roles[c.MemberRole] {
		return "member", true
	}
	return "", false
}

// claimRoles reads the role names out of a claim, in whichever of the shapes
// providers use:
//
//   - a list of strings (Entra ID "roles", Keycloak "realm_access.roles")
//   - a single string
//   - an object whose keys are the roles (Zitadel, which maps each role to
//     the organisations that granted it)
//
// A name that is not a top-level claim is tried as a dotted path into nested
// objects, which is how Keycloak nests its realm roles. A top-level match wins,
// since Zitadel's claim names contain no dots but others' may.
func claimRoles(claims map[string]json.RawMessage, name string) map[string]bool {
	raw, ok := claims[name]
	if !ok {
		raw, ok = nestedClaim(claims, name)
	}
	roles := map[string]bool{}
	if !ok {
		return roles
	}
	var list []string
	if json.Unmarshal(raw, &list) == nil {
		for _, r := range list {
			roles[r] = true
		}
		return roles
	}
	var one string
	if json.Unmarshal(raw, &one) == nil {
		roles[one] = true
		return roles
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) == nil {
		for r := range obj {
			roles[r] = true
		}
	}
	return roles
}

func nestedClaim(claims map[string]json.RawMessage, path string) (json.RawMessage, bool) {
	parts := strings.Split(path, ".")
	cur := claims
	for i, part := range parts {
		raw, ok := cur[part]
		if !ok {
			return nil, false
		}
		if i == len(parts)-1 {
			return raw, true
		}
		var next map[string]json.RawMessage
		if json.Unmarshal(raw, &next) != nil {
			return nil, false
		}
		cur = next
	}
	return nil, false
}

// managedElsewhere returns why an account may not sign in by method, or "" if
// it may. A provider-managed account signs in only through that provider:
// a password or email link would bypass the provider's decision to refuse it.
func managedElsewhere(u User, method string) string {
	if u.ManagedBy == "" || u.ManagedBy == method {
		return ""
	}
	return "This account is managed by your organisation's single sign-on. Sign in with it instead."
}

// completeOIDCLogin is completeOAuthLogin for the OIDC provider, which adds
// the access policy and account creation.
func (a *Admin) completeOIDCLogin(w http.ResponseWriter, r *http.Request, p oauthProvider, ident oauthIdentity) {
	cfg := a.state.OIDC()
	role, allowed := cfg.roleFor(ident.Claims)
	if !allowed {
		a.log.Info("admin: oidc sign-in refused by role policy", "label", ident.Label)
		a.state.RecordAudit("", "auth.login.oidc.refused", ident.Label, a.clientIP(r))
		a.renderLogin(w, r, "", "Your "+p.name+" account has not been granted access to llmesh.")
		return
	}

	u, ok := a.state.LookupUserByOAuth(p.key, ident.ID)
	if !ok {
		if !cfg.Provision {
			a.log.Info("admin: sign-in refused for unlinked account", "provider", p.key, "label", ident.Label)
			a.renderLogin(w, r, "", "That "+p.name+" account is not linked to an llmesh account. Sign in with your password, then link it under Settings.")
			return
		}
		var err error
		u, err = a.state.ProvisionOIDCUser(provisionedUsername(ident), role, ident)
		if err != nil {
			a.log.Error("admin: oidc provisioning failed", "label", ident.Label, "error", err)
			a.renderLogin(w, r, "", "Could not create your llmesh account. Ask an administrator.")
			return
		}
		a.state.RecordAudit(u.Username, "user.provision.oidc", ident.Label+" role="+u.Role, a.clientIP(r))
	}
	if u.Disabled {
		a.renderLogin(w, r, "", "Account disabled.")
		return
	}

	a.syncManagedRole(r, u, role)
	if ident.Label != "" && ident.Label != u.OIDCLabel {
		_ = a.state.UpdateUser(u.Username, func(user *User) { user.OIDCLabel = ident.Label })
	}
	a.state.RecordAudit(u.Username, "auth.login."+p.key, ident.Label, a.clientIP(r))
	a.startSession(w, r, u.Username)
	http.Redirect(w, r, "/portal/", http.StatusFound)
}

// syncManagedRole applies the provider's role to an account it manages. The
// last active admin is never demoted this way: a role removed at the provider
// by mistake would otherwise leave the router with no one able to fix it.
func (a *Admin) syncManagedRole(r *http.Request, u User, role string) {
	if u.ManagedBy != providerOIDC || role == "" || role == u.Role {
		return
	}
	if a.state.isPrivileged(u.Username) && a.state.otherActivePrivileged(u.Username) == 0 {
		a.log.Warn("admin: kept the last admin's role despite the provider", "user", u.Username, "provider_role", role)
		return
	}
	if err := a.state.UpdateUser(u.Username, func(user *User) { user.Role = role }); err != nil {
		a.log.Error("admin: role sync failed", "user", u.Username, "error", err)
		return
	}
	a.state.RecordAudit(u.Username, "user.role_sync.oidc", u.Role+"->"+role, a.clientIP(r))
}

// provisionedUsername derives a username for a new account from what the
// provider reports: its username, then the local part of a verified address,
// then a fixed fallback. The result is reduced to characters that are safe in
// URLs, logs, and the portal, and ProvisionOIDCUser adds a suffix on collision.
func provisionedUsername(ident oauthIdentity) string {
	base := ident.Username
	if base == "" && strings.Contains(ident.Label, "@") {
		base = ident.Label
	}
	// "alice@acme.zitadel.cloud" and "alice@example.com" both become "alice".
	if i := strings.IndexByte(base, '@'); i > 0 {
		base = base[:i]
	}
	var b strings.Builder
	for _, c := range strings.ToLower(base) {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
			b.WriteRune(c)
		default:
			b.WriteByte('-')
		}
	}
	name := strings.Trim(b.String(), "-.")
	if len(name) > 32 {
		name = strings.Trim(name[:32], "-.")
	}
	if name == "" {
		name = "user"
	}
	return name
}
