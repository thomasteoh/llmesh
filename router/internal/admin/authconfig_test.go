package admin

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func newTestState(t *testing.T) *State {
	t.Helper()
	s, err := LoadState(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// Every provider goes through the same storage and the same validation, so
// each of these runs against all of them rather than pinning one.
func TestOAuthConfigRoundTrip(t *testing.T) {
	for _, key := range oauthProviderOrder {
		t.Run(key, func(t *testing.T) {
			s := newTestState(t)
			name := oauthProviders[key].name

			if got := s.OAuth(key); got.Configured() {
				t.Fatal("a fresh router must not offer this sign-in")
			}
			if err := s.SetOAuth(key, name, OAuthConfig{Enabled: true, ClientID: "cid-1", ClientSecret: "shh"}); err != nil {
				t.Fatal(err)
			}
			got := s.OAuth(key)
			if !got.Configured() || got.ClientID != "cid-1" || got.ClientSecret != "shh" {
				t.Fatalf("round-trip lost the configuration: %+v", got)
			}

			// Re-saving with a blank secret is what the settings form submits
			// when the admin edits anything else, since the page never renders
			// the secret back.
			if err := s.SetOAuth(key, name, OAuthConfig{Enabled: true, ClientID: "cid-2"}); err != nil {
				t.Fatal(err)
			}
			if got := s.OAuth(key); got.ClientSecret != "shh" || got.ClientID != "cid-2" {
				t.Fatalf("blank secret must keep the stored one: %+v", got)
			}

			// Switching off keeps the credentials but stops offering the button.
			if err := s.SetOAuth(key, name, OAuthConfig{Enabled: false, ClientID: "cid-2"}); err != nil {
				t.Fatal(err)
			}
			if got := s.OAuth(key); got.Configured() {
				t.Fatal("disabled config must not be Configured")
			} else if got.ClientSecret == "" {
				t.Fatal("disabling must not discard the secret")
			}
		})
	}
}

// TestOAuthConfigsAreIndependent pins the thing a shared settings namespace
// most easily gets wrong: one provider's credentials reading or overwriting
// another's.
func TestOAuthConfigsAreIndependent(t *testing.T) {
	s := newTestState(t)
	if err := s.SetOAuth(providerGitHub, "GitHub", OAuthConfig{Enabled: true, ClientID: "gh-id", ClientSecret: "gh-secret"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetOAuth(providerGoogle, "Google", OAuthConfig{Enabled: true, ClientID: "goog-id", ClientSecret: "goog-secret"}); err != nil {
		t.Fatal(err)
	}
	if got := s.OAuth(providerGitHub); got.ClientID != "gh-id" || got.ClientSecret != "gh-secret" {
		t.Fatalf("google config overwrote github's: %+v", got)
	}
	if got := s.OAuth(providerGoogle); got.ClientID != "goog-id" || got.ClientSecret != "goog-secret" {
		t.Fatalf("github config leaked into google's: %+v", got)
	}

	// Clearing one secret leaves the other alone.
	if err := s.ClearOAuthClientSecret(providerGoogle); err != nil {
		t.Fatal(err)
	}
	if s.OAuth(providerGitHub).ClientSecret != "gh-secret" {
		t.Fatal("clearing google's secret took github's")
	}
	if s.OAuth(providerGoogle).ClientSecret != "" {
		t.Fatal("google's secret survived being cleared")
	}
	// An unknown provider reads as unconfigured rather than as someone else.
	if s.OAuth("nonesuch").Configured() {
		t.Fatal("an unknown provider resolved to a configuration")
	}
}

func TestOAuthConfigRejectsIncompleteEnable(t *testing.T) {
	for _, key := range oauthProviderOrder {
		t.Run(key, func(t *testing.T) {
			s := newTestState(t)
			name := oauthProviders[key].name
			if err := s.SetOAuth(key, name, OAuthConfig{Enabled: true, ClientSecret: "shh"}); err == nil {
				t.Fatal("enabling without a client ID must fail")
			}
			if err := s.SetOAuth(key, name, OAuthConfig{Enabled: true, ClientID: "cid"}); err == nil {
				t.Fatal("enabling with no secret stored and none supplied must fail")
			}
			if s.OAuth(key).Configured() {
				t.Fatal("a rejected save must leave sign-in off")
			}
		})
	}
}

func TestClearOAuthClientSecret(t *testing.T) {
	for _, key := range oauthProviderOrder {
		t.Run(key, func(t *testing.T) {
			s := newTestState(t)
			if err := s.SetOAuth(key, oauthProviders[key].name, OAuthConfig{Enabled: true, ClientID: "id", ClientSecret: "shh"}); err != nil {
				t.Fatal(err)
			}
			if err := s.ClearOAuthClientSecret(key); err != nil {
				t.Fatal(err)
			}
			if got := s.OAuth(key); got.ClientSecret != "" {
				t.Fatal("secret survived being cleared")
			}
		})
	}
}

func TestSMTPRoundTripAndDefaults(t *testing.T) {
	s := newTestState(t)
	if s.SMTP().Configured() {
		t.Fatal("a fresh router must not offer email sign-in")
	}

	// Port omitted: the transport implies it.
	if err := s.SetSMTP(SMTPConfig{
		Enabled: true, Host: "smtp.example.com", From: "llmesh@example.com", Security: "starttls",
	}); err != nil {
		t.Fatal(err)
	}
	got := s.SMTP()
	if got.Port != 587 {
		t.Fatalf("STARTTLS should default to 587, got %d", got.Port)
	}
	if !got.Configured() {
		t.Fatalf("expected configured: %+v", got)
	}

	if err := s.SetSMTP(SMTPConfig{
		Enabled: true, Host: "smtp.example.com", From: "llmesh@example.com", Security: "tls",
	}); err != nil {
		t.Fatal(err)
	}
	if got := s.SMTP(); got.Port != 465 {
		t.Fatalf("implicit TLS should default to 465, got %d", got.Port)
	}

	// An unrecognised security mode falls back rather than being stored.
	if err := s.SetSMTP(SMTPConfig{
		Enabled: true, Host: "h", From: "a@b.com", Security: "carrier-pigeon", Port: 25,
	}); err != nil {
		t.Fatal(err)
	}
	if got := s.SMTP(); got.Security != defaultSMTPSecurity {
		t.Fatalf("unknown security mode should fall back, got %q", got.Security)
	}
}

func TestSMTPPasswordPreservedOnBlankSave(t *testing.T) {
	s := newTestState(t)
	base := SMTPConfig{Enabled: true, Host: "h", Port: 587, From: "a@b.com", Username: "u", Password: "pw", Security: "starttls"}
	if err := s.SetSMTP(base); err != nil {
		t.Fatal(err)
	}
	next := base
	next.Password = ""
	next.Host = "h2"
	if err := s.SetSMTP(next); err != nil {
		t.Fatal(err)
	}
	got := s.SMTP()
	if got.Password != "pw" {
		t.Fatalf("blank password must keep the stored one, got %q", got.Password)
	}
	if got.Host != "h2" {
		t.Fatalf("other fields must still save, got host %q", got.Host)
	}
	if err := s.ClearSMTPPassword(); err != nil {
		t.Fatal(err)
	}
	if s.SMTP().Password != "" {
		t.Fatal("password survived being cleared")
	}
}

func TestSMTPRejectsIncompleteEnable(t *testing.T) {
	s := newTestState(t)
	if err := s.SetSMTP(SMTPConfig{Enabled: true, From: "a@b.com"}); err == nil {
		t.Fatal("enabling without a host must fail")
	}
	if err := s.SetSMTP(SMTPConfig{Enabled: true, Host: "h"}); err == nil {
		t.Fatal("enabling without a From address must fail")
	}
	if err := s.SetSMTP(SMTPConfig{Host: "h", From: "not an address"}); err == nil {
		t.Fatal("an unparseable From must fail")
	}
	if err := s.SetSMTP(SMTPConfig{Host: "h", From: "a@b.com", Port: 70000}); err == nil {
		t.Fatal("an out-of-range port must fail")
	}
}

func TestNormalizeAndValidateEmail(t *testing.T) {
	if got := NormalizeEmail("  Alice@Example.COM "); got != "alice@example.com" {
		t.Fatalf("got %q", got)
	}
	valid := []string{"a@b.com", "first.last+tag@sub.example.co.uk"}
	for _, e := range valid {
		if !ValidEmail(e) {
			t.Errorf("%q should be valid", e)
		}
	}
	// Display-name forms and anything that could break out of a header are
	// refused: an identity has to be the bare address.
	invalid := []string{
		"", "nope", "a@b@c.com", "Alice <a@b.com>",
		"a@b.com\r\nBcc: victim@x.com", "a@b.com, c@d.com", "a b@c.com",
	}
	for _, e := range invalid {
		if ValidEmail(e) {
			t.Errorf("%q should be rejected", e)
		}
	}
}

// TestSchemaUpgradeFromPreIdentityDatabase opens a database written by a build
// that predates federated sign-in entirely. The new columns arrive by ALTER rather than
// by CREATE TABLE on that path, and the unique indexes are built afterwards, so
// it is the path most likely to break an existing deployment on upgrade.
func TestSchemaUpgradeFromPreIdentityDatabase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")

	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`
		CREATE TABLE users (
			username          TEXT PRIMARY KEY,
			password_hash     TEXT NOT NULL DEFAULT '',
			role              TEXT NOT NULL DEFAULT 'member',
			disabled          INTEGER NOT NULL DEFAULT 0,
			csrf_token        TEXT NOT NULL DEFAULT '',
			send_isolation    INTEGER NOT NULL DEFAULT 0,
			receive_isolation INTEGER NOT NULL DEFAULT 0
		);
		INSERT INTO users (username, password_hash, role) VALUES ('alice', 'hash', 'admin');
	`); err != nil {
		t.Fatal(err)
	}
	old.Close()

	s, err := LoadState(path)
	if err != nil {
		t.Fatalf("opening a pre-identity database failed: %v", err)
	}

	u, ok := s.LookupUser("alice")
	if !ok {
		t.Fatal("the existing user did not survive the upgrade")
	}
	if u.Role != "admin" || u.PasswordHash != "hash" {
		t.Fatalf("existing fields were mangled: %+v", u)
	}
	// Password-only sign-in is exactly what it was, with no identity attached.
	if u.Email != "" || u.EmailVerified || u.GitHubUserID != "" || u.GoogleUserID != "" {
		t.Fatalf("upgrade invented an identity: %+v", u)
	}

	// And the new columns are writable, so the indexes were built over them.
	if err := s.UpdateUser("alice", func(usr *User) {
		usr.Email = "alice@example.com"
		usr.EmailVerified = true
		usr.GitHubUserID = "4242"
		usr.GoogleUserID = "sub-4242"
	}); err != nil {
		t.Fatal(err)
	}
	if got, ok := s.LookupUserByVerifiedEmail("alice@example.com"); !ok || got.Username != "alice" {
		t.Fatal("verified email does not resolve after upgrade")
	}
	for provider, id := range map[string]string{providerGitHub: "4242", providerGoogle: "sub-4242"} {
		if got, ok := s.LookupUserByOAuth(provider, id); !ok || got.Username != "alice" {
			t.Fatalf("%s id does not resolve after upgrade", provider)
		}
	}

	// The uniqueness the indexes exist to enforce holds on an upgraded database.
	if err := s.AddUser(User{Username: "bob", Role: "member"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateUser("bob", func(usr *User) { usr.GitHubUserID = "4242" }); err == nil {
		t.Fatal("a duplicate GitHub id was accepted after upgrade")
	}
	if err := s.UpdateUser("bob", func(usr *User) { usr.GoogleUserID = "sub-4242" }); err == nil {
		t.Fatal("a duplicate Google id was accepted after upgrade")
	}
	if err := s.UpdateUser("bob", func(usr *User) {
		usr.Email = "alice@example.com"
		usr.EmailVerified = true
	}); err == nil {
		t.Fatal("a duplicate verified email was accepted after upgrade")
	}
	// An unverified duplicate is fine: it is not an identity.
	if err := s.UpdateUser("bob", func(usr *User) { usr.Email = "alice@example.com" }); err != nil {
		t.Fatalf("an unverified duplicate should be allowed: %v", err)
	}
}

// TestSchemaUpgradeFromGitHubOnlyDatabase opens a database written by the build
// that had GitHub sign-in but not Google. That is the upgrade anyone already
// running the previous branch will perform, and it is the one where the Google
// columns arrive by ALTER on a table that already carries linked identities.
func TestSchemaUpgradeFromGitHubOnlyDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")

	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`
		CREATE TABLE users (
			username          TEXT PRIMARY KEY,
			password_hash     TEXT NOT NULL DEFAULT '',
			role              TEXT NOT NULL DEFAULT 'member',
			disabled          INTEGER NOT NULL DEFAULT 0,
			csrf_token        TEXT NOT NULL DEFAULT '',
			send_isolation    INTEGER NOT NULL DEFAULT 0,
			receive_isolation INTEGER NOT NULL DEFAULT 0,
			email             TEXT NOT NULL DEFAULT '',
			email_verified    INTEGER NOT NULL DEFAULT 0,
			github_user_id    TEXT NOT NULL DEFAULT '',
			github_login      TEXT NOT NULL DEFAULT ''
		);
		CREATE UNIQUE INDEX idx_users_github_user_id ON users(github_user_id) WHERE github_user_id <> '';
		CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL DEFAULT '');
		INSERT INTO users (username, role, email, email_verified, github_user_id, github_login)
			VALUES ('alice', 'admin', 'alice@example.com', 1, '4242', 'octocat');
		INSERT INTO settings (key, value) VALUES
			('auth.github.enabled', '1'),
			('auth.github.client_id', 'iv1.existing'),
			('auth.github.client_secret', 'existing-secret');
	`); err != nil {
		t.Fatal(err)
	}
	old.Close()

	s, err := LoadState(path)
	if err != nil {
		t.Fatalf("opening a github-only database failed: %v", err)
	}

	// The existing link and the existing configuration both survive: the
	// settings keys are already namespaced per provider, so adding one moves
	// nothing.
	if got, ok := s.LookupUserByOAuth(providerGitHub, "4242"); !ok || got.Username != "alice" {
		t.Fatal("an existing GitHub link did not survive the upgrade")
	}
	if got := s.OAuth(providerGitHub); !got.Configured() || got.ClientSecret != "existing-secret" {
		t.Fatalf("existing GitHub configuration lost: %+v", got)
	}
	if got, ok := s.LookupUserByVerifiedEmail("alice@example.com"); !ok || got.Username != "alice" {
		t.Fatal("an existing verified address did not survive the upgrade")
	}

	// Google arrives unconfigured and unlinked, so nothing changes for anyone
	// until an admin sets it up.
	if s.OAuth(providerGoogle).Configured() {
		t.Fatal("the upgrade switched Google sign-in on")
	}
	u, _ := s.LookupUser("alice")
	if u.GoogleUserID != "" || u.GoogleEmail != "" {
		t.Fatalf("the upgrade invented a Google identity: %+v", u)
	}

	// And the new column is usable and unique.
	if err := s.UpdateUser("alice", func(usr *User) { usr.GoogleUserID = "sub-1" }); err != nil {
		t.Fatal(err)
	}
	if err := s.AddUser(User{Username: "bob", Role: "member"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateUser("bob", func(usr *User) { usr.GoogleUserID = "sub-1" }); err == nil {
		t.Fatal("a duplicate Google id was accepted after upgrade")
	}
}
