# DEBUG.md Fix Progress

Working through `DEBUG.md` (bug review of W1-1…W1-7) in its suggested fix
order: C1+C2 → H1+H2 → H4+H5 → H3 → H6+M4 → rest.

Legend: ✅ fixed (code + tests) · ✅* verified already fixed in-tree, no change
needed · ⬜ remaining.

## Critical

| ID | Status | What was done |
|----|--------|---------------|
| C1 | ✅ (server side; harness ⬜) | All `/admin/*` routes now wrapped in `s.requireOperator` (operator JWT, same token as `/api/*`). Open surfaces left: `/agent/enroll`, `/agent/releases/*`, `/api/login`, `/api/setup/*`. Tests: `TestAdminDevicesAuthGate` (401 w/o token, 200 with), `TestAdminBootstrapStillWorks` (mint stays open; token needed on the replay), dispatch + device-events tests now login first. |
| C2 | ✅ | `server/cmd/server/main.go`: non-`dev` env with a built-in default in play (JWT secret, admin/admin, Meili master key) → `log.Fatal` with the exact env var to set; dev → loud warning. |

## High

| ID | Status | What was done |
|----|--------|---------------|
| H1 | ✅ | `server/internal/ingest/ingest.go`: single downlink pump goroutine now owns **every** `stream.Send` (grpc-go `ServerStream.Send` is not concurrency-safe). Ack/heartbeat sends go through a channel + `pumpDone` select so pump shutdown can't deadlock the stream handler. |
| H2 | ✅ | One-time bootstrap tokens with a 15-min grace window (`Config.BootstrapGraceWindow`). Fresh enroll claims the token (`consumedTokens[btok] = {devID, at}`); replay **within** grace returns the **same** device id (idempotent persist-retry); **lapsed** replay → `PermissionDenied` and the record is pruned. Test steps rewritten: in-grace replay keeps `devID`; post-grace case ages `ct.at` under `svc.mu` instead of shrinking the window (machine clock advances in ~528µs quanta, so `time.Since` is 0 across a tick). |
| H3 | ✅ | JWT renewal piggybacked on heartbeats. Proto: `HeartbeatAck.jwt = 4` (regenerated). Server mints a fresh 720h device JWT when remaining < JWTLifetime/4. Agent: `uplink.WithJWTChangeHook` → mutex-guarded live JWT (`getJWT`/`setJWT`) shared with the mTLS unary interceptor, persisted to the identity file (serialize with the rotator's persist callback). Test: `TestHeartbeatAckRenewsExpiringJWT` (fresh token → empty ack.Jwt; hand-signed token with 10m remaining → renewed, verifies for same device, ≈full lifetime) and `TestUplinkAdoptsRenewedJWT` (hook fires once; reconnect sends `Bearer jwt-renewed`). |
| H4/H5 | ✅ | `scripts/install.sh`: plist `ProgramArguments` now separate `<string>` elements; env moved from the bogus `EnvironmentFile` key into an `EnvironmentVariables` dict built from `agent.env`, with `xml_escape` covering `& < > " '`; verified by running the darwin branch with stubbed `uname`/`curl`/`launchctl` — plist parses via `plistlib` and the token `tok<"&>'x'q` round-trips byte-exact. `scripts/install.ps1`: `sc.exe create` now splatted as an array (`@("create", $svc, "binPath=", $binPath, "start=", "auto")`); re-run path does `sc.exe config $svc binPath=` before restart. (No pwsh on this box — ps1 verified by review.) |
| H6 | ✅ | `cmd/server` now `log.Fatal`s when PostgreSQL is unreachable unless `RMMWAY_ALLOW_MEMORY_FALLBACK=1` is set explicitly; migrate budget 60s. |

## Medium

| ID | Status | What was done |
|----|--------|---------------|
| M1 | ✅ | `collectors.go`: `net.IOCountersWithContext(ctx, true)` + `isLoopback` filter ("lo"/"lo0" exact, case-insensitive "loopback" contains). |
| M2 | ✅ | Uplink now ships a partial batch when a collector in the batch errors. Test: `TestUplinkPartialCollectionStillSendsMetrics`. |
| M3 | ✅ | Reconnect backoff resets after a session that received ≥1 ack. Reset decision happens **before** the reconnect sleep so a healthy session's own drop gets the short wait. Test: `TestBackoffResetsAfterHealthySession` (drops session A after its ack, asserts next connect within minBackoff+jitter bound). |
| M4 | ✅ | Offline sweeper: store `SweepOffline` on a 30s goroutine in cmd/server + `IndexerHook.Touch` on stream close (nil-safe hook). |
| M5 | ✅* | Dispatcher pending-slot leak was already fixed in-tree — verified by review, no change. |
| M6 | ✅ | `agent/main.go`: `grpcTarget` built from the resolved config (port/host), not the raw env string. |
| M7 | ✅ | `retryMeiliSync`: 30s resync loop when the indexer was down during ingest (by design, documented). |
| M8 | ✅* | Superseded-stream ctx cancellation already fixed in-tree — verified by review. |
| M9 | ✅ | Batched INSERTs via `unnest($n::type[])` in `store/pg.go` (metrics, events, logs). |

## Low

| ID | Status | What was done |
|----|--------|---------------|
| L1 | ⬜ | `git rm --cached agent/agent server/server` + `.gitignore` entries (build artifacts committed). |
| L2 | ✅ | `RMMWAY_DEVICE_ID` dropped from both installers (device id comes from enroll); replaced with an NB comment in each. |
| L3 | ✅ | `agent/main.go`: `stripBootstrapTokenFromConfig` — after successful enroll the agent rewrites `--config` with the `RMMWAY_BOOTSTRAP_TOKEN` line removed (mode 0600, warn-not-fatal). `enroll/facts.go` doc updated to describe the split: agent strips, server grace window covers persist-failure retry. |
| L4 | ✅ | `errors.Is(err, errTokenMissing)` → `codes.Unavailable` (was `FailedPrecondition`) in JWT interceptor / Stream / RefreshLeaf paths. |
| L5 | ✅ | Ingest clamps metric/log/event timestamps to ±24h of server time; out-of-band timestamps fall back to arrival time. |
| L6 | ✅ | `store.Migrate` takes a session-scoped advisory lock (concurrent boots can't both migrate) + 60s budget. |
| L7 | ✅ | `/healthz` readiness check cached 5s (healthCache) instead of a DB round-trip per probe. |
| L8 | ✅ | Login rate limiter: 10 fails/5min per client IP → 15min lockout, 429 + `Retry-After`, success clears. Config `LoginRateLimit *bool` (nil = enabled). Tests: `TestLoginRateLimit` (10 fails from one IP → 429 even with correct creds; second IP unaffected). |
| L9 | ✅ | Disk samples keyed `device@mountpoint` (multi-partition hosts no longer collide on the `disk.used_percent` metric name). |
| L10 | ⬜ | e2e: guard `len(boot.BootstrapToken) < 12` (a failed mint silently enrolls with a garbage token). |
| L11 | ✅ | `install.ps1`: dead `-not (Test-Path "env:PATH")` branch removed; stale "W1-4" notes in both installers gone (only accurate milestone labels in file headers remain). |

## Extra fixes found along the way (not in DEBUG.md)

- **H4 fix had its own bug: `xml_escape` quote typo** — the `&gt;` line was `s="${s//>/&gt;'}"` (missing the opening `'`), which `bash -n` caught only as a confusing EOF-quote error at the end of the file. Now `s="${s//>/'&gt;'}"` matching its siblings; `bash -n` clean.
- **CRLF churn in git diffs** — this box's global `core.autocrlf` re-CRLF'd every checkout; committed `.sh` blobs (LF) diffed as full-file churn and the H5 rewrite of `install.ps1` showed 165/156. Added to root `.gitattributes`: `*.ps1 text eol=crlf` (PS 5.1 misreads non-CRLF ps1; index normalizes to LF so diffs stay minimal) and `*.sh -text eol=lf` (scripts break under CRLF); `git add --renormalize` + re-checkout. New scripts worktree state: ps1 CRLF, sh LF, diffs minimal.

- **loginLimiter nil-entry panic** — `recordOK` stores a `nil` per-IP entry; `purgeLocked` dereferenced it (`TestLoginSuccessAndFailures` crashed). Purge now deletes nil entries.
- **Windows: CRLF checkout broke minisign fixtures** — `core.autocrlf=true` re-CRLF'd `fixture.bin`/`*.minisig` on checkout, so `internal/update` failed with "Invalid signature" (33 vs 32 bytes; committed blob is LF). Fixed with a root `.gitattributes` (`*.minisig`/`*.bin`/testdata `-text` + `eol=lf`) and a byte-exact re-checkout. Update tests pass.
- **Windows: script timeout killed only the interpreter** — `sh -c "sleep 5"` left `sleep.exe` holding the output pipes, so `cmd.Wait` blocked to natural exit (a 300ms timeout took the full 5s). `internal/exec` now puts each interpreter in its own **Job Object** with `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE` (`exec_windows.go`): the timeout kill takes the whole tree down and releases the pipes. Unix side gets no-op `setup/ join/cleanup ProcessGroup` stubs. `TestRunScriptTimeout` now passes in 0.30s.
- **Go 1.27 API churn** — `os.Process.Sys()`/`.Handle()` removed; job assignment uses `os.Process.WithHandle` (requires go ≥1.26 → `agent/go.mod` bumped 1.25.0 → 1.26.0). `syscall`/`windows.SECURITY_ATTRIBUTES` unresolvable in this toolchain → `CreateJobObjectW(NULL, NULL)` default descriptor.

## Test status (as of this writing)

- **server module**: all packages pass (`go test ./...`), `go vet` clean; ingest suite stable across 5 consecutive runs.
- **agent module**: `go vet ./...` clean; **all packages pass** (caps, collectors, enroll, exec, logship, rotate, secure, update, uplink).
- **installers**: `bash -n scripts/install.sh` clean; darwin/plist branch functionally exercised with stubs (see H4/H5). `install.ps1` syntax verified by review only (no pwsh on this machine).
- `-race` not exercised on this machine (needs cgo, not installed).
- Test caveat for future work on this machine: monotonic `time.Now()` advances in ~528µs quanta — tests must not compare sub-tick durations (age stored timestamps instead of shrinking windows).

## Remaining work (planned order)

1. **C1 (harness)** — every e2e main (`server/cmd/e2e/main.go`, `adddevice`, `milestone`, `webhook`, `automation`, `logs`, `export`, `trust`, `caps`) + `cmd/mtlscheck` must login via `/api/login` (admin/admin) and send `Authorization: Bearer` on all `/admin/*` calls; rewrite `adddevice`'s open-bootstrap + 403-replay assertions for C1 + grace semantics.
2. **L10** — e2e bootstrap-token length guard.
3. **L1** — untrack committed binaries + `.gitignore`.
4. **Docs/env** — DEVELOPER.md "open /admin/*" lines; `RMMWAY_ENV` in `docker-compose.prod.yml`/`.env.prod.example`; confirm the frontend only uses `/api/*`.
5. **Final** — full `go build` + `go test` both modules, GOOS=linux cross-build of the agent (exec changed).
