package admin

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

// newMailTestAdmin returns an Admin with email sign-in configured against a
// fake relay whose messages the test can read.
func newMailTestAdmin(t *testing.T) (*Admin, *fakeSMTP) {
	t.Helper()
	a := newTestAdmin(t)
	f := startFakeSMTP(t)
	if err := a.state.SetSMTP(f.config()); err != nil {
		t.Fatal(err)
	}
	return a, f
}

// waitForMessages polls until n messages have arrived, since sign-in mail is
// sent off the request path on purpose.
func waitForMessages(t *testing.T, f *fakeSMTP, n int) []string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if msgs := f.messages(); len(msgs) >= n {
			return msgs
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("expected %d messages, got %d", n, len(f.messages()))
	return nil
}

// assertNoMessages gives an async send a fair chance to happen before
// concluding that it did not.
func assertNoMessages(t *testing.T, f *fakeSMTP) {
	t.Helper()
	time.Sleep(150 * time.Millisecond)
	if msgs := f.messages(); len(msgs) != 0 {
		t.Fatalf("expected no mail, got %d messages:\n%s", len(msgs), strings.Join(msgs, "\n--\n"))
	}
}

var linkRe = regexp.MustCompile(`https?://[^\s]+token=([^\s]+)`)

func tokenFromMessage(t *testing.T, msg string) string {
	t.Helper()
	m := linkRe.FindStringSubmatch(msg)
	if m == nil {
		t.Fatalf("no link found in message:\n%s", msg)
	}
	tok, err := url.QueryUnescape(m[1])
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// verifyEmail puts an address on a user and completes verification, the way a
// user does from their own settings page.
func verifyEmail(t *testing.T, a *Admin, f *fakeSMTP, username, email string) {
	t.Helper()
	before := len(f.messages())
	session := signIn(a, username)
	form := url.Values{"email": {email}}
	req := httptest.NewRequest("POST", "/portal/settings/email", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(session)
	rr := httptest.NewRecorder()
	a.requireAuth(a.handleEmailUpdate)(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("set email: got %d: %s", rr.Code, rr.Body.String())
	}
	msgs := waitForMessages(t, f, before+1)
	tok := tokenFromMessage(t, msgs[len(msgs)-1])

	vr := httptest.NewRequest("GET", "/portal/settings/email/verify?token="+url.QueryEscape(tok), nil)
	vrr := httptest.NewRecorder()
	a.handleEmailVerify(vrr, vr)
	u, _ := a.state.LookupUser(username)
	if !u.EmailVerified || u.Email != email {
		t.Fatalf("verification did not take: %+v (page said %s)", u, vrr.Body.String())
	}
}

// requestMagicLink posts the login page's email form.
func requestMagicLink(t *testing.T, a *Admin, email string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{"email": {email}}
	req := httptest.NewRequest("POST", "/portal/login/magic", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	a.handleMagicLinkRequest(rr, req)
	return rr
}

func TestLoginPageOffersOnlyConfiguredMethods(t *testing.T) {
	a := newTestAdmin(t)
	addTestUser(t, a, "alice", "admin")

	render := func() string {
		rr := httptest.NewRecorder()
		a.handleLogin(rr, httptest.NewRequest("GET", "/portal/login", nil))
		return rr.Body.String()
	}

	body := render()
	if strings.Contains(body, "Continue with GitHub") || strings.Contains(body, "Email me a sign-in link") {
		t.Fatalf("an unconfigured router offered an alternative sign-in:\n%s", body)
	}

	if err := a.state.SetGitHubAuth(GitHubAuthConfig{Enabled: true, ClientID: "id", ClientSecret: "shh"}); err != nil {
		t.Fatal(err)
	}
	body = render()
	if !strings.Contains(body, "Continue with GitHub") {
		t.Fatal("GitHub sign-in configured but not offered")
	}
	if strings.Contains(body, "Email me a sign-in link") {
		t.Fatal("email sign-in offered without SMTP")
	}

	f := startFakeSMTP(t)
	if err := a.state.SetSMTP(f.config()); err != nil {
		t.Fatal(err)
	}
	body = render()
	if !strings.Contains(body, "Email me a sign-in link") {
		t.Fatal("SMTP configured but email sign-in not offered")
	}

	// Switching a method off hides it again without discarding its settings.
	if err := a.state.SetGitHubAuth(GitHubAuthConfig{Enabled: false, ClientID: "id"}); err != nil {
		t.Fatal(err)
	}
	if body = render(); strings.Contains(body, "Continue with GitHub") {
		t.Fatal("a disabled method is still offered")
	}
}

func TestMagicLinkEndToEnd(t *testing.T) {
	a, f := newMailTestAdmin(t)
	addTestUser(t, a, "alice", "member")
	verifyEmail(t, a, f, "alice", "alice@example.com")

	before := len(f.messages())
	requestMagicLink(t, a, "Alice@Example.com") // case should not matter
	msgs := waitForMessages(t, f, before+1)
	tok := tokenFromMessage(t, msgs[len(msgs)-1])

	// Following the link shows a confirmation, and must NOT spend the token —
	// mail scanners fetch links before their recipients do.
	get := httptest.NewRequest("GET", "/portal/login/magic/verify?token="+url.QueryEscape(tok), nil)
	grr := httptest.NewRecorder()
	a.handleMagicLinkVerify(grr, get)
	if !strings.Contains(grr.Body.String(), "Confirm sign-in") {
		t.Fatalf("expected a confirmation page, got:\n%s", grr.Body.String())
	}
	for _, c := range grr.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			t.Fatal("a GET on the link signed the user in")
		}
	}

	// The button on that page does spend it.
	post := func() *httptest.ResponseRecorder {
		form := url.Values{"token": {tok}}
		req := httptest.NewRequest("POST", "/portal/login/magic/verify", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rr := httptest.NewRecorder()
		a.handleMagicLinkVerify(rr, req)
		return rr
	}
	rr := post()
	if rr.Code != http.StatusFound || rr.Header().Get("Location") != "/portal/" {
		t.Fatalf("expected sign-in, got %d %q: %s", rr.Code, rr.Header().Get("Location"), rr.Body.String())
	}
	var sess *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == sessionCookie {
			sess = c
		}
	}
	if sess == nil {
		t.Fatal("no session cookie issued")
	}
	if name, ok := a.sessions.lookup(sess.Value); !ok || name != "alice" {
		t.Fatalf("session is for %q (ok=%v)", name, ok)
	}

	// And it is spent.
	if rr := post(); !strings.Contains(rr.Body.String(), "invalid or has expired") {
		t.Fatalf("a sign-in link worked twice:\n%s", rr.Body.String())
	}
}

func TestMagicLinkDoesNotRevealWhoHasAnAccount(t *testing.T) {
	a, f := newMailTestAdmin(t)
	addTestUser(t, a, "alice", "member")
	verifyEmail(t, a, f, "alice", "alice@example.com")

	known := requestMagicLink(t, a, "alice@example.com").Body.String()
	waitForMessages(t, f, len(f.messages())+0) // let the send settle
	unknown := requestMagicLink(t, a, "nobody@example.com").Body.String()
	malformed := requestMagicLink(t, a, "not-an-address").Body.String()

	if known != unknown || known != malformed {
		t.Fatal("the response differs by whether the address has an account, which enumerates users")
	}
	if !strings.Contains(known, "If that address belongs to an account here") {
		t.Fatalf("unexpected response:\n%s", known)
	}
}

func TestMagicLinkNotSentToUnverifiedOrDisabled(t *testing.T) {
	a, f := newMailTestAdmin(t)
	addTestUser(t, a, "alice", "member")

	// Claimed but never verified: someone could have typed anyone's address.
	if err := a.state.UpdateUser("alice", func(u *User) { u.Email = "alice@example.com" }); err != nil {
		t.Fatal(err)
	}
	requestMagicLink(t, a, "alice@example.com")
	assertNoMessages(t, f)

	// Verified, then disabled.
	verifyEmail(t, a, f, "alice", "alice@example.com")
	if err := a.state.UpdateUser("alice", func(u *User) { u.Disabled = true }); err != nil {
		t.Fatal(err)
	}
	before := len(f.messages())
	requestMagicLink(t, a, "alice@example.com")
	time.Sleep(150 * time.Millisecond)
	if len(f.messages()) != before {
		t.Fatal("a disabled account was sent a sign-in link")
	}
}

func TestMagicLinkRefusedWhenSMTPNotConfigured(t *testing.T) {
	a := newTestAdmin(t)
	rr := requestMagicLink(t, a, "alice@example.com")
	if !strings.Contains(rr.Body.String(), "not configured") {
		t.Fatalf("expected a refusal, got:\n%s", rr.Body.String())
	}
}

func TestRequestingANewLinkInvalidatesTheOld(t *testing.T) {
	a, f := newMailTestAdmin(t)
	addTestUser(t, a, "alice", "member")
	verifyEmail(t, a, f, "alice", "alice@example.com")

	n := len(f.messages())
	requestMagicLink(t, a, "alice@example.com")
	first := tokenFromMessage(t, waitForMessages(t, f, n+1)[n])
	requestMagicLink(t, a, "alice@example.com")
	second := tokenFromMessage(t, waitForMessages(t, f, n+2)[n+1])

	if first == second {
		t.Fatal("the same token was issued twice")
	}
	if _, ok := a.authTokens.lookup(first, purposeLogin); ok {
		t.Fatal("the older link still works")
	}
	if _, ok := a.authTokens.lookup(second, purposeLogin); !ok {
		t.Fatal("the newest link does not work")
	}
}

func TestChangingEmailInvalidatesOutstandingLinks(t *testing.T) {
	a, f := newMailTestAdmin(t)
	addTestUser(t, a, "alice", "member")
	verifyEmail(t, a, f, "alice", "alice@example.com")

	n := len(f.messages())
	requestMagicLink(t, a, "alice@example.com")
	tok := tokenFromMessage(t, waitForMessages(t, f, n+1)[n])

	// Moving the address off the account must strip the old one of its power,
	// or losing control of a mailbox would not be recoverable by changing it.
	session := signIn(a, "alice")
	form := url.Values{"email": {"alice2@example.com"}}
	req := httptest.NewRequest("POST", "/portal/settings/email", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(session)
	a.requireAuth(a.handleEmailUpdate)(httptest.NewRecorder(), req)

	if _, ok := a.authTokens.lookup(tok, purposeLogin); ok {
		t.Fatal("a sign-in link for the old address survived the change")
	}
	if u, _ := a.state.LookupUser("alice"); u.EmailVerified {
		t.Fatal("the new address was treated as already verified")
	}
}

func TestEmailCannotBeVerifiedTwiceByDifferentUsers(t *testing.T) {
	a, f := newMailTestAdmin(t)
	addTestUser(t, a, "alice", "member")
	addTestUser(t, a, "bob", "member")
	verifyEmail(t, a, f, "alice", "shared@example.com")

	session := signIn(a, "bob")
	form := url.Values{"email": {"shared@example.com"}}
	req := httptest.NewRequest("POST", "/portal/settings/email", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(session)
	rr := httptest.NewRecorder()
	a.requireAuth(a.handleEmailUpdate)(rr, req)

	if !strings.Contains(rr.Body.String(), "already in use by another user") {
		t.Fatalf("expected the claim to be refused, got:\n%s", rr.Body.String())
	}
	if u, _ := a.state.LookupUser("bob"); u.Email != "" {
		t.Fatalf("bob claimed a verified address: %+v", u)
	}
	// The address still resolves to exactly one account.
	if u, ok := a.state.LookupUserByVerifiedEmail("shared@example.com"); !ok || u.Username != "alice" {
		t.Fatal("the shared address stopped resolving to alice")
	}
}

func TestEmailVerificationIsIdempotent(t *testing.T) {
	// A mail scanner following the link first must not leave the user's own
	// click reporting failure.
	a, f := newMailTestAdmin(t)
	addTestUser(t, a, "alice", "member")

	session := signIn(a, "alice")
	form := url.Values{"email": {"alice@example.com"}}
	req := httptest.NewRequest("POST", "/portal/settings/email", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(session)
	a.requireAuth(a.handleEmailUpdate)(httptest.NewRecorder(), req)

	tok := tokenFromMessage(t, waitForMessages(t, f, 1)[0])
	for i := range 2 {
		rr := httptest.NewRecorder()
		a.handleEmailVerify(rr, httptest.NewRequest("GET", "/portal/settings/email/verify?token="+url.QueryEscape(tok), nil))
		if strings.Contains(rr.Body.String(), "invalid or has expired") {
			t.Fatalf("visit %d reported the link as spent:\n%s", i+1, rr.Body.String())
		}
	}
	if u, _ := a.state.LookupUser("alice"); !u.EmailVerified {
		t.Fatal("address not verified")
	}
}

func TestClearingEmailRemovesTheIdentity(t *testing.T) {
	a, f := newMailTestAdmin(t)
	addTestUser(t, a, "alice", "member")
	verifyEmail(t, a, f, "alice", "alice@example.com")

	session := signIn(a, "alice")
	form := url.Values{"email": {""}}
	req := httptest.NewRequest("POST", "/portal/settings/email", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(session)
	rr := httptest.NewRecorder()
	a.requireAuth(a.handleEmailUpdate)(rr, req)

	u, _ := a.state.LookupUser("alice")
	if u.Email != "" || u.EmailVerified {
		t.Fatalf("clearing left the identity behind: %+v", u)
	}
	if _, ok := a.state.LookupUserByVerifiedEmail("alice@example.com"); ok {
		t.Fatal("the cleared address still resolves to an account")
	}
}

func TestEmailUpdateRejectsMalformedAddress(t *testing.T) {
	a, _ := newMailTestAdmin(t)
	addTestUser(t, a, "alice", "member")
	session := signIn(a, "alice")
	form := url.Values{"email": {"alice@example.com\r\nBcc: victim@x.com"}}
	req := httptest.NewRequest("POST", "/portal/settings/email", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(session)
	rr := httptest.NewRecorder()
	a.requireAuth(a.handleEmailUpdate)(rr, req)

	if !strings.Contains(rr.Body.String(), "not a valid email address") {
		t.Fatalf("expected a rejection, got:\n%s", rr.Body.String())
	}
	if u, _ := a.state.LookupUser("alice"); u.Email != "" {
		t.Fatalf("a malformed address was stored: %q", u.Email)
	}
}
