package admin

import (
	"net/url"
	"path/filepath"
	"testing"
	"time"
)

// Disabling a user must cut off every credential they hold, not only their
// portal session: a key or token that kept working would leave them able to
// use the router with nothing in the portal saying why.
func TestDisabledUserCredentialsAreRefused(t *testing.T) {
	s, _ := LoadState(filepath.Join(t.TempDir(), "state.json"))
	s.AddUser(User{Username: "carol", Role: "member"})
	s.AddAPIKey(testAPIKey("prod", "carol", "sk-carol-abc123", "normal"))
	s.AddClientToken(testClientToken("box", "carol", "ct-carol-abc123"))

	if !s.ValidAPIKey("sk-carol-abc123") {
		t.Fatal("an active user's key was refused")
	}
	if _, ok := s.LookupActiveClientToken("ct-carol-abc123"); !ok {
		t.Fatal("an active user's client token was refused")
	}

	if err := s.UpdateUser("carol", func(u *User) { u.Disabled = true }); err != nil {
		t.Fatal(err)
	}
	if s.ValidAPIKey("sk-carol-abc123") {
		t.Fatal("a disabled user's API key still authenticates")
	}
	if _, ok := s.LookupActiveClientToken("ct-carol-abc123"); ok {
		t.Fatal("a disabled user's client token still authenticates")
	}
	// The portal still needs to see the token to manage it.
	if _, ok := s.LookupClientToken("ct-carol-abc123"); !ok {
		t.Fatal("the portal lookup hides a disabled user's token")
	}

	// Re-enabling restores access without reissuing anything.
	if err := s.UpdateUser("carol", func(u *User) { u.Disabled = false }); err != nil {
		t.Fatal(err)
	}
	if !s.ValidAPIKey("sk-carol-abc123") {
		t.Fatal("re-enabling did not restore the key")
	}
	if _, ok := s.LookupActiveClientToken("ct-carol-abc123"); !ok {
		t.Fatal("re-enabling did not restore the client token")
	}
}

// A key or token with no matching user row predates accounts; there is no one
// to have disabled, so it keeps working.
func TestOwnerlessCredentialsUnaffected(t *testing.T) {
	s, _ := LoadState(filepath.Join(t.TempDir(), "state.json"))
	s.AddAPIKey(testAPIKey("legacy", "", "sk-legacy-abc123", "normal"))
	s.AddClientToken(testClientToken("legacy", "", "ct-legacy-abc123"))
	if !s.ValidAPIKey("sk-legacy-abc123") {
		t.Fatal("an ownerless key was refused")
	}
	if _, ok := s.LookupActiveClientToken("ct-legacy-abc123"); !ok {
		t.Fatal("an ownerless client token was refused")
	}
	if s.ValidAPIKey("sk-unknown") {
		t.Fatal("an unknown key was accepted")
	}
}

func TestUserDisableDisconnectsClients(t *testing.T) {
	a, h := connTestAdmin(t)
	addTestUser(t, a, "admin", "admin")
	addTestUser(t, a, "carol", "member")
	ct := testClientToken("box", "carol", "ct-carol-abc123")
	if err := a.state.AddClientToken(ct); err != nil {
		t.Fatal(err)
	}
	conn := dialClient(t, h, "box", "carol", ct.TokenHash)
	deadline := time.Now().Add(2 * time.Second)
	for len(h.ConnectedClientsByToken(ct.TokenHash)) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("client never registered")
		}
		time.Sleep(10 * time.Millisecond)
	}

	postAs(t, a, "admin", "/portal/settings/users/disable",
		url.Values{"username": {"carol"}}, a.handleUserDisable)

	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			break // closed by the router, as it should be
		}
	}
	if n := len(h.ConnectedClientsByToken(ct.TokenHash)); n != 0 {
		t.Fatalf("%d client(s) still connected after their owner was disabled", n)
	}
}
