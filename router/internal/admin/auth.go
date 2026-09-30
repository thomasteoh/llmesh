package admin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"time"

	"golang.org/x/crypto/bcrypt"
)

type contextKey int

const ctxUser contextKey = 1

const sessionCookie = "admin_session"
const sessionTTL = 24 * time.Hour
const bcryptCost = 12

// dummyHash is a valid bcrypt hash compared against on the user-not-found path
// so login timing does not reveal whether a username exists.
var dummyHash = func() string {
	h, _ := bcrypt.GenerateFromPassword([]byte("llmesh-dummy-password"), bcryptCost)
	return string(h)
}()

// sessionUser returns the authenticated User for this request, or User{} if not authenticated.
func (a *Admin) sessionUser(r *http.Request) (User, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return User{}, false
	}
	username, ok := a.sessions.lookup(c.Value)
	if !ok {
		return User{}, false
	}
	u, found := a.state.LookupUser(username)
	if !found || u.Disabled {
		return User{}, false
	}
	return u, true
}

// requireAuth wraps a handler, redirecting to /portal/login if no valid session.
func (a *Admin) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, ok := a.sessionUser(r)
		if !ok {
			// A script-issued request follows redirects transparently, so a 302
			// here would arrive as a perfectly good 200 holding the login page
			// and the action would look like it worked while doing nothing.
			// Answer those with a status the page cannot mistake for success.
			if r.Header.Get(portalFetchHeader) != "" {
				w.Header().Set("X-Portal-Location", "/portal/login")
				http.Error(w, "session expired", http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, "/portal/login", http.StatusFound)
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), ctxUser, u))
		next(w, a.withSubject(r, u))
	}
}

// requireCSRF wraps a handler, validating the CSRF token on POST requests.
// Token is extracted from the "csrf_token" form value or "X-CSRF-Token" header.
// Returns 403 if the token is missing or invalid, then short-circuits.
func (a *Admin) requireCSRF(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			next(w, r)
			return
		}
		u := ctxGetUser(r)
		// Extract token from form value or header
		token := r.Header.Get("X-CSRF-Token")
		if token == "" {
			_ = r.ParseForm()
			token = r.FormValue("csrf_token")
		}
		if !a.state.ConsumeCSRF(u.Username, token) {
			// Refresh a new token so the page can re-render with a fresh one.
			newToken, err := a.state.RefreshCSRFToken(u.Username)
			if err == nil && newToken != "" {
				if c, err := r.Cookie(sessionCookie); err == nil {
					a.sessions.setCSRF(c.Value, newToken)
				}
			}
			http.Error(w, "forbidden: invalid CSRF token", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

// ctxGetUser retrieves the authenticated User from the request context.
// Only valid inside a requireAuth-wrapped handler.
func ctxGetUser(r *http.Request) User {
	return r.Context().Value(ctxUser).(User)
}

// loginPage is the login template's data. It is a struct rather than a map so
// that a field the template names but the handler stopped supplying is an
// execution error rather than a silently blank sign-in button.
type loginPage struct {
	Error     string
	Notice    string
	CSRFToken string
	// Providers lists the federated sign-in buttons to draw, and EmailEnabled
	// whether to offer a link by mail. A router with neither configured shows
	// exactly the username-and-password form it always did.
	Providers    []loginProvider
	EmailEnabled bool
	// EmailSubmitted keeps the address in the field after a failed attempt, so
	// a typo is corrected rather than retyped.
	EmailSubmitted string
}

// renderLogin draws the sign-in page, offering whichever alternative methods an
// admin has configured.
func (a *Admin) renderLogin(w http.ResponseWriter, r *http.Request, notice, errMsg string) {
	a.renderLoginWithEmail(w, r, notice, errMsg, "")
}

// loginProvider is one federated sign-in button.
type loginProvider struct {
	Key  string
	Name string
	// Path is where the button sends the browser to start the flow.
	Path string
}

func (a *Admin) renderLoginWithEmail(w http.ResponseWriter, r *http.Request, notice, errMsg, email string) {
	var providers []loginProvider
	for _, key := range oauthProviderOrder {
		p, _, ready := a.providerReady(key)
		if !ready {
			continue
		}
		providers = append(providers, loginProvider{Key: key, Name: p.name, Path: oauthStartPath(key)})
	}
	a.renderStandalone(w, "login", loginPage{
		Error:          errMsg,
		Notice:         notice,
		Providers:      providers,
		EmailEnabled:   a.state.SMTP().Configured(),
		EmailSubmitted: email,
	})
}

// startSession issues a fresh session and its CSRF token for username, and sets
// the session cookie. Every way of signing in ends here, so a magic link and a
// GitHub callback produce exactly the session a password does — no path gets a
// longer-lived cookie or skips the CSRF token by being written separately.
func (a *Admin) startSession(w http.ResponseWriter, r *http.Request, username string) {
	sid := a.sessions.createWithMeta(username, a.clientIP(r), r.UserAgent())
	// Generate a fresh CSRF token and store it in the session (not in state —
	// session-scoped tokens let concurrent tabs operate independently).
	if csrfToken, err := generateCSRFToken(); err == nil {
		a.sessions.setCSRF(sid, csrfToken)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    sid,
		Path:     "/portal",
		HttpOnly: true,
		Secure:   a.isSecure(r), // Secure over direct TLS or a trusted TLS-terminating proxy
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
}

func (a *Admin) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		a.renderLogin(w, r, "", "")
		return
	}
	r.ParseForm()
	username := r.FormValue("username")
	password := r.FormValue("password")
	u, ok := a.state.LookupUser(username)
	if !ok {
		// Run a dummy comparison so a missing user takes the same time as a
		// wrong password, and return the same message — no user enumeration.
		bcrypt.CompareHashAndPassword([]byte(dummyHash), []byte(password))
		a.renderLogin(w, r, "", "Invalid credentials.")
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)); err != nil {
		a.renderLogin(w, r, "", "Invalid credentials.")
		return
	}
	// "Account disabled" is only revealed to someone who supplied the correct
	// password, so it does not leak account existence to an attacker.
	if u.Disabled {
		a.renderLogin(w, r, "", "Account disabled.")
		return
	}
	if msg := managedElsewhere(u, "password"); msg != "" {
		a.renderLogin(w, r, "", msg)
		return
	}
	a.startSession(w, r, username)
	http.Redirect(w, r, "/portal/", http.StatusFound)
}

func (a *Admin) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Look up username BEFORE deleting the session.
	var username string
	if c, err := r.Cookie(sessionCookie); err == nil {
		username, _ = a.sessions.lookup(c.Value)
		a.sessions.delete(c.Value)
	}
	if username != "" {
		a.state.UpdateUser(username, func(user *User) { user.CSRFToken = "" })
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/portal", MaxAge: -1})
	http.Redirect(w, r, "/portal/login", http.StatusFound)
}

func (a *Admin) handleSetup(w http.ResponseWriter, r *http.Request) {
	if !a.state.NeedsSetup() {
		http.Redirect(w, r, "/portal/login", http.StatusFound)
		return
	}
	if r.Method == http.MethodGet {
		a.renderStandalone(w, "setup", map[string]string{"Error": ""})
		return
	}
	a.handleSetupPost(w, r)
}

func (a *Admin) handleSetupPost(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	username := r.FormValue("username")
	password := r.FormValue("password")
	confirm := r.FormValue("confirm")
	if username == "" || password == "" {
		a.renderStandalone(w, "setup", map[string]string{"Error": "Username and password are required."})
		return
	}
	if password != confirm {
		a.renderStandalone(w, "setup", map[string]string{"Error": "Passwords do not match."})
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		a.renderStandalone(w, "setup", map[string]string{"Error": "Internal error."})
		return
	}
	if err := a.state.AddFirstAdmin(User{
		Username:     username,
		PasswordHash: string(hash),
		Role:         "admin",
	}); err != nil {
		a.renderStandalone(w, "setup", map[string]string{"Error": err.Error()})
		return
	}
	http.Redirect(w, r, "/portal/login", http.StatusFound)
}

// HashPassword hashes a plaintext password using bcrypt.
func HashPassword(password string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	return string(b), err
}

// generateTempPassword returns a random, high-entropy password suitable for an
// admin-initiated reset. The plaintext is shown to the admin once (never stored)
// so they can hand it to the user, who is expected to change it after signing in.
func generateTempPassword() (string, error) {
	b := make([]byte, 12) // 96 bits of entropy
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
