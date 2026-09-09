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

// webhookChannel delivers notifications via a generic HTTP POST webhook.
type webhookChannel struct {
	cfg *ChannelConfig
}

func newWebhookChannel(cfg *ChannelConfig) *webhookChannel {
	return &webhookChannel{cfg: cfg}
}

type webhookPayload struct {
	Category string                 `json:"category"`
	Title    string                 `json:"title"`
	Message  string                 `json:"message"`
	Data     map[string]interface{} `json:"data,omitempty"`
	Source   string                 `json:"source"`
	Timestamp string                `json:"timestamp"`
}

func (c *webhookChannel) Send(ctx context.Context, req SendRequest) error {
	url, _ := c.cfg.Config["url"].(string)
	if url == "" {
		return fmt.Errorf("webhook channel: url not configured")
	}
	method, _ := c.cfg.Config["method"].(string)
	if method == "" {
		method = "POST"
	}
	headers := map[string]string{}
	if h, ok := c.cfg.Config["headers"].(map[string]interface{}); ok {
		for k, v := range h {
			if s, ok := v.(string); ok {
				headers[k] = s
			}
		}
	}

	payload := webhookPayload{
		Category:  req.Category,
		Title:     req.Title,
		Message:   req.Message,
		Data:      req.Data,
		Source:    "rmmway",
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req2, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req2.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req2.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req2)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("webhook returned %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

func (c *webhookChannel) Test(ctx context.Context) error {
	return c.Send(ctx, SendRequest{
		Category: "test",
		Title:    "Test notification",
		Message:  "This is a test notification from the RMMWay notification system.",
	})
}