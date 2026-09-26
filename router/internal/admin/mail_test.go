package admin

import (
	"bufio"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestComposeMessage(t *testing.T) {
	when := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	msg := composeMessage("from@x.com", "to@y.com", "Sign in", "hello\nworld", when)

	for _, want := range []string{
		"From: from@x.com\r\n",
		"To: to@y.com\r\n",
		"Subject: Sign in\r\n",
		"Content-Type: text/plain; charset=utf-8\r\n",
		"\r\n\r\nhello\r\nworld\r\n",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q\n---\n%s", want, msg)
		}
	}
	if !strings.Contains(msg, "Date: Wed, 04 Mar 2026 05:06:07 +0000") {
		t.Errorf("message missing the expected Date header\n---\n%s", msg)
	}
}

func TestComposeMessageRejectsHeaderInjection(t *testing.T) {
	// A newline reaching a header would let a caller append headers of their
	// own — a Bcc, most obviously.
	msg := composeMessage("from@x.com", "to@y.com", "Hi\r\nBcc: victim@z.com", "body", time.Now())
	head, _, _ := strings.Cut(msg, "\r\n\r\n")
	for _, line := range strings.Split(head, "\r\n") {
		if strings.HasPrefix(line, "Bcc:") {
			t.Fatalf("header injection succeeded:\n%s", head)
		}
	}
	if !strings.Contains(head, "Subject: HiBcc: victim@z.com\r\n") {
		t.Fatalf("expected the newline to be stripped, got:\n%s", head)
	}
}

func TestComposeMessageDotStuffs(t *testing.T) {
	// A body line of "." would otherwise end the DATA command early and
	// truncate the message.
	msg := composeMessage("f@x.com", "t@y.com", "s", "before\n.\nafter", time.Now())
	if !strings.Contains(msg, "\r\n..\r\n") {
		t.Fatalf("leading dot not stuffed:\n%q", msg)
	}
	if !strings.Contains(msg, "after") {
		t.Fatal("body truncated")
	}
}

// fakeSMTP is a minimal server that speaks just enough of the protocol for
// net/smtp to deliver one message, so the send path is exercised end to end
// rather than only its message formatting.
type fakeSMTP struct {
	addr     string
	mu       sync.Mutex
	received []string
	ln       net.Listener
}

func startFakeSMTP(t *testing.T) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{addr: ln.Addr().String(), ln: ln}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(conn)
		}
	}()
	return f
}

func (f *fakeSMTP) serve(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)
	write := func(s string) {
		w.WriteString(s + "\r\n")
		w.Flush()
	}
	write("220 fake ESMTP")
	var body strings.Builder
	inData := false
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if inData {
			if line == "." {
				inData = false
				f.mu.Lock()
				f.received = append(f.received, body.String())
				f.mu.Unlock()
				body.Reset()
				write("250 queued")
				continue
			}
			body.WriteString(line + "\n")
			continue
		}
		switch {
		case strings.HasPrefix(line, "EHLO"):
			// No extensions advertised: this fake speaks plain SMTP only.
			write("250-fake")
			write("250 SIZE 10240000")
		case strings.HasPrefix(line, "HELO"):
			write("250 fake")
		case strings.HasPrefix(line, "MAIL FROM"), strings.HasPrefix(line, "RCPT TO"):
			write("250 ok")
		case line == "DATA":
			inData = true
			write("354 go ahead")
		case line == "QUIT":
			write("221 bye")
			return
		default:
			write("250 ok")
		}
	}
}

func (f *fakeSMTP) messages() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.received...)
}

func (f *fakeSMTP) config() SMTPConfig {
	host, portStr, _ := net.SplitHostPort(f.addr)
	port := 0
	for _, c := range portStr {
		port = port*10 + int(c-'0')
	}
	return SMTPConfig{Enabled: true, Host: host, Port: port, From: "llmesh@example.com", Security: "none"}
}

func TestSendMailDelivers(t *testing.T) {
	f := startFakeSMTP(t)
	if err := sendMail(f.config(), "user@example.com", "Sign in", "link here", time.Now()); err != nil {
		t.Fatal(err)
	}
	msgs := f.messages()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if !strings.Contains(msgs[0], "To: user@example.com") || !strings.Contains(msgs[0], "link here") {
		t.Fatalf("unexpected message:\n%s", msgs[0])
	}
}

func TestSendMailRejectsBadAddresses(t *testing.T) {
	f := startFakeSMTP(t)
	cfg := f.config()
	if err := sendMail(cfg, "not-an-address", "s", "b", time.Now()); err == nil {
		t.Fatal("expected an invalid recipient to be refused")
	}
	bad := cfg
	bad.From = "nonsense"
	if err := sendMail(bad, "user@example.com", "s", "b", time.Now()); err == nil {
		t.Fatal("expected an invalid From to be refused")
	}
	if len(f.messages()) != 0 {
		t.Fatal("a refused message was still delivered")
	}
}

func TestSendMailRequiresSTARTTLSWhenAsked(t *testing.T) {
	// The fake advertises no STARTTLS. Asking for it and getting silence must
	// fail rather than quietly fall back to sending in the clear.
	f := startFakeSMTP(t)
	cfg := f.config()
	cfg.Security = "starttls"
	err := sendMail(cfg, "user@example.com", "s", "b", time.Now())
	if err == nil {
		t.Fatal("expected a failure when STARTTLS is unavailable")
	}
	if !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("expected the error to name STARTTLS, got %v", err)
	}
	if len(f.messages()) != 0 {
		t.Fatal("message sent unencrypted after STARTTLS was requested")
	}
}

func TestSendMailUnconfigured(t *testing.T) {
	if err := sendMail(SMTPConfig{}, "a@b.com", "s", "b", time.Now()); err == nil {
		t.Fatal("expected an error with no host configured")
	}
}
