# W4-4 — 🎯 "provable trust" demo (Block 2 milestone)

Evidence for the W4-4 definition of done: *a skeptic can (a) verify a release
signature + read the SBOM, and (b) export a client and confirm the data is
theirs.* This is the capstone of **Block 2 (Trust & Supply Chain)**: every
claim RMMWay makes about itself — "this binary is the one we shipped", "here
is what it's made of", "this data is yours and only yours" — is checkable by
an external party who trusts the builders with **nothing**.

**Harness:** `server/cmd/e2e/trust` (`make trust-e2e`), self-contained and
self-tearing-down. Runs in CI on every push (the `test` job).

## What it proves (in order)

**PART A — a skeptic verifies a signed release + reads the SBOM.** The
builder side (the party we do not trust) does the ordinary release dance:
cross-build the real agent, scan a CycloneDX SBOM with the pinned syft, mint
a minisign "release key", and sign the binary + SBOM + `SHA256SUMS` with
`tools/signer` (the exact signer CI uses). The **skeptic** side is a fresh
trust domain holding *only* what a downloader gets — the artifacts, their
`.minisig` signatures, the manifest — plus the public key delivered
out-of-band. The secret key never enters that directory. The skeptic then:

1. **A4.** verifies `SHA256SUMS` is signed by the public key, and re-hashes
   every listed file (`sha256sum -c` semantics) — the checksum manifest is
   itself authenticated, so the checksums are trustworthy;
2. **A5.** verifies the **binary** and the **SBOM** signatures against the
   public key using `go-minisign` — an *independent* implementation of the
   minisign format (interop-proven against the reference `minisign(1)` CLI in
   W3-4), not the signer that made them;
3. **A6. TAMPER.** flips one byte of the binary → the signature is
   **REJECTED** (`Invalid signature`) — the check has teeth;
4. **A7. WRONG KEY.** verifies under a different public key → **REJECTED**
   (`Incompatible key identifiers`) — the check is bound to *the* key, so a
   lookalike release under an attacker's key is caught;
5. **A8. READ THE SBOM.** parses the CycloneDX 1.7 document: the
   `metadata.component` is the binary *by name and by sha256* — the hash in
   the SBOM is recomputed from the real file and must match — and the
   dependency list actually reflects the agent (its real go.mod deps
   `go-minisign`, `gopsutil`, `grpc` are all present).

**PART B — a client owner exports a client and confirms the data is theirs.**
A client owner (also a skeptic about the server) enrolls one device on a
scratch Timescale DB, feeds it known samples with known values, then —
through the **real operator HTTP surface** (login, auth gate, the
`/devices/{id}/export` route; `401` unauthenticated, `404` unknown device) —
does a **one-click export** and confirms the bundle is *theirs*:

1. **B3.** the bundle verifies against its **OWN manifest** (self-describing:
   every file's sha256 + size + row count, no stray files — W4-3);
2. **B4. IDENTITY.** `device.json` describes exactly the client they own —
   `id` / `hostname` / `os` / `arch` / interface IP all match the enrolled
   device. This is "the data is *mine*", not "the data is *a* client's";
3. **B5. CONTENT.** the `metrics.parquet` holds **exactly** the samples they
   fed: the row count equals the samples sent (8,640 = 1 day × 3 series ×
   30 s), a spot sample's value + labels match the known input, `ts ≡
   timestamp_ms` on every row under an independent standard Parquet reader,
   and the time range is exactly the window fed;
4. **B6. TAMPER.** one flipped byte inside `metrics.parquet` → `Verify`
   **REJECTS** — the export cannot be silently altered in transit or at rest
   without detection.

## Result (this run)

```
== A4. skeptic: verify SHA256SUMS (signature + every listed checksum) ==
sha256 OK  rmmway-agent-linux-amd64
sha256 OK  rmmway-agent-linux-amd64.cdx.json
== A5. skeptic: verify the binary + SBOM signatures against the public key ==
minisign OK  rmmway-agent-linux-amd64  (sig comment: "...; rmmway release v0.9.0")
minisign OK  rmmway-agent-linux-amd64.cdx.json  (sig comment: "...; rmmway release v0.9.0")
== A6. skeptic: TAMPER (flip one byte) -> the signature is REJECTED ==
tampered binary REJECTED (ok=false err=Invalid signature) — a flipped byte is caught
== A7. skeptic: WRONG KEY (different public key) -> REJECTED ==
wrong-key verification REJECTED (ok=false err=Incompatible key identifiers)
== A8. skeptic: read the SBOM (CycloneDX) + tie it to the binary + its deps ==
SBOM: bomFormat=CycloneDX specVersion=1.7
SBOM component "rmmway-agent-linux-amd64" sha256 == the real binary's sha256
SBOM lists 16 components incl. the agent's real deps (go-minisign, gopsutil, grpc)
== B3. confirm it's THEIRS: verify against its OWN manifest ==
self-describing Verify OK: 6 files, metrics=8640 rollups=4320 alerts=3 (all hashes/rows check out)
== B4. confirm IDENTITY: device.json matches the client they own ==
identity confirmed: dev_trust_e2e / fileserver-trust / linux/amd64 / 10.1.2.3
== B5. confirm CONTENT: the Parquet is exactly the samples they fed ==
standard reader re-read 8640 rows; ts == timestamp_ms on every row; spot sample matches the value they fed
time range = [08/20 00:00, 08/20 23:59] — exactly the window they fed
== B6. TAMPER: flip one byte in metrics.parquet -> Verify REJECTS ==
tampered bundle REJECTED: metrics.parquet: sha256 27e29b0d…, manifest says fd6d4611…
== PASS ==
```

## The same flow on the REAL published release (external cross-check)

The harness proves the *mechanism* end-to-end on a throwaway key so it is
reproducible anywhere. The identical skeptic flow was also run against the
**real GitHub release `v0.5.0`** (29 assets) using **only the public key
committed in the repo** (`keys/minisign.pub`, key id `019BF5A0CA5040DD`):

```
$ gh release download v0.5.0 --dir /tmp/rmmway-real     # 29 assets
$ sha256sum -c SHA256SUMS                                # repo layout
... 12/12 OK
$ rmmway-signer verify -p keys/minisign.pub <14 signed artifacts>
13/13 OK  (5 agent binaries + 5 agent SBOMs + install.sh + install.ps1 + SHA256SUMS)
+ OK  rmmway-server.cdx.json                             # server image SBOM: 14/14
$ diff <(tail -1 minisign.pub from the release) keys/minisign.pub
IDENTICAL
```

and every SBOM reads as CycloneDX 1.7 with its component hash equal to the
real artifact's hash (5/5 agent binaries, 14–15 Go components each; the
server image SBOM carries the debian 12.15 OS component + distro packages,
979 components). A skeptic with `keys/minisign.pub` — or the key pinned
in-binary by the agent (W4-2) — can reproduce all of it with nothing else.

## Why this is "provable" (the trust model)

- **One trust anchor.** The minisign public key is the *only* thing the
  skeptic must accept out-of-band. It is committed to the repo, shipped in
  every release, and pinned in the agent binary (W4-2) — so even an
  auto-updating agent can check its own replacement. Everything else —
  checksums, binaries, SBOMs, installers — derives from that one key.
- **Independent verification.** Signatures are made by `tools/signer` and
  checked by `go-minisign` (a separate implementation, interop-tested against
  the reference C CLI in W3-4); the export bundle is checked by
  `export.Verify` against the bundle's *own* manifest with an independent
  standard Parquet reader. Neither side needs to trust the other's code —
  only the format + the key.
- **Data portability.** The export is standard Parquet + JSON (W4-3), so
  "the data is theirs" is not an RMMWay promise — the client owner can open
  it in duckdb/pandas/polars, verify the hashes, and re-import it anywhere.

## Notes & caveats

- **Part A is throwaway-key by design in the harness.** It mints its own
  release key so the e2e is hermetic (no secret key in CI, no dependence on a
  specific published release). The real-release cross-check above is the
  same flow with the real key + the real release. Locally, `make sign` +
  `make verify-sigs` covers the same ground with the committed key.
- **SBOM scanner is pinned** (syft 1.51.0 via `scripts/install-syft.sh`,
  sha256-verified tarball) so the SBOM is reproducible from the same binary.
- **Part B needs Timescale** (raw metrics + the 1-minute continuous
  aggregate) and a user that can `CREATE DATABASE` for the scratch DB —
  `RMMWAY_PG_DSN=postgres://postgres@localhost:5432/postgres` locally, the
  `timescale/timescaledb` CI service in GitHub Actions.
- The fixture is small by design (1 day × 3 series = 8,640 samples); the
  full export guarantees (windowing, re-import into a fresh DB) are carried
  by `make export-e2e`, which runs in the same CI job.

## How to run

```sh
# Part A needs only `go` (syft is auto-installed, pinned, if absent).
# Part B needs a Timescale PG where the user can CREATE DATABASE.
make trust-e2e
# or:
cd server
RMMWAY_PG_DSN=postgres://postgres@localhost:5432/postgres go run ./cmd/e2e/trust
```
