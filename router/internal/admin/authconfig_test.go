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

func TestGitHubAuthRoundTrip(t *testing.T) {
	s := newTestState(t)

	if got := s.GitHubAuth(); got.Configured() {
		t.Fatal("a fresh router must not offer GitHub sign-in")
	}
	if err := s.SetGitHubAuth(GitHubAuthConfig{Enabled: true, ClientID: "iv1.abc", ClientSecret: "shh"}); err != nil {
		t.Fatal(err)
	}
	got := s.GitHubAuth()
	if !got.Configured() || got.ClientID != "iv1.abc" || got.ClientSecret != "shh" {
		t.Fatalf("round-trip lost the configuration: %+v", got)
	}

	// Re-saving with a blank secret is what the settings form submits when the
	// admin edits anything else, since the page never renders the secret back.
	if err := s.SetGitHubAuth(GitHubAuthConfig{Enabled: true, ClientID: "iv1.xyz"}); err != nil {
		t.Fatal(err)
	}
	if got := s.GitHubAuth(); got.ClientSecret != "shh" || got.ClientID != "iv1.xyz" {
		t.Fatalf("blank secret must keep the stored one: %+v", got)
	}

	// Switching off keeps the credentials but stops offering the button.
	if err := s.SetGitHubAuth(GitHubAuthConfig{Enabled: false, ClientID: "iv1.xyz"}); err != nil {
		t.Fatal(err)
	}
	if got := s.GitHubAuth(); got.Configured() {
		t.Fatal("disabled config must not be Configured")
	} else if got.ClientSecret == "" {
		t.Fatal("disabling must not discard the secret")
	}
}

func TestGitHubAuthRejectsIncompleteEnable(t *testing.T) {
	s := newTestState(t)
	if err := s.SetGitHubAuth(GitHubAuthConfig{Enabled: true, ClientSecret: "shh"}); err == nil {
		t.Fatal("enabling without a client ID must fail")
	}
	if err := s.SetGitHubAuth(GitHubAuthConfig{Enabled: true, ClientID: "iv1.abc"}); err == nil {
		t.Fatal("enabling with no secret stored and none supplied must fail")
	}
	if s.GitHubAuth().Configured() {
		t.Fatal("a rejected save must leave sign-in off")
	}
}

func TestClearGitHubClientSecret(t *testing.T) {
	s := newTestState(t)
	if err := s.SetGitHubAuth(GitHubAuthConfig{Enabled: true, ClientID: "id", ClientSecret: "shh"}); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearGitHubClientSecret(); err != nil {
		t.Fatal(err)
	}
	if got := s.GitHubAuth(); got.ClientSecret != "" {
		t.Fatal("secret survived being cleared")
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
// that predates federated sign-in. The new columns arrive by ALTER rather than
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
	if u.Email != "" || u.EmailVerified || u.GitHubUserID != "" {
		t.Fatalf("upgrade invented an identity: %+v", u)
	}

	// And the new columns are writable, so the indexes were built over them.
	if err := s.UpdateUser("alice", func(usr *User) {
		usr.Email = "alice@example.com"
		usr.EmailVerified = true
		usr.GitHubUserID = "4242"
	}); err != nil {
		t.Fatal(err)
	}
	if got, ok := s.LookupUserByVerifiedEmail("alice@example.com"); !ok || got.Username != "alice" {
		t.Fatal("verified email does not resolve after upgrade")
	}
	if got, ok := s.LookupUserByGitHubID("4242"); !ok || got.Username != "alice" {
		t.Fatal("GitHub id does not resolve after upgrade")
	}

	// The uniqueness the indexes exist to enforce holds on an upgraded database.
	if err := s.AddUser(User{Username: "bob", Role: "member"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateUser("bob", func(usr *User) { usr.GitHubUserID = "4242" }); err == nil {
		t.Fatal("a duplicate GitHub id was accepted after upgrade")
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
