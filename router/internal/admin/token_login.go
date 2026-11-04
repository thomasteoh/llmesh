package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

// handleTokenLogin signs an agentic (machine) client in via a provider access
// token instead of the browser authorization-code flow. It accepts a Bearer
// token that a Zitadel machine user obtained with the client_credentials grant
// (an API app in the llmesh project), validates it against the provider's
// userinfo endpoint, and establishes a portal session for the machine account.
//
// The route is intentionally narrow: it only ever signs in the machine account
// the router is configured to trust (provisioned as admin), and never accepts a
// browser authorization code. The token is validated by calling the same
// userinfo endpoint the browser flow uses, so the provider vouches for the
// identity exactly as it does for a human.
//
// The endpoint is registered unconditionally, like the other sign-in methods,
// and refuses when OIDC is not configured or no userinfo endpoint is known.
func (a *Admin) handleTokenLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// The token is read from the Authorization header, never from a form or
	// query value: a machine credential must not be accepted from a URL.
	const bearer = "Bearer "
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, bearer) || strings.TrimSpace(auth[len(bearer):]) == "" {
		http.Error(w, "missing bearer token", http.StatusUnauthorized)
		return
	}
	token := strings.TrimSpace(auth[len(bearer):])

	cfg := a.state.OIDC()
	if !cfg.Discovered() || cfg.UserInfoURL == "" {
		http.Error(w, "oidc sign-in is not configured", http.StatusNotFound)
		return
	}

	ident, err := a.fetchTokenIdentity(r.Context(), cfg.UserInfoURL, token)
	if err != nil {
		a.log.Info("admin: token login rejected", "error", err)
		http.Error(w, "token rejected", http.StatusUnauthorized)
		return
	}
	if ident.ID == "" {
		http.Error(w, "token carried no identity", http.StatusUnauthorized)
		return
	}

	u, err := a.adminForTokenIdentity(ident, r)
	if err != nil {
		a.log.Error("admin: token login provisioning failed", "label", ident.Label, "error", err)
		http.Error(w, "could not provision account", http.StatusInternalServerError)
		return
	}

	a.state.RecordAudit(u.Username, "auth.login.token", ident.Label, a.clientIP(r))
	_, csrf := a.startSessionWithCSRF(w, r, u.Username, "")

	// The client needs the CSRF token that guards the provisioning endpoints,
	// so it is returned here rather than fetched from a rendered page.
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"username":   u.Username,
		"csrf_token": csrf,
		"location":   "/portal/",
	})
}

// fetchTokenIdentity calls the provider userinfo endpoint with the bearer
// token and parses the identity. The endpoint and parsing are the same ones
// the browser flow uses, so the provider vouches for the machine identity.
func (a *Admin) fetchTokenIdentity(ctx context.Context, userInfoURL, token string) (oauthIdentity, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, userInfoURL, nil)
	if err != nil {
		return oauthIdentity{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	body, status, err := a.readOAuthResponse(req)
	if err != nil {
		return oauthIdentity{}, err
	}
	if status != http.StatusOK {
		return oauthIdentity{}, &tokenStatusError{status: status}
	}
	return oidcIdentity(body)
}

// adminForTokenIdentity returns the llmesh account for a validated machine
// identity, provisioning an admin account the first time it is seen. Only the
// provider-vouched machine identity is accepted: this is the trusted path.
func (a *Admin) adminForTokenIdentity(ident oauthIdentity, r *http.Request) (User, error) {
	if u, ok := a.state.LookupUserByOAuth(providerOIDC, ident.ID); ok {
		if u.Disabled {
			return User{}, errDisabled
		}
		return u, nil
	}
	u, err := a.state.ProvisionOIDCUser(provisionedUsername(ident), "admin", ident)
	if err != nil {
		return User{}, err
	}
	a.state.RecordAudit(u.Username, "user.provision.oidc.token", ident.Label, a.clientIP(r))
	return u, nil
}

// tokenStatusError is the error for a non-200 userinfo response.
type tokenStatusError struct {
	status int
}

func (e *tokenStatusError) Error() string {
	return "userinfo endpoint returned " + http.StatusText(e.status)
}

// errDisabled marks an account that is disabled and so refused.
var errDisabled = &disabledError{}

type disabledError struct{}

func (*disabledError) Error() string { return "account disabled" }
