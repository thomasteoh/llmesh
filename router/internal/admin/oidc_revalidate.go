package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Revalidation with the identity provider (design §9): accounts the provider
// manages are re-checked on a schedule using the refresh token issued when
// they signed in. An account the provider refuses — the refresh is rejected,
// the subject changed, or the user no longer holds a role that grants access
// — is disabled, which through PrincipalActive ends its sessions and stops
// its API keys and client tokens at once. Roles, teams, and attributes are
// re-synced for accounts that pass.
//
// A provider that cannot be reached is not a refusal: nothing is disabled on
// a network error or a server error, only on an answer that says no.

// SetOIDCRefreshToken stores a managed account's refresh token. It is held in
// plaintext like the other secrets in the state database; see SECURITY.md.
func (s *State) SetOIDCRefreshToken(username, token string) error {
	_, err := s.db.Exec(`UPDATE users SET oidc_refresh = ? WHERE username = ?`, token, username)
	return err
}

// oidcRefreshToken returns a user's stored refresh token.
func (s *State) oidcRefreshToken(username string) string {
	var t string
	_ = s.db.QueryRow(`SELECT oidc_refresh FROM users WHERE username = ?`, username).Scan(&t)
	return t
}

// DisabledBy names what disabled an account: "oidc" for revalidation, or ""
// for an admin (or not disabled).
func (s *State) DisabledBy(username string) string {
	var by string
	_ = s.db.QueryRow(`SELECT disabled_by FROM users WHERE username = ?`, username).Scan(&by)
	return by
}

// SetDisabledBy disables or enables an account and records by what.
func (s *State) SetDisabledBy(username string, disabled bool, by string) error {
	if err := s.UpdateUser(username, func(u *User) { u.Disabled = disabled }); err != nil {
		return err
	}
	if !disabled {
		by = ""
	}
	_, err := s.db.Exec(`UPDATE users SET disabled_by = ? WHERE username = ?`, by, username)
	if disabled {
		// A disabled account keeps no refresh token: re-enabling it takes a
		// fresh sign-in, which issues a new one.
		_ = s.SetOIDCRefreshToken(username, "")
	}
	return err
}

// managedWithRefresh lists enabled provider-managed accounts that hold a
// refresh token.
func (s *State) managedWithRefresh() []User {
	rows, err := s.db.Query(`SELECT username FROM users WHERE managed_by = ? AND disabled = 0 AND oidc_refresh <> ''`, providerOIDC)
	if err != nil {
		return nil
	}
	var names []string
	for rows.Next() {
		var n string
		if rows.Scan(&n) == nil {
			names = append(names, n)
		}
	}
	rows.Close()
	var out []User
	for _, n := range names {
		if u, ok := s.LookupUser(n); ok {
			out = append(out, u)
		}
	}
	return out
}

// errProviderRefused marks an answer from the provider that says no, as
// distinct from failing to get an answer.
type errProviderRefused struct{ reason string }

func (e errProviderRefused) Error() string { return "provider refused: " + e.reason }

// refreshOIDC trades a refresh token for fresh tokens and the identity behind
// them.
func (a *Admin) refreshOIDC(ctx context.Context, refresh string) (oauthIdentity, error) {
	p, cfg, ready := a.providerReady(providerOIDC)
	if !ready {
		return oauthIdentity{}, fmt.Errorf("OIDC sign-in is not configured")
	}
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}}
	if !p.basicAuth {
		form.Set("client_id", cfg.ClientID)
		form.Set("client_secret", cfg.ClientSecret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return oauthIdentity{}, err
	}
	if p.basicAuth {
		req.SetBasicAuth(url.QueryEscape(cfg.ClientID), url.QueryEscape(cfg.ClientSecret))
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	body, status, err := a.readOAuthResponse(req)
	if err != nil {
		return oauthIdentity{}, err
	}
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		Error        string `json:"error"`
	}
	_ = json.Unmarshal(body, &tok)
	switch {
	case tok.Error == "invalid_grant":
		// RFC 6749 §5.2: a revoked, expired, or deactivated grant is
		// invalid_grant. That, and only that, is the provider saying no about
		// this user. invalid_client (an expired client secret),
		// invalid_request, and the rest describe this router's configuration,
		// and disabling everyone over them would lock the router out.
		return oauthIdentity{}, errProviderRefused{reason: "refresh token rejected (invalid_grant)"}
	case status != http.StatusOK || tok.AccessToken == "":
		if tok.Error != "" {
			return oauthIdentity{}, fmt.Errorf("token endpoint returned %d %s", status, tok.Error)
		}
		return oauthIdentity{}, fmt.Errorf("token endpoint returned %d", status)
	}
	ident, err := a.fetchOAuthIdentity(ctx, p, tok.AccessToken)
	if err != nil {
		return oauthIdentity{}, err
	}
	ident.RefreshToken = tok.RefreshToken
	return ident, nil
}

// revalidateOIDC re-checks every managed account with a refresh token once.
//
// Decisions are gathered before any is applied. If the provider appears to
// refuse most accounts at once, that is far likelier to be a misconfiguration
// (the wrong issuer, a changed role name) than a mass deactivation, so the run
// disables no one and says so; an admin can act on the log.
func (a *Admin) revalidateOIDC(ctx context.Context) {
	cfg := a.state.OIDC()
	type outcome struct {
		u       User
		ident   oauthIdentity
		refused string
	}
	var outcomes []outcome
	for _, u := range a.state.managedWithRefresh() {
		if ctx.Err() != nil {
			return
		}
		ident, refused, ok := a.revalidateOne(ctx, cfg, u)
		if ok {
			outcomes = append(outcomes, outcome{u, ident, refused})
		}
	}
	refusedCount := 0
	for _, o := range outcomes {
		if o.refused != "" {
			refusedCount++
		}
	}
	if refusedCount >= 3 && refusedCount*2 > len(outcomes) {
		a.log.Error("admin: oidc revalidation would disable most accounts at once; disabling none — check the provider configuration",
			"refused", refusedCount, "checked", len(outcomes))
		return
	}
	for _, o := range outcomes {
		if o.refused != "" {
			a.disableByProvider(o.u, o.refused)
			continue
		}
		roles, _ := cfg.rolesFor(o.ident.Claims)
		a.syncManagedAccess(o.u, cfg, roles, o.ident, "")
		if o.ident.RefreshToken != "" {
			_ = a.state.SetOIDCRefreshToken(o.u.Username, o.ident.RefreshToken)
		}
	}
}

// revalidateOne asks the provider about one account. ok is false when no
// answer was obtained; refused is non-empty when the provider said no.
func (a *Admin) revalidateOne(ctx context.Context, cfg OIDCConfig, u User) (ident oauthIdentity, refused string, ok bool) {
	ident, err := a.refreshOIDC(ctx, a.state.oidcRefreshToken(u.Username))
	var r errProviderRefused
	switch {
	case err == nil && ident.ID != u.OIDCSubject:
		return ident, "the refreshed identity is a different account", true
	case err == nil:
		if _, allowed := cfg.rolesFor(ident.Claims); !allowed {
			return ident, "no longer holds a role granting access", true
		}
		return ident, "", true
	case isRefused(err, &r):
		return ident, r.reason, true
	default:
		a.log.Warn("admin: oidc revalidation skipped", "user", u.Username, "error", err)
		return ident, "", false
	}
}

// disableByProvider disables an account the provider no longer vouches for,
// unless it is the last active owner or admin, which it never disables: a
// router with no one able to administer it is worse than one admin too many.
func (a *Admin) disableByProvider(u User, reason string) {
	if a.state.isPrivileged(u.Username) && a.state.otherActivePrivileged(u.Username) == 0 {
		a.log.Error("admin: identity provider refused the last admin; leaving the account enabled",
			"user", u.Username, "reason", reason)
		return
	}
	if err := a.state.SetDisabledBy(u.Username, true, providerOIDC); err != nil {
		a.log.Error("admin: disabling after revalidation", "user", u.Username, "error", err)
		return
	}
	for _, t := range a.state.ClientTokensFor(u.Username, false) {
		a.hub.CloseByToken(t.TokenHash)
	}
	a.log.Info("admin: account disabled by identity provider", "user", u.Username, "reason", reason)
	a.state.RecordAudit(u.Username, "user.disable.oidc", reason, "")
}

func isRefused(err error, out *errProviderRefused) bool {
	if r, ok := err.(errProviderRefused); ok {
		*out = r
		return true
	}
	return false
}

// RunOIDCRevalidation re-checks managed accounts on the configured interval
// until ctx ends. The interval is read each minute, so changing it in the
// portal takes effect without a restart.
func (a *Admin) RunOIDCRevalidation(ctx context.Context) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	var last time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			every := a.state.OIDC().RevalidateMinutes
			if every <= 0 || now.Sub(last) < time.Duration(every)*time.Minute {
				continue
			}
			last = now
			a.revalidateOIDC(ctx)
		}
	}
}
