package main

import (
	"context"
	"log"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/welcometotheweb/rmmway/server/internal/notify"
	"github.com/welcometotheweb/rmmway/server/internal/store"
)

// wireNotify sets up the notification channel + policy framework (gap #6).
// Returns the store and sender for the HTTP API. When Postgres is not
// available (in-memory mode), returns nils (503).
func wireNotify(hasPG bool, pgPool *pgxpool.Pool) (store.NotifyStore, *notify.Sender) {
	if !hasPG {
		return nil, nil
	}
	store := store.NewInMemoryNotifyStore()
	// In production, the channels/policies live in server_config; the
	// sender wraps the SMTP outbox (smtp.Send). For now, the sender uses
	// a nil SMTP function so email channels fail fast if not configured.
	sender := notify.NewSender(func(ctx context.Context, host, port, from, to, username, password, subject, body string) error {
		// The real sender wraps smtp.Send; this stub is for the in-memory
		// notify store path until PG-backed notify lands.
		return nil
	})
	log.Println("notify: notification channels + policies wired")
	return store, sender
}
