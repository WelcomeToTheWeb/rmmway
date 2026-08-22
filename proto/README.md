# proto/ — shared gRPC protocol definitions

Agent ↔ server protocol (enroll, heartbeat, metrics push, command dispatch).

W0-1 creates this directory as the shared source of truth. **W0-2** adds the
`.proto` files plus `Makefile` targets to generate Go stubs into
`server/gen/` and `agent/gen/` (both git-ignored).

Layout (added by W0-2):

```
proto/
  rmmway/
    agent/v1/
      agent.proto      # AgentService: Enroll, Heartbeat (bidi stream)
      metrics.proto    # MetricBatch messages
      commands.proto   # Command dispatch messages
  Makefile             # proto target, protoc + buf config
```
