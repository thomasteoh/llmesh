package admin

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/bcrypt"
	"llmesh/pkg/types"
)

func TestNeedsSetup_Empty(t *testing.T) {
	s, err := LoadState(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !s.NeedsSetup() {
		t.Fatal("expected NeedsSetup=true for fresh state")
	}
}

func TestAddUser_LookupUser(t *testing.T) {
	f := filepath.Join(t.TempDir(), "state.json")
	s, _ := LoadState(f)
	if err := s.AddUser(User{Username: "alice", PasswordHash: "h", Role: "admin"}); err != nil {
		t.Fatal(err)
	}
	if s.NeedsSetup() {
		t.Fatal("expected NeedsSetup=false after AddUser")
	}
	u, ok := s.LookupUser("alice")
	if !ok || u.Role != "admin" {
		t.Fatalf("got %+v ok=%v", u, ok)
	}
	// persists across reload
	s2, _ := LoadState(f)
	if _, ok := s2.LookupUser("alice"); !ok {
		t.Fatal("user not persisted")
	}
}

func TestLookupUser_NotFound(t *testing.T) {
	s, _ := LoadState(filepath.Join(t.TempDir(), "state.json"))
	_, ok := s.LookupUser("nobody")
	if ok {
		t.Fatal("expected not found")
	}
}

func TestActiveAdminCount(t *testing.T) {
	s, _ := LoadState(filepath.Join(t.TempDir(), "state.json"))
	s.AddUser(User{Username: "a", Role: "admin", Disabled: false})
	s.AddUser(User{Username: "b", Role: "admin", Disabled: true})
	s.AddUser(User{Username: "c", Role: "member", Disabled: false})
	if n := s.ActiveAdminCount(); n != 1 {
		t.Fatalf("want 1, got %d", n)
	}
}

func TestAPIKey_AddRevoke(t *testing.T) {
	s, _ := LoadState(filepath.Join(t.TempDir(), "state.json"))
	k := testAPIKey("prod", "alice", "sk-alice-abc123", "high")
	if err := s.AddAPIKey(k); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.LookupAPIKey("sk-alice-abc123"); !ok {
		t.Fatal("key not found after add")
	}
	if err := s.RevokeAPIKey("alice", HashSecret("sk-alice-abc123"), false); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.LookupAPIKey("sk-alice-abc123"); ok {
		t.Fatal("key found after revoke")
	}
}

func TestAPIKey_LabelUniqueness(t *testing.T) {
	s, _ := LoadState(filepath.Join(t.TempDir(), "state.json"))
	s.AddAPIKey(testAPIKey("dev", "alice", "sk-alice-1", "normal"))
	if err := s.AddAPIKey(testAPIKey("dev", "alice", "sk-alice-2", "normal")); err == nil {
		t.Fatal("expected error for duplicate label")
	}
	// different owner OK
	if err := s.AddAPIKey(testAPIKey("dev", "bob", "sk-bob-1", "normal")); err != nil {
		t.Fatalf("different owner should be allowed: %v", err)
	}
}

func TestClientToken_AddRevoke(t *testing.T) {
	s, _ := LoadState(filepath.Join(t.TempDir(), "state.json"))
	tok := testClientToken("mac", "alice", "ct-alice-abc123")
	s.AddClientToken(tok)
	if _, ok := s.LookupClientToken("ct-alice-abc123"); !ok {
		t.Fatal("token not found after add")
	}
	s.RevokeClientToken("alice", HashSecret("ct-alice-abc123"), false)
	if _, ok := s.LookupClientToken("ct-alice-abc123"); ok {
		t.Fatal("token found after revoke")
	}
}

func TestValidAPIKey(t *testing.T) {
	s, _ := LoadState(filepath.Join(t.TempDir(), "state.json"))
	s.AddAPIKey(testAPIKey("x", "alice", "sk-alice-xyz", "normal"))
	if !s.ValidAPIKey("sk-alice-xyz") {
		t.Fatal("expected valid")
	}
	if s.ValidAPIKey("sk-unknown") {
		t.Fatal("expected invalid")
	}
}

func TestPriorityFor(t *testing.T) {
	s, _ := LoadState(filepath.Join(t.TempDir(), "state.json"))
	s.AddAPIKey(testAPIKey("hi", "a", "sk-a-hi", "high"))
	if p := s.PriorityFor("sk-a-hi"); p != types.PriorityHigh {
		t.Fatalf("want PriorityHigh, got %v", p)
	}
	if p := s.PriorityFor("sk-unknown"); p != types.PriorityNormal {
		t.Fatalf("want PriorityNormal for unknown, got %v", p)
	}
}

func TestGenAPIKeyValue(t *testing.T) {
	k, err := GenAPIKeyValue("alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(k) < 10 || k[:9] != "sk-alice-" {
		t.Fatalf("unexpected key format: %s", k)
	}
}

func TestUpdateUser(t *testing.T) {
	f := filepath.Join(t.TempDir(), "state.json")
	s, _ := LoadState(f)
	s.AddUser(User{Username: "alice", PasswordHash: "old", Role: "member"})
	if err := s.UpdateUser("alice", func(u *User) { u.PasswordHash = "new" }); err != nil {
		t.Fatal(err)
	}
	// persists
	s2, _ := LoadState(f)
	u, _ := s2.LookupUser("alice")
	if u.PasswordHash != "new" {
		t.Fatalf("want 'new', got %q", u.PasswordHash)
	}
	// not found returns error
	if err := s.UpdateUser("nobody", func(u *User) {}); err == nil {
		t.Fatal("expected error for missing user")
	}
}

func TestAddUser_DuplicateRejected(t *testing.T) {
	s, _ := LoadState(filepath.Join(t.TempDir(), "state.json"))
	s.AddUser(User{Username: "alice", Role: "admin"})
	if err := s.AddUser(User{Username: "alice", Role: "member"}); err == nil {
		t.Fatal("expected error for duplicate username")
	}
}

// --- CSRF unit tests ---

func TestGenerateCSRFToken(t *testing.T) {
	token, err := generateCSRFToken()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token == "" {
		t.Fatal("expected non-empty token")
	}
	// 32 bytes = 64 hex chars
	if len(token) != 64 {
		t.Fatalf("expected 64 hex chars, got %d", len(token))
	}
	// Verify it's valid hex
	for _, c := range token {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Fatalf("invalid hex char: %c", c)
		}
	}
}

func TestHashToken(t *testing.T) {
	token := "my-token"
	h := hashToken(token)
	if h == "" {
		t.Fatal("expected non-empty hash")
	}
	// Hash should be different from plaintext
	if h == token {
		t.Fatal("hash should not equal plaintext")
	}
	// Hash should be deterministic
	h2 := hashToken(token)
	if h != h2 {
		t.Fatal("hash should be deterministic")
	}
	// Hash should be 64 hex chars (SHA-256 = 32 bytes)
	if len(h) != 64 {
		t.Fatalf("expected 64 hex chars, got %d", len(h))
	}
	// Verify it matches manual computation
	expected := sha256.Sum256([]byte(token))
	expectedHex := hex.EncodeToString(expected[:])
	if h != expectedHex {
		t.Fatalf("hash mismatch: got %s, want %s", h, expectedHex)
	}
}

func TestValidateCSRF(t *testing.T) {
	s, _ := LoadState(filepath.Join(t.TempDir(), "state.json"))
	hash, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	s.AddUser(User{Username: "alice", PasswordHash: string(hash), Role: "admin"})

	// Generate and store a valid token
	token, err := s.RefreshCSRFToken("alice")
	if err != nil {
		t.Fatalf("RefreshCSRFToken error: %v", err)
	}

	// Valid token returns true
	if !s.ConsumeCSRF("alice", token) {
		t.Fatal("expected valid token to succeed")
	}
}

func TestValidateCSRF_InvalidToken(t *testing.T) {
	s, _ := LoadState(filepath.Join(t.TempDir(), "state.json"))
	_ = s.AddUser(User{Username: "alice", PasswordHash: "h", Role: "admin"})

	// Invalid token returns false
	if s.ConsumeCSRF("alice", "wrong-token") {
		t.Fatal("expected invalid token to fail")
	}
}

func TestValidateCSRF_EmptyToken(t *testing.T) {
	s, _ := LoadState(filepath.Join(t.TempDir(), "state.json"))
	hash, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	s.AddUser(User{Username: "alice", PasswordHash: string(hash), Role: "admin"})

	// Empty submitted token returns false
	if s.ConsumeCSRF("alice", "") {
		t.Fatal("expected empty token to fail")
	}
}

func TestValidateCSRF_EmptyStoredToken(t *testing.T) {
	s, _ := LoadState(filepath.Join(t.TempDir(), "state.json"))
	_ = s.AddUser(User{Username: "alice", PasswordHash: "h", Role: "admin", CSRFToken: ""})

	// Empty stored token returns false
	if s.ConsumeCSRF("alice", "some-token") {
		t.Fatal("expected empty stored token to fail")
	}
}

func TestRefreshCSRFToken(t *testing.T) {
	s, _ := LoadState(filepath.Join(t.TempDir(), "state.json"))
	hash, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	s.AddUser(User{Username: "alice", PasswordHash: string(hash), Role: "admin"})

	// Returns non-empty token for valid user
	token1, err := s.RefreshCSRFToken("alice")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token1 == "" {
		t.Fatal("expected non-empty token")
	}

	// Token hash is stored in state
	u, ok := s.LookupUser("alice")
	if !ok {
		t.Fatal("user not found")
	}
	if u.CSRFToken == "" {
		t.Fatal("expected CSRFToken hash to be stored")
	}
	// Verify the hash matches the token
	if hashToken(token1) != u.CSRFToken {
		t.Fatalf("stored hash %s doesn't match token hash %s", u.CSRFToken, hashToken(token1))
	}

	// Token is different from previous
	token2, err := s.RefreshCSRFToken("alice")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token1 == token2 {
		t.Fatal("expected different tokens")
	}

	// Returns error for unknown user
	if _, err := s.RefreshCSRFToken("nobody"); err == nil {
		t.Fatal("expected error for unknown user")
	}
}

func TestCSRFConsume(t *testing.T) {
	s, _ := LoadState(filepath.Join(t.TempDir(), "state.json"))
	hash, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	s.AddUser(User{Username: "alice", PasswordHash: string(hash), Role: "admin"})

	token, err := s.RefreshCSRFToken("alice")
	if err != nil {
		t.Fatalf("RefreshCSRFToken error: %v", err)
	}

	// Valid token succeeds
	if !s.ConsumeCSRF("alice", token) {
		t.Fatal("expected valid token to succeed")
	}

	// Second use should fail (one-time use)
	if s.ConsumeCSRF("alice", token) {
		t.Fatal("expected consumed token to fail on second use")
	}

	// Invalid token fails and state unchanged (token already consumed, so still fails)
	if s.ConsumeCSRF("alice", "wrong-token") {
		t.Fatal("expected invalid token to fail")
	}
}

// --- Hashed-secret helpers and migration ---

func TestDeleteUser_RequiresDisabled(t *testing.T) {
	s, _ := LoadState(filepath.Join(t.TempDir(), "state.json"))
	s.AddUser(User{Username: "bob", Role: "member", Disabled: false})
	if err := s.DeleteUser("admin", "bob"); err == nil {
		t.Fatal("expected error deleting an enabled user")
	}
	if _, ok := s.LookupUser("bob"); !ok {
		t.Fatal("enabled user must not be deleted")
	}
	// After disabling, deletion succeeds.
	s.UpdateUser("bob", func(u *User) { u.Disabled = true })
	if err := s.DeleteUser("admin", "bob"); err != nil {
		t.Fatalf("deleting disabled user: %v", err)
	}
	if _, ok := s.LookupUser("bob"); ok {
		t.Fatal("user still present after delete")
	}
}

func TestDeleteUser_CascadesCredentials(t *testing.T) {
	s, _ := LoadState(filepath.Join(t.TempDir(), "state.json"))
	s.AddUser(User{Username: "carol", Role: "member", Disabled: true})
	s.AddAPIKey(testAPIKey("prod", "carol", "sk-carol-abc123", "normal"))
	s.AddClientToken(testClientToken("box", "carol", "ct-carol-abc123"))
	if err := s.DeleteUser("admin", "carol"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if keys := s.APIKeysFor("carol", false); len(keys) != 0 {
		t.Fatalf("api keys must be revoked on delete, got %d", len(keys))
	}
	if _, ok := s.LookupAPIKey("sk-carol-abc123"); ok {
		t.Fatal("deleted user's API key still authenticates")
	}
	if toks := s.ClientTokensFor("carol", false); len(toks) != 0 {
		t.Fatalf("client tokens must be revoked on delete, got %d", len(toks))
	}
}

func TestDeleteUser_Self(t *testing.T) {
	s, _ := LoadState(filepath.Join(t.TempDir(), "state.json"))
	s.AddUser(User{Username: "admin", Role: "admin", Disabled: true})
	if err := s.DeleteUser("admin", "admin"); err == nil {
		t.Fatal("expected error deleting self")
	}
}

func TestDeleteUser_LastAdmin(t *testing.T) {
	s, _ := LoadState(filepath.Join(t.TempDir(), "state.json"))
	s.AddUser(User{Username: "solo", Role: "admin", Disabled: true})
	if err := s.DeleteUser("other", "solo"); err == nil {
		t.Fatal("expected error deleting the last admin account")
	}
	// A second admin makes deleting the disabled one safe.
	s.AddUser(User{Username: "keeper", Role: "admin", Disabled: false})
	if err := s.DeleteUser("keeper", "solo"); err != nil {
		t.Fatalf("deleting a non-last disabled admin: %v", err)
	}
}

func TestDeleteUser_NotFound(t *testing.T) {
	s, _ := LoadState(filepath.Join(t.TempDir(), "state.json"))
	if err := s.DeleteUser("admin", "ghost"); err == nil {
		t.Fatal("expected error deleting a nonexistent user")
	}
}

func testAPIKey(label, owner, plain, priority string) APIKey {
	return APIKey{Label: label, Owner: owner, KeyHash: HashSecret(plain), KeyPrefix: SecretPrefix(plain), Priority: priority}
}

func testClientToken(name, owner, plain string) ClientToken {
	return ClientToken{Name: name, Owner: owner, TokenHash: HashSecret(plain), TokenPrefix: SecretPrefix(plain)}
}

func TestSecretPrefix(t *testing.T) {
	p := SecretPrefix("sk-alice-0123456789abcdef0123456789abcdef")
	if p != "sk-alice-0123…" {
		t.Fatalf("unexpected prefix %q", p)
	}
	if got := SecretPrefix("short"); got != "****" {
		t.Fatalf("short secrets must be fully masked, got %q", got)
	}
}

// TestMigrateSecretColumns verifies that a database created with the old
// plaintext schema is rewritten to hashed storage on open, and that plaintext
// lookups still resolve to the migrated rows.
func TestMigrateSecretColumns(t *testing.T) {
	dir := t.TempDir()
	dbfile := filepath.Join(dir, "state.db")

	db, err := sql.Open("sqlite", dbfile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		CREATE TABLE api_keys (
			key            TEXT PRIMARY KEY,
			label          TEXT NOT NULL DEFAULT '',
			owner          TEXT NOT NULL DEFAULT '',
			priority       TEXT NOT NULL DEFAULT 'normal',
			max_concurrent INTEGER NOT NULL DEFAULT 0,
			created_at     TEXT NOT NULL DEFAULT '',
			UNIQUE(owner, label)
		);
		CREATE TABLE client_tokens (
			token       TEXT PRIMARY KEY,
			name        TEXT NOT NULL DEFAULT '',
			owner       TEXT NOT NULL DEFAULT '',
			created_at  TEXT NOT NULL DEFAULT '',
			owner_slots TEXT NOT NULL DEFAULT '{}',
			UNIQUE(owner, name)
		);
		INSERT INTO api_keys (key, label, owner, priority, max_concurrent, created_at)
			VALUES ('sk-alice-0123456789abcdef0123456789abcdef', 'prod', 'alice', 'high', 3, '2026-01-01T00:00:00Z');
		INSERT INTO client_tokens (token, name, owner, created_at, owner_slots)
			VALUES ('ct-bob-0123456789abcdef0123456789abcdef', 'mac', 'bob', '2026-01-01T00:00:00Z', '{"m1":2}');
	`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := LoadState(dbfile)
	if err != nil {
		t.Fatal(err)
	}

	k, ok := s.LookupAPIKey("sk-alice-0123456789abcdef0123456789abcdef")
	if !ok {
		t.Fatal("migrated key not found by plaintext lookup")
	}
	if k.KeyHash != HashSecret("sk-alice-0123456789abcdef0123456789abcdef") {
		t.Fatal("migrated key hash mismatch")
	}
	if k.KeyPrefix != "sk-alice-0123…" || k.Label != "prod" || k.Priority != "high" || k.MaxConcurrent != 3 {
		t.Fatalf("migrated key fields wrong: %+v", k)
	}

	tok, ok := s.LookupClientToken("ct-bob-0123456789abcdef0123456789abcdef")
	if !ok {
		t.Fatal("migrated token not found by plaintext lookup")
	}
	if tok.Name != "mac" || tok.Owner != "bob" || tok.OwnerSlots["m1"] != 2 {
		t.Fatalf("migrated token fields wrong: %+v", tok)
	}

	// The plaintext columns must be gone.
	if tableHasColumn(s.db, "api_keys", "key") || tableHasColumn(s.db, "client_tokens", "token") {
		t.Fatal("plaintext columns still present after migration")
	}
}
