// Package smtp is the A-2 SMTP outbox: the server's ability to send mail
// (setup-wizard "test" messages now; operator notifications later) using
// ONLY the standard library (net/smtp + crypto/tls), with the transport
// security level derived from the port so the wizard has one knob to show:
//
//	465 -> implicit TLS (the connection IS TLS from the first byte)
//	587 -> STARTTLS (plaintext dial, upgraded before credentials)
//	other (25) -> plaintext (LAN relays, tests)
//
// Config is plain-JSON (persisted in server_config by the setup wizard) so
// the same struct round-trips the API boundary.
package smtp

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Config is the SMTP outbox configuration (JSON-serialized in
// server_config). Password is the only secret; it is never returned by
// GET /api/setup (the public form omits it).
type Config struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`     // 465 | 587 | 25 (0 -> 587)
	From     string `json:"from"`     // sender address (MAIL FROM + From:)
	Username string `json:"username"` // AUTH username ("" = no auth)
	Password string `json:"password"` // AUTH password
}

// Normalize fills defaults and validates. A zero Config (Host == "") means
// "SMTP not configured" and is NOT an error (the wizard makes the outbox
// optional).
func (c Config) Normalize() (Config, error) {
	out := c
	out.Host = strings.TrimSpace(out.Host)
	if out.Host == "" {
		return out, nil // not configured
	}
	if out.Port == 0 {
		out.Port = 587
	}
	if out.Port < 1 || out.Port > 65535 {
		return out, fmt.Errorf("smtp port must be 1-65535 (got %d)", out.Port)
	}
	from := strings.TrimSpace(out.From)
	if from == "" {
		return out, fmt.Errorf("smtp from address is required")
	}
	if !strings.Contains(from, "@") {
		return out, fmt.Errorf("smtp from address %q is not a valid address", from)
	}
	out.From = from
	return out, nil
}

// IsConfigured reports whether the outbox has enough settings to send.
func (c Config) IsConfigured() bool {
	c, _ = c.Normalize()
	return c.Host != ""
}

// SecurityMode is the transport security derived from the port.
type SecurityMode int

const (
	Plain SecurityMode = iota // port 25 etc.
	StartTLS                  // port 587
	ImplicitTLS               // port 465
)

func (c Config) SecurityMode() SecurityMode {
	switch c.Port {
	case 465:
		return ImplicitTLS
	case 587:
		return StartTLS
	default:
		return Plain
	}
}

// Send delivers one plaintext mail message to a single recipient.
// subject/textBody are UTF-8; the message is a minimal RFC 5322 mail
// (From/To/Date/Subject/Message-ID + body). Timeouts come from ctx.
func Send(ctx context.Context, cfg Config, to, subject, textBody string) error {
	cfg, err := cfg.Normalize()
	if err != nil {
		return err
	}
	if !cfg.IsConfigured() {
		return fmt.Errorf("smtp outbox not configured")
	}
	if to == "" {
		to = cfg.From
	}

	host := cfg.Host
	addr := net.JoinHostPort(host, strconv.Itoa(cfg.Port))

	client, conn, err := dial(ctx, cfg, host, addr)
	if err != nil {
		return err
	}
	defer func() {
		_ = client.Quit()
		_ = conn.Close()
	}()

	// Credentials, if any (plaintext dial -> upgrade -> auth is the 587
	// order; implicit TLS dials authenticated-by-TLS already).
	if cfg.Username != "" {
		if err := client.Auth(smtp.PlainAuth("", cfg.Username, cfg.Password, host)); err != nil {
			return fmt.Errorf("smtp auth: %w", err)
		}
	}

	if err := client.Mail(cfg.From); err != nil {
		return fmt.Errorf("smtp mail from: %w", err)
	}
	if err := client.Rcpt(to); err != nil {
		return fmt.Errorf("smtp rcpt to: %w", err)
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("smtp data: %w", err)
	}
	msg := buildMessage(cfg.From, to, subject, textBody)
	if _, err := io.WriteString(w, msg); err != nil {
		return fmt.Errorf("smtp write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp data end: %w", err)
	}
	return nil
}

// SendTest sends the setup wizard's verification mail (the recipient
// defaults to the configured From address, which is what the operator typed
// as their own address).
func SendTest(ctx context.Context, cfg Config, to string) error {
	now := time.Now().UTC()
	body := "This is a test message from the RMMWay SMTP outbox.\n" +
		"If you can read this, the outbox is configured correctly and\n" +
		"the server can send mail through " + cfg.Host + ":" + strconv.Itoa(cfg.Port) + ".\n" +
		"\nSent at " + now.Format(time.RFC3339) + "\n"
	return Send(ctx, cfg, to, "RMMWay: SMTP outbox test", body)
}

// dial opens the SMTP connection per the port-derived security mode and
// performs the STARTTLS / implicit-TLS upgrade BEFORE the caller sends
// credentials. conn is the underlying net.Conn (the caller closes it).
func dial(ctx context.Context, cfg Config, host, addr string) (client *smtp.Client, conn net.Conn, err error) {
	d := &net.Dialer{Timeout: 10 * time.Second}
	switch cfg.SecurityMode() {
	case ImplicitTLS:
		c, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, nil, fmt.Errorf("smtp dial %s: %w", addr, err)
		}
		tc := tls.Client(c, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
		if err := tc.HandshakeContext(ctx); err != nil {
			_ = c.Close()
			return nil, nil, fmt.Errorf("smtp tls handshake: %w", err)
		}
		client, err = smtp.NewClient(tc, host)
		return client, tc, err
	default:
		c, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, nil, fmt.Errorf("smtp dial %s: %w", addr, err)
		}
		client, err = smtp.NewClient(c, host)
		if err != nil {
			_ = c.Close()
			return nil, nil, fmt.Errorf("smtp client: %w", err)
		}
		if cfg.SecurityMode() == StartTLS {
			if ok, _ := client.Extension("STARTTLS"); !ok {
				_ = client.Quit()
				_ = c.Close()
				return nil, nil, fmt.Errorf("smtp: server %s does not offer STARTTLS on port 587", host)
			}
			if err := client.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
				_ = client.Quit()
				_ = c.Close()
				return nil, nil, fmt.Errorf("smtp starttls: %w", err)
			}
		}
		return client, c, nil
	}
}

// buildMessage assembles a minimal RFC 5322 message (single recipient,
// plaintext body).
func buildMessage(from, to, subject, textBody string) string {
	now := time.Now().UTC()
	var b strings.Builder
	b.WriteString("From: " + from + "\r\n")
	b.WriteString("To: " + to + "\r\n")
	b.WriteString("Subject: " + subject + "\r\n")
	b.WriteString("Date: " + now.Format("Mon, 02 Jan 2006 15:04:05 -0700") + "\r\n")
	b.WriteString("Message-ID: <" + strconv.FormatInt(now.UnixNano(), 36) + "@rmmway>\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n")
	b.WriteString("\r\n")
	b.WriteString(textBody)
	return b.String()
}

// ---- tiny in-process SMTP sink (tests + the A-2 e2e) ------------------------

// Sink is a minimal SMTP server that accepts one or more plaintext messages
// and records them. It answers the EHLO/MAIL/RCPT/DATA/QUIT dialogue with
// the correct codes and captures the DATA payload of each message. It does
// not advertise STARTTLS, so it is for the Plain mode (tests).
type Sink struct {
	listener net.Listener
	mu       sync.Mutex
	mails    []string
}

// NewSink listens on 127.0.0.1:0 and serves until Close.
func NewSink() (*Sink, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &Sink{listener: ln}
	go s.serve()
	return s, nil
}

// Addr is the sink's address ("127.0.0.1:port").
func (s *Sink) Addr() string { return s.listener.Addr().String() }

// Port is the sink's port.
func (s *Sink) Port() int {
	_, p, _ := net.SplitHostPort(s.listener.Addr().String())
	n, _ := strconv.Atoi(p)
	return n
}

// Mails returns the captured DATA payloads (one per message).
func (s *Sink) Mails() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string{}, s.mails...)
}

// Close stops accepting connections.
func (s *Sink) Close() error { return s.listener.Close() }

func (s *Sink) serve() {
	for {
		c, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.handle(c)
	}
}

func (s *Sink) handle(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	w := func(code int, msg string) {
		_, _ = fmt.Fprintf(c, "%d %s\r\n", code, msg)
	}
	w(220, "rmmway-sink ESMTP")
	inData := false
	var data strings.Builder
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if inData {
			if line == "." {
				inData = false
				s.mu.Lock()
				s.mails = append(s.mails, data.String())
				s.mu.Unlock()
				data.Reset()
				w(250, "accepted")
			} else {
				data.WriteString(line + "\n")
			}
			continue
		}
		cmd := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(cmd, "EHLO"):
			_, _ = fmt.Fprintf(c, "250-rmmway-sink hello\r\n250-AUTH PLAIN\r\n250 ok\r\n")
		case strings.HasPrefix(cmd, "HELO"):
			w(250, "rmmway-sink hello")
		case strings.HasPrefix(cmd, "AUTH "):
			w(235, "authenticated")
		case strings.HasPrefix(cmd, "MAIL FROM"):
			w(250, "ok")
		case strings.HasPrefix(cmd, "RCPT TO"):
			w(250, "ok")
		case cmd == "DATA":
			inData = true
			w(354, "go ahead")
		case cmd == "QUIT":
			w(221, "bye")
			return
		case cmd == "RSET" || cmd == "NOOP":
			w(250, "ok")
		default:
			w(500, "unsupported")
		}
	}
}
