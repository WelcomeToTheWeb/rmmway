# proto/ — shared gRPC protocol definitions

Agent ↔ server protocol. **W0-2** delivers the `.proto` files + codegen;
**W1-5** implements the server side, **W1-4** the agent side.

## Layout

```
proto/
  rmmway/agent/v1/
    agent.proto      # AgentService: Enroll + Stream (bidi heartbeat/command)
    metrics.proto    # MetricBatch / Metric (the five W1-2 metric families)
    commands.proto   # Command dispatch (RunScript, Reboot; W3-3 capability tokens)
  gen/               # generated Go (committed; `make proto` regenerates)
    go.mod           # module github.com/welcometotheweb/rmmway/proto/gen
```

## Usage

```sh
make proto       # buf generate -> proto/gen/ + go mod tidy
make proto-lint  # buf lint
```

Both `server/` and `agent/` import the stubs via
`replace github.com/welcometotheweb/rmmway/proto/gen => ../proto/gen`.

## Auth model

- **Enroll**: one-time bootstrap token (from the `curl|sh` line) → device_id +
  long-lived agent JWT. Never called again after first success (W1-4).
- **Stream**: every RPC carries `Authorization: Bearer <jwt>`; W1-5 verifies,
  W3-1 layers mTLS on top, W3-3 adds per-command capability tokens.
- **Dedup**: metrics dedupe on `(device_id, name, source, timestamp_ms)`,
  making offline replay at-least-once and idempotent (see IDEA.md §1).
