# DEBUG.md — Bug review of W1-1 … W1-7

Reviewed 2026-08-22 against `main` (`fc0e171`). Scope: agent (`cmd/agent`,
`internal/{client,collectors,enroll,uplink}`), server (`cmd/server`,
`internal/{ingest,store,httpapi}`, migrations), installers
(`scripts/install.{sh,ps1}`), build script, CI. Findings ordered by severity
within each category. File:line refs are approximate.

## Critical

### C1 — `/admin/*` is completely unauthenticated (open enroll mint + inventory leak)
`server/internal/httpapi/httpapi.go:102-104`, `server/cmd/server/main.go:268`
`POST /admin/bootstrap` mints one-time enrollment tokens for **any** caller who
can reach `:8080` — no auth at all. Anyone on the network can enroll arbitrary
devices into the estate (and later receive commands / push fake metrics), and
`GET /admin/devices` + `/admin/search` leak the full inventory. The header
comment says "open for machine callers (the curl|sh installer)", but the
installer does not call `/admin/bootstrap` — the operator passes the token in
by hand — so openness buys nothing. This is the fleet's join gate; it must be
auth-gated (operator JWT) before any real deployment, not wait for W3.

### C2 — Insecure defaults are silently active in production
`server/cmd/server/main.go:164-168`, `server/internal/ingest/ingest.go:39-41`,
`server/internal/httpapi/httpapi.go:68-76`
If env vars are unset, the server boots with: JWT signing secret
`rmmway-dev-secret-change-me` (agent + operator tokens forgeable by anyone who
reads the source), operator login `admin`/`admin`, and Meilisearch master key
`rmmway-dev-master-key`. There is **no warning, no refusal to boot, no log
line**. An operator JWT grants full `/api/*` access; a forged agent JWT (with
any known device id, which `/admin/devices` happily lists) grants stream
access. At minimum: refuse to boot (or log a loud, repeated warning) when the
secret/credentials are the built-in defaults and `RMMWAY_ENV != dev`.

## High

### H1 — Concurrent `stream.Send` on the agent Stream → grpc panic, server crash
`server/internal/ingest/ingest.go:353-359` vs `:384`
The downlink pump goroutine calls `stream.Send(resp)` for dispatched commands
while the Recv-loop goroutine calls `stream.Send(ack)` for every heartbeat.
grpc-go's `Send` is **not** safe for concurrent use — the first command
dispatched during an active heartbeat race panics the server process
(all devices disconnect). Currently only reachable once a dispatch endpoint
exists (W2), but the bug is live in shipped code. Serialize sends through one
goroutine (e.g. send acks via the same `ch` the pump drains).

### H2 — Bootstrap token is consumed *before* the device is persisted
`server/internal/ingest/ingest.go:284-307`
`Enroll` deletes the one-time token from the map, then calls `mintJWT` and
`devices.Register`. If either fails (PG blip, disk, anything), the token is
gone and the agent gets an error — the operator must mint a brand-new token
and re-run the installer. Mirror case on the agent side
(`agent/internal/enroll/agent.go:100-106`): if identity **persist** fails
after a successful enroll, the token is consumed server-side but nothing is on
disk, so the next boot cannot enroll at all. Fix: only delete the token after
`Register` succeeds (or make Enroll idempotent per token with a short grace
window).

### H3 — Agent JWT expires after 30 days with no refresh path → permanent outage
`server/internal/ingest/ingest.go:42-43` (720h lifetime),
`agent/internal/enroll/agent.go:62-69`, `agent/internal/uplink/uplink.go:106-134`
The agent JWT is valid 30 days; the agent reuses the persisted identity
forever and *never* re-enrolls (W1-4 DoD). Day 31: every RPC returns
Unauthenticated, and `Uplink.Run` retries the same dead JWT forever with
30s-capped backoff — the device silently goes dark with no operator-visible
error beyond log spam. Needs a renewal story (re-Enroll with the existing
device id before expiry, or HeartbeatAck-driven refresh) before W2-5's
"monitored in 5 min" gate ages into "blind after 30 days".

### H4 — macOS installer path is broken: launchd job cannot start
`scripts/install.sh:146-157`
Two defects in the generated plist: (1) `ProgramArguments` contains the whole
command as **one** string (`${run_cmd}` = binary + args) — launchd will try to
exec a file literally named "`/usr/local/bin/rmmway-agent run --config …`";
arguments must be separate `<string>` elements. (2) `EnvironmentFile` is not a
launchd key (that's systemd); env must come via `EnvironmentVariables`. Result:
the W1-3 DoD ("a clean VM goes 0 → installed agent in one pasted line on each
OS") fails on macOS — the plist is written but the service never runs.

### H5 — Windows installer: `sc.exe create` invocation is broken
`scripts/install.ps1:108-109`
`$scArgs = "create $svc binPath= $exe start= auto"` is passed to `& sc.exe`
as a **single** argument; sc.exe receives one string containing spaces and
fails with an invalid-parameter error, so the service is never registered and
`Die` fires (or worse, on re-run `Restart-Service` targets nothing). Needs
splatting (`sc.exe create $svc binPath= $exe start= auto` as separate args).
Secondary: when the service already exists, the script restarts it without
updating binPath — a changed config/server never takes effect.

### H6 — Silent fallback to in-memory stores when migrations fail
`server/cmd/server/main.go:179-191`
If Postgres is down (or the migrations dir is missing in the image), the
server logs one WARN and runs with in-memory device/metric stores: enrolled
devices are forgotten on restart (agents then fail auth as "unknown device"),
all metrics are lost, and `/healthz` can still report green-ish state. For a
monitoring product this is a silent data-loss mode — it should be fatal outside
an explicit `RMMWAY_ALLOW_MEMORY_FALLBACK=1`.

## Medium

### M1 — Network collector emits one host-wide aggregate, not per-interface
`agent/internal/collectors/collectors.go:103-112`
`net.IOCountersWithContext(ctx, false)` with `pernic=false` returns a single
pseudo-interface named `"all"` (verified empirically on Linux). The package
doc and W1-2 promise `net.bytes_total <iface>` *per interface*; instead the
agent emits one `net.bytes_total[all]` sample and the `lo` skip is dead code.
Fix: pass `true` (and then filter loopback).

### M2 — Uplink drops ALL metrics whenever any collector family partially fails
`agent/internal/uplink/uplink.go:205-209`
`sendHeartbeat` only attaches the batch `if err == nil`. The W1-2 collector is
deliberately built to return `(batch, partialError)` — samples for the families
that worked — but the uplink discards the whole batch on any error. A single
flaky probe (e.g. one permission-denied mount class) silently zeroes the
device's metric stream. Should send the batch whenever it has samples and log
the partial error.

### M3 — Reconnect backoff never resets after a healthy session
`agent/internal/uplink/uplink.go:106-133`
`backoff` is initialized once in `Run` and only ever doubles (capped at 30s).
After the first blip in a process's lifetime, every later disconnect — even
months apart, even after hours of healthy streaming — starts at the previously
escalated value instead of `MinBackoff`. Reset backoff when a session ran
successfully (e.g. at least one heartbeat acked).

### M4 — Devices are never marked offline
`server/internal/store/pg.go:106-109`, `server/internal/ingest/ingest.go:320-397`
`online` is set `true` at enroll/heartbeat and never set `false` anywhere:
there is no sweeper and `Stream` exit doesn't flip it. The W2-1 device list
("lists live devices with status") and the Meili index will show dead devices
as online forever. Also: `IndexerHook.Touch` is not called on stream close, so
search results lag reality in both directions. Needs a last_seen-based janitor
(+ Touch on disconnect).

### M5 — Dispatcher leaks `pending` entries when the device is unreachable
`server/internal/ingest/store.go:46-55`
`Dispatch` inserts into `d.pending` **before** `sink.Push`; when Push returns
false (no live stream) the error is returned but the pending entry stays
forever. `results` is also never pruned. Both maps grow unbounded.

### M6 — gRPC target derived from the HTTP URL's port when one is present
`agent/cmd/agent/main.go:188-201`
`grpcTarget` uses `u.Port()` from `RMMWAY_SERVER` if present. If an operator
sets `RMMWAY_SERVER=http://rmm.example.com:8080` (the health/API port), the
agent dials gRPC on 8080 instead of defaulting to 50051 — a silent
misconfiguration that reads like a server outage. The HTTP port should not be
reused; default to 50051 unless `RMMWAY_GRPC_ADDR` is set.

### M7 — Meilisearch outage at boot disables indexing until the next restart
`server/cmd/server/main.go:206-220`
If `FullSync` fails (Meili briefly down during a rolling start), both
`indexer` and `mSearch` stay nil: new devices are never indexed (`Touch` is a
nil-safe no-op) and `/admin/search` 503s — until the server is restarted.
Should construct the hook anyway and let its retry path heal, or retry
FullSync on a timer.

### M8 — Old stream for a re-connecting device is never told to close
`server/internal/ingest/ingest.go:334-343`
On duplicate stream open the code closes the old channel (comment: "the old
connection gets told to go away") but nothing is ever *sent* on it, so the old
stream's Recv loop keeps running until the agent itself drops it. Two live
streams per device coexist briefly; benign today, but the comment promises
behavior that doesn't exist. Send an explicit "replaced" frame or cancel the
old stream's context.

### M9 — Metrics ingest is one round-trip per sample
`server/internal/store/pg.go:41-54`
Each sample is a separate `tx.Exec` inside one transaction — N round-trips per
batch. Fine at 1 device; at estate scale this is the ingest bottleneck. Use a
pgx batch or `COPY` with `ON CONFLICT` via an insert-into-temp + merge.

## Low

### L1 — Build artifacts committed to git
`agent/agent` and `server/server` (compiled binaries) are tracked in the repo
(`git ls-files`). `.gitignore` covers `bin/`/`dist/` but not these root-level
outputs. Remove from the index and extend `.gitignore`.

### L2 — Installers write `RMMWAY_DEVICE_ID`, which the agent never reads
`scripts/install.sh:106`, `scripts/install.ps1:88`
Dead config key — `loadConfig` (`agent/cmd/agent/main.go:150-163`) only knows
`RMMWAY_SERVER`, `RMMWAY_BOOTSTRAP_TOKEN`, `RMMWAY_GRPC_ADDR`. Misleads
operators into thinking they can set the device id.

### L3 — Bootstrap token lingers in config after enrollment
`scripts/install.sh:104-105`, `agent/internal/enroll/facts.go:13-15` (doc)
The docs say the consumed token "can be safely cleared", but nothing clears it
from `agent.env` / the systemd EnvironmentFile. It's 0600, but a spent
credential sitting on every endpoint is a habit, not a feature. Agent could
strip it from its config file after successful persist.

### L4 — `JWTInterceptor` treats a DB error as "unknown device"
`server/internal/ingest/ingest.go:208` (and `:329` in `Stream`)
`ok, _ := s.devices.Contains(...)` swallows the error: a transient PG hiccup
rejects *every* agent with Unauthenticated instead of surfacing
`codes.Unavailable`. Same pattern in the Stream auth path.

### L5 — No sanity window on agent-supplied timestamps
`server/internal/store/pg.go:45-50`
`ts = to_timestamp(timestamp_ms/1000)` trusts the agent completely; a 0 or
garbage `timestamp_ms` lands rows in 1970 chunks (or the far future), evading
the 90-day retention policy and polluting continuous aggregates. Clamp to a
plausible window (e.g. now ± 24h) or fall back to `ingested_at`.

### L6 — Migrations: no advisory lock, tight 5s boot budget
`server/internal/store/store.go:188-242`, `server/cmd/server/main.go:179`
Two servers booting concurrently can race the same migration (the loser fails
on the PK insert and — per H6 — drops to memory mode). A
`pg_advisory_lock` around `Migrate` fixes it. Also the whole migration pass
gets a single 5-second context; first-boot `CREATE EXTENSION … CASCADE` +
hypertable + policies can exceed that on slow hosts.

### L7 — `/healthz` opens fresh connections to all five backends per request
`server/cmd/server/main.go:53-155, 243-259`
Every probe does a real `pgx.Connect`, NATS connect, Redis ping, etc. Anything
polling `/healthz` (load balancers do this every few seconds) creates constant
connection churn. Cache probe results for a few seconds or reuse the pool.

### L8 — Login endpoint is an unauthenticated PBKDF2 work amplifier
`server/internal/httpapi/httpapi.go:114-132`
100k PBKDF2 iterations per attempt with no rate limiting or lockout; a few
request/s pins a core. Add per-IP throttling (and consider argon2id with a
stored salt instead of the per-boot salt, which also means every restart
invalidates nothing — fine, but the per-boot salt buys little).

### L9 — Disk collector can emit duplicate samples for one device
`agent/internal/collectors/collectors.go:90-100`
The same block device mounted at several mountpoints produces several samples
with identical `(name, source=device, timestamp_ms)` — the server's
`ON CONFLICT DO NOTHING` then silently keeps only the first. Keying `source`
on mountpoint (or device+mountpoint) would preserve both.

### L10 — e2e harness panics instead of failing cleanly
`server/cmd/e2e/main.go:70`
`boot.BootstrapToken[:12]` slices an unchecked string — if `/admin/bootstrap`
returned nothing (server down / mint disabled) the harness panics rather than
printing `FAIL: bootstrap: empty token`.

### L11 — Windows installer dead code + stale comment
`scripts/install.ps1:77, 123-124`
`-not (Test-Path "env:PATH")` is always false (`PATH` always exists), so the
PATH-update branch never runs. The trailing "enrollment + connect/run loop land
in W1-4" note is stale (W1-4 is done); same stale note in `install.sh:172-173`.

---

## Verified during review
- `gopsutil` `IOCounters(false)` returns exactly one aggregate entry named
  `"all"` on Linux (ran against the agent's go.mod) — confirms M1.
- `make build` / unit suites not re-run here; findings are from code reading
  plus that one live check.

## Suggested fix order
1. C1, C2 (auth surface) — same PR, small.
2. H1 (send serialization) + H2 (token-consumption ordering).
3. H4, H5 (installers) — W1-3 DoD is currently false on macOS/Windows.
4. H3 (JWT renewal design) — decide the mechanism before W2-5.
5. H6 + M4 (failure modes + offline sweeper).
6. Rest as capacity allows; M1/M2 materially affect data quality for W2-3 baselining.
