package smtp

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestNormalize(t *testing.T) {
	c, err := Config{Host: " mail.acme.test ", Port: 0, From: "ops@acme.test"}.Normalize()
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.Host != "mail.acme.test" || c.Port != 587 || c.From != "ops@acme.test" {
		t.Fatalf("unexpected defaults: %+v", c)
	}
	if c.SecurityMode() != StartTLS {
		t.Fatalf("port 587 should be STARTTLS, got %v", c.SecurityMode())
	}
	if _, err := (Config{Host: "h", From: "not-an-address"}).Normalize(); err == nil {
		t.Fatal("bad from address should fail")
	}
	if _, err := (Config{Host: "h", Port: 70000, From: "a@b.c"}).Normalize(); err == nil {
		t.Fatal("bad port should fail")
	}
	zero, err := Config{}.Normalize()
	if err != nil || zero.IsConfigured() {
		t.Fatalf("empty config should be 'not configured' without error: %+v err=%v", zero, err)
	}
	if (Config{Host: "h", Port: 465}).SecurityMode() != ImplicitTLS {
		t.Fatal("port 465 should be implicit TLS")
	}
	if (Config{Host: "h", Port: 25}).SecurityMode() != Plain {
		t.Fatal("port 25 should be plain")
	}
}

func TestSendPlainToSink(t *testing.T) {
	sink, err := NewSink()
	if err != nil {
		t.Fatalf("sink: %v", err)
	}
	defer sink.Close()

	cfg := Config{
		Host: "127.0.0.1", Port: sink.Port(), From: "rmmway@acme.test",
		Username: "mailer", Password: "secret",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := SendTest(ctx, cfg, ""); err != nil {
		t.Fatalf("send test: %v", err)
	}
	mails := sink.Mails()
	if len(mails) != 1 {
		t.Fatalf("want 1 captured mail, got %d", len(mails))
	}
	m := mails[0]
	for _, want := range []string{
		"From: rmmway@acme.test",
		"To: rmmway@acme.test", // empty recipient defaults to From
		"Subject: RMMWay: SMTP outbox test",
		"Content-Type: text/plain; charset=utf-8",
		"RMMWay SMTP outbox",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("captured mail missing %q:\n%s", want, m)
		}
	}
}

func TestSendNotConfigured(t *testing.T) {
	if err := Send(context.Background(), Config{}, "", "s", "b"); err == nil {
		t.Fatal("send with empty config should fail")
	}
}
