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
	"strconv"
	"strings"
	"time"
)

// GitHub's OAuth endpoints. Overridable per-Admin so tests can point the flow at
// an httptest server instead of reaching the real GitHub.
const (
	defaultGitHubAuthorizeURL = "https://github.com/login/oauth/authorize"
	defaultGitHubTokenURL     = "https://github.com/login/oauth/access_token"
	defaultGitHubAPIBaseURL   = "https://api.github.com"
)

// githubCallbackPath is the redirect URI registered with the GitHub OAuth app.
// It sits under /portal so the session cookie, whose Path is /portal, is sent
// with the callback — the link flow needs to know who is already signed in.
const githubCallbackPath = "/portal/auth/github/callback"

// oauthStateCookie carries the CSRF nonce for an in-flight authorization, along
// with which of the two flows started it.
const oauthStateCookie = "portal_gh_state"

// oauthStateTTL bounds how long a started authorization stays valid. Long
// enough to sign in to GitHub and approve the app, short enough that an
// abandoned attempt does not linger.
const oauthStateTTL = 10 * time.Minute

// The two flows that share the callback. "login" exchanges a GitHub identity
// for a session; "link" attaches one to the account already signed in.
const (
	oauthModeLogin = "login"
	oauthModeLink  = "link"
)

func (a *Admin) githubAuthorizeURL() string {
	if a.ghAuthorizeURL != "" {
		return a.ghAuthorizeURL
	}
	return defaultGitHubAuthorizeURL
}

func (a *Admin) githubTokenURL() string {
	if a.ghTokenURL != "" {
		return a.ghTokenURL
	}
	return defaultGitHubTokenURL
}

func (a *Admin) githubAPIBaseURL() string {
	if a.ghAPIBaseURL != "" {
		return a.ghAPIBaseURL
	}
	return defaultGitHubAPIBaseURL
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

// GitHubCallbackURL is the redirect URI this router expects GitHub to call. It
// is shown on the settings page so an admin can paste it into the OAuth app
// rather than assemble it by hand and get it subtly wrong.
func (a *Admin) GitHubCallbackURL(r *http.Request) string {
	return a.portalBaseURL(r) + githubCallbackPath
}

// setOAuthState starts an authorization: it mints a nonce, remembers it and the
// flow in a short-lived cookie, and returns the nonce to send to GitHub.
func (a *Admin) setOAuthState(w http.ResponseWriter, r *http.Request, mode string) string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	nonce := base64.RawURLEncoding.EncodeToString(b)
	http.SetCookie(w, &http.Cookie{
		Name:     oauthStateCookie,
		Value:    mode + ":" + nonce,
		Path:     "/portal",
		HttpOnly: true,
		Secure:   a.isSecure(r),
		// Lax, not Strict: the callback is a top-level navigation arriving from
		// github.com, and Strict would withhold the cookie exactly then.
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(oauthStateTTL.Seconds()),
	})
	return nonce
}

// takeOAuthState validates the nonce GitHub echoed back against the cookie and
// clears the cookie either way, so a state cannot be replayed.
func (a *Admin) takeOAuthState(w http.ResponseWriter, r *http.Request) (mode string, ok bool) {
	c, err := r.Cookie(oauthStateCookie)
	http.SetCookie(w, &http.Cookie{Name: oauthStateCookie, Path: "/portal", MaxAge: -1})
	if err != nil {
		return "", false
	}
	mode, nonce, found := strings.Cut(c.Value, ":")
	if !found || nonce == "" {
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

// beginGitHub redirects the browser to GitHub's authorization screen.
func (a *Admin) beginGitHub(w http.ResponseWriter, r *http.Request, mode string) {
	cfg := a.state.GitHubAuth()
	if !cfg.Configured() {
		http.Error(w, "GitHub sign-in is not configured", http.StatusNotFound)
		return
	}
	nonce := a.setOAuthState(w, r, mode)
	q := url.Values{
		"client_id":    {cfg.ClientID},
		"redirect_uri": {a.GitHubCallbackURL(r)},
		// read:user is all this needs: an account id and a handle to show. No
		// email scope, because the GitHub identity is matched on the linked
		// account id and never on an address.
		"scope":        {"read:user"},
		"state":        {nonce},
		"allow_signup": {"false"},
	}
	http.Redirect(w, r, a.githubAuthorizeURL()+"?"+q.Encode(), http.StatusFound)
}

func (a *Admin) handleGitHubLogin(w http.ResponseWriter, r *http.Request) {
	// Already signed in: nothing to do, and running the flow would only swap
	// one session for another.
	if _, ok := a.sessionUser(r); ok {
		http.Redirect(w, r, "/portal/", http.StatusFound)
		return
	}
	a.beginGitHub(w, r, oauthModeLogin)
}

// handleGitHubLink starts the flow that attaches a GitHub account to the signed-in
// user. It is a POST behind CSRF: a GET would let another site start a link.
func (a *Admin) handleGitHubLink(w http.ResponseWriter, r *http.Request) {
	a.beginGitHub(w, r, oauthModeLink)
}

// githubIdentity is the part of GitHub's user object this router keeps.
type githubIdentity struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
}

func (a *Admin) handleGitHubCallback(w http.ResponseWriter, r *http.Request) {
	cfg := a.state.GitHubAuth()
	if !cfg.Configured() {
		http.Error(w, "GitHub sign-in is not configured", http.StatusNotFound)
		return
	}
	mode, ok := a.takeOAuthState(w, r)
	if !ok {
		a.githubFailure(w, r, oauthModeLogin, "That GitHub sign-in attempt could not be verified. Please try again.")
		return
	}
	// GitHub reports a user's refusal here rather than by not calling back.
	if errCode := r.URL.Query().Get("error"); errCode != "" {
		a.githubFailure(w, r, mode, "GitHub sign-in was cancelled.")
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		a.githubFailure(w, r, mode, "GitHub did not return an authorization code.")
		return
	}

	ident, err := a.exchangeGitHubCode(r.Context(), cfg, code, a.GitHubCallbackURL(r))
	if err != nil {
		a.log.Error("admin: github oauth exchange", "mode", mode, "error", err)
		a.githubFailure(w, r, mode, "Could not complete GitHub sign-in. Please try again.")
		return
	}
	ghID := strconv.FormatInt(ident.ID, 10)

	if mode == oauthModeLink {
		a.completeGitHubLink(w, r, ghID, ident.Login)
		return
	}
	a.completeGitHubLogin(w, r, ghID, ident.Login)
}

// completeGitHubLogin signs in the account this GitHub identity is linked to.
// An unlinked identity is refused rather than provisioned: access to this router
// is granted by an admin creating an account, and a GitHub login is a way to
// reach an existing one, not a way to obtain one.
func (a *Admin) completeGitHubLogin(w http.ResponseWriter, r *http.Request, ghID, ghLogin string) {
	u, ok := a.state.LookupUserByGitHubID(ghID)
	if !ok {
		a.log.Info("admin: github sign-in refused for unlinked account", "github_login", ghLogin)
		a.renderLogin(w, r, "", "That GitHub account is not linked to an llmesh account. Sign in with your password, then link it under Settings.")
		return
	}
	if u.Disabled {
		a.renderLogin(w, r, "", "Account disabled.")
		return
	}
	// Keep the stored handle current, so the portal does not go on showing a
	// name its owner has since changed. The id is what the match was on.
	if ghLogin != "" && ghLogin != u.GitHubLogin {
		_ = a.state.UpdateUser(u.Username, func(user *User) { user.GitHubLogin = ghLogin })
	}
	a.state.RecordAudit(u.Username, "auth.login.github", ghLogin, a.clientIP(r))
	a.startSession(w, r, u.Username)
	http.Redirect(w, r, "/portal/", http.StatusFound)
}

// completeGitHubLink attaches the identity to the session user's account.
func (a *Admin) completeGitHubLink(w http.ResponseWriter, r *http.Request, ghID, ghLogin string) {
	u, ok := a.sessionUser(r)
	if !ok {
		http.Redirect(w, r, "/portal/login", http.StatusFound)
		return
	}
	if err := a.state.UpdateUser(u.Username, func(user *User) {
		user.GitHubUserID = ghID
		user.GitHubLogin = ghLogin
	}); err != nil {
		a.renderSettings(w, r, u, "", err.Error())
		return
	}
	a.state.RecordAudit(u.Username, "auth.github.link", ghLogin, a.clientIP(r))
	a.renderSettings(w, r, u, fmt.Sprintf("GitHub account @%s linked.", ghLogin), "")
}

// githubFailure reports a failed flow on whichever page the user started from.
func (a *Admin) githubFailure(w http.ResponseWriter, r *http.Request, mode, msg string) {
	if mode == oauthModeLink {
		if u, ok := a.sessionUser(r); ok {
			a.renderSettings(w, r, u, "", msg)
			return
		}
	}
	a.renderLogin(w, r, "", msg)
}

// exchangeGitHubCode trades the authorization code for a token and reads the
// identity behind it.
func (a *Admin) exchangeGitHubCode(ctx context.Context, cfg GitHubAuthConfig, code, redirectURI string) (githubIdentity, error) {
	form := url.Values{
		"client_id":     {cfg.ClientID},
		"client_secret": {cfg.ClientSecret},
		"code":          {code},
		"redirect_uri":  {redirectURI},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.githubTokenURL(), strings.NewReader(form.Encode()))
	if err != nil {
		return githubIdentity{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := a.oauthClient().Do(req)
	if err != nil {
		return githubIdentity{}, err
	}
	defer resp.Body.Close()
	// Bound the read: an endpoint that streams forever should not exhaust memory.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return githubIdentity{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return githubIdentity{}, fmt.Errorf("token endpoint returned %s", resp.Status)
	}
	var tok struct {
		AccessToken      string `json:"access_token"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return githubIdentity{}, fmt.Errorf("decode token response: %w", err)
	}
	if tok.Error != "" {
		// The description can echo back request values; it goes to the router
		// log, never to the browser.
		return githubIdentity{}, fmt.Errorf("github: %s: %s", tok.Error, tok.ErrorDescription)
	}
	if tok.AccessToken == "" {
		return githubIdentity{}, fmt.Errorf("github returned no access token")
	}
	return a.fetchGitHubUser(ctx, tok.AccessToken)
}

func (a *Admin) fetchGitHubUser(ctx context.Context, accessToken string) (githubIdentity, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.githubAPIBaseURL()+"/user", nil)
	if err != nil {
		return githubIdentity{}, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := a.oauthClient().Do(req)
	if err != nil {
		return githubIdentity{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return githubIdentity{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return githubIdentity{}, fmt.Errorf("user endpoint returned %s", resp.Status)
	}
	var ident githubIdentity
	if err := json.Unmarshal(body, &ident); err != nil {
		return githubIdentity{}, fmt.Errorf("decode user response: %w", err)
	}
	if ident.ID == 0 {
		return githubIdentity{}, fmt.Errorf("github returned no user id")
	}
	return ident, nil
}

// handleGitHubUnlink detaches the session user's GitHub account.
func (a *Admin) handleGitHubUnlink(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	if err := a.state.UpdateUser(u.Username, func(user *User) {
		user.GitHubUserID = ""
		user.GitHubLogin = ""
	}); err != nil {
		a.renderSettings(w, r, u, "", err.Error())
		return
	}
	a.state.RecordAudit(u.Username, "auth.github.unlink", u.GitHubLogin, a.clientIP(r))
	a.renderSettings(w, r, u, "GitHub account unlinked.", "")
}
