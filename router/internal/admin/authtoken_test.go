package admin

import (
	"testing"
	"time"
)

func TestAuthTokenIssueAndConsume(t *testing.T) {
	s := newAuthTokenStore()
	tok := s.issue("alice", purposeLogin, "a@b.com", time.Minute)
	if tok == "" {
		t.Fatal("empty token")
	}

	// The plaintext must not be the key: a dump of the store should not hand
	// over a working link.
	if _, ok := s.entries[tok]; ok {
		t.Fatal("token stored under its plaintext")
	}

	e, ok := s.lookup(tok, purposeLogin)
	if !ok || e.Username != "alice" || e.Email != "a@b.com" {
		t.Fatalf("lookup returned %+v %v", e, ok)
	}
	// lookup must not spend it, or the confirmation page would burn the token
	// it is about to offer.
	if _, ok := s.lookup(tok, purposeLogin); !ok {
		t.Fatal("lookup spent the token")
	}

	if _, ok := s.consume(tok, purposeLogin); !ok {
		t.Fatal("consume failed")
	}
	if _, ok := s.consume(tok, purposeLogin); ok {
		t.Fatal("a sign-in link worked twice")
	}
}

func TestAuthTokenPurposeIsolation(t *testing.T) {
	s := newAuthTokenStore()
	// A 24-hour verification link must not double as a sign-in credential.
	tok := s.issue("alice", purposeVerify, "a@b.com", time.Minute)
	if _, ok := s.lookup(tok, purposeLogin); ok {
		t.Fatal("a verification token was accepted for sign-in")
	}
	if _, ok := s.lookup(tok, purposeVerify); !ok {
		t.Fatal("verification token rejected for its own purpose")
	}
}

func TestAuthTokenExpiry(t *testing.T) {
	s := newAuthTokenStore()
	tok := s.issue("alice", purposeLogin, "a@b.com", -time.Second)
	if _, ok := s.lookup(tok, purposeLogin); ok {
		t.Fatal("an expired token was accepted")
	}
}

func TestAuthTokenRevokeAndSweep(t *testing.T) {
	s := newAuthTokenStore()
	first := s.issue("alice", purposeLogin, "a@b.com", time.Minute)
	second := s.issue("alice", purposeLogin, "a@b.com", time.Minute)
	bobs := s.issue("bob", purposeLogin, "b@b.com", time.Minute)
	verify := s.issue("alice", purposeVerify, "a@b.com", time.Minute)

	s.revoke("alice", purposeLogin)
	for _, tok := range []string{first, second} {
		if _, ok := s.lookup(tok, purposeLogin); ok {
			t.Fatal("revoke left one of alice's sign-in tokens live")
		}
	}
	if _, ok := s.lookup(bobs, purposeLogin); !ok {
		t.Fatal("revoke took another user's token")
	}
	if _, ok := s.lookup(verify, purposeVerify); !ok {
		t.Fatal("revoking sign-in tokens took the verification token too")
	}

	// Expired entries are dropped on the next issue, so the map cannot grow
	// without bound on a router nobody ever signs in to successfully.
	s.issue("carol", purposeLogin, "c@b.com", -time.Second)
	before := len(s.entries)
	s.issue("dave", purposeLogin, "d@b.com", time.Minute)
	if len(s.entries) != before {
		t.Fatalf("expected the expired entry to be swept: %d entries before, %d after", before, len(s.entries))
	}
}

func TestAuthTokenRejectsEmpty(t *testing.T) {
	s := newAuthTokenStore()
	if _, ok := s.lookup("", purposeLogin); ok {
		t.Fatal("empty token accepted")
	}
}
