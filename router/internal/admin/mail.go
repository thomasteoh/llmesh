package admin

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// smtpDialTimeout bounds every stage of a send. A relay that accepts the
// connection and then stalls would otherwise hold a portal request open for as
// long as it liked, and the user is waiting on that request to be told their
// link is on its way.
const smtpDialTimeout = 15 * time.Second

// sendMail delivers one message through the configured relay. It is called from
// request handlers, so it is deliberately synchronous and short-deadlined: the
// admin saving SMTP settings wants to know now whether they work, and a user
// asking for a sign-in link should not be told one was sent if it was not.
func sendMail(cfg SMTPConfig, to, subject, body string, now time.Time) error {
	if cfg.Host == "" || cfg.Port == 0 {
		return fmt.Errorf("SMTP is not configured")
	}
	if !ValidEmail(to) {
		return fmt.Errorf("invalid recipient address")
	}
	from := cfg.From
	if !ValidEmail(from) {
		return fmt.Errorf("invalid From address %q", from)
	}

	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	dialer := &net.Dialer{Timeout: smtpDialTimeout}
	tlsCfg := &tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12}

	var conn net.Conn
	var err error
	if cfg.Security == "tls" {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, tlsCfg)
	} else {
		conn, err = dialer.Dial("tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("connect to %s: %w", addr, err)
	}
	// A deadline on the raw connection covers every later stage, since net/smtp
	// offers no per-command timeout of its own.
	_ = conn.SetDeadline(now.Add(smtpDialTimeout))

	c, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("SMTP handshake: %w", err)
	}
	defer c.Close()

	if cfg.Security == "starttls" {
		ok, _ := c.Extension("STARTTLS")
		if !ok {
			return fmt.Errorf("server at %s does not offer STARTTLS; choose a different transport security", addr)
		}
		if err := c.StartTLS(tlsCfg); err != nil {
			return fmt.Errorf("STARTTLS: %w", err)
		}
	}

	if cfg.Username != "" {
		auth := smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)
		if err := c.Auth(auth); err != nil {
			// net/smtp refuses PLAIN over an unencrypted link; say so plainly
			// rather than leaving the admin to read the wrapped error.
			return fmt.Errorf("SMTP auth: %w", err)
		}
	}

	if err := c.Mail(from); err != nil {
		return fmt.Errorf("MAIL FROM: %w", err)
	}
	if err := c.Rcpt(to); err != nil {
		return fmt.Errorf("RCPT TO %s: %w", to, err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("DATA: %w", err)
	}
	if _, err := w.Write([]byte(composeMessage(from, to, subject, body, now))); err != nil {
		w.Close()
		return fmt.Errorf("write message: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("finish message: %w", err)
	}
	return c.Quit()
}

// composeMessage builds an RFC 5322 plain-text message. Header values are
// stripped of CR and LF: the subject is ours today, but a header that can carry
// a newline is a header-injection bug waiting for the first caller who passes
// something a user typed.
func composeMessage(from, to, subject, body string, now time.Time) string {
	var b strings.Builder
	b.WriteString("From: " + headerSafe(from) + "\r\n")
	b.WriteString("To: " + headerSafe(to) + "\r\n")
	b.WriteString("Subject: " + headerSafe(subject) + "\r\n")
	b.WriteString("Date: " + now.Format(time.RFC1123Z) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("Auto-Submitted: auto-generated\r\n")
	b.WriteString("\r\n")
	// Normalise to CRLF and dot-stuff, so a body line of "." cannot end the
	// message early.
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		if strings.HasPrefix(line, ".") {
			line = "." + line
		}
		b.WriteString(line + "\r\n")
	}
	return b.String()
}

func headerSafe(s string) string {
	return strings.NewReplacer("\r", "", "\n", "").Replace(s)
}
