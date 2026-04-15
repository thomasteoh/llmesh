package admin

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// hubInterface is satisfied by *hub.Hub. Defined here to avoid import cycle.
// handler.go will use the concrete *hub.Hub type once it defines the real Admin struct.
type hubInterface interface {
	IsConnected(token string) bool
	LastSeenTime(token string) time.Time
	ConnectedModels(token string) []string
	ActiveClientCount() int
	CloseByToken(token string)
}

// render is a stub until handler.go defines the real Admin struct and template engine.
// This allows pages.go to compile before Task 7.
func (a *Admin) render(w http.ResponseWriter, name string, data interface{}) {
	w.WriteHeader(http.StatusOK)
}

// --- Shared page data types ---

type basePage struct {
	Page     string
	Username string
	IsAdmin  bool
	Flash    string
	Error    string
}

type DashboardPage struct {
	basePage
	TotalRequests int64
	ActiveClients int
	APIKeyCount   int
	TokenCount    int
	Clients       []ClientRow
}

type ClientRow struct {
	Name     string
	Token    string
	Status   string // "connected" | "offline" | "never_connected"
	LastSeen string
	Models   string
}

type APIKeysPage struct {
	basePage
	Keys      []APIKey
	NewKey    string
	FormError string
}

type ClientTokensPage struct {
	basePage
	Tokens    []ClientTokenRow
	NewToken  string
	FormError string
}

type ClientTokenRow struct {
	ClientToken
	Status   string
	LastSeen string
}

type SettingsPage struct {
	basePage
	Users []UserRow
}

type UserRow struct {
	User
	IsSelf bool
}

// --- Dashboard ---

func (a *Admin) handleDashboard(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	tokens := a.state.ClientTokensFor("", true)
	clients := make([]ClientRow, 0, len(tokens))
	for _, t := range tokens {
		row := ClientRow{
			Name:  t.Owner + "/" + t.Name,
			Token: t.Token,
		}
		if a.hub.IsConnected(t.Token) {
			row.Status = "connected"
			mods := a.hub.ConnectedModels(t.Token)
			sort.Strings(mods)
			row.Models = strings.Join(mods, ", ")
		} else if ls := a.hub.LastSeenTime(t.Token); !ls.IsZero() {
			row.Status = "offline"
			row.LastSeen = humanTime(ls)
		} else {
			row.Status = "never_connected"
		}
		clients = append(clients, row)
	}
	data := DashboardPage{
		basePage:      basePage{Page: "dashboard", Username: u.Username, IsAdmin: u.Role == "admin"},
		TotalRequests: a.reqCount(),
		ActiveClients: a.hub.ActiveClientCount(),
		APIKeyCount:   a.state.APIKeyCount(),
		TokenCount:    a.state.ClientTokenCount(),
		Clients:       clients,
	}
	a.render(w, "dashboard", data)
}

// --- API Keys ---

func (a *Admin) handleAPIKeys(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	a.renderAPIKeys(w, u, "", "")
}

func (a *Admin) renderAPIKeys(w http.ResponseWriter, u User, newKey, formErr string) {
	keys := a.state.APIKeysFor(u.Username, u.Role == "admin")
	a.render(w, "api-keys", APIKeysPage{
		basePage:  basePage{Page: "api-keys", Username: u.Username, IsAdmin: u.Role == "admin"},
		Keys:      keys,
		NewKey:    newKey,
		FormError: formErr,
	})
}

func (a *Admin) handleAPIKeyCreate(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	r.ParseForm()
	label := strings.TrimSpace(r.FormValue("label"))
	priority := r.FormValue("priority")
	if priority == "" {
		priority = "normal"
	}
	if label == "" {
		a.renderAPIKeys(w, u, "", "Label is required.")
		return
	}
	keyVal, err := GenAPIKeyValue(u.Username)
	if err != nil {
		a.renderAPIKeys(w, u, "", "Failed to generate key.")
		return
	}
	k := APIKey{
		Label:     label,
		Owner:     u.Username,
		Key:       keyVal,
		Priority:  priority,
		CreatedAt: time.Now().UTC(),
	}
	if err := a.state.AddAPIKey(k); err != nil {
		a.renderAPIKeys(w, u, "", err.Error())
		return
	}
	a.renderAPIKeys(w, u, keyVal, "")
}

func (a *Admin) handleAPIKeyRevoke(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	r.ParseForm()
	key := r.FormValue("key")
	a.state.RevokeAPIKey(u.Username, key, u.Role == "admin")
	http.Redirect(w, r, "/admin/api-keys", http.StatusFound)
}

// --- Client Tokens ---

func (a *Admin) handleClientTokens(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	a.renderClientTokens(w, u, "", "")
}

func (a *Admin) renderClientTokens(w http.ResponseWriter, u User, newToken, formErr string) {
	rawTokens := a.state.ClientTokensFor(u.Username, u.Role == "admin")
	rows := make([]ClientTokenRow, 0, len(rawTokens))
	for _, t := range rawTokens {
		row := ClientTokenRow{ClientToken: t}
		if a.hub.IsConnected(t.Token) {
			row.Status = "connected"
		} else if ls := a.hub.LastSeenTime(t.Token); !ls.IsZero() {
			row.Status = "offline"
			row.LastSeen = humanTime(ls)
		} else {
			row.Status = "never_connected"
		}
		rows = append(rows, row)
	}
	a.render(w, "client-tokens", ClientTokensPage{
		basePage:  basePage{Page: "client-tokens", Username: u.Username, IsAdmin: u.Role == "admin"},
		Tokens:    rows,
		NewToken:  newToken,
		FormError: formErr,
	})
}

func (a *Admin) handleClientTokenCreate(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	r.ParseForm()
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		a.renderClientTokens(w, u, "", "Name is required.")
		return
	}
	tokVal, err := GenClientTokenValue(u.Username)
	if err != nil {
		a.renderClientTokens(w, u, "", "Failed to generate token.")
		return
	}
	t := ClientToken{
		Name:      name,
		Owner:     u.Username,
		Token:     tokVal,
		CreatedAt: time.Now().UTC(),
	}
	if err := a.state.AddClientToken(t); err != nil {
		a.renderClientTokens(w, u, "", err.Error())
		return
	}
	a.renderClientTokens(w, u, tokVal, "")
}

func (a *Admin) handleClientTokenRevoke(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	r.ParseForm()
	token := r.FormValue("token")
	a.state.RevokeClientToken(u.Username, token, u.Role == "admin")
	a.hub.CloseByToken(token)
	http.Redirect(w, r, "/admin/client-tokens", http.StatusFound)
}

// --- Docs ---

func (a *Admin) handleDocs(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	a.render(w, "docs", basePage{Page: "docs", Username: u.Username, IsAdmin: u.Role == "admin"})
}

// --- Settings ---

func (a *Admin) handleSettings(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	a.renderSettings(w, u, "", "")
}

func (a *Admin) renderSettings(w http.ResponseWriter, u User, flash, errMsg string) {
	users := a.state.Users()
	rows := make([]UserRow, 0, len(users))
	for _, usr := range users {
		rows = append(rows, UserRow{User: usr, IsSelf: usr.Username == u.Username})
	}
	a.render(w, "settings", SettingsPage{
		basePage: basePage{Page: "settings", Username: u.Username, IsAdmin: u.Role == "admin", Flash: flash, Error: errMsg},
		Users:    rows,
	})
}

func (a *Admin) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	r.ParseForm()
	current := r.FormValue("current")
	newPw := r.FormValue("new")
	confirm := r.FormValue("confirm")
	if newPw != confirm {
		a.renderSettings(w, u, "", "New passwords do not match.")
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(current)); err != nil {
		a.renderSettings(w, u, "", "Current password is incorrect.")
		return
	}
	hash, err := HashPassword(newPw)
	if err != nil {
		a.renderSettings(w, u, "", "Internal error.")
		return
	}
	a.state.UpdateUser(u.Username, func(user *User) { user.PasswordHash = hash })
	a.renderSettings(w, u, "Password updated.", "")
}

func (a *Admin) handleAddUser(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	r.ParseForm()
	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")
	if username == "" || password == "" {
		a.renderSettings(w, u, "", "Username and password are required.")
		return
	}
	if _, exists := a.state.LookupUser(username); exists {
		a.renderSettings(w, u, "", fmt.Sprintf("Username %q already exists.", username))
		return
	}
	hash, err := HashPassword(password)
	if err != nil {
		a.renderSettings(w, u, "", "Internal error.")
		return
	}
	a.state.AddUser(User{Username: username, PasswordHash: hash, Role: "member"})
	a.renderSettings(w, u, fmt.Sprintf("User %q created.", username), "")
}

func (a *Admin) handleUserDisable(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	r.ParseForm()
	target := r.FormValue("username")
	if target == u.Username {
		a.renderSettings(w, u, "", "Cannot disable yourself.")
		return
	}
	a.state.UpdateUser(target, func(user *User) { user.Disabled = true })
	http.Redirect(w, r, "/admin/settings", http.StatusFound)
}

func (a *Admin) handleUserEnable(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	target := r.FormValue("username")
	a.state.UpdateUser(target, func(user *User) { user.Disabled = false })
	http.Redirect(w, r, "/admin/settings", http.StatusFound)
}

func (a *Admin) handleUserPromote(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	target := r.FormValue("username")
	a.state.UpdateUser(target, func(user *User) { user.Role = "admin" })
	http.Redirect(w, r, "/admin/settings", http.StatusFound)
}

func (a *Admin) handleUserDemote(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	r.ParseForm()
	target := r.FormValue("username")
	if target == u.Username {
		a.renderSettings(w, u, "", "Cannot demote yourself.")
		return
	}
	if a.state.ActiveAdminCount() <= 1 {
		a.renderSettings(w, u, "", "Cannot demote: at least one active admin must remain.")
		return
	}
	a.state.UpdateUser(target, func(user *User) { user.Role = "member" })
	http.Redirect(w, r, "/admin/settings", http.StatusFound)
}

// humanTime formats a time as a human-readable relative string.
func humanTime(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return t.Format("2006-01-02")
	}
}
