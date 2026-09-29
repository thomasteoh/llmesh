package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"llmesh/router/internal/authz"
)

// Access management from an OpenID Connect provider.
//
// With a roles claim configured, the provider decides who may sign in and, for
// accounts it created, what role they hold here:
//
//   - A sign-in whose roles include none of the mapped roles is refused,
//     whether or not the identity is linked.
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
	if c.RolesClaim != "" && len(c.roleMapping()) == 0 {
		return fmt.Errorf("map at least one provider role to an llmesh role for the roles claim to grant access")
	}
	// Without a role requirement, provisioning would hand an account to anyone
	// the provider can authenticate — and many providers let anyone register.
	if c.Provision && c.RolesClaim == "" {
		return fmt.Errorf("creating accounts on first sign-in requires a roles claim and a role to require")
	}
	return nil
}

// roleMapping is every provider role that grants an llmesh role.
func (c OIDCConfig) roleMapping() map[string]string { return c.RoleMap }

// rolesFor decides whether an identity may sign in and which llmesh roles
// the provider grants it. With the policy off every identity is allowed and
// the provider grants nothing (nil), meaning "the provider has no say".
func (c OIDCConfig) rolesFor(claims map[string]json.RawMessage) (roles []string, allowed bool) {
	if !c.accessPolicyOn() {
		return nil, true
	}
	held := claimRoles(claims, c.RolesClaim)
	set := map[string]bool{}
	for provider, role := range c.roleMapping() {
		if held[provider] {
			set[role] = true
		}
	}
	for r := range set {
		roles = append(roles, r)
	}
	sort.Strings(roles)
	return roles, len(roles) > 0
}

// teamsFor returns the managed teams the provider puts the identity in, and
// every team the mapping manages (so absence from one can be applied too).
func (c OIDCConfig) teamsFor(claims map[string]json.RawMessage) (in, managed []string) {
	if c.GroupsClaim == "" || len(c.TeamMap) == 0 {
		return nil, nil
	}
	groups := claimRoles(claims, c.GroupsClaim)
	seen := map[string]bool{}
	for group, team := range c.TeamMap {
		if !seen[team] {
			managed = append(managed, team)
			seen[team] = true
		}
		if groups[group] {
			in = append(in, team)
		}
	}
	sort.Strings(in)
	sort.Strings(managed)
	return in, managed
}

// attrsFor reads the mapped claims as attributes. A claim that is a string,
// number, or boolean maps to its text; anything else is skipped.
func (c OIDCConfig) attrsFor(claims map[string]json.RawMessage) map[string]string {
	out := map[string]string{}
	for claim, attr := range c.AttrMap {
		raw, ok := claims[claim]
		if !ok {
			raw, ok = nestedClaim(claims, claim)
		}
		if !ok {
			continue
		}
		var v any
		if json.Unmarshal(raw, &v) != nil {
			continue
		}
		switch t := v.(type) {
		case string:
			out[attr] = t
		case float64, bool:
			out[attr] = fmt.Sprint(t)
		}
	}
	return out
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
// the access policy, account creation, and role/team/attribute sync.
func (a *Admin) completeOIDCLogin(w http.ResponseWriter, r *http.Request, p oauthProvider, ident oauthIdentity) {
	cfg := a.state.OIDC()
	roles, allowed := cfg.rolesFor(ident.Claims)
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
		u, err = a.state.ProvisionOIDCUser(provisionedUsername(ident), legacyRoleOf(roles), ident)
		if err != nil {
			a.log.Error("admin: oidc provisioning failed", "label", ident.Label, "error", err)
			a.renderLogin(w, r, "", "Could not create your llmesh account. Ask an administrator.")
			return
		}
		a.state.RecordAudit(u.Username, "user.provision.oidc", ident.Label+" roles="+strings.Join(roles, ","), a.clientIP(r))
	}
	if u.Disabled {
		// An account the provider's revalidation disabled comes back when the
		// provider vouches for it again. One an admin disabled stays disabled.
		if u.ManagedBy != providerOIDC || a.state.DisabledBy(u.Username) != providerOIDC {
			a.renderLogin(w, r, "", "Account disabled.")
			return
		}
		if err := a.state.SetDisabledBy(u.Username, false, ""); err != nil {
			a.renderLogin(w, r, "", "Account disabled.")
			return
		}
		a.state.RecordAudit(u.Username, "user.enable.oidc", ident.Label, a.clientIP(r))
	}

	a.syncManagedAccess(u, cfg, roles, ident, a.clientIP(r))
	if ident.Label != "" && ident.Label != u.OIDCLabel {
		_ = a.state.UpdateUser(u.Username, func(user *User) { user.OIDCLabel = ident.Label })
	}
	if u.ManagedBy == providerOIDC && cfg.RevalidateMinutes > 0 && ident.RefreshToken != "" {
		if err := a.state.SetOIDCRefreshToken(u.Username, ident.RefreshToken); err != nil {
			a.log.Error("admin: storing refresh token", "user", u.Username, "error", err)
		}
	}
	a.state.RecordAudit(u.Username, "auth.login."+p.key, ident.Label, a.clientIP(r))
	a.startSession(w, r, u.Username)
	http.Redirect(w, r, "/portal/", http.StatusFound)
}

// legacyRoleOf is the users.role value for a set of llmesh roles.
func legacyRoleOf(roles []string) string {
	for _, r := range roles {
		if r == authz.RoleOwner || r == authz.RoleAdmin {
			return "admin"
		}
	}
	return "member"
}

// syncManagedAccess applies what the provider says to an account it manages:
// its router-wide roles become exactly the mapped roles, its membership of
// mapped teams follows its groups, and its mapped attributes are refreshed.
// Accounts made locally are left alone. The last active owner or admin never
// loses that role this way: a role removed at the provider by mistake would
// otherwise leave the router with no one able to fix it.
func (a *Admin) syncManagedAccess(u User, cfg OIDCConfig, roles []string, ident oauthIdentity, ip string) {
	if u.ManagedBy != providerOIDC {
		return
	}
	s := a.state
	if cfg.accessPolicyOn() && len(roles) > 0 {
		want := map[string]bool{}
		for _, r := range roles {
			if s.roleExists(r) {
				want[r] = true
			}
		}
		have := map[string]bool{}
		for _, r := range a.globalRoles(u.Username) {
			have[r] = true
		}
		for r := range want {
			if !have[r] {
				if err := s.Bind(RoleBinding{Principal: userPrincipal(u.Username), Role: r}); err == nil {
					s.RecordAudit(u.Username, "role.bind.oidc", r, ip)
				}
			}
		}
		for r := range have {
			// Owners are made by owners; the provider only ever adds roles
			// it maps, never takes the owner role away.
			if want[r] || r == authz.RoleOwner {
				continue
			}
			if err := s.Unbind(RoleBinding{Principal: userPrincipal(u.Username), Role: r}); err != nil {
				a.log.Warn("admin: kept a role despite the provider", "user", u.Username, "role", r, "reason", err)
				continue
			}
			s.RecordAudit(u.Username, "role.unbind.oidc", r, ip)
		}
	}
	if in, managed := cfg.teamsFor(ident.Claims); len(managed) > 0 {
		member := map[string]bool{}
		for _, t := range in {
			member[t] = true
		}
		current, _ := s.TeamsOf(u.Username)
		isIn := map[string]bool{}
		for _, t := range current {
			isIn[t] = true
		}
		for _, t := range managed {
			switch {
			case member[t] && !isIn[t]:
				if err := s.AddTeamMember(t, u.Username, false); err == nil {
					s.RecordAudit(u.Username, "team.member.add.oidc", t, ip)
				}
			case !member[t] && isIn[t]:
				if err := s.RemoveTeamMember(t, u.Username); err == nil {
					s.RecordAudit(u.Username, "team.member.remove.oidc", t, ip)
				}
			}
		}
	}
	if len(cfg.AttrMap) > 0 {
		attrs := s.UserAttrs(u.Username)
		mapped := cfg.attrsFor(ident.Claims)
		for _, attr := range cfg.AttrMap {
			if v, ok := mapped[attr]; ok {
				attrs[attr] = v
			} else {
				delete(attrs, attr)
			}
		}
		_ = s.SetUserAttrs(u.Username, attrs)
	}
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
