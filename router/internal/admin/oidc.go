package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// A generic OpenID Connect provider, for a self-hosted or organisational
// identity provider such as Zitadel, Keycloak, Authentik, or Entra ID.
//
// GitHub and Google have fixed endpoints written into their descriptors. An
// OIDC provider's live at an issuer the admin names, so they are read from the
// issuer's discovery document when the admin saves it and stored beside the
// credentials. Nothing is discovered at sign-in time: a provider that is down
// or misconfigured shows up as an error on the settings page, where the admin
// can act on it, rather than as a failed sign-in for a user who cannot.

const (
	oidcIssuerKey       = "auth.oidc.issuer"
	oidcNameKey         = "auth.oidc.name"
	oidcAuthMethodKey   = "auth.oidc.auth_method"
	oidcAuthorizeURLKey = "auth.oidc.authorize_url"
	oidcTokenURLKey     = "auth.oidc.token_url"
	oidcUserInfoURLKey  = "auth.oidc.userinfo_url"
	oidcScopesKey       = "auth.oidc.extra_scopes"
	oidcRolesClaimKey   = "auth.oidc.roles_claim"
	oidcMemberRoleKey   = "auth.oidc.member_role"
	oidcAdminRoleKey    = "auth.oidc.admin_role"
	oidcProvisionKey    = "auth.oidc.provision"
)

// defaultOIDCName is what the login button says until an admin names the
// provider something their users will recognise.
const defaultOIDCName = "Single sign-on"

// How the router authenticates to the token endpoint. The two are mutually
// exclusive: RFC 6749 §2.3 forbids a client from using more than one, and some
// providers enforce it. Basic is the default because it is what most providers,
// Zitadel included, configure a web application for unless told otherwise.
const (
	oidcAuthBasic = "client_secret_basic"
	oidcAuthPost  = "client_secret_post"
)

var oidcAuthMethods = map[string]bool{oidcAuthBasic: true, oidcAuthPost: true}

// OIDCConfig is the provider-specific half of the OIDC sign-in configuration.
// The client ID and secret are stored with every other provider's, through
// OAuthConfig.
type OIDCConfig struct {
	// Issuer is the provider's issuer identifier, exactly as its discovery
	// document states it.
	Issuer string
	// Name is what users see on the login button and in their settings.
	Name string
	// AuthMethod is oidcAuthBasic or oidcAuthPost.
	AuthMethod string

	// The endpoints discovered from Issuer. Empty until discovery succeeds.
	AuthorizeURL string
	TokenURL     string
	UserInfoURL  string

	// ExtraScopes are requested alongside the fixed ones, for a provider that
	// only releases roles when asked (space-separated).
	ExtraScopes string
	// The access policy; see oidc_access.go. RolesClaim empty turns it off.
	RolesClaim string
	MemberRole string
	AdminRole  string
	// Provision creates an account for a permitted identity seen for the
	// first time, instead of refusing it as unlinked.
	Provision bool
}

// Discovered reports whether the endpoints a sign-in needs are known.
func (c OIDCConfig) Discovered() bool {
	return c.AuthorizeURL != "" && c.TokenURL != "" && c.UserInfoURL != ""
}

// DisplayName is Name, or the default when the admin has not set one.
func (c OIDCConfig) DisplayName() string {
	if c.Name != "" {
		return c.Name
	}
	return defaultOIDCName
}

// OIDC returns the stored OIDC provider configuration.
func (s *State) OIDC() OIDCConfig {
	v := s.settings(oidcIssuerKey, oidcNameKey, oidcAuthMethodKey,
		oidcAuthorizeURLKey, oidcTokenURLKey, oidcUserInfoURLKey,
		oidcScopesKey, oidcRolesClaimKey, oidcMemberRoleKey, oidcAdminRoleKey, oidcProvisionKey)
	method := v[oidcAuthMethodKey]
	if !oidcAuthMethods[method] {
		method = oidcAuthBasic
	}
	return OIDCConfig{
		Issuer:       v[oidcIssuerKey],
		Name:         v[oidcNameKey],
		AuthMethod:   method,
		AuthorizeURL: v[oidcAuthorizeURLKey],
		TokenURL:     v[oidcTokenURLKey],
		UserInfoURL:  v[oidcUserInfoURLKey],
		ExtraScopes:  v[oidcScopesKey],
		RolesClaim:   v[oidcRolesClaimKey],
		MemberRole:   v[oidcMemberRoleKey],
		AdminRole:    v[oidcAdminRoleKey],
		Provision:    v[oidcProvisionKey] == "1",
	}
}

// SetOIDC stores the OIDC provider configuration, endpoints included. Callers
// fill the endpoints from discoverOIDC; storing an issuer without them leaves
// the provider unusable until it is saved again.
func (s *State) SetOIDC(c OIDCConfig) error {
	if c.AuthMethod == "" {
		c.AuthMethod = oidcAuthBasic
	}
	if !oidcAuthMethods[c.AuthMethod] {
		return fmt.Errorf("unknown token authentication method %q", c.AuthMethod)
	}
	c.ExtraScopes = strings.Join(strings.Fields(c.ExtraScopes), " ")
	c.RolesClaim = strings.TrimSpace(c.RolesClaim)
	c.MemberRole = strings.TrimSpace(c.MemberRole)
	c.AdminRole = strings.TrimSpace(c.AdminRole)
	if err := c.validateAccess(); err != nil {
		return err
	}
	return s.putSettings(map[string]string{
		oidcScopesKey:       c.ExtraScopes,
		oidcRolesClaimKey:   c.RolesClaim,
		oidcMemberRoleKey:   c.MemberRole,
		oidcAdminRoleKey:    c.AdminRole,
		oidcProvisionKey:    boolSetting(c.Provision),
		oidcIssuerKey:       c.Issuer,
		oidcNameKey:         strings.TrimSpace(c.Name),
		oidcAuthMethodKey:   c.AuthMethod,
		oidcAuthorizeURLKey: c.AuthorizeURL,
		oidcTokenURLKey:     c.TokenURL,
		oidcUserInfoURLKey:  c.UserInfoURL,
	})
}

// normalizeIssuer trims an admin-entered issuer to the form discovery compares
// against. A trailing slash is dropped because the spec builds the discovery
// URL by appending to the issuer, and "https://id.example/" would otherwise
// produce a double slash some providers refuse.
func normalizeIssuer(raw string) string {
	return strings.TrimRight(strings.TrimSpace(raw), "/")
}

// requireSecureURL rejects an endpoint that is not HTTPS. Plain HTTP is
// allowed only on a loopback host, which is how a provider under development
// is reached and cannot be intercepted on a network. Everything else carries
// an authorization code, a client secret, or an access token, and must be
// encrypted in transit.
func requireSecureURL(label, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%s %q is not an absolute URL", label, raw)
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" && isLoopbackHost(u.Hostname()) {
		return nil
	}
	return fmt.Errorf("%s %q must use https", label, raw)
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// discoverOIDC reads an issuer's discovery document and returns the endpoints
// a sign-in needs.
//
// The document's own issuer must match the one requested, as OpenID Connect
// Discovery §4.3 requires. Without that check, an issuer URL that redirects or
// is served by a proxy could hand this router another provider's endpoints,
// and every subject it then reported would be matched as if it came from the
// issuer the admin configured.
func (a *Admin) discoverOIDC(ctx context.Context, issuer string) (OIDCConfig, error) {
	issuer = normalizeIssuer(issuer)
	if err := requireSecureURL("issuer", issuer); err != nil {
		return OIDCConfig{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		issuer+"/.well-known/openid-configuration", nil)
	if err != nil {
		return OIDCConfig{}, err
	}
	req.Header.Set("Accept", "application/json")
	body, status, err := a.readOAuthResponse(req)
	if err != nil {
		return OIDCConfig{}, fmt.Errorf("could not reach the issuer's discovery document: %w", err)
	}
	if status != http.StatusOK {
		return OIDCConfig{}, fmt.Errorf("the issuer's discovery document returned HTTP %d", status)
	}
	var doc struct {
		Issuer                string `json:"issuer"`
		AuthorizationEndpoint string `json:"authorization_endpoint"`
		TokenEndpoint         string `json:"token_endpoint"`
		UserInfoEndpoint      string `json:"userinfo_endpoint"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return OIDCConfig{}, fmt.Errorf("the issuer's discovery document is not valid JSON: %w", err)
	}
	if normalizeIssuer(doc.Issuer) != issuer {
		return OIDCConfig{}, fmt.Errorf("the discovery document names its issuer as %q, not %q", doc.Issuer, issuer)
	}
	endpoints := []struct{ label, url string }{
		{"authorization endpoint", doc.AuthorizationEndpoint},
		{"token endpoint", doc.TokenEndpoint},
		{"userinfo endpoint", doc.UserInfoEndpoint},
	}
	for _, e := range endpoints {
		if e.url == "" {
			return OIDCConfig{}, fmt.Errorf("the discovery document has no %s", e.label)
		}
		if err := requireSecureURL(e.label, e.url); err != nil {
			return OIDCConfig{}, err
		}
	}
	return OIDCConfig{
		Issuer:       issuer,
		AuthorizeURL: doc.AuthorizationEndpoint,
		TokenURL:     doc.TokenEndpoint,
		UserInfoURL:  doc.UserInfoEndpoint,
	}, nil
}

// oidcSubjectID is the identity an OIDC account is stored and matched under.
//
// A subject is unique only within its issuer (OpenID Connect Core §2), so the
// issuer is part of the id. If an admin points the router at a different
// provider, links made against the old one stop matching instead of letting
// whoever holds the same subject string at the new one sign in to them.
func oidcSubjectID(issuer, sub string) string {
	return issuer + "#" + sub
}

// oidcIdentity reads an OpenID Connect userinfo document.
//
// As with Google, the subject is the identity and the rest is only displayed:
// the label is a verified address if there is one, then the provider's
// username, then the subject itself. The issuer is attached by the caller,
// which is the only party that knows it.
func oidcIdentity(body []byte) (oauthIdentity, error) {
	var v struct {
		Sub               string          `json:"sub"`
		Email             string          `json:"email"`
		EmailVerified     json.RawMessage `json:"email_verified"`
		PreferredUsername string          `json:"preferred_username"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return oauthIdentity{}, fmt.Errorf("decode userinfo: %w", err)
	}
	// The whole document is kept for the access policy, which reads a claim
	// the admin names and so cannot be a field here.
	var claims map[string]json.RawMessage
	if err := json.Unmarshal(body, &claims); err != nil {
		return oauthIdentity{}, fmt.Errorf("decode userinfo: %w", err)
	}
	label := ""
	if claimIsTrue(v.EmailVerified) {
		label = NormalizeEmail(v.Email)
	}
	if label == "" {
		label = strings.TrimSpace(v.PreferredUsername)
	}
	if label == "" {
		label = v.Sub
	}
	return oauthIdentity{
		ID:       v.Sub,
		Label:    label,
		Username: strings.TrimSpace(v.PreferredUsername),
		Claims:   claims,
	}, nil
}

// claimIsTrue reads a boolean claim that some providers (AWS Cognito among
// them) send as the string "true" rather than a JSON boolean.
func claimIsTrue(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s == "true" || s == `"true"`
}
