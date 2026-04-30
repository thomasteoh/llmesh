package admin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

type contextKey int

const ctxUser contextKey = 1

const sessionCookie = "admin_session"
const sessionTTL = 24 * time.Hour
const bcryptCost = 12

// sessionStore is an in-memory store of active sessions.
type sessionStore struct {
	mu      sync.Mutex
	entries map[string]sessionEntry
}

type sessionEntry struct {
	Username  string
	Expiry    time.Time
	CSRFToken string // plaintext CSRF token to serve with first page after login
}

func newSessionStore() *sessionStore {
	return &sessionStore{entries: make(map[string]sessionEntry)}
}

func (s *sessionStore) create(username string) string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	id := hex.EncodeToString(b)
	s.mu.Lock()
	s.entries[id] = sessionEntry{Username: username, Expiry: time.Now().Add(sessionTTL)}
	s.mu.Unlock()
	return id
}

func (s *sessionStore) lookup(id string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[id]
	if !ok {
		return "", false
	}
	if time.Now().After(e.Expiry) {
		delete(s.entries, id)
		return "", false
	}
	return e.Username, true
}

func (s *sessionStore) delete(id string) {
	s.mu.Lock()
	delete(s.entries, id)
	s.mu.Unlock()
}

func (s *sessionStore) setCSRF(id, token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.entries[id]; ok {
		e.CSRFToken = token
		s.entries[id] = e
	}
}

func (s *sessionStore) getCSRF(id string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[id]
	if !ok {
		return "", false
	}
	return e.CSRFToken, true
}

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
			http.Redirect(w, r, "/portal/login", http.StatusFound)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxUser, u)))
	}
}

// requireAdmin wraps a handler, returning 403 if the session user is not an admin.
func (a *Admin) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return a.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		u := r.Context().Value(ctxUser).(User)
		if u.Role != "admin" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next(w, r)
	})
}

// ctxGetUser retrieves the authenticated User from the request context.
// Only valid inside a requireAuth-wrapped handler.
func ctxGetUser(r *http.Request) User {
	return r.Context().Value(ctxUser).(User)
}

func (a *Admin) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		a.renderStandalone(w, "login", map[string]string{"Error": ""})
		return
	}
	r.ParseForm()
	username := r.FormValue("username")
	password := r.FormValue("password")
	u, ok := a.state.LookupUser(username)
	if !ok {
		a.renderStandalone(w, "login", map[string]string{"Error": "Invalid credentials."})
		return
	}
	if u.Disabled {
		a.renderStandalone(w, "login", map[string]string{"Error": "Account disabled."})
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)); err != nil {
		a.renderStandalone(w, "login", map[string]string{"Error": "Invalid credentials."})
		return
	}
	sid := a.sessions.create(username)
	// Generate a fresh CSRF token for the user and store it in the session.
	csrfToken, err := a.state.RefreshCSRFToken(username)
	if err == nil && csrfToken != "" {
		a.sessions.setCSRF(sid, csrfToken)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    sid,
		Path:     "/portal",
		HttpOnly: true,
		Secure:   r.TLS != nil, // only Secure over HTTPS; allow over HTTP for local dev
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
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
	if err := a.state.AddUser(User{
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
