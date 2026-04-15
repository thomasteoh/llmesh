package admin

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"llmesh/pkg/types"
)

type User struct {
	Username     string `json:"username"`
	PasswordHash string `json:"password_hash"`
	Role         string `json:"role"`     // "admin" | "member"
	Disabled     bool   `json:"disabled"`
}

type APIKey struct {
	Label     string    `json:"label"`
	Owner     string    `json:"owner"`
	Key       string    `json:"key"`
	Priority  string    `json:"priority"` // "high" | "normal" | "low"
	CreatedAt time.Time `json:"created_at"`
}

type ClientToken struct {
	Name      string    `json:"name"`
	Owner     string    `json:"owner"`
	Token     string    `json:"token"`
	CreatedAt time.Time `json:"created_at"`
}

type stateData struct {
	Users        []User        `json:"users"`
	APIKeys      []APIKey      `json:"api_keys"`
	ClientTokens []ClientToken `json:"client_tokens"`
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

// save writes state to disk. Caller must hold write lock.
func (s *State) save() error {
	data, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0600)
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

// --- Client Tokens ---

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

func (s *State) ClientTokenCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.data.ClientTokens)
}

// --- Token generation ---

func genRandom(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// GenAPIKeyValue returns "sk-{owner}-{16 hex chars}".
func GenAPIKeyValue(owner string) (string, error) {
	r, err := genRandom(8)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("sk-%s-%s", owner, r), nil
}

// GenClientTokenValue returns "ct-{owner}-{16 hex chars}".
func GenClientTokenValue(owner string) (string, error) {
	r, err := genRandom(8)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("ct-%s-%s", owner, r), nil
}
