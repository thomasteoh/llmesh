package admin

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Email sign-in, in two halves.
//
// The first half is a user proving they own an address: they put it on their
// own account under Settings and follow a link sent to it. Until they do, the
// address is a claim and nothing more — it cannot be signed in with, and it
// does not stop anyone else from claiming and verifying the same one.
//
// The second half is signing in with it. Both halves only exist while an admin
// has SMTP configured; with no way to send mail there is no point offering it,
// so the login page does not mention it.

// handleMagicLinkRequest takes an address from the login page and, if it belongs
// to an active account, mails a sign-in link to it.
//
// The response says the same thing either way. Whether an address has an account
// here is not something an unauthenticated caller gets to learn, which is the
// same reason handleLogin answers a missing user and a wrong password alike.
func (a *Admin) handleMagicLinkRequest(w http.ResponseWriter, r *http.Request) {
	cfg := a.state.SMTP()
	if !cfg.Configured() {
		a.renderLogin(w, r, "", "Email sign-in is not configured on this router.")
		return
	}
	if err := r.ParseForm(); err != nil {
		a.renderLogin(w, r, "", "Bad request.")
		return
	}
	email := NormalizeEmail(r.FormValue("email"))
	const sent = "If that address belongs to an account here, a sign-in link is on its way. It expires in 15 minutes."
	if !ValidEmail(email) {
		// A malformed address cannot belong to an account, but saying so would
		// still be a different answer than the one a valid address gets, so it
		// is not treated as a different case.
		a.renderLogin(w, r, sent, "")
		return
	}

	u, found := a.state.LookupUserByVerifiedEmail(email)
	if found && !u.Disabled {
		// Asking for a link invalidates any earlier one, so a link left sitting
		// in an inbox stops working as soon as its owner asks for another.
		a.authTokens.revoke(u.Username, purposeLogin)
		tok := a.authTokens.issue(u.Username, purposeLogin, email, magicLinkTTL)
		link := a.portalBaseURL(r) + "/portal/login/magic/verify?token=" + url.QueryEscape(tok)
		a.state.RecordAudit(u.Username, "auth.magiclink.request", email, a.clientIP(r))
		a.sendAsync(cfg, email, "Sign in to the llmesh portal", magicLinkBody(a.name, u.Username, link))
	} else {
		a.log.Info("admin: magic-link request for unknown or inactive address", "ip", a.clientIP(r))
	}
	a.renderLogin(w, r, sent, "")
}

// handleMagicLinkVerify completes an email sign-in.
//
// The GET only shows a button; the POST behind it spends the token. That split
// exists because mail providers and security appliances routinely fetch every
// link in a message before the recipient sees it, and a GET that signed someone
// in would be spent by a scanner before it reached them.
func (a *Admin) handleMagicLinkVerify(w http.ResponseWriter, r *http.Request) {
	token := r.FormValue("token")
	if r.Method != http.MethodPost {
		if _, ok := a.authTokens.lookup(token, purposeLogin); !ok {
			a.renderLogin(w, r, "", "That sign-in link is invalid or has expired. Request a new one.")
			return
		}
		a.renderStandalone(w, "magic-confirm", magicConfirmPage{Token: token})
		return
	}
	entry, ok := a.authTokens.consume(token, purposeLogin)
	if !ok {
		a.renderLogin(w, r, "", "That sign-in link is invalid or has expired. Request a new one.")
		return
	}
	u, found := a.state.LookupUser(entry.Username)
	// Re-check the address: an account whose verified email changed after the
	// link was sent must not still be reachable through the old one.
	if !found || u.Disabled || !u.EmailVerified || u.Email != entry.Email {
		a.renderLogin(w, r, "", "That sign-in link is no longer valid.")
		return
	}
	if msg := managedElsewhere(u, "email"); msg != "" {
		a.renderLogin(w, r, "", msg)
		return
	}
	a.state.RecordAudit(u.Username, "auth.login.magiclink", entry.Email, a.clientIP(r))
	a.startSession(w, r, u.Username)
	http.Redirect(w, r, "/portal/", http.StatusFound)
}

// magicConfirmPage is the data for the one-button page a sign-in link lands on.
type magicConfirmPage struct {
	Token string
}

// handleEmailUpdate sets or clears the signed-in user's address. A new address
// is stored unverified and a verification link is sent to it; it becomes a
// sign-in identity only once that link is followed.
func (a *Admin) handleEmailUpdate(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	email := NormalizeEmail(r.FormValue("email"))

	if email == "" {
		if err := a.state.UpdateUser(u.Username, func(user *User) {
			user.Email = ""
			user.EmailVerified = false
		}); err != nil {
			a.renderSettings(w, r, u, "", err.Error())
			return
		}
		a.authTokens.revoke(u.Username, purposeLogin)
		a.authTokens.revoke(u.Username, purposeVerify)
		a.state.RecordAudit(u.Username, "auth.email.clear", "", a.clientIP(r))
		a.renderSettings(w, r, u, "Email address removed.", "")
		return
	}
	if !ValidEmail(email) {
		a.renderSettings(w, r, u, "", fmt.Sprintf("%q is not a valid email address.", r.FormValue("email")))
		return
	}
	if email == u.Email && u.EmailVerified {
		a.renderSettings(w, r, u, "That address is already verified.", "")
		return
	}
	// Refuse early with a clear message when the address is already somebody
	// else's verified identity. The unique index would refuse it too, but only
	// at the point of writing, and only in the words SQLite uses.
	if other, taken := a.state.LookupUserByVerifiedEmail(email); taken && other.Username != u.Username {
		a.renderSettings(w, r, u, "", "That email address is already in use by another user.")
		return
	}
	cfg := a.state.SMTP()
	if !cfg.Configured() {
		a.renderSettings(w, r, u, "", "Email sign-in is not configured on this router, so an address cannot be verified.")
		return
	}
	if err := a.state.UpdateUser(u.Username, func(user *User) {
		user.Email = email
		user.EmailVerified = false
	}); err != nil {
		a.renderSettings(w, r, u, "", err.Error())
		return
	}
	// The old address is no longer an identity, so links issued against it go.
	a.authTokens.revoke(u.Username, purposeLogin)
	a.state.RecordAudit(u.Username, "auth.email.set", email, a.clientIP(r))
	a.sendVerification(w, r, cfg, u.Username, email)
}

// handleEmailResend sends a fresh verification link to an address already
// claimed but not yet verified.
func (a *Admin) handleEmailResend(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	fresh, ok := a.state.LookupUser(u.Username)
	if !ok || fresh.Email == "" {
		a.renderSettings(w, r, u, "", "Set an email address first.")
		return
	}
	if fresh.EmailVerified {
		a.renderSettings(w, r, u, "That address is already verified.", "")
		return
	}
	cfg := a.state.SMTP()
	if !cfg.Configured() {
		a.renderSettings(w, r, u, "", "Email sign-in is not configured on this router.")
		return
	}
	a.sendVerification(w, r, cfg, u.Username, fresh.Email)
}

// sendVerification issues a verification link and reports the outcome on the
// settings page. Unlike a sign-in link this one is sent synchronously: the user
// is looking at the page that asked for it, and a relay that rejects the message
// should say so here rather than in a log the user cannot read. There is no
// enumeration concern — the address is one they just typed into their own
// account.
func (a *Admin) sendVerification(w http.ResponseWriter, r *http.Request, cfg SMTPConfig, username, email string) {
	u, _ := a.state.LookupUser(username)
	a.authTokens.revoke(username, purposeVerify)
	tok := a.authTokens.issue(username, purposeVerify, email, verifyLinkTTL)
	link := a.portalBaseURL(r) + "/portal/settings/email/verify?token=" + url.QueryEscape(tok)
	err := sendMail(cfg, email, "Confirm your llmesh portal email address",
		verifyBody(a.name, username, link), time.Now())
	if err != nil {
		a.log.Error("admin: send verification mail", "user", username, "error", err)
		a.renderSettings(w, r, u, "",
			"Address saved, but the verification email could not be sent: "+err.Error())
		return
	}
	a.renderSettings(w, r, u,
		"A verification link has been sent to "+email+". Follow it to finish enabling email sign-in.", "")
}

// handleEmailVerify marks an address verified.
//
// The link is deliberately idempotent and not spent on use, for the same
// link-prefetching reason as the sign-in confirmation page — except that here
// a prefetch doing the work is harmless, since reaching the token at all proves
// control of the inbox. Making it single-use would only mean the user's own
// click reports failure after a scanner already succeeded.
func (a *Admin) handleEmailVerify(w http.ResponseWriter, r *http.Request) {
	entry, ok := a.authTokens.lookup(r.FormValue("token"), purposeVerify)
	if !ok {
		a.renderLogin(w, r, "", "That verification link is invalid or has expired. Request a new one from Settings.")
		return
	}
	u, found := a.state.LookupUser(entry.Username)
	if !found || u.Email != entry.Email {
		a.renderLogin(w, r, "", "That verification link no longer matches the address on the account.")
		return
	}
	if !u.EmailVerified {
		if err := a.state.UpdateUser(u.Username, func(user *User) { user.EmailVerified = true }); err != nil {
			a.renderLogin(w, r, "", err.Error())
			return
		}
		a.state.RecordAudit(u.Username, "auth.email.verify", entry.Email, a.clientIP(r))
	}
	// Whoever followed the link may not be signed in here, so send them to the
	// login page with the good news rather than to a settings page they cannot see.
	if _, signedIn := a.sessionUser(r); signedIn {
		http.Redirect(w, r, "/portal/settings", http.StatusFound)
		return
	}
	a.renderLogin(w, r, entry.Email+" is verified. You can now sign in with an email link.", "")
}

// sendAsync delivers a message without making the caller wait.
//
// A sign-in request must take the same time whether or not the address has an
// account, and handing the work to the relay inline would make "found" visibly
// slower than "not found". A failure has nobody to report to — the page has
// already said the same thing it says in every case — so it goes to the log.
func (a *Admin) sendAsync(cfg SMTPConfig, to, subject, body string) {
	go func() {
		if err := sendMail(cfg, to, subject, body, time.Now()); err != nil {
			a.log.Error("admin: send sign-in mail", "error", err)
		}
	}()
}

func magicLinkBody(routerName, username, link string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "A sign-in link was requested for %s on the llmesh portal", username)
	if routerName != "" {
		fmt.Fprintf(&b, " (%s)", routerName)
	}
	b.WriteString(".\n\nOpen this link to sign in:\n\n")
	b.WriteString(link)
	b.WriteString("\n\nThe link can be used once and expires in 15 minutes.\n" +
		"If you did not request it, no action is needed and nobody has gained access to the account.\n")
	return b.String()
}

func verifyBody(routerName, username, link string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "This address was added to the llmesh portal account %s", username)
	if routerName != "" {
		fmt.Fprintf(&b, " (%s)", routerName)
	}
	b.WriteString(".\n\nConfirm it by opening this link:\n\n")
	b.WriteString(link)
	b.WriteString("\n\nThe link expires in 24 hours.\n" +
		"Until it is followed, the address cannot be used to sign in.\n" +
		"If you did not expect this, you can ignore the message.\n")
	return b.String()
}
