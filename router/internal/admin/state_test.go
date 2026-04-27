package admin

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"testing"

	"llmesh/pkg/types"
	"golang.org/x/crypto/bcrypt"
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
	k := APIKey{Label: "prod", Owner: "alice", Key: "sk-alice-abc123", Priority: "high"}
	if err := s.AddAPIKey(k); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.LookupAPIKey("sk-alice-abc123"); !ok {
		t.Fatal("key not found after add")
	}
	if err := s.RevokeAPIKey("alice", "sk-alice-abc123", false); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.LookupAPIKey("sk-alice-abc123"); ok {
		t.Fatal("key found after revoke")
	}
}

func TestAPIKey_LabelUniqueness(t *testing.T) {
	s, _ := LoadState(filepath.Join(t.TempDir(), "state.json"))
	s.AddAPIKey(APIKey{Label: "dev", Owner: "alice", Key: "sk-alice-1"})
	if err := s.AddAPIKey(APIKey{Label: "dev", Owner: "alice", Key: "sk-alice-2"}); err == nil {
		t.Fatal("expected error for duplicate label")
	}
	// different owner OK
	if err := s.AddAPIKey(APIKey{Label: "dev", Owner: "bob", Key: "sk-bob-1"}); err != nil {
		t.Fatalf("different owner should be allowed: %v", err)
	}
}

func TestClientToken_AddRevoke(t *testing.T) {
	s, _ := LoadState(filepath.Join(t.TempDir(), "state.json"))
	tok := ClientToken{Name: "mac", Owner: "alice", Token: "ct-alice-abc123"}
	s.AddClientToken(tok)
	if _, ok := s.LookupClientToken("ct-alice-abc123"); !ok {
		t.Fatal("token not found after add")
	}
	s.RevokeClientToken("alice", "ct-alice-abc123", false)
	if _, ok := s.LookupClientToken("ct-alice-abc123"); ok {
		t.Fatal("token found after revoke")
	}
}

func TestValidAPIKey(t *testing.T) {
	s, _ := LoadState(filepath.Join(t.TempDir(), "state.json"))
	s.AddAPIKey(APIKey{Label: "x", Owner: "alice", Key: "sk-alice-xyz"})
	if !s.ValidAPIKey("sk-alice-xyz") {
		t.Fatal("expected valid")
	}
	if s.ValidAPIKey("sk-unknown") {
		t.Fatal("expected invalid")
	}
}

func TestPriorityFor(t *testing.T) {
	s, _ := LoadState(filepath.Join(t.TempDir(), "state.json"))
	s.AddAPIKey(APIKey{Label: "hi", Owner: "a", Key: "sk-a-hi", Priority: "high"})
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

