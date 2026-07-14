package admin

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"llmesh/pkg/types"
	_ "modernc.org/sqlite"
)

type User struct {
	Username     string `json:"username"`
	PasswordHash string `json:"password_hash"`
	Role         string `json:"role"` // "admin" | "member"
	Disabled     bool   `json:"disabled"`
	CSRFToken    string `json:"csrf_token,omitempty"` // SHA-256 hash of current valid CSRF token
}

// APIKey is a stored API key. The key material itself is never persisted:
// KeyHash is the SHA-256 of the key (the lookup identifier) and KeyPrefix is
// a short display prefix shown in the portal. The full key is only available
// at creation time.
type APIKey struct {
	Label         string
	Owner         string
	KeyHash       string // SHA-256 hex of the key
	KeyPrefix     string // display prefix, e.g. "sk-alice-1a2b…"
	Priority      string // "high" | "normal" | "low"
	MaxConcurrent int    // 0 = unlimited
	CreatedAt     time.Time
}

// ClientToken is a stored worker token, hashed at rest like APIKey.
type ClientToken struct {
	Name        string
	Owner       string
	TokenHash   string // SHA-256 hex of the token
	TokenPrefix string // display prefix, e.g. "ct-alice-1a2b…"
	CreatedAt   time.Time
	OwnerSlots  map[string]int // model → slots reserved for owner; 0/unset = fully shared
}

// HashSecret returns the hex SHA-256 of an API key or client token, the form
// in which secrets are stored and looked up. The inputs are 128-bit random
// values, so a fast unsalted hash is appropriate (unlike passwords).
func HashSecret(s string) string { return hashToken(s) }

// SecretPrefix returns a short display prefix for a generated secret. Keys and
// tokens end in 32 random hex chars; keep the type/owner part plus 4 chars so
// entries remain recognisable without exposing usable key material.
func SecretPrefix(s string) string {
	if len(s) > 32 {
		return s[:len(s)-28] + "…"
	}
	if len(s) > 8 {
		return s[:8] + "…"
	}
	return "****"
}

// UpstreamRouter configures an upstream (orchestrator) router that this router
// connects to as a client.
type UpstreamRouter struct {
	Name     string `json:"name"`
	URL      string `json:"url"`      // base URL, e.g. "https://orchestrator.example.com"
	Token    string `json:"token"`    // client token issued on the upstream router
	Priority string `json:"priority"` // "high" | "normal" | "low"; default "normal"
}

// legacyAPIKey / legacyClientToken mirror the plaintext shapes found in old
// state.json files; used only for first-startup import.
type legacyAPIKey struct {
	Label         string    `json:"label"`
	Owner         string    `json:"owner"`
	Key           string    `json:"key"`
	Priority      string    `json:"priority"`
	MaxConcurrent int       `json:"max_concurrent,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

type legacyClientToken struct {
	Name       string         `json:"name"`
	Owner      string         `json:"owner"`
	Token      string         `json:"token"`
	CreatedAt  time.Time      `json:"created_at"`
	OwnerSlots map[string]int `json:"owner_slots,omitempty"`
}

// stateData is kept only for migrating legacy state.json files on first startup.
type stateData struct {
	Users           []User              `json:"users"`
	APIKeys         []legacyAPIKey      `json:"api_keys"`
	ClientTokens    []legacyClientToken `json:"client_tokens"`
	ModelAliases    map[string][]string `json:"model_aliases,omitempty"`
	UpstreamRouters []UpstreamRouter    `json:"upstream_routers,omitempty"`
}

// UnmarshalJSON handles migration from the old map[string]string format.
func (sd *stateData) UnmarshalJSON(data []byte) error {
	var raw struct {
		Users           []User              `json:"users"`
		APIKeys         []legacyAPIKey      `json:"api_keys"`
		ClientTokens    []legacyClientToken `json:"client_tokens"`
		ModelAliases    json.RawMessage     `json:"model_aliases,omitempty"`
		UpstreamRouters []UpstreamRouter    `json:"upstream_routers,omitempty"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	sd.Users = raw.Users
	sd.APIKeys = raw.APIKeys
	sd.ClientTokens = raw.ClientTokens
	sd.UpstreamRouters = raw.UpstreamRouters
	if len(raw.ModelAliases) > 0 {
		var newFmt map[string][]string
		if err := json.Unmarshal(raw.ModelAliases, &newFmt); err == nil {
			sd.ModelAliases = newFmt
			return nil
		}
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

// State is the mutable runtime state, persisted to a SQLite database.
type State struct {
	db *sql.DB

	// aliasCache holds an immutable snapshot of the alias→models map. It is read
	// on every inference request and dispatch cycle, so we cache it in memory and
	// invalidate (store nil) on any alias mutation rather than hitting SQLite each call.
	aliasCache atomic.Pointer[map[string][]string]

	// optCache holds the request-optimization toggles. Read on the per-request
	// hot path and per scheduler drain, so it is cached and invalidated (store
	// nil) on any settings mutation rather than queried each call.
	optCache atomic.Pointer[types.RequestOptimization]
}

// dbPath converts a .json path to a .db path so that tests using .json paths
// transparently get a SQLite file instead.
func dbPath(path string) string {
	if strings.HasSuffix(path, ".json") {
		return strings.TrimSuffix(path, ".json") + ".db"
	}
	return path
}

// LoadState opens (or creates) the SQLite database at path.
// If path ends in .json it is converted to .db.
// On first open, if a legacy state.json exists at the original path, its data is imported.
func LoadState(path string) (*State, error) {
	dbfile := dbPath(path)
	db, err := sql.Open("sqlite", dbfile)
	if err != nil {
		return nil, fmt.Errorf("open state db: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set WAL mode: %w", err)
	}
	if err := createSchema(db); err != nil {
		db.Close()
		return nil, err
	}
	if err := migrateSecretColumns(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate secrets to hashed storage: %w", err)
	}
	s := &State{db: db}
	if err := s.maybeMigrateJSON(path, dbfile); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func createSchema(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS users (
			username      TEXT PRIMARY KEY,
			password_hash TEXT NOT NULL DEFAULT '',
			role          TEXT NOT NULL DEFAULT 'member',
			disabled      INTEGER NOT NULL DEFAULT 0,
			csrf_token    TEXT NOT NULL DEFAULT ''
		);
		CREATE TABLE IF NOT EXISTS api_keys (
			key_hash       TEXT PRIMARY KEY,
			key_prefix     TEXT NOT NULL DEFAULT '',
			label          TEXT NOT NULL DEFAULT '',
			owner          TEXT NOT NULL DEFAULT '',
			priority       TEXT NOT NULL DEFAULT 'normal',
			max_concurrent INTEGER NOT NULL DEFAULT 0,
			created_at     TEXT NOT NULL DEFAULT '',
			UNIQUE(owner, label)
		);
		CREATE TABLE IF NOT EXISTS client_tokens (
			token_hash   TEXT PRIMARY KEY,
			token_prefix TEXT NOT NULL DEFAULT '',
			name         TEXT NOT NULL DEFAULT '',
			owner        TEXT NOT NULL DEFAULT '',
			created_at   TEXT NOT NULL DEFAULT '',
			owner_slots  TEXT NOT NULL DEFAULT '{}',
			UNIQUE(owner, name)
		);
		CREATE TABLE IF NOT EXISTS model_aliases (
			alias TEXT NOT NULL,
			model TEXT NOT NULL,
			PRIMARY KEY (alias, model)
		);
		CREATE TABLE IF NOT EXISTS upstream_routers (
			url      TEXT PRIMARY KEY,
			name     TEXT NOT NULL DEFAULT '',
			token    TEXT NOT NULL DEFAULT '',
			priority TEXT NOT NULL DEFAULT 'normal'
		);
		CREATE TABLE IF NOT EXISTS settings (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL DEFAULT ''
		);
		CREATE TABLE IF NOT EXISTS audit_log (
			id        INTEGER PRIMARY KEY AUTOINCREMENT,
			at        TEXT NOT NULL,
			actor     TEXT NOT NULL DEFAULT '',
			action    TEXT NOT NULL DEFAULT '',
			target    TEXT NOT NULL DEFAULT '',
			ip        TEXT NOT NULL DEFAULT ''
		);
	`)
	if err != nil {
		return err
	}
	// Non-destructive migration: add priority column for existing databases.
	// SQLite returns an error if the column already exists; ignore it.
	_, _ = db.Exec(`ALTER TABLE upstream_routers ADD COLUMN priority TEXT NOT NULL DEFAULT 'normal'`)
	return nil
}

// tableHasColumn reports whether the named table has the named column.
func tableHasColumn(db *sql.DB, table, column string) bool {
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if rows.Scan(&name) == nil && name == column {
			return true
		}
	}
	return false
}

// migrateSecretColumns rewrites databases that still store plaintext API keys
// or client tokens (schema versions before hashing-at-rest) into the hashed
// shape. Each secret is replaced by its SHA-256 plus a display prefix; the
// plaintext is not retained anywhere.
func migrateSecretColumns(db *sql.DB) error {
	if tableHasColumn(db, "api_keys", "key") {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		rows, err := tx.Query(`SELECT key, label, owner, priority, max_concurrent, created_at FROM api_keys`)
		if err != nil {
			return err
		}
		type oldKey struct {
			key, label, owner, priority, createdAt string
			maxConcurrent                          int
		}
		var olds []oldKey
		for rows.Next() {
			var o oldKey
			if err := rows.Scan(&o.key, &o.label, &o.owner, &o.priority, &o.maxConcurrent, &o.createdAt); err != nil {
				rows.Close()
				return err
			}
			olds = append(olds, o)
		}
		rows.Close()
		if _, err := tx.Exec(`
			DROP TABLE api_keys;
			CREATE TABLE api_keys (
				key_hash       TEXT PRIMARY KEY,
				key_prefix     TEXT NOT NULL DEFAULT '',
				label          TEXT NOT NULL DEFAULT '',
				owner          TEXT NOT NULL DEFAULT '',
				priority       TEXT NOT NULL DEFAULT 'normal',
				max_concurrent INTEGER NOT NULL DEFAULT 0,
				created_at     TEXT NOT NULL DEFAULT '',
				UNIQUE(owner, label)
			);
		`); err != nil {
			return err
		}
		for _, o := range olds {
			if _, err := tx.Exec(
				`INSERT INTO api_keys (key_hash, key_prefix, label, owner, priority, max_concurrent, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
				HashSecret(o.key), SecretPrefix(o.key), o.label, o.owner, o.priority, o.maxConcurrent, o.createdAt,
			); err != nil {
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	if tableHasColumn(db, "client_tokens", "token") {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		rows, err := tx.Query(`SELECT token, name, owner, created_at, owner_slots FROM client_tokens`)
		if err != nil {
			return err
		}
		type oldTok struct {
			token, name, owner, createdAt, ownerSlots string
		}
		var olds []oldTok
		for rows.Next() {
			var o oldTok
			if err := rows.Scan(&o.token, &o.name, &o.owner, &o.createdAt, &o.ownerSlots); err != nil {
				rows.Close()
				return err
			}
			olds = append(olds, o)
		}
		rows.Close()
		if _, err := tx.Exec(`
			DROP TABLE client_tokens;
			CREATE TABLE client_tokens (
				token_hash   TEXT PRIMARY KEY,
				token_prefix TEXT NOT NULL DEFAULT '',
				name         TEXT NOT NULL DEFAULT '',
				owner        TEXT NOT NULL DEFAULT '',
				created_at   TEXT NOT NULL DEFAULT '',
				owner_slots  TEXT NOT NULL DEFAULT '{}',
				UNIQUE(owner, name)
			);
		`); err != nil {
			return err
		}
		for _, o := range olds {
			if _, err := tx.Exec(
				`INSERT INTO client_tokens (token_hash, token_prefix, name, owner, created_at, owner_slots) VALUES (?, ?, ?, ?, ?, ?)`,
				HashSecret(o.token), SecretPrefix(o.token), o.name, o.owner, o.createdAt, o.ownerSlots,
			); err != nil {
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// AuditEntry is a single entry in the persistent audit log.
type AuditEntry struct {
	ID     int64     `json:"id"`
	At     time.Time `json:"at"`
	Actor  string    `json:"actor"`
	Action string    `json:"action"`
	Target string    `json:"target"`
	IP     string    `json:"ip"`
}

// RecordAudit appends an entry to the audit log. Errors are silently dropped
// since audit logging must never break the action that triggered it.
func (s *State) RecordAudit(actor, action, target, ip string) {
	_, _ = s.db.Exec(
		`INSERT INTO audit_log (at, actor, action, target, ip) VALUES (?, ?, ?, ?, ?)`,
		time.Now().UTC().Format(time.RFC3339), actor, action, target, ip,
	)
}

// GetAuditLog returns up to limit entries from the audit log, newest first.
func (s *State) GetAuditLog(limit int) ([]AuditEntry, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := s.db.Query(
		`SELECT id, at, actor, action, target, ip FROM audit_log ORDER BY id DESC LIMIT ?`, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var atStr string
		if err := rows.Scan(&e.ID, &atStr, &e.Actor, &e.Action, &e.Target, &e.IP); err != nil {
			continue
		}
		e.At, _ = time.Parse(time.RFC3339, atStr)
		out = append(out, e)
	}
	return out, rows.Err()
}

// maybeMigrateJSON imports data from a legacy state.json if the DB has no users yet.
func (s *State) maybeMigrateJSON(jsonPath, dbfile string) error {
	// When called with a .db path directly, look for a peer .json file to migrate from.
	candidateJSON := jsonPath
	if jsonPath == dbfile {
		candidateJSON = strings.TrimSuffix(dbfile, ".db") + ".json"
	}
	var count int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM users").Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	data, err := os.ReadFile(candidateJSON)
	if errors.Is(err, os.ErrNotExist) || len(data) == 0 {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read legacy state: %w", err)
	}
	var sd stateData
	if err := json.Unmarshal(data, &sd); err != nil {
		return fmt.Errorf("parse legacy state: %w", err)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, u := range sd.Users {
		disabled := boolInt(u.Disabled)
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO users (username, password_hash, role, disabled, csrf_token) VALUES (?, ?, ?, ?, ?)`,
			u.Username, u.PasswordHash, u.Role, disabled, u.CSRFToken,
		); err != nil {
			return err
		}
	}
	for _, k := range sd.APIKeys {
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO api_keys (key_hash, key_prefix, label, owner, priority, max_concurrent, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			HashSecret(k.Key), SecretPrefix(k.Key), k.Label, k.Owner, k.Priority, k.MaxConcurrent, k.CreatedAt.Format(time.RFC3339),
		); err != nil {
			return err
		}
	}
	for _, t := range sd.ClientTokens {
		slots := marshalOwnerSlots(t.OwnerSlots)
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO client_tokens (token_hash, token_prefix, name, owner, created_at, owner_slots) VALUES (?, ?, ?, ?, ?, ?)`,
			HashSecret(t.Token), SecretPrefix(t.Token), t.Name, t.Owner, t.CreatedAt.Format(time.RFC3339), slots,
		); err != nil {
			return err
		}
	}
	for alias, models := range sd.ModelAliases {
		for _, model := range models {
			if _, err := tx.Exec(
				`INSERT OR IGNORE INTO model_aliases (alias, model) VALUES (?, ?)`,
				alias, model,
			); err != nil {
				return err
			}
		}
	}
	for _, r := range sd.UpstreamRouters {
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO upstream_routers (url, name, token) VALUES (?, ?, ?)`,
			r.URL, r.Name, r.Token,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func marshalOwnerSlots(m map[string]int) string {
	if len(m) == 0 {
		return "{}"
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// --- Setup ---

func (s *State) NeedsSetup() bool {
	var count int
	s.db.QueryRow("SELECT COUNT(*) FROM users").Scan(&count)
	return count == 0
}

// --- Users ---

func (s *State) LookupUser(username string) (User, bool) {
	var u User
	var disabled int
	err := s.db.QueryRow(
		`SELECT username, password_hash, role, disabled, csrf_token FROM users WHERE username = ?`,
		username,
	).Scan(&u.Username, &u.PasswordHash, &u.Role, &disabled, &u.CSRFToken)
	if err != nil {
		return User{}, false
	}
	u.Disabled = disabled != 0
	return u, true
}

func (s *State) AddUser(u User) error {
	_, err := s.db.Exec(
		`INSERT INTO users (username, password_hash, role, disabled, csrf_token) VALUES (?, ?, ?, ?, ?)`,
		u.Username, u.PasswordHash, u.Role, boolInt(u.Disabled), u.CSRFToken,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return fmt.Errorf("username %q already exists", u.Username)
		}
		return err
	}
	return nil
}

// AddFirstAdmin atomically creates the initial admin account, but only if no
// users exist yet. This closes the first-run race where two concurrent setup
// requests both pass NeedsSetup() and each create an admin.
func (s *State) AddFirstAdmin(u User) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var count int
	if err := tx.QueryRow("SELECT COUNT(*) FROM users").Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return fmt.Errorf("setup has already been completed")
	}
	if _, err := tx.Exec(
		`INSERT INTO users (username, password_hash, role, disabled, csrf_token) VALUES (?, ?, ?, ?, ?)`,
		u.Username, u.PasswordHash, u.Role, boolInt(u.Disabled), u.CSRFToken,
	); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *State) UpdateUser(username string, fn func(*User)) error {
	u, ok := s.LookupUser(username)
	if !ok {
		return fmt.Errorf("user not found: %s", username)
	}
	fn(&u)
	_, err := s.db.Exec(
		`UPDATE users SET password_hash = ?, role = ?, disabled = ?, csrf_token = ? WHERE username = ?`,
		u.PasswordHash, u.Role, boolInt(u.Disabled), u.CSRFToken, username,
	)
	return err
}

func (s *State) Users() []User {
	rows, err := s.db.Query(`SELECT username, password_hash, role, disabled, csrf_token FROM users ORDER BY username`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		var disabled int
		if err := rows.Scan(&u.Username, &u.PasswordHash, &u.Role, &disabled, &u.CSRFToken); err == nil {
			u.Disabled = disabled != 0
			out = append(out, u)
		}
	}
	return out
}

func (s *State) ActiveAdminCount() int {
	var count int
	s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE role = 'admin' AND disabled = 0`).Scan(&count)
	return count
}

func (s *State) DemoteUser(actor, target string) error {
	if actor == target {
		return fmt.Errorf("cannot demote yourself")
	}
	if s.ActiveAdminCount() <= 1 {
		return fmt.Errorf("cannot demote: at least one active admin must remain")
	}
	res, err := s.db.Exec(`UPDATE users SET role = 'member' WHERE username = ?`, target)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("user not found: %s", target)
	}
	return nil
}

// --- API Keys ---

// LookupAPIKey finds a key record by the plaintext key presented by a caller.
func (s *State) LookupAPIKey(key string) (APIKey, bool) {
	return s.LookupAPIKeyByHash(HashSecret(key))
}

// LookupAPIKeyByHash finds a key record by its stored hash — the identifier
// the portal uses in forms, since the plaintext is never available again.
func (s *State) LookupAPIKeyByHash(hash string) (APIKey, bool) {
	var k APIKey
	var createdAt string
	err := s.db.QueryRow(
		`SELECT key_hash, key_prefix, label, owner, priority, max_concurrent, created_at FROM api_keys WHERE key_hash = ?`,
		hash,
	).Scan(&k.KeyHash, &k.KeyPrefix, &k.Label, &k.Owner, &k.Priority, &k.MaxConcurrent, &createdAt)
	if err != nil {
		return APIKey{}, false
	}
	k.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	return k, true
}

func (s *State) APIKeysFor(owner string, isAdmin bool) []APIKey {
	var (
		rows *sql.Rows
		err  error
	)
	if isAdmin {
		rows, err = s.db.Query(`SELECT key_hash, key_prefix, label, owner, priority, max_concurrent, created_at FROM api_keys ORDER BY owner, label`)
	} else {
		rows, err = s.db.Query(`SELECT key_hash, key_prefix, label, owner, priority, max_concurrent, created_at FROM api_keys WHERE owner = ? ORDER BY label`, owner)
	}
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []APIKey
	for rows.Next() {
		var k APIKey
		var createdAt string
		if err := rows.Scan(&k.KeyHash, &k.KeyPrefix, &k.Label, &k.Owner, &k.Priority, &k.MaxConcurrent, &createdAt); err == nil {
			k.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
			out = append(out, k)
		}
	}
	return out
}

func (s *State) AddAPIKey(k APIKey) error {
	_, err := s.db.Exec(
		`INSERT INTO api_keys (key_hash, key_prefix, label, owner, priority, max_concurrent, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		k.KeyHash, k.KeyPrefix, k.Label, k.Owner, k.Priority, k.MaxConcurrent, k.CreatedAt.Format(time.RFC3339),
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return fmt.Errorf("label %q already exists for this user", k.Label)
		}
		return err
	}
	return nil
}

// UpdateAPIKeyPriority updates a key identified by its hash.
func (s *State) UpdateAPIKeyPriority(keyHash, priority string) error {
	switch priority {
	case "high", "normal", "low":
	default:
		return fmt.Errorf("invalid priority %q", priority)
	}
	res, err := s.db.Exec(`UPDATE api_keys SET priority = ? WHERE key_hash = ?`, priority, keyHash)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("key not found")
	}
	return nil
}

// UpdateAPIKeyMaxConcurrent updates a key identified by its hash.
func (s *State) UpdateAPIKeyMaxConcurrent(keyHash string, limit int) error {
	if limit < 0 {
		return fmt.Errorf("max_concurrent must be >= 0")
	}
	res, err := s.db.Exec(`UPDATE api_keys SET max_concurrent = ? WHERE key_hash = ?`, limit, keyHash)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("key not found")
	}
	return nil
}

func (s *State) MaxConcurrentFor(key string) int {
	k, ok := s.LookupAPIKey(key)
	if !ok {
		return 0
	}
	return k.MaxConcurrent
}

// RevokeAPIKey deletes a key identified by its hash.
func (s *State) RevokeAPIKey(owner, keyHash string, isAdmin bool) error {
	var (
		res sql.Result
		err error
	)
	if isAdmin {
		res, err = s.db.Exec(`DELETE FROM api_keys WHERE key_hash = ?`, keyHash)
	} else {
		res, err = s.db.Exec(`DELETE FROM api_keys WHERE key_hash = ? AND owner = ?`, keyHash, owner)
	}
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("key not found")
	}
	return nil
}

func (s *State) APIKeyCount() int {
	var count int
	s.db.QueryRow(`SELECT COUNT(*) FROM api_keys`).Scan(&count)
	return count
}

func (s *State) ValidAPIKey(key string) bool {
	_, ok := s.LookupAPIKey(key)
	return ok
}

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

func (s *State) LabelFor(key string) string {
	k, ok := s.LookupAPIKey(key)
	if !ok {
		return ""
	}
	return k.Owner + "/" + k.Label
}

// --- Clients ---

// LookupClientToken finds a token record by the plaintext token presented by
// a connecting client.
func (s *State) LookupClientToken(token string) (ClientToken, bool) {
	return s.LookupClientTokenByHash(HashSecret(token))
}

// LookupClientTokenByHash finds a token record by its stored hash — the
// identifier used by portal forms and by the hub's connection registry.
func (s *State) LookupClientTokenByHash(hash string) (ClientToken, bool) {
	var t ClientToken
	var createdAt, ownerSlotsJSON string
	err := s.db.QueryRow(
		`SELECT token_hash, token_prefix, name, owner, created_at, owner_slots FROM client_tokens WHERE token_hash = ?`,
		hash,
	).Scan(&t.TokenHash, &t.TokenPrefix, &t.Name, &t.Owner, &createdAt, &ownerSlotsJSON)
	if err != nil {
		return ClientToken{}, false
	}
	t.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	json.Unmarshal([]byte(ownerSlotsJSON), &t.OwnerSlots)
	return t, true
}

func (s *State) ClientTokensFor(owner string, isAdmin bool) []ClientToken {
	var (
		rows *sql.Rows
		err  error
	)
	if isAdmin {
		rows, err = s.db.Query(`SELECT token_hash, token_prefix, name, owner, created_at, owner_slots FROM client_tokens ORDER BY owner, name`)
	} else {
		rows, err = s.db.Query(`SELECT token_hash, token_prefix, name, owner, created_at, owner_slots FROM client_tokens WHERE owner = ? ORDER BY name`, owner)
	}
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []ClientToken
	for rows.Next() {
		var t ClientToken
		var createdAt, ownerSlotsJSON string
		if err := rows.Scan(&t.TokenHash, &t.TokenPrefix, &t.Name, &t.Owner, &createdAt, &ownerSlotsJSON); err == nil {
			t.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
			json.Unmarshal([]byte(ownerSlotsJSON), &t.OwnerSlots)
			out = append(out, t)
		}
	}
	return out
}

func (s *State) AddClientToken(t ClientToken) error {
	_, err := s.db.Exec(
		`INSERT INTO client_tokens (token_hash, token_prefix, name, owner, created_at, owner_slots) VALUES (?, ?, ?, ?, ?, ?)`,
		t.TokenHash, t.TokenPrefix, t.Name, t.Owner, t.CreatedAt.Format(time.RFC3339), marshalOwnerSlots(t.OwnerSlots),
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return fmt.Errorf("name %q already exists for this user", t.Name)
		}
		return err
	}
	return nil
}

// RevokeClientToken deletes a token identified by its hash.
func (s *State) RevokeClientToken(owner, tokenHash string, isAdmin bool) error {
	var (
		res sql.Result
		err error
	)
	if isAdmin {
		res, err = s.db.Exec(`DELETE FROM client_tokens WHERE token_hash = ?`, tokenHash)
	} else {
		res, err = s.db.Exec(`DELETE FROM client_tokens WHERE token_hash = ? AND owner = ?`, tokenHash, owner)
	}
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("token not found")
	}
	return nil
}

// SetClientTokenOwnerSlots updates the per-model owner-slot reservation for a
// token identified by its hash.
func (s *State) SetClientTokenOwnerSlots(owner, tokenHash, model string, slots int, isAdmin bool) error {
	t, ok := s.LookupClientTokenByHash(tokenHash)
	if !ok || (!isAdmin && t.Owner != owner) {
		return fmt.Errorf("token not found")
	}
	if slots <= 0 {
		delete(t.OwnerSlots, model)
	} else {
		if t.OwnerSlots == nil {
			t.OwnerSlots = make(map[string]int)
		}
		t.OwnerSlots[model] = slots
	}
	_, err := s.db.Exec(`UPDATE client_tokens SET owner_slots = ? WHERE token_hash = ?`, marshalOwnerSlots(t.OwnerSlots), tokenHash)
	return err
}

func (s *State) ClientTokenCount() int {
	var count int
	s.db.QueryRow(`SELECT COUNT(*) FROM client_tokens`).Scan(&count)
	return count
}

// --- Upstream Routers ---

func (s *State) GetUpstreamRouters() []UpstreamRouter {
	rows, err := s.db.Query(`SELECT url, name, token, COALESCE(priority, 'normal') FROM upstream_routers ORDER BY url`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []UpstreamRouter
	for rows.Next() {
		var r UpstreamRouter
		if err := rows.Scan(&r.URL, &r.Name, &r.Token, &r.Priority); err == nil {
			out = append(out, r)
		}
	}
	return out
}

func (s *State) AddUpstreamRouter(r UpstreamRouter) error {
	if r.URL == "" || r.Token == "" {
		return fmt.Errorf("url and token are required")
	}
	parsed, err := url.ParseRequestURI(r.URL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("url must start with http:// or https://")
	}
	r.URL = strings.ToLower(strings.TrimRight(r.URL, "/"))
	switch r.Priority {
	case "", "normal":
		r.Priority = "normal"
	case "high", "low":
		// valid
	default:
		return fmt.Errorf("priority must be one of: high, normal, low")
	}
	_, err = s.db.Exec(
		`INSERT INTO upstream_routers (url, name, token, priority) VALUES (?, ?, ?, ?)`,
		r.URL, r.Name, r.Token, r.Priority,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return fmt.Errorf("upstream %q is already configured", r.URL)
		}
		return err
	}
	return nil
}

func (s *State) RemoveUpstreamRouter(rawURL string) error {
	res, err := s.db.Exec(`DELETE FROM upstream_routers WHERE url = ?`, rawURL)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("upstream %q not found", rawURL)
	}
	return nil
}

// --- Token generation ---

func genRandom(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func GenAPIKeyValue(owner string) (string, error) {
	r, err := genRandom(16)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("sk-%s-%s", owner, r), nil
}

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
	u, ok := s.LookupUser(username)
	if !ok || u.CSRFToken == "" || token == "" {
		return false
	}
	h := hashToken(token)
	if subtle.ConstantTimeCompare([]byte(u.CSRFToken), []byte(h)) != 1 {
		return false
	}
	s.db.Exec(`UPDATE users SET csrf_token = '' WHERE username = ?`, username)
	return true
}

// RefreshCSRFToken generates a new CSRF token for the named user and persists it.
// Returns the plaintext token (to embed in forms).
func (s *State) RefreshCSRFToken(username string) (string, error) {
	if _, ok := s.LookupUser(username); !ok {
		return "", fmt.Errorf("user not found: %s", username)
	}
	token, err := generateCSRFToken()
	if err != nil {
		return "", err
	}
	if _, err := s.db.Exec(`UPDATE users SET csrf_token = ? WHERE username = ?`, hashToken(token), username); err != nil {
		return "", err
	}
	return token, nil
}

// --- Model Aliases ---

// AliasMap returns the alias→models map. The result is a cached, immutable
// snapshot shared across callers; callers must not mutate it. The cache is
// rebuilt lazily after any alias mutation invalidates it.
func (s *State) AliasMap() map[string][]string {
	if cached := s.aliasCache.Load(); cached != nil {
		return *cached
	}
	rows, err := s.db.Query(`SELECT alias, model FROM model_aliases ORDER BY alias, model`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := make(map[string][]string)
	for rows.Next() {
		var alias, model string
		if err := rows.Scan(&alias, &model); err == nil {
			out[alias] = append(out[alias], model)
		}
	}
	s.aliasCache.Store(&out)
	return out
}

func (s *State) AddAlias(alias, model string) error {
	if alias == "" || model == "" {
		return fmt.Errorf("alias and model must not be blank")
	}
	_, err := s.db.Exec(`INSERT INTO model_aliases (alias, model) VALUES (?, ?)`, alias, model)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return fmt.Errorf("alias %q → %q already exists", alias, model)
		}
		return err
	}
	s.aliasCache.Store(nil)
	return nil
}

func (s *State) DeleteAlias(alias, model string) error {
	res, err := s.db.Exec(`DELETE FROM model_aliases WHERE alias = ? AND model = ?`, alias, model)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("alias %q → %q not found", alias, model)
	}
	s.aliasCache.Store(nil)
	return nil
}

func (s *State) DeleteAliasGroup(alias string) error {
	res, err := s.db.Exec(`DELETE FROM model_aliases WHERE alias = ?`, alias)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("alias %q not found", alias)
	}
	s.aliasCache.Store(nil)
	return nil
}

// --- Request optimization settings ---

// optKeys maps each settings-table key to a pointer accessor on a
// RequestOptimization struct. It is the single source of truth for which
// toggles exist, used by both the reader and the validating writer.
var optKeys = map[string]func(*types.RequestOptimization) *bool{
	"reqopt.coalesce_normalize": func(o *types.RequestOptimization) *bool { return &o.CoalesceNormalize },
	"reqopt.prefix_affinity":    func(o *types.RequestOptimization) *bool { return &o.PrefixAffinity },
	"reqopt.clean_requests":     func(o *types.RequestOptimization) *bool { return &o.CleanRequests },
	"reqopt.clean_aggressive":   func(o *types.RequestOptimization) *bool { return &o.CleanAggressive },
	"reqopt.clamp_params":       func(o *types.RequestOptimization) *bool { return &o.ClampParams },
}

// RequestOpts returns the request-optimization toggles. The result is a cached
// snapshot rebuilt lazily after any settings mutation; safe to call on the hot path.
func (s *State) RequestOpts() types.RequestOptimization {
	if cached := s.optCache.Load(); cached != nil {
		return *cached
	}
	var o types.RequestOptimization
	rows, err := s.db.Query(`SELECT key, value FROM settings WHERE key LIKE 'reqopt.%'`)
	if err != nil {
		return o
	}
	defer rows.Close()
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			continue
		}
		if accessor, ok := optKeys[key]; ok {
			*accessor(&o) = value == "1"
		}
	}
	s.optCache.Store(&o)
	return o
}

// SetRequestOpt persists a single request-optimization toggle and invalidates
// the cache. The key must be one of the known reqopt.* keys.
func (s *State) SetRequestOpt(key string, enabled bool) error {
	if _, ok := optKeys[key]; !ok {
		return fmt.Errorf("unknown setting %q", key)
	}
	val := "0"
	if enabled {
		val = "1"
	}
	if _, err := s.db.Exec(
		`INSERT INTO settings (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, val,
	); err != nil {
		return err
	}
	s.optCache.Store(nil)
	return nil
}
