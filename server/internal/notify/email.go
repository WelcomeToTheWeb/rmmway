package notify

import (
	"context"
	"fmt"
	"strconv"
	"time"
)

// emailChannel delivers notifications via the SMTP outbox (reusing
// server/internal/smtp).
type emailChannel struct {
	cfg      *ChannelConfig
	smtpSend func(ctx context.Context, host, port string, from, to, username, password, subject, body string) error
}

func newEmailChannel(cfg *ChannelConfig, smtpSend func(ctx context.Context, host, port string, from, to, username, password, subject, body string) error) *emailChannel {
	return &emailChannel{cfg: cfg, smtpSend: smtpSend}
}

func (c *emailChannel) Send(ctx context.Context, req SendRequest) error {
	if c.smtpSend == nil {
		return fmt.Errorf("email channel: smtp sender not configured")
	}
	host, _ := c.cfg.Config["host"].(string)
	portInt, _ := c.cfg.Config["port"].(float64)
	port := strconv.Itoa(int(portInt))
	from, _ := c.cfg.Config["from"].(string)
	to, _ := c.cfg.Config["to"].(string)
	username, _ := c.cfg.Config["username"].(string)
	password, _ := c.cfg.Config["password"].(string)

	subject := "[RMMWay] " + req.Title
	body := req.Message + "\n\n" + fmt.Sprintf("Sent at %s", time.Now().UTC().Format(time.RFC3339))
	return c.smtpSend(ctx, host, port, from, to, username, password, subject, body)
}

func (c *emailChannel) Test(ctx context.Context) error {
	return c.Send(ctx, SendRequest{
		Category: "test",
		Title:    "Test notification",
		Message:  "This is a test notification from the RMMWay notification system.",
	})
}