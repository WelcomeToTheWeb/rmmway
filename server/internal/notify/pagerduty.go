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

// pagerDutyChannel delivers notifications via the PagerDuty Events API v2.
type pagerDutyChannel struct {
	cfg *ChannelConfig
}

func newPagerDutyChannel(cfg *ChannelConfig) *pagerDutyChannel {
	return &pagerDutyChannel{cfg: cfg}
}

type pdPayload struct {
	RoutingKey  string `json:"routing_key"`
	EventAction string `json:"event_action"`
	Description string `json:"description"`
	Payload     struct {
		Summary   string `json:"summary"`
		Severity  string `json:"severity"`
		Timestamp string `json:"timestamp"`
		Source    string `json:"source"`
	} `json:"payload"`
}

func (c *pagerDutyChannel) Send(ctx context.Context, req SendRequest) error {
	apiKey, _ := c.cfg.Config["api_key"].(string)
	if apiKey == "" {
		return fmt.Errorf("pagerduty channel: api_key not configured")
	}

	severity := "info"
	if req.Category == "escalation" {
		severity = "critical"
	}
	eventAction := "trigger"
	if req.Category == "escalation" {
		eventAction = "trigger"
	}

	payload := pdPayload{
		RoutingKey:  apiKey,
		EventAction: eventAction,
		Description: fmt.Sprintf("[%s] %s", req.Category, req.Title),
		Payload: struct {
			Summary   string `json:"summary"`
			Severity  string `json:"severity"`
			Timestamp string `json:"timestamp"`
			Source    string `json:"source"`
		}{
			Summary:   req.Message,
			Severity:  severity,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Source:    "rmmway",
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return c.post(ctx, "https://events.pagerduty.com/v2/enqueue", body)
}

func (c *pagerDutyChannel) Test(ctx context.Context) error {
	return c.Send(ctx, SendRequest{
		Category: "test",
		Title:    "Test notification",
		Message:  "This is a test notification from the RMMWay notification system.",
	})
}

func (c *pagerDutyChannel) post(ctx context.Context, url string, body []byte) error {
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
		return fmt.Errorf("pagerduty returned %d: %s", resp.StatusCode, string(b))
	}
	return nil
}
