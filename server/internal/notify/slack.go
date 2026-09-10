package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// slackChannel delivers notifications via Slack incoming webhooks.
type slackChannel struct {
	cfg *ChannelConfig
}

func newSlackChannel(cfg *ChannelConfig) *slackChannel {
	return &slackChannel{cfg: cfg}
}

type slackPayload struct {
	Channel     string                   `json:"channel,omitempty"`
	Username    string                   `json:"username,omitempty"`
	IconEmoji   string                   `json:"icon_emoji,omitempty"`
	Text        string                   `json:"text"`
	Blocks      []map[string]interface{} `json:"blocks,omitempty"`
	Attachments []map[string]interface{} `json:"attachments,omitempty"`
}

func (c *slackChannel) Send(ctx context.Context, req SendRequest) error {
	webhookURL, _ := c.cfg.Config["webhook_url"].(string)
	if webhookURL == "" {
		return fmt.Errorf("slack channel: webhook_url not configured")
	}
	channel, _ := c.cfg.Config["channel"].(string)
	username, _ := c.cfg.Config["username"].(string)
	if username == "" {
		username = "RMMWay"
	}

	severity := "info"
	if req.Category == "escalation" {
		severity = "error"
	}
	color := "#36a64f"
	if req.Category == "escalation" {
		color = "#ff0000"
	}

	payload := slackPayload{
		Channel:   channel,
		Username:  username,
		IconEmoji: ":robot:",
		Text:      fmt.Sprintf("[%s] %s", req.Category, req.Title),
		Attachments: []map[string]interface{}{
			{
				"color": color,
				"fields": []map[string]interface{}{
					{"title": "Severity", "value": severity, "short": true},
					{"title": "Time", "value": time.Now().UTC().Format(time.RFC3339), "short": true},
				},
				"text": req.Message,
			},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return c.post(ctx, webhookURL, body)
}

func (c *slackChannel) Test(ctx context.Context) error {
	return c.Send(ctx, SendRequest{
		Category: "test",
		Title:    "Test notification",
		Message:  "This is a test notification from the RMMWay notification system.",
	})
}

func (c *slackChannel) post(ctx context.Context, url string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("slack webhook returned %d: %s", resp.StatusCode, string(b))
	}
	return nil
}
