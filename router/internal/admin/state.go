package admin

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"llmesh/pkg/types"
)

type User struct {
	Username     string `json:"username"`
	PasswordHash string `json:"password_hash"`
	Role         string `json:"role"`     // "admin" | "member"
	Disabled     bool   `json:"disabled"`
	CSRFToken    string `json:"csrf_token,omitempty"` // SHA-256 hash of current valid CSRF token
}

type APIKey struct {
	Label         string    `json:"label"`
	Owner         string    `json:"owner"`
	Key           string    `json:"key"`
	Priority      string    `json:"priority"`       // "high" | "normal" | "low"
	MaxConcurrent int       `json:"max_concurrent,omitempty"` // 0 = unlimited
	CreatedAt     time.Time `json:"created_at"`
}

type ClientToken struct {
	Name       string         `json:"name"`
	Owner      string         `json:"owner"`
	Token      string         `json:"token"`
	CreatedAt  time.Time      `json:"created_at"`
	OwnerSlots map[string]int `json:"owner_slots,omitempty"` // model → slots reserved for owner; 0/unset = fully shared
}

// UpstreamRouter configures an upstream (orchestrator) router that this router
// connects to as a client. Models are advertised automatically from locally
// connected GPU clients; no manual model configuration is required.
type UpstreamRouter struct {
	Name  string `json:"name"`
	URL   string `json:"url"`   // base URL, e.g. "https://orchestrator.example.com"
	Token string `json:"token"` // client token issued on the upstream router
}

type stateData struct {
	Users           []User              `json:"users"`
	APIKeys         []APIKey            `json:"api_keys"`
	ClientTokens    []ClientToken       `json:"client_tokens"`
	ModelAliases    map[string][]string `json:"model_aliases,omitempty"`
	UpstreamRouters []UpstreamRouter    `json:"upstream_routers,omitempty"`
}

// UnmarshalJSON handles migration from the old map[string]string format.
func (sd *stateData) UnmarshalJSON(data []byte) error {
	var raw struct {
		Users           []User          `json:"users"`
		APIKeys         []APIKey        `json:"api_keys"`
		ClientTokens    []ClientToken   `json:"client_tokens"`
		ModelAliases    json.RawMessage `json:"model_aliases,omitempty"`
		UpstreamRouters []UpstreamRouter `json:"upstream_routers,omitempty"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	sd.Users = raw.Users
	sd.APIKeys = raw.APIKeys
	sd.ClientTokens = raw.ClientTokens
	sd.UpstreamRouters = raw.UpstreamRouters
	if len(raw.ModelAliases) > 0 {
		// Try new format first: map[string][]string
		var newFmt map[string][]string
		if err := json.Unmarshal(raw.ModelAliases, &newFmt); err == nil {
			sd.ModelAliases = newFmt
			return nil
		}
		// Fall back to old format: map[string]string → migrate
		var oldFmt map[string]string
		if err := json.Unmarshal(raw.ModelAliases, &oldFmt); err == nil {
			sd.ModelAliases = make(map[string][]string, len(oldFmt))
			for alias, model := range oldFmt {
				sd.ModelAliases[alias] = []string{model}
			}
		}
	}
	return nil
}

// State is the mutable runtime state, persisted to state.json.
type State struct {
	mu   sync.RWMutex
	path string
	data stateData
}

// LoadState loads state from path. Returns empty state if file does not exist.
func LoadState(path string) (*State, error) {
	s := &State{path: path}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state: %w", err)
	}
	if len(data) == 0 {
		return s, nil // empty file treated as no state yet
	}
	if err := json.Unmarshal(data, &s.data); err != nil {
		return nil, fmt.Errorf("parse state: %w", err)
	}
	return s, nil
}

// save writes state to disk atomically. Caller must hold write lock.
func (s *State) save() error {
	data, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *State) NeedsSetup() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.data.Users) == 0
}

// --- Users ---

func (s *State) LookupUser(username string) (User, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, u := range s.data.Users {
		if u.Username == username {
			return u, true
		}
	}
	return User{}, false
}

func (s *State) AddUser(u User) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.data.Users {
		if existing.Username == u.Username {
			return fmt.Errorf("username %q already exists", u.Username)
		}
	}
	s.data.Users = append(s.data.Users, u)
	return s.save()
}

// UpdateUser applies fn to the named user and saves. Returns error if not found.
func (s *State) UpdateUser(username string, fn func(*User)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.Users {
		if s.data.Users[i].Username == username {
			fn(&s.data.Users[i])
			return s.save()
		}
	}
	return fmt.Errorf("user not found: %s", username)
}

func (s *State) Users() []User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]User, len(s.data.Users))
	copy(out, s.data.Users)
	return out
}

func (s *State) ActiveAdminCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, u := range s.data.Users {
		if u.Role == "admin" && !u.Disabled {
			n++
		}
	}
	return n
}

// DemoteUser demotes the named user to member, returning an error if the operation
// would violate the at-least-one-active-admin invariant or if the user is the actor.
func (s *State) DemoteUser(actor, target string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if actor == target {
		return fmt.Errorf("cannot demote yourself")
	}
	// Count active admins before applying the change.
	count := 0
	for _, u := range s.data.Users {
		if u.Role == "admin" && !u.Disabled {
			count++
		}
	}
	if count <= 1 {
		return fmt.Errorf("cannot demote: at least one active admin must remain")
	}
	for i := range s.data.Users {
		if s.data.Users[i].Username == target {
			s.data.Users[i].Role = "member"
			break
		}
	}
	return s.save()
}

// --- API Keys ---

func (s *State) LookupAPIKey(key string) (APIKey, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, k := range s.data.APIKeys {
		if k.Key == key {
			return k, true
		}
	}
	return APIKey{}, false
}

// APIKeysFor returns keys visible to owner. Admins (isAdmin=true) see all keys.
func (s *State) APIKeysFor(owner string, isAdmin bool) []APIKey {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []APIKey
	for _, k := range s.data.APIKeys {
		if isAdmin || k.Owner == owner {
			out = append(out, k)
		}
	}
	return out
}

func (s *State) AddAPIKey(k APIKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.data.APIKeys {
		if existing.Owner == k.Owner && existing.Label == k.Label {
			return fmt.Errorf("label %q already exists for this user", k.Label)
		}
	}
	s.data.APIKeys = append(s.data.APIKeys, k)
	return s.save()
}

// UpdateAPIKeyPriority changes the priority of the given key. Admin-only operation.
func (s *State) UpdateAPIKeyPriority(key, priority string) error {
	switch priority {
	case "high", "normal", "low":
	default:
		return fmt.Errorf("invalid priority %q", priority)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.APIKeys {
		if s.data.APIKeys[i].Key == key {
			s.data.APIKeys[i].Priority = priority
			return s.save()
		}
	}
	return fmt.Errorf("key not found")
}

// UpdateAPIKeyMaxConcurrent sets the MaxConcurrent limit for a key. 0 = unlimited. Admin-only.
func (s *State) UpdateAPIKeyMaxConcurrent(key string, limit int) error {
	if limit < 0 {
		return fmt.Errorf("max_concurrent must be >= 0")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.APIKeys {
		if s.data.APIKeys[i].Key == key {
			s.data.APIKeys[i].MaxConcurrent = limit
			return s.save()
		}
	}
	return fmt.Errorf("key not found")
}

// MaxConcurrentFor satisfies the api.LimitProvider interface.
// Returns the per-key max concurrent limit (0 = unlimited).
func (s *State) MaxConcurrentFor(key string) int {
	k, ok := s.LookupAPIKey(key)
	if !ok {
		return 0
	}
	return k.MaxConcurrent
}

// RevokeAPIKey removes the key. Non-admins can only revoke their own keys.
func (s *State) RevokeAPIKey(owner, key string, isAdmin bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, k := range s.data.APIKeys {
		if k.Key == key && (isAdmin || k.Owner == owner) {
			s.data.APIKeys = append(s.data.APIKeys[:i], s.data.APIKeys[i+1:]...)
			return s.save()
		}
	}
	return fmt.Errorf("key not found")
}

func (s *State) APIKeyCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.data.APIKeys)
}

// ValidAPIKey satisfies the api.APIKeyStore interface.
func (s *State) ValidAPIKey(key string) bool {
	_, ok := s.LookupAPIKey(key)
	return ok
}

// PriorityFor satisfies the api.APIKeyStore interface.
func (s *State) PriorityFor(key string) types.Priority {
	k, ok := s.LookupAPIKey(key)
	if !ok {
		return types.PriorityNormal
	}
	return types.PriorityFromString(k.Priority)
}

func (s *State) OwnerFor(key string) string {
	k, ok := s.LookupAPIKey(key)
	if !ok {
		return ""
	}
	return k.Owner
}

// LabelFor returns "owner/label" for the given API key, or "" if not found.
func (s *State) LabelFor(key string) string {
	k, ok := s.LookupAPIKey(key)
	if !ok {
		return ""
	}
	return k.Owner + "/" + k.Label
}

// --- Clients ---

func (s *State) LookupClientToken(token string) (ClientToken, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, t := range s.data.ClientTokens {
		if t.Token == token {
			return t, true
		}
	}
	return ClientToken{}, false
}

// ClientTokensFor returns tokens visible to owner. Admins see all.
func (s *State) ClientTokensFor(owner string, isAdmin bool) []ClientToken {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []ClientToken
	for _, t := range s.data.ClientTokens {
		if isAdmin || t.Owner == owner {
			out = append(out, t)
		}
	}
	return out
}

func (s *State) AddClientToken(t ClientToken) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.data.ClientTokens {
		if existing.Owner == t.Owner && existing.Name == t.Name {
			return fmt.Errorf("name %q already exists for this user", t.Name)
		}
	}
	s.data.ClientTokens = append(s.data.ClientTokens, t)
	return s.save()
}

// RevokeClientToken removes the token. Non-admins can only revoke their own tokens.
func (s *State) RevokeClientToken(owner, token string, isAdmin bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, t := range s.data.ClientTokens {
		if t.Token == token && (isAdmin || t.Owner == owner) {
			s.data.ClientTokens = append(s.data.ClientTokens[:i], s.data.ClientTokens[i+1:]...)
			return s.save()
		}
	}
	return fmt.Errorf("token not found")
}

// SetClientTokenOwnerSlots sets the owner_slots value for a specific model on the given token.
// Non-admins may only update their own tokens.
// slots <= 0 removes the model key (restores full sharing for that model).
func (s *State) SetClientTokenOwnerSlots(owner, token, model string, slots int, isAdmin bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, t := range s.data.ClientTokens {
		if t.Token == token && (isAdmin || t.Owner == owner) {
			if slots <= 0 {
				delete(s.data.ClientTokens[i].OwnerSlots, model)
			} else {
				if s.data.ClientTokens[i].OwnerSlots == nil {
					s.data.ClientTokens[i].OwnerSlots = make(map[string]int)
				}
				s.data.ClientTokens[i].OwnerSlots[model] = slots
			}
			return s.save()
		}
	}
	return fmt.Errorf("token not found")
}

func (s *State) ClientTokenCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.data.ClientTokens)
}

// --- Upstream Routers ---

// GetUpstreamRouters returns a copy of the configured upstream routers.
func (s *State) GetUpstreamRouters() []UpstreamRouter {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]UpstreamRouter, len(s.data.UpstreamRouters))
	copy(out, s.data.UpstreamRouters)
	return out
}

// AddUpstreamRouter adds an upstream router configuration and saves state.
func (s *State) AddUpstreamRouter(r UpstreamRouter) error {
	if r.URL == "" || r.Token == "" {
		return fmt.Errorf("url and token are required")
	}
	parsed, err := url.ParseRequestURI(r.URL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("url must start with http:// or https://")
	}
	// Normalise: lowercase scheme+host, strip trailing slash.
	r.URL = strings.ToLower(strings.TrimRight(r.URL, "/"))
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.data.UpstreamRouters {
		if existing.URL == r.URL {
			return fmt.Errorf("upstream %q is already configured", r.URL)
		}
	}
	s.data.UpstreamRouters = append(s.data.UpstreamRouters, r)
	return s.save()
}

// RemoveUpstreamRouter removes the upstream router with the given URL and saves state.
func (s *State) RemoveUpstreamRouter(url string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, r := range s.data.UpstreamRouters {
		if r.URL == url {
			s.data.UpstreamRouters = append(s.data.UpstreamRouters[:i], s.data.UpstreamRouters[i+1:]...)
			return s.save()
		}
	}
	return fmt.Errorf("upstream %q not found", url)
}

// --- Token generation ---

func genRandom(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// GenAPIKeyValue returns "sk-{owner}-{32 hex chars}" (128 bits of entropy).
func GenAPIKeyValue(owner string) (string, error) {
	r, err := genRandom(16)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("sk-%s-%s", owner, r), nil
}

// GenClientTokenValue returns "ct-{owner}-{32 hex chars}" (128 bits of entropy).
func GenClientTokenValue(owner string) (string, error) {
	r, err := genRandom(16)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("ct-%s-%s", owner, r), nil
}

// --- CSRF Token helpers ---

func generateCSRFToken() (string, error) {
	return genRandom(32)
}

func hashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// ValidateCSRF checks the submitted token against the stored hash.
func (u User) ValidateCSRF(token string) bool {
	if u.CSRFToken == "" || token == "" {
		return false
	}
	h := hashToken(token)
	return subtle.ConstantTimeCompare([]byte(u.CSRFToken), []byte(h)) == 1
}

// ConsumeCSRF validates a CSRF token for the given user and atomically
// invalidates it (one-time use). Returns true if valid, false otherwise.
func (s *State) ConsumeCSRF(username, token string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.Users {
		if s.data.Users[i].Username == username {
			u := &s.data.Users[i]
			if u.CSRFToken == "" || token == "" {
				return false
			}
			h := hashToken(token)
			if subtle.ConstantTimeCompare([]byte(u.CSRFToken), []byte(h)) != 1 {
				return false
			}
			// Consume: invalidate the token so it cannot be reused.
			u.CSRFToken = ""
			if err := s.save(); err != nil {
				return false
			}
			return true
		}
	}
	return false
}

// RefreshCSRFToken generates a new CSRF token for the named user and persists it.
// Returns the plaintext token (to embed in forms).
func (s *State) RefreshCSRFToken(username string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.Users {
		if s.data.Users[i].Username == username {
			token, err := generateCSRFToken()
			if err != nil {
				return "", err
			}
			s.data.Users[i].CSRFToken = hashToken(token)
			if err := s.save(); err != nil {
				return "", err
			}
			return token, nil
		}
	}
	return "", fmt.Errorf("user not found: %s", username)
}

// --- Model Aliases ---

// AliasMap returns a copy of the current alias→[]models map.
func (s *State) AliasMap() map[string][]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string][]string, len(s.data.ModelAliases))
	for k, v := range s.data.ModelAliases {
		cp := make([]string, len(v))
		copy(cp, v)
		out[k] = cp
	}
	return out
}

// AddAlias appends model to the target list for alias.
// Returns an error if alias or model is blank, or if the pair already exists.
func (s *State) AddAlias(alias, model string) error {
	if alias == "" || model == "" {
		return fmt.Errorf("alias and model must not be blank")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.ModelAliases == nil {
		s.data.ModelAliases = make(map[string][]string)
	}
	for _, existing := range s.data.ModelAliases[alias] {
		if existing == model {
			return fmt.Errorf("alias %q → %q already exists", alias, model)
		}
	}
	s.data.ModelAliases[alias] = append(s.data.ModelAliases[alias], model)
	return s.save()
}

// DeleteAlias removes a specific model from the alias's target list.
// If the list becomes empty, the alias key is removed entirely.
func (s *State) DeleteAlias(alias, model string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	targets, exists := s.data.ModelAliases[alias]
	if !exists {
		return fmt.Errorf("alias %q not found", alias)
	}
	newTargets := targets[:0]
	found := false
	for _, t := range targets {
		if t == model {
			found = true
			continue
		}
		newTargets = append(newTargets, t)
	}
	if !found {
		return fmt.Errorf("alias %q → %q not found", alias, model)
	}
	if len(newTargets) == 0 {
		delete(s.data.ModelAliases, alias)
	} else {
		s.data.ModelAliases[alias] = newTargets
	}
	return s.save()
}

// DeleteAliasGroup removes an entire alias entry regardless of how many targets it has.
func (s *State) DeleteAliasGroup(alias string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.data.ModelAliases[alias]; !exists {
		return fmt.Errorf("alias %q not found", alias)
	}
	delete(s.data.ModelAliases, alias)
	return s.save()
}
