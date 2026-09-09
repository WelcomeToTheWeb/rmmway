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

// teamsChannel delivers notifications via Microsoft Teams incoming webhooks.
type teamsChannel struct {
	cfg *ChannelConfig
}

func newTeamsChannel(cfg *ChannelConfig) *teamsChannel {
	return &teamsChannel{cfg: cfg}
}

type teamsCard struct {
	Type     string `json:"@type"`
	Context  string `json:"@context"`
	Sections []struct {
		ActivityTitle string `json:"activityTitle"`
		Text          string `json:"text"`
		Facts         []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"facts"`
	} `json:"sections"`
}

func (c *teamsChannel) Send(ctx context.Context, req SendRequest) error {
	webhookURL, _ := c.cfg.Config["webhook_url"].(string)
	if webhookURL == "" {
		return fmt.Errorf("teams channel: webhook_url not configured")
	}

	severity := "Info"
	if req.Category == "escalation" {
		severity = "Critical"
	}

	card := teamsCard{
		Type:    "Message/Card",
		Context: "https://schemas.microsoft.com/adc/v1/card.json",
		Sections: []struct {
			ActivityTitle string `json:"activityTitle"`
			Text          string `json:"text"`
			Facts         []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"facts"`
		}{
			{
				ActivityTitle: fmt.Sprintf("[%s] %s", req.Category, req.Title),
				Text:          req.Message,
				Facts: []struct {
					Name  string `json:"name"`
					Value string `json:"value"`
				}{
					{"Severity", severity},
					{"Time", time.Now().UTC().Format(time.RFC3339)},
				},
			},
		},
	}
	body, err := json.Marshal(card)
	if err != nil {
		return err
	}
	return c.post(ctx, webhookURL, body)
}

func (c *teamsChannel) Test(ctx context.Context) error {
	return c.Send(ctx, SendRequest{
		Category: "test",
		Title:    "Test notification",
		Message:  "This is a test notification from the RMMWay notification system.",
	})
}

func (c *teamsChannel) post(ctx context.Context, url string, body []byte) error {
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
		return fmt.Errorf("teams webhook returned %d: %s", resp.StatusCode, string(b))
	}
	return nil
}