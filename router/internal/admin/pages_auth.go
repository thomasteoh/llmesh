package admin

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Admin-facing configuration of the two alternative sign-in methods.
//
// Both forms follow the same rule for their secret field: blank means "leave
// the stored one alone". The page never renders a secret back, so a blank field
// is what an admin editing any other setting will submit, and reading it as
// "erase it" would silently break sign-in. Erasing is its own explicit action.

func (a *Admin) handleGitHubAuthUpdate(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if r.FormValue("clear_secret") != "" {
		if err := a.state.ClearGitHubClientSecret(); err != nil {
			a.renderSettings(w, r, u, "", err.Error())
			return
		}
		// A configuration with no secret cannot complete a sign-in, so leaving
		// it switched on would offer users a button that always fails.
		if err := a.state.SetGitHubAuth(GitHubAuthConfig{
			Enabled:  false,
			ClientID: a.state.GitHubAuth().ClientID,
		}); err != nil {
			a.renderSettings(w, r, u, "", err.Error())
			return
		}
		a.state.RecordAudit(u.Username, "settings.auth.github.clear_secret", "", a.clientIP(r))
		a.renderSettings(w, r, u, "GitHub client secret cleared and GitHub sign-in switched off.", "")
		return
	}

	cfg := GitHubAuthConfig{
		Enabled:      r.FormValue("enabled") != "",
		ClientID:     r.FormValue("client_id"),
		ClientSecret: r.FormValue("client_secret"),
	}
	if err := a.state.SetGitHubAuth(cfg); err != nil {
		a.renderSettings(w, r, u, "", err.Error())
		return
	}
	// The secret is deliberately absent from the audit target.
	a.state.RecordAudit(u.Username, "settings.auth.github",
		"enabled="+strconv.FormatBool(cfg.Enabled), a.clientIP(r))
	a.log.Info("admin: github sign-in settings updated", "actor", u.Username, "enabled", cfg.Enabled)
	if cfg.Enabled {
		a.renderSettings(w, r, u, "GitHub sign-in is on. Users can link their account under Sign-in methods.", "")
		return
	}
	a.renderSettings(w, r, u, "GitHub sign-in settings saved.", "")
}

func (a *Admin) handleSMTPUpdate(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if r.FormValue("clear_password") != "" {
		if err := a.state.ClearSMTPPassword(); err != nil {
			a.renderSettings(w, r, u, "", err.Error())
			return
		}
		a.state.RecordAudit(u.Username, "settings.auth.smtp.clear_password", "", a.clientIP(r))
		a.renderSettings(w, r, u, "SMTP password cleared.", "")
		return
	}

	// An unparseable port becomes 0, which SetSMTP reads as "use the default
	// for this transport" — the same as leaving the field empty.
	port, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("port")))
	cfg := SMTPConfig{
		Enabled:  r.FormValue("enabled") != "",
		Host:     r.FormValue("host"),
		Port:     port,
		Username: r.FormValue("username"),
		Password: r.FormValue("password"),
		From:     r.FormValue("from"),
		Security: r.FormValue("security"),
	}
	if err := a.state.SetSMTP(cfg); err != nil {
		a.renderSettings(w, r, u, "", err.Error())
		return
	}
	a.state.RecordAudit(u.Username, "settings.auth.smtp",
		"enabled="+strconv.FormatBool(cfg.Enabled), a.clientIP(r))
	a.log.Info("admin: smtp settings updated", "actor", u.Username, "enabled", cfg.Enabled, "host", cfg.Host)
	if cfg.Enabled {
		a.renderSettings(w, r, u, "Email sign-in is on. Send a test message to confirm the relay works.", "")
		return
	}
	a.renderSettings(w, r, u, "SMTP settings saved.", "")
}

// handleSMTPTest sends one message to an address the admin names, so a relay
// that rejects mail says so here rather than the first time a user is waiting
// on a sign-in link that never comes. It reports the relay's own error, which
// is the only thing that makes an SMTP misconfiguration diagnosable.
func (a *Admin) handleSMTPTest(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	cfg := a.state.SMTP()
	if cfg.Host == "" {
		a.renderSettings(w, r, u, "", "Save an SMTP host before sending a test message.")
		return
	}
	to := NormalizeEmail(r.FormValue("to"))
	if to == "" {
		// Default to the admin's own address when they have one; the common
		// case is checking that mail arrives at all.
		if fresh, ok := a.state.LookupUser(u.Username); ok {
			to = fresh.Email
		}
	}
	if !ValidEmail(to) {
		a.renderSettings(w, r, u, "", "Enter an address to send the test message to.")
		return
	}
	body := "This is a test message from the llmesh portal"
	if a.name != "" {
		body += " (" + a.name + ")"
	}
	body += ".\n\nIf you are reading it, sign-in links will reach their recipients.\n"
	if err := sendMail(cfg, to, "llmesh portal test message", body, time.Now()); err != nil {
		a.log.Error("admin: smtp test", "actor", u.Username, "error", err)
		a.renderSettings(w, r, u, "", "Test message failed: "+err.Error())
		return
	}
	a.state.RecordAudit(u.Username, "settings.auth.smtp.test", to, a.clientIP(r))
	a.renderSettings(w, r, u, "Test message sent to "+to+".", "")
}
