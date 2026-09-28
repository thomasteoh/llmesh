package admin

import (
	"fmt"
	"net/mail"
	"strconv"
	"strings"
)

// Alternative sign-in methods are configured here rather than in the config
// file, because both are set up by an admin in the portal at a point where
// restarting the router to add a client id or an SMTP host would be a poor
// trade. Each is stored in the settings table and each is inert until an admin
// both fills it in and switches it on — a half-entered GitHub app never shows
// up as a sign-in button on the login page.

// oauthSettingKeys returns the settings-table keys holding one provider's
// credentials. Each provider gets its own namespace under auth.<provider>.
func oauthSettingKeys(provider string) (enabled, clientID, clientSecret string) {
	base := "auth." + provider + "."
	return base + "enabled", base + "client_id", base + "client_secret"
}

const (
	smtpEnabledKey  = "auth.smtp.enabled"
	smtpHostKey     = "auth.smtp.host"
	smtpPortKey     = "auth.smtp.port"
	smtpUsernameKey = "auth.smtp.username"
	smtpPasswordKey = "auth.smtp.password"
	smtpFromKey     = "auth.smtp.from"
	smtpSecurityKey = "auth.smtp.security"
)

// OAuthConfig is the OAuth app an admin registered with one provider.
//
// ClientSecret is held in the settings table in plaintext, as upstream router
// tokens already are: the router has no key to encrypt it under that it does
// not also store beside it. It is never rendered back to the portal, never
// logged, and the database file is expected to be protected accordingly.
type OAuthConfig struct {
	Enabled      bool
	ClientID     string
	ClientSecret string
}

// Configured reports whether this provider's sign-in should be offered. Both
// halves matter: switching it off leaves the credentials in place for later,
// and credentials alone are not a decision to turn it on.
func (c OAuthConfig) Configured() bool {
	return c.Enabled && c.ClientID != "" && c.ClientSecret != ""
}

// SMTPConfig is the mail relay used to send sign-in and verification links.
// Password carries the same caveat as GitHubAuthConfig.ClientSecret.
type SMTPConfig struct {
	Enabled  bool
	Host     string
	Port     int
	Username string
	Password string
	From     string
	// Security is "starttls" (submission on 587), "tls" (implicit TLS on 465),
	// or "none". It is validated on write, so readers can switch on it directly.
	Security string
}

// Configured reports whether email sign-in should be offered.
func (c SMTPConfig) Configured() bool {
	return c.Enabled && c.Host != "" && c.Port > 0 && c.From != ""
}

// smtpSecurityModes is the set of accepted Security values, and the default.
var smtpSecurityModes = map[string]bool{"starttls": true, "tls": true, "none": true}

const defaultSMTPSecurity = "starttls"

// setting reads a single settings row, returning "" when absent.
func (s *State) setting(key string) string {
	var v string
	s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	return v
}

// settings reads several settings rows in one query. Keys with no row are
// absent from the result, so callers keep their own defaults.
func (s *State) settings(keys ...string) map[string]string {
	out := make(map[string]string, len(keys))
	if len(keys) == 0 {
		return out
	}
	args := make([]any, len(keys))
	for i, k := range keys {
		args[i] = k
	}
	q := `SELECT key, value FROM settings WHERE key IN (?` + strings.Repeat(`, ?`, len(keys)-1) + `)`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err == nil {
			out[k] = v
		}
	}
	return out
}

// putSettings upserts several settings rows in one transaction, so a partly
// applied configuration cannot be left behind by a mid-write failure.
func (s *State) putSettings(kv map[string]string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for k, v := range kv {
		if _, err := tx.Exec(
			`INSERT INTO settings (key, value) VALUES (?, ?)
			 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, k, v,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// OAuth returns one provider's stored configuration.
func (s *State) OAuth(provider string) OAuthConfig {
	enabledKey, idKey, secretKey := oauthSettingKeys(provider)
	v := s.settings(enabledKey, idKey, secretKey)
	return OAuthConfig{
		Enabled:      v[enabledKey] == "1",
		ClientID:     v[idKey],
		ClientSecret: v[secretKey],
	}
}

// SetOAuth stores one provider's configuration. An empty ClientSecret keeps the
// stored one, so an admin can re-save the form — which never shows the secret
// back to them — without blanking it.
//
// providerName is used only in error messages, so they name the provider the
// admin is looking at rather than its URL key.
func (s *State) SetOAuth(provider, providerName string, c OAuthConfig) error {
	enabledKey, idKey, secretKey := oauthSettingKeys(provider)
	c.ClientID = strings.TrimSpace(c.ClientID)
	c.ClientSecret = strings.TrimSpace(c.ClientSecret)
	kv := map[string]string{
		enabledKey: boolSetting(c.Enabled),
		idKey:      c.ClientID,
	}
	if c.ClientSecret != "" {
		kv[secretKey] = c.ClientSecret
	}
	if c.Enabled {
		if c.ClientID == "" {
			return fmt.Errorf("a client ID is required to enable %s sign-in", providerName)
		}
		if c.ClientSecret == "" && s.setting(secretKey) == "" {
			return fmt.Errorf("a client secret is required to enable %s sign-in", providerName)
		}
	}
	return s.putSettings(kv)
}

// ClearOAuthClientSecret forgets a provider's stored secret, for rotating an
// app or decommissioning one. Enabling that sign-in again requires a new secret.
func (s *State) ClearOAuthClientSecret(provider string) error {
	_, _, secretKey := oauthSettingKeys(provider)
	_, err := s.db.Exec(`DELETE FROM settings WHERE key = ?`, secretKey)
	return err
}

// SMTP returns the stored mail configuration.
func (s *State) SMTP() SMTPConfig {
	v := s.settings(smtpEnabledKey, smtpHostKey, smtpPortKey, smtpUsernameKey,
		smtpPasswordKey, smtpFromKey, smtpSecurityKey)
	port, _ := strconv.Atoi(v[smtpPortKey])
	security := v[smtpSecurityKey]
	if !smtpSecurityModes[security] {
		security = defaultSMTPSecurity
	}
	return SMTPConfig{
		Enabled:  v[smtpEnabledKey] == "1",
		Host:     v[smtpHostKey],
		Port:     port,
		Username: v[smtpUsernameKey],
		Password: v[smtpPasswordKey],
		From:     v[smtpFromKey],
		Security: security,
	}
}

// SetSMTP stores the mail configuration. As with the GitHub secret, an empty
// Password keeps the stored one.
func (s *State) SetSMTP(c SMTPConfig) error {
	c.Host = strings.TrimSpace(c.Host)
	c.From = strings.TrimSpace(c.From)
	c.Username = strings.TrimSpace(c.Username)
	if !smtpSecurityModes[c.Security] {
		c.Security = defaultSMTPSecurity
	}
	if c.Port == 0 {
		// Default to the port implied by the transport rather than rejecting a
		// blank field: the pairing is fixed and guessing it is not a kindness.
		if c.Security == "tls" {
			c.Port = 465
		} else {
			c.Port = 587
		}
	}
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("invalid SMTP port %d", c.Port)
	}
	if c.From != "" {
		if _, err := mail.ParseAddress(c.From); err != nil {
			return fmt.Errorf("invalid From address %q", c.From)
		}
	}
	if c.Enabled {
		if c.Host == "" {
			return fmt.Errorf("an SMTP host is required to enable email sign-in")
		}
		if c.From == "" {
			return fmt.Errorf("a From address is required to enable email sign-in")
		}
	}
	kv := map[string]string{
		smtpEnabledKey:  boolSetting(c.Enabled),
		smtpHostKey:     c.Host,
		smtpPortKey:     strconv.Itoa(c.Port),
		smtpUsernameKey: c.Username,
		smtpFromKey:     c.From,
		smtpSecurityKey: c.Security,
	}
	if c.Password != "" {
		kv[smtpPasswordKey] = c.Password
	}
	return s.putSettings(kv)
}

// ClearSMTPPassword forgets the stored password, for relays that take none.
func (s *State) ClearSMTPPassword() error {
	_, err := s.db.Exec(`DELETE FROM settings WHERE key = ?`, smtpPasswordKey)
	return err
}

func boolSetting(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// NormalizeEmail lowercases and trims an address so that the same mailbox typed
// two ways is one identity. The local part is technically case-sensitive per
// RFC 5321, but no mail provider in practice treats it as such, and honouring
// that would let Alice@x and alice@x be two accounts for one inbox.
func NormalizeEmail(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// ValidEmail reports whether an address is one we are willing to send to. It
// rejects the display-name forms mail.ParseAddress accepts ("A <a@b>"), since
// an identity has to be the bare address.
func ValidEmail(s string) bool {
	if s == "" || strings.ContainsAny(s, " <>,;\r\n\"") {
		return false
	}
	addr, err := mail.ParseAddress(s)
	if err != nil {
		return false
	}
	return addr.Address == s && strings.Count(s, "@") == 1
}
