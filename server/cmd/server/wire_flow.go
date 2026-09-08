package main

import (
	"context"
	"encoding/base64"
	"log"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	agentv1 "github.com/welcometotheweb/rmmway/proto/gen/rmmway/agent/v1"
	"github.com/welcometotheweb/rmmway/server/internal/flow"
	"github.com/welcometotheweb/rmmway/server/internal/ingest"
)

// busFlowNotifier is the flow-engine -> bus bridge (W5-2/W6-2): every node
// transition is logged AND published as an "automation" event so the
// webhook / SSE framework journals + delivers it. Pure move out of
// main.go (wave-0 F2).
type busFlowNotifier struct {
	log *log.Logger
	pub func(subject, deviceID, message string, data map[string]any)
}

func (n busFlowNotifier) Notify(ctx context.Context, run *flow.Run, nodeID, reason string) {
	if n.log != nil {
		n.log.Printf("flow: NOTIFY run %d (%s) node=%s device=%s: %s", run.ID, run.FlowName, nodeID, run.DeviceID, reason)
	}
	n.pub(flow.SubjectNotify, run.DeviceID, "flow "+run.FlowName+" node="+nodeID+": "+reason, map[string]any{
		"action": "notify", "run_id": run.ID, "flow": run.FlowName,
		"node": nodeID, "device_id": run.DeviceID, "message": reason,
	})
}

// wireFlowBus creates the NATS/JetStream event bus (W5-2). Flows are
// Postgres-backed (the replay-safe run state), so the engine needs hasPG;
// when NATS is down the server degrades to in-memory mode for the rest of
// the stack, so the flow engine is disabled too (its whole point is that
// the chain runs OVER the bus). Returns nil when disabled. Pure move out
// of main() (wave-0 F2).
func wireFlowBus(hasPG bool) flow.Bus {
	if !hasPG {
		return nil
	}
	fb, err := flow.NewNatsBus(context.Background(), env("RMMWAY_NATS_URL", "nats://localhost:4222"), "RMMWAY_EVENTS", "flow-engine")
	if err != nil {
		log.Printf("WARN: nats event bus unavailable (%v) — flow engine disabled", err)
		return nil
	}
	log.Println("nats event bus ready (stream RMMWAY_EVENTS)")
	return fb
}

// wireFlowEngine builds + starts the event-driven automation engine (W5-2).
// Automations are DAGs of trigger -> script/check/notify nodes executed
// OVER the NATS event bus: every hop of every run is a bus event, the
// Postgres tables hold only the replay-safe run state. Real triggers come
// from the sampler (polls the metrics hypertable); synthetic ones from
// POST /api/flows/{id}/trigger. Returns nil when disabled/failed. Pure
// move out of main() (wave-0 F2).
func wireFlowEngine(hasPG bool, flowBus flow.Bus, pgPool *pgxpool.Pool, svc *ingest.Service,
	publishEvent func(subject, deviceID, message string, data map[string]any),
) *flow.Engine {
	var flowEngine *flow.Engine
	if hasPG && flowBus != nil {
		remediate := func(ctx context.Context, deviceID, lang, script string) (string, error) {
			return svc.Dispatcher().Dispatch(deviceID, &agentv1.Command_RunScript{
				RunScript: &agentv1.RunScript{
					Lang:      lang,
					ScriptB64: base64.StdEncoding.EncodeToString([]byte(script)),
				},
			})
		}
		flowEngine = flow.New(flow.NewStore(pgPool), flowBus, remediate, svc.Dispatcher().Result,
			busFlowNotifier{log: log.New(os.Stderr, "flow: ", 0), pub: publishEvent},
			flowInterval("RMMWAY_FLOW_SWEEP", 5*time.Second), flowInterval("RMMWAY_FLOW_SAMPLE", 60*time.Second))
		flowEngine = flowEngine.WithLogger(log.New(os.Stderr, "flow: ", 0))
		if err := flowEngine.Start(context.Background()); err != nil {
			log.Printf("WARN: flow engine start failed (%v) — flows disabled", err)
			flowEngine = nil
		} else {
			log.Println("flow engine: event-driven chains started (sampler + sweep on the NATS bus)")
		}
	} else if hasPG {
		log.Println("flow engine: disabled (nats event bus unavailable)")
	}
	return flowEngine
}
