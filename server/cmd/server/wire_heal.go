package main

import (
	"context"
	"encoding/base64"
	"log"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
	"github.com/welcometotheweb/rmmway/server/internal/flow"
	"github.com/welcometotheweb/rmmway/server/internal/heal"
	"github.com/welcometotheweb/rmmway/server/internal/ingest"
)

// busHealNotifier is the heal-engine -> bus bridge (W5-1/W6-2): escalations
// are logged AND published as "automation" events so the webhook / SSE
// framework journals + delivers them. Pure move out of main.go (wave-0 F2).
type busHealNotifier struct {
	log *log.Logger
	pub func(subject, deviceID, message string, data map[string]any)
}

func (n busHealNotifier) Escalate(run *heal.Run, reason string) {
	if n.log != nil {
		n.log.Printf("selfheal: ESCALATED run %d playbook=%s device=%s source=%q: %s (ticket=heal_runs.id=%d)",
			run.ID, run.PlaybookKey, run.DeviceID, run.Source, reason, run.ID)
	}
	n.pub(flow.SubjectNotify, run.DeviceID, "selfheal escalated "+run.PlaybookKey+": "+reason, map[string]any{
		"action": "escalated", "run_id": run.ID, "playbook": run.PlaybookKey,
		"device_id": run.DeviceID, "source": run.Source, "reason": reason,
	})
}

// wireHealEngine builds + runs the self-healing playbook engine (W5-1):
// Detect -> verify-safe -> remediate -> confirm (re-measure) -> escalate.
// Postgres-backed (the run state machine + replay-safety live in the DB),
// so it needs hasPG; remediations go through the same capability-gated
// command dispatch as operator-run actions (W3-3 token on every script).
// Returns nil when disabled. Pure move out of main() (wave-0 F2).
func wireHealEngine(hasPG bool, pgPool *pgxpool.Pool, svc *ingest.Service,
	publishEvent func(subject, deviceID, message string, data map[string]any),
) *heal.Engine {
	var healEngine *heal.Engine
	if hasPG {
		if d, on := healInterval(); on {
			hst := heal.NewStore(pgPool)
			remediate := func(ctx context.Context, deviceID, lang, script string) (string, error) {
				return svc.Dispatcher().Dispatch(deviceID, &agentv1.Command_RunScript{
					RunScript: &agentv1.RunScript{
						Lang:      lang,
						ScriptB64: base64.StdEncoding.EncodeToString([]byte(script)),
					},
				})
			}
			healEngine = heal.New(hst, remediate, svc.Dispatcher().Result, busHealNotifier{
				log: log.New(os.Stderr, "selfheal: ", 0), pub: publishEvent,
			})
			healErrCh := make(chan error, 1)
			go healEngine.Run(context.Background(), d, healErrCh)
			go func() {
				for err := range healErrCh {
					log.Printf("selfheal: %v", err)
				}
			}()
			log.Printf("selfheal: playbook engine started (interval %s; playbooks seeded by 0005_selfheal.sql)", d)
		} else {
			log.Println("selfheal: disabled (RMMWAY_HEAL_INTERVAL=off)")
		}
	}
	return healEngine
}
