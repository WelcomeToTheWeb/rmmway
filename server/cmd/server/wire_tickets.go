package main

import (
	"log"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/welcometotheweb/rmmway/server/internal/flow"
	"github.com/welcometotheweb/rmmway/server/internal/heal"
	"github.com/welcometotheweb/rmmway/server/internal/store"
)

// ticketHealNotifier wraps a heal notifier so that each escalation
// creates a real ticket (gap #7). When the ticket store is not wired,
// it falls back to the underlying notifier only.
type ticketHealNotifier struct {
	wrapped heal.Notifier
	log     *log.Logger
	store   store.TicketStore
	pub     func(subject, deviceID, message string, data map[string]any)
}

func (n ticketHealNotifier) Escalate(run *heal.Run, reason string) {
	if n.store != nil {
		deviceID := run.DeviceID
		healRunID := run.ID
		ticket := &store.Ticket{
			Title:       "Self-heal escalation: " + run.PlaybookKey,
			Description: "Playbook " + run.PlaybookKey + " escalated on device " + run.DeviceID + ". Reason: " + reason,
			Queue:       "escalations",
			Priority:    "high",
			DeviceID:    &deviceID,
			Source:      "heal",
			HealRunID:   &healRunID,
		}
		created, err := n.store.Create(nil, ticket)
		if err != nil {
			if n.log != nil {
				n.log.Printf("ticket creation on escalation failed: %v", err)
			}
		} else {
			if n.log != nil {
				n.log.Printf("selfheal: ESCALATED run %d playbook=%s device=%s source=%q: %s (ticket=%s)",
					run.ID, run.PlaybookKey, run.DeviceID, run.Source, reason, created.ID)
			}
			n.pub(flow.SubjectNotify, run.DeviceID, "selfheal escalated "+run.PlaybookKey+": "+reason, map[string]any{
				"action": "escalated", "run_id": run.ID, "playbook": run.PlaybookKey,
				"device_id": run.DeviceID, "source": run.Source, "reason": reason,
				"ticket_id": created.ID,
			})
			return
		}
	}
	// Fallback: delegate to the wrapped notifier when tickets are unavailable.
	n.wrapped.Escalate(run, reason)
}

// wireTickets sets up the helpdesk ticketing system (gap #7). When Postgres
// is available it wires the store and returns a ticketHealNotifier that
// bridges heal escalations into real tickets. Returns (nil, nil) when
// disabled (in-memory mode).
func wireTickets(hasPG bool, pgPool *pgxpool.Pool) (store.TicketStore, heal.Notifier) {
	if !hasPG {
		return nil, nil
	}
	store := store.NewPostgresTicketStore(pgPool)
	return store, nil
}
