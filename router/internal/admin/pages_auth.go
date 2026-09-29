package admin

import (
	"fmt"
	"llmesh/router/internal/authz"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Admin-facing configuration of the two alternative sign-in methods.
//
// Both forms follow the same rule for their secret field: blank means "leave
// the stored one alone". The page never renders a secret back, so a blank field
// is what an admin editing any other setting will submit, and reading it as
// "erase it" would silently break sign-in. Erasing is its own explicit action.

// handleOAuthSettingsUpdate saves one provider's credentials. The same handler
// serves every provider, so none can acquire a subtly different rule about
// what a blank secret means or when a configuration counts as enabled.
func (a *Admin) handleOAuthSettingsUpdate(providerKey string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := ctxGetUser(r)
		p, ok := a.providerFor(providerKey)
		if !ok {
			http.NotFound(w, r)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if r.FormValue("clear_secret") != "" {
			if err := a.state.ClearOAuthClientSecret(providerKey); err != nil {
				a.renderSettings(w, r, u, "", err.Error())
				return
			}
			// A configuration with no secret cannot complete a sign-in, so
			// leaving it switched on would offer users a button that always fails.
			if err := a.state.SetOAuth(providerKey, p.name, OAuthConfig{
				Enabled:  false,
				ClientID: a.state.OAuth(providerKey).ClientID,
			}); err != nil {
				a.renderSettings(w, r, u, "", err.Error())
				return
			}
			a.state.RecordAudit(u.Username, "settings.auth."+providerKey+".clear_secret", "", a.clientIP(r))
			a.renderSettings(w, r, u,
				p.name+" client secret cleared and "+p.name+" sign-in switched off.", "")
			return
		}

		cfg := OAuthConfig{
			Enabled:      r.FormValue("enabled") != "",
			ClientID:     r.FormValue("client_id"),
			ClientSecret: r.FormValue("client_secret"),
		}
		auditTarget := "enabled=" + strconv.FormatBool(cfg.Enabled)

		// OIDC's endpoints are discovered before anything is stored, so a
		// mistyped issuer is reported here and leaves the working
		// configuration as it was.
		var oidc OIDCConfig
		if providerKey == providerOIDC {
			var err error
			oidc, err = a.oidcFromForm(r, cfg.Enabled)
			if err != nil {
				a.renderSettings(w, r, u, "", err.Error())
				return
			}
			p.name = oidc.DisplayName()
			auditTarget += fmt.Sprintf(" issuer=%s roles_claim=%q roles=%d provision=%t",
				oidc.Issuer, oidc.RolesClaim, len(oidc.RoleMap), oidc.Provision)
		}

		if err := a.state.SetOAuth(providerKey, p.name, cfg); err != nil {
			a.renderSettings(w, r, u, "", err.Error())
			return
		}
		relink := ""
		if providerKey == providerOIDC {
			previous := a.state.OIDC().Issuer
			if err := a.state.SetOIDC(oidc); err != nil {
				a.renderSettings(w, r, u, "", err.Error())
				return
			}
			// Links are scoped to their issuer, so moving to another one
			// detaches every existing link at once. Say so, rather than let
			// users find out at the login page.
			if previous != "" && previous != oidc.Issuer {
				relink = " Accounts linked to " + previous + " must be linked again."
			}
		}
		// The secret is deliberately absent from the audit target.
		a.state.RecordAudit(u.Username, "settings.auth."+providerKey, auditTarget, a.clientIP(r))
		a.log.Info("admin: oauth sign-in settings updated",
			"actor", u.Username, "provider", providerKey, "enabled", cfg.Enabled)
		if cfg.Enabled {
			a.renderSettings(w, r, u,
				p.name+" sign-in is on. Users can link their account under Sign-in methods."+relink, "")
			return
		}
		a.renderSettings(w, r, u, p.name+" sign-in settings saved."+relink, "")
	}
}

// oidcFromForm reads the OIDC-specific settings and discovers the issuer's
// endpoints. An empty issuer is allowed only while sign-in stays off, so an
// admin can save a name or method ahead of pointing it at a provider.
func (a *Admin) oidcFromForm(r *http.Request, enabling bool) (OIDCConfig, error) {
	method := r.FormValue("auth_method")
	if method == "" {
		method = oidcAuthBasic
	}
	if !oidcAuthMethods[method] {
		return OIDCConfig{}, fmt.Errorf("unknown token authentication method %q", method)
	}
	oc := OIDCConfig{
		Name:        strings.TrimSpace(r.FormValue("name")),
		AuthMethod:  method,
		ExtraScopes: r.FormValue("extra_scopes"),
		RolesClaim:  strings.TrimSpace(r.FormValue("roles_claim")),
		Provision:   r.FormValue("provision") != "",
		GroupsClaim: strings.TrimSpace(r.FormValue("groups_claim")),
	}
	var err error
	if oc.RoleMap, err = parseMapLines(r.FormValue("role_map")); err != nil {
		return OIDCConfig{}, fmt.Errorf("role mapping: %w", err)
	}
	for provider, role := range oc.RoleMap {
		if !a.state.roleExists(role) {
			return OIDCConfig{}, fmt.Errorf("role mapping: %q maps to unknown role %q", provider, role)
		}
		// Mapping a role hands it to whoever the provider names, so it takes
		// what granting the role directly would.
		if !a.canGrantRole(r, role) {
			return OIDCConfig{}, fmt.Errorf("role mapping: you cannot map to %q, which carries permissions you do not hold", role)
		}
		if role == authz.RoleTeamMaintainer || role == authz.RoleTeamMember {
			return OIDCConfig{}, fmt.Errorf("role mapping: team roles come from the team mapping, not the role mapping")
		}
	}
	if oc.TeamMap, err = parseMapLines(r.FormValue("team_map")); err != nil {
		return OIDCConfig{}, fmt.Errorf("team mapping: %w", err)
	}
	for group, team := range oc.TeamMap {
		if _, ok := a.state.LookupTeam(team); !ok {
			return OIDCConfig{}, fmt.Errorf("team mapping: %q maps to unknown team %q", group, team)
		}
	}
	if oc.AttrMap, err = parseMapLines(r.FormValue("attr_map")); err != nil {
		return OIDCConfig{}, fmt.Errorf("attribute mapping: %w", err)
	}
	for _, attr := range oc.AttrMap {
		if !attrKeyPattern.MatchString(attr) {
			return OIDCConfig{}, fmt.Errorf("attribute mapping: %q is not a valid attribute name", attr)
		}
	}
	if v := strings.TrimSpace(r.FormValue("revalidate_minutes")); v != "" {
		if oc.RevalidateMinutes, err = strconv.Atoi(v); err != nil {
			return OIDCConfig{}, fmt.Errorf("revalidation interval must be a number of minutes")
		}
	}
	// Checked before discovery, so a policy mistake is reported without a
	// round trip to the provider.
	if err := oc.validateAccess(); err != nil {
		return OIDCConfig{}, err
	}
	issuer := normalizeIssuer(r.FormValue("issuer"))
	if issuer == "" {
		if enabling {
			return OIDCConfig{}, fmt.Errorf("an issuer URL is required to enable %s sign-in", oc.DisplayName())
		}
		return oc, nil
	}
	d, err := a.discoverOIDC(r.Context(), issuer)
	if err != nil {
		return OIDCConfig{}, fmt.Errorf("could not use issuer %s: %v", issuer, err)
	}
	oc.Issuer, oc.AuthorizeURL, oc.TokenURL, oc.UserInfoURL = d.Issuer, d.AuthorizeURL, d.TokenURL, d.UserInfoURL
	return oc, nil
}

func (a *Admin) handleSMTPUpdate(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if r.FormValue("clear_password") != "" {
		if err := a.state.ClearSMTPPassword(); err != nil {
			a.renderSettings(w, r, u, "", err.Error())
			return
		}
		a.state.RecordAudit(u.Username, "settings.auth.smtp.clear_password", "", a.clientIP(r))
		a.renderSettings(w, r, u, "SMTP password cleared.", "")
		return
	}

	// An unparseable port becomes 0, which SetSMTP reads as "use the default
	// for this transport" — the same as leaving the field empty.
	port, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("port")))
	cfg := SMTPConfig{
		Enabled:  r.FormValue("enabled") != "",
		Host:     r.FormValue("host"),
		Port:     port,
		Username: r.FormValue("username"),
		Password: r.FormValue("password"),
		From:     r.FormValue("from"),
		Security: r.FormValue("security"),
	}
	if err := a.state.SetSMTP(cfg); err != nil {
		a.renderSettings(w, r, u, "", err.Error())
		return
	}
	a.state.RecordAudit(u.Username, "settings.auth.smtp",
		"enabled="+strconv.FormatBool(cfg.Enabled), a.clientIP(r))
	a.log.Info("admin: smtp settings updated", "actor", u.Username, "enabled", cfg.Enabled, "host", cfg.Host)
	if cfg.Enabled {
		a.renderSettings(w, r, u, "Email sign-in is on. Send a test message to confirm the relay works.", "")
		return
	}
	a.renderSettings(w, r, u, "SMTP settings saved.", "")
}

// handleSMTPTest sends one message to an address the admin names, so a relay
// that rejects mail says so here rather than the first time a user is waiting
// on a sign-in link that never comes. It reports the relay's own error, which
// is the only thing that makes an SMTP misconfiguration diagnosable.
func (a *Admin) handleSMTPTest(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	cfg := a.state.SMTP()
	if cfg.Host == "" {
		a.renderSettings(w, r, u, "", "Save an SMTP host before sending a test message.")
		return
	}
	to := NormalizeEmail(r.FormValue("to"))
	if to == "" {
		// Default to the admin's own address when they have one; the common
		// case is checking that mail arrives at all.
		if fresh, ok := a.state.LookupUser(u.Username); ok {
			to = fresh.Email
		}
	}
	if !ValidEmail(to) {
		a.renderSettings(w, r, u, "", "Enter an address to send the test message to.")
		return
	}
	body := "This is a test message from the llmesh portal"
	if a.name != "" {
		body += " (" + a.name + ")"
	}
	body += ".\n\nIf you are reading it, sign-in links will reach their recipients.\n"
	if err := sendMail(cfg, to, "llmesh portal test message", body, time.Now()); err != nil {
		a.log.Error("admin: smtp test", "actor", u.Username, "error", err)
		a.renderSettings(w, r, u, "", "Test message failed: "+err.Error())
		return
	}
	a.state.RecordAudit(u.Username, "settings.auth.smtp.test", to, a.clientIP(r))
	a.renderSettings(w, r, u, "Test message sent to "+to+".", "")
}

// parseMapLines reads "left=right" lines (blank lines ignored) into a map.
func parseMapLines(text string) (map[string]string, error) {
	out := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !ok || k == "" || v == "" {
			return nil, fmt.Errorf("%q is not name=value", line)
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}
