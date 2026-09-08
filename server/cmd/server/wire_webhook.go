package main

import (
	"context"
	"log"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/welcometotheweb/rmmway/server/internal/flow"
	"github.com/welcometotheweb/rmmway/server/internal/webhook"
)

// wireWebhook builds + starts the webhook + event-stream framework (W6-2):
// journals every bus event, fans it out to live SSE subscribers, and
// delivers signed (HMAC) webhooks to user-defined endpoints with
// cursor-based retries + replay. Needs hasPG (journal + endpoints) and
// the bus (the events to expose); in-memory mode has neither. Returns the
// service (nil when disabled/failed) and the SEPARATE durable consumer bus
// it must be closed with at shutdown. Pure move out of main() (wave-0 F2).
func wireWebhook(hasPG bool, flowBus flow.Bus, pgPool *pgxpool.Pool) (*webhook.Service, flow.Bus) {
	var webhookSvc *webhook.Service
	var webhookBus flow.Bus
	if hasPG && flowBus != nil {
		// A SEPARATE durable consumer on the same stream: the flow engine
		// ("flow-engine") and the webhook framework ("webhook-engine") each
		// must see every event, so they can't share one consumer.
		whb, err := flow.NewNatsBus(context.Background(), env("RMMWAY_NATS_URL", "nats://localhost:4222"), "RMMWAY_EVENTS", "webhook-engine")
		if err != nil {
			log.Printf("WARN: nats webhook bus unavailable (%v) — webhooks disabled", err)
		} else {
			webhookBus = whb
			whs := webhook.NewStore(pgPool)
			webhookSvc = webhook.New(whs, whb).WithLogger(log.New(os.Stderr, "webhook: ", 0))
			if err := webhookSvc.Start(context.Background()); err != nil {
				log.Printf("WARN: webhook framework start failed (%v)", err)
				webhookSvc = nil
			} else {
				log.Println("webhook framework: signed webhooks + SSE event stream live (sweep 2s)")
			}
		}
	}
	return webhookSvc, webhookBus
}
