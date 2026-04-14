package admin

import (
	"path/filepath"
	"testing"

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

