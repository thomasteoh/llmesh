package admin

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Federated sign-in, in the shape every provider shares.
//
// Providers differ only in where the browser is sent, what a token buys, how
// the returned user document is read, and which columns hold the result. That
// variation lives in oauthProvider descriptors; the redirect, the state check,
// the code exchange, and the three ways a flow can end are all here, so a
// second provider cannot quietly acquire weaker CSRF handling or a different
// answer for an unlinked account.

// oauthCallbackPath is the redirect URI registered with each provider. It sits
// under /portal so the session cookie, whose Path is /portal, is sent with the
// callback — the link flow needs to know who is already signed in.
func oauthCallbackPath(provider string) string {
	return "/portal/auth/" + provider + "/callback"
}

// oauthStartPath is where the login page and the settings page send a browser
// to begin a flow.
func oauthStartPath(provider string) string {
	return "/portal/auth/" + provider
}

// oauthStateCookie carries the CSRF nonce for an in-flight authorization, along
// with which provider and which of the two flows started it.
const oauthStateCookie = "portal_oauth_state"

// oauthStateTTL bounds how long a started authorization stays valid. Long
// enough to sign in to the provider and approve the app, short enough that an
// abandoned attempt does not linger.
const oauthStateTTL = 10 * time.Minute

// The two flows that share a callback. "login" exchanges a provider identity
// for a session; "link" attaches one to the account already signed in.
const (
	oauthModeLogin = "login"
	oauthModeLink  = "link"
)

// oauthIdentity is a provider account as this router keeps it: a stable id to
// match on, and something human to show.
type oauthIdentity struct {
	// ID is the provider's immutable account identifier. Matching is on this
	// and never on an address or a handle, both of which their owners can
	// change and, once changed, someone else can take.
	ID string
	// Label is what the portal displays — a GitHub handle, a Google address.
	// It is refreshed on each sign-in and carries no authority.
	Label string
}

// oauthProvider describes one federated sign-in provider.
type oauthProvider struct {
	// key names the provider in URLs, settings keys, and audit entries.
	key string
	// name is what users are shown.
	name string

	authorizeURL string
	tokenURL     string
	userInfoURL  string
	scope        string
	// extraAuthParams and extraTokenParams carry the parameters one provider
	// wants and another does not.
	extraAuthParams  map[string]string
	extraTokenParams map[string]string

	// identity reads the provider's user document.
	identity func([]byte) (oauthIdentity, error)
	// get and set move an identity on and off a user record.
	get func(User) oauthIdentity
	set func(*User, oauthIdentity)
}

// providerFor returns the descriptor for a provider key, with any test endpoint
// overrides applied.
func (a *Admin) providerFor(key string) (oauthProvider, bool) {
	p, ok := oauthProviders[key]
	if !ok {
		return oauthProvider{}, false
	}
	if o, ok := a.oauthOverrides[key]; ok {
		if o.authorizeURL != "" {
			p.authorizeURL = o.authorizeURL
		}
		if o.tokenURL != "" {
			p.tokenURL = o.tokenURL
		}
		if o.userInfoURL != "" {
			p.userInfoURL = o.userInfoURL
		}
	}
	return p, true
}

// oauthEndpoints replaces a provider's endpoints so tests can point a flow at
// an httptest server instead of reaching the real provider.
type oauthEndpoints struct {
	authorizeURL string
	tokenURL     string
	userInfoURL  string
}

func (a *Admin) oauthClient() *http.Client {
	if a.httpClient != nil {
		return a.httpClient
	}
	return &http.Client{Timeout: 15 * time.Second}
}

// portalBaseURL is the absolute origin a browser reached the portal on, used to
// build links that have to survive leaving the site and coming back: the OAuth
// redirect URI and the sign-in links sent by mail.
func (a *Admin) portalBaseURL(r *http.Request) string {
	scheme := "http"
	if a.isSecure(r) {
		scheme = "https"
	}
	return scheme + "://" + a.effectiveHost(r)
}

// OAuthCallbackURL is the redirect URI this router expects a provider to call.
// It is shown on the settings page so an admin can paste it into the provider's
// console rather than assemble it by hand and get it subtly wrong.
func (a *Admin) OAuthCallbackURL(r *http.Request, provider string) string {
	return a.portalBaseURL(r) + oauthCallbackPath(provider)
}

// setOAuthState starts an authorization: it mints a nonce, remembers it along
// with the provider and flow in a short-lived cookie, and returns the nonce to
// send to the provider.
func (a *Admin) setOAuthState(w http.ResponseWriter, r *http.Request, provider, mode string) string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	nonce := base64.RawURLEncoding.EncodeToString(b)
	http.SetCookie(w, &http.Cookie{
		Name:     oauthStateCookie,
		Value:    provider + ":" + mode + ":" + nonce,
		Path:     "/portal",
		HttpOnly: true,
		Secure:   a.isSecure(r),
		// Lax, not Strict: the callback is a top-level navigation arriving from
		// the provider, and Strict would withhold the cookie exactly then.
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(oauthStateTTL.Seconds()),
	})
	return nonce
}

// takeOAuthState validates the nonce the provider echoed back against the
// cookie and clears the cookie either way, so a state cannot be replayed.
//
// The provider the flow started with is checked too: a state minted for one
// provider must not be spendable at another's callback, or a provider willing
// to hand out an id of someone else's choosing could complete a flow begun
// against a provider that is not.
func (a *Admin) takeOAuthState(w http.ResponseWriter, r *http.Request, provider string) (mode string, ok bool) {
	c, err := r.Cookie(oauthStateCookie)
	http.SetCookie(w, &http.Cookie{Name: oauthStateCookie, Path: "/portal", MaxAge: -1})
	if err != nil {
		return "", false
	}
	gotProvider, rest, found := strings.Cut(c.Value, ":")
	if !found {
		return "", false
	}
	mode, nonce, found := strings.Cut(rest, ":")
	if !found || nonce == "" {
		return "", false
	}
	if gotProvider != provider {
		return "", false
	}
	if subtle.ConstantTimeCompare([]byte(nonce), []byte(r.URL.Query().Get("state"))) != 1 {
		return "", false
	}
	if mode != oauthModeLogin && mode != oauthModeLink {
		return "", false
	}
	return mode, true
}

// beginOAuth redirects the browser to the provider's authorization screen.
func (a *Admin) beginOAuth(w http.ResponseWriter, r *http.Request, providerKey, mode string) {
	p, ok := a.providerFor(providerKey)
	if !ok {
		http.NotFound(w, r)
		return
	}
	cfg := a.state.OAuth(providerKey)
	if !cfg.Configured() {
		http.Error(w, p.name+" sign-in is not configured", http.StatusNotFound)
		return
	}
	nonce := a.setOAuthState(w, r, providerKey, mode)
	q := url.Values{
		"client_id":     {cfg.ClientID},
		"redirect_uri":  {a.OAuthCallbackURL(r, providerKey)},
		"response_type": {"code"},
		"scope":         {p.scope},
		"state":         {nonce},
	}
	for k, v := range p.extraAuthParams {
		q.Set(k, v)
	}
	http.Redirect(w, r, p.authorizeURL+"?"+q.Encode(), http.StatusFound)
}

// handleOAuthLogin starts the flow that signs a user in.
func (a *Admin) handleOAuthLogin(providerKey string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Already signed in: nothing to do, and running the flow would only
		// swap one session for another.
		if _, ok := a.sessionUser(r); ok {
			http.Redirect(w, r, "/portal/", http.StatusFound)
			return
		}
		a.beginOAuth(w, r, providerKey, oauthModeLogin)
	}
}

// handleOAuthLink starts the flow that attaches a provider account to the
// signed-in user. It is a POST behind CSRF: a GET would let another site start
// a link.
func (a *Admin) handleOAuthLink(providerKey string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a.beginOAuth(w, r, providerKey, oauthModeLink)
	}
}

func (a *Admin) handleOAuthCallback(providerKey string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := a.providerFor(providerKey)
		if !ok {
			http.NotFound(w, r)
			return
		}
		cfg := a.state.OAuth(providerKey)
		if !cfg.Configured() {
			http.Error(w, p.name+" sign-in is not configured", http.StatusNotFound)
			return
		}
		mode, ok := a.takeOAuthState(w, r, providerKey)
		if !ok {
			a.oauthFailure(w, r, oauthModeLogin,
				"That "+p.name+" sign-in attempt could not be verified. Please try again.")
			return
		}
		// Providers report a user's refusal here rather than by not calling back.
		if errCode := r.URL.Query().Get("error"); errCode != "" {
			a.oauthFailure(w, r, mode, p.name+" sign-in was cancelled.")
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" {
			a.oauthFailure(w, r, mode, p.name+" did not return an authorization code.")
			return
		}

		ident, err := a.exchangeOAuthCode(r.Context(), p, cfg, code, a.OAuthCallbackURL(r, providerKey))
		if err != nil {
			a.log.Error("admin: oauth exchange", "provider", providerKey, "mode", mode, "error", err)
			a.oauthFailure(w, r, mode, "Could not complete "+p.name+" sign-in. Please try again.")
			return
		}

		if mode == oauthModeLink {
			a.completeOAuthLink(w, r, p, ident)
			return
		}
		a.completeOAuthLogin(w, r, p, ident)
	}
}

// completeOAuthLogin signs in the account this identity is linked to.
//
// An unlinked identity is refused rather than provisioned: access to this
// router is granted by an admin creating an account, and a federated login is a
// way to reach an existing one, not a way to obtain one.
func (a *Admin) completeOAuthLogin(w http.ResponseWriter, r *http.Request, p oauthProvider, ident oauthIdentity) {
	u, ok := a.state.LookupUserByOAuth(p.key, ident.ID)
	if !ok {
		a.log.Info("admin: sign-in refused for unlinked account", "provider", p.key, "label", ident.Label)
		a.renderLogin(w, r, "", "That "+p.name+" account is not linked to an llmesh account. Sign in with your password, then link it under Settings.")
		return
	}
	if u.Disabled {
		a.renderLogin(w, r, "", "Account disabled.")
		return
	}
	// Keep the stored label current, so the portal does not go on showing a
	// name its owner has since changed. The id is what the match was on.
	if ident.Label != "" && ident.Label != p.get(u).Label {
		_ = a.state.UpdateUser(u.Username, func(user *User) {
			p.set(user, oauthIdentity{ID: ident.ID, Label: ident.Label})
		})
	}
	a.state.RecordAudit(u.Username, "auth.login."+p.key, ident.Label, a.clientIP(r))
	a.startSession(w, r, u.Username)
	http.Redirect(w, r, "/portal/", http.StatusFound)
}

// completeOAuthLink attaches the identity to the session user's account.
func (a *Admin) completeOAuthLink(w http.ResponseWriter, r *http.Request, p oauthProvider, ident oauthIdentity) {
	u, ok := a.sessionUser(r)
	if !ok {
		http.Redirect(w, r, "/portal/login", http.StatusFound)
		return
	}
	if err := a.state.UpdateUser(u.Username, func(user *User) { p.set(user, ident) }); err != nil {
		a.renderSettings(w, r, u, "", err.Error())
		return
	}
	a.state.RecordAudit(u.Username, "auth."+p.key+".link", ident.Label, a.clientIP(r))
	a.renderSettings(w, r, u, fmt.Sprintf("%s account %s linked.", p.name, ident.Label), "")
}

// oauthFailure reports a failed flow on whichever page the user started from.
func (a *Admin) oauthFailure(w http.ResponseWriter, r *http.Request, mode, msg string) {
	if mode == oauthModeLink {
		if u, ok := a.sessionUser(r); ok {
			a.renderSettings(w, r, u, "", msg)
			return
		}
	}
	a.renderLogin(w, r, "", msg)
}

// handleOAuthUnlink detaches the session user's account from a provider.
func (a *Admin) handleOAuthUnlink(providerKey string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := ctxGetUser(r)
		p, ok := a.providerFor(providerKey)
		if !ok {
			http.NotFound(w, r)
			return
		}
		previous := p.get(u).Label
		if err := a.state.UpdateUser(u.Username, func(user *User) {
			p.set(user, oauthIdentity{})
		}); err != nil {
			a.renderSettings(w, r, u, "", err.Error())
			return
		}
		a.state.RecordAudit(u.Username, "auth."+p.key+".unlink", previous, a.clientIP(r))
		a.renderSettings(w, r, u, p.name+" account unlinked.", "")
	}
}

// exchangeOAuthCode trades the authorization code for a token and reads the
// identity behind it.
func (a *Admin) exchangeOAuthCode(ctx context.Context, p oauthProvider, cfg OAuthConfig, code, redirectURI string) (oauthIdentity, error) {
	form := url.Values{
		"client_id":     {cfg.ClientID},
		"client_secret": {cfg.ClientSecret},
		"code":          {code},
		"redirect_uri":  {redirectURI},
	}
	for k, v := range p.extraTokenParams {
		form.Set(k, v)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return oauthIdentity{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	body, status, err := a.readOAuthResponse(req)
	if err != nil {
		return oauthIdentity{}, err
	}
	if status != http.StatusOK {
		// The body can echo request values back; it goes to the router log via
		// the caller, never to the browser.
		return oauthIdentity{}, fmt.Errorf("token endpoint returned %d", status)
	}
	var tok struct {
		AccessToken      string `json:"access_token"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return oauthIdentity{}, fmt.Errorf("decode token response: %w", err)
	}
	if tok.Error != "" {
		return oauthIdentity{}, fmt.Errorf("%s: %s: %s", p.key, tok.Error, tok.ErrorDescription)
	}
	if tok.AccessToken == "" {
		return oauthIdentity{}, fmt.Errorf("%s returned no access token", p.key)
	}
	return a.fetchOAuthIdentity(ctx, p, tok.AccessToken)
}

func (a *Admin) fetchOAuthIdentity(ctx context.Context, p oauthProvider, accessToken string) (oauthIdentity, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.userInfoURL, nil)
	if err != nil {
		return oauthIdentity{}, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	if p.key == providerGitHub {
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	}
	body, status, err := a.readOAuthResponse(req)
	if err != nil {
		return oauthIdentity{}, err
	}
	if status != http.StatusOK {
		return oauthIdentity{}, fmt.Errorf("user endpoint returned %d", status)
	}
	ident, err := p.identity(body)
	if err != nil {
		return oauthIdentity{}, err
	}
	if ident.ID == "" {
		return oauthIdentity{}, fmt.Errorf("%s returned no account id", p.key)
	}
	return ident, nil
}

// readOAuthResponse performs a request and reads a bounded body: an endpoint
// that streams forever should not exhaust memory.
func (a *Admin) readOAuthResponse(req *http.Request) ([]byte, int, error) {
	resp, err := a.oauthClient().Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}
