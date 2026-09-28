package admin

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"sync"
	"time"
)

// Token purposes. A token is only ever accepted for the purpose it was issued
// for, so a 24-hour address-verification link cannot be replayed as a sign-in.
const (
	purposeLogin  = "login"
	purposeVerify = "verify"
)

const (
	// A sign-in link is a bearer credential sitting in an inbox, so it lives
	// only as long as it takes someone to switch to their mail and back.
	magicLinkTTL = 15 * time.Minute
	// A verification link proves control of an address rather than granting
	// access, so it can wait for someone who reads mail once a day.
	verifyLinkTTL = 24 * time.Hour
)

// authToken is an issued email link.
type authToken struct {
	Username string
	Purpose  string
	// Email is the address the link was sent to. Verification compares it
	// against the account's current claim, so changing the claimed address
	// invalidates a link already in flight for the old one.
	Email  string
	Expiry time.Time
}

// authTokenStore holds outstanding email links, keyed by the hash of the token
// rather than the token itself. The tokens live only in inboxes and in this
// map; hashing means a heap dump or an accidental log of the map does not hand
// over working credentials, exactly as the API key table already reasons.
//
// The store is in-memory, like sessionStore: a router restart invalidates
// outstanding links, which is an acceptable cost for a 15-minute credential and
// keeps single-use enforcement honest without a second source of truth.
type authTokenStore struct {
	mu      sync.Mutex
	entries map[string]authToken
}

func newAuthTokenStore() *authTokenStore {
	return &authTokenStore{entries: make(map[string]authToken)}
}

// issue mints a token for username and returns the plaintext, which is the only
// time it exists outside the recipient's inbox.
func (s *authTokenStore) issue(username, purpose, email string, ttl time.Duration) string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	// URL-safe, since this goes in a query string that a mail client will line-wrap.
	tok := base64.RawURLEncoding.EncodeToString(b)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	s.entries[hashAuthToken(tok)] = authToken{
		Username: username,
		Purpose:  purpose,
		Email:    email,
		Expiry:   time.Now().Add(ttl),
	}
	return tok
}

// lookup returns a live token matching purpose, without spending it.
func (s *authTokenStore) lookup(tok, purpose string) (authToken, bool) {
	if tok == "" {
		return authToken{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[hashAuthToken(tok)]
	if !ok || time.Now().After(e.Expiry) {
		return authToken{}, false
	}
	if subtle.ConstantTimeCompare([]byte(e.Purpose), []byte(purpose)) != 1 {
		return authToken{}, false
	}
	return e, true
}

// consume returns a live token and spends it, so a sign-in link works once.
func (s *authTokenStore) consume(tok, purpose string) (authToken, bool) {
	e, ok := s.lookup(tok, purpose)
	if !ok {
		return authToken{}, false
	}
	s.mu.Lock()
	delete(s.entries, hashAuthToken(tok))
	s.mu.Unlock()
	return e, true
}

// revoke drops every outstanding token of a purpose for one user. Issuing a new
// sign-in link invalidates the previous one, so a forwarded or intercepted older
// link stops working the moment its owner asks for another.
func (s *authTokenStore) revoke(username, purpose string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, e := range s.entries {
		if e.Username == username && e.Purpose == purpose {
			delete(s.entries, k)
		}
	}
}

// sweepLocked drops expired entries. Called on issue rather than from a ticker:
// the map only grows when a link is issued, so that is the only moment it can
// need trimming, and it avoids a goroutine with no lifecycle.
func (s *authTokenStore) sweepLocked() {
	now := time.Now()
	for k, e := range s.entries {
		if now.After(e.Expiry) {
			delete(s.entries, k)
		}
	}
}

func hashAuthToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}
