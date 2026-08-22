# RMMWay — Product & Architecture Strategy

> A senior-architect ideation pass across all 10 categories. Ideas are concrete and
> effort/impact-rated. Unconventional or risky ideas are flagged. Bias throughout:
> exploit the **self-hosted, modern-stack** nature (Go+gRPC, TimescaleDB, NATS,
> Meilisearch, React+Tauri) as a structural advantage over incumbent SaaS RMMs
> (NinjaOne, Datto RMM, Syncro, Atera, Halo, Kaseya, N-able/N-Central).

**Legend** — Effort: S (<~2 eng-weeks), M (2–8 weeks), L (>8 weeks / multi-sprint).
Impact: Low / Medium / High. ⚑ = unconventional or risky.

---

## 1. Agent Innovation

### One-command bootstrap via SSH/SMB/WMI, no installer
- Description: Technicians paste a single line — `curl | sh`, an SMB-dropped
  `.ps1`, or a WMI remote-exec — and the agent self-installs, self-enrolls
  (pulling an agent JWT from the server), and reports back. No MSI/DMG build
  matrix per platform, no GPO authoring. Ship the agent as a **single static
  Go binary** per OS (no runtime deps, works air-gapped from day one).
- Effort: M — Impact: High
- Why it wins: Incumbents push ~100MB installer suites. A 15–25MB static binary
  that bootstraps in 30s collapses onboarding to near-zero.

### WSL/edge agent tier (sub-10MB, ~15MB RAM)
- Description: A second, stripped agent build that runs on Raspberry Pis, thin
  clients, NAS boxes, and WSL2 — exposes metrics + a tiny command channel,
  skips the full inventory/remote-control surface. Feature-flagged via a build
  tag so the core stays lean. Target: meaningful telemetry on a $35 Pi 4.
- Effort: M — Impact: Medium
- Why it wins: Incumbents' agents simply don't run on edge hardware. This is
  how you capture "every device in the estate," not just the managed Windows
  fleet — a genuine coverage moat.

### Agent mesh / gossip for LAN discovery
- Description: Agents on the same subnet gossip a signed presence beacon
  (NATS on a LAN multicast fallback, or a tiny gossip layer like
  SWIM). When a new agent enrolls it learns the subnet topology from neighbors
  instead of a full server round-trip. Enables fast local device-to-device
  checks ("is the printer on this VLAN reachable?") without the server in the loop.
- Effort: L — Impact: Medium
- ⚑ Risk: adds an unmanaged P2P surface. **Must be off by default and mTLS
  scoped to site** — a bug here is a lateral-movement vector. High value for
  distributed MSPs (fewer server round-trips), but the security bar is brutal.

### Battery-aware scheduling for laptop fleets
- Description: Agent classifies the host (AC/DC, thermal headroom, CPU steal)
  and shifts non-urgent work (inventory sweeps, patch staging, deep scans) into
  AC-powered + idle windows. Uses the platform's existing power/thermal metrics —
  no new collectors. Expose a per-client "power budget" knob MSPs can tune.
- Effort: M — Impact: Medium
- Why it wins: This is the #1 complaint about RMM agents on endpoint laptops
  ("it drains my battery / slows my machine"). Solving it is a retention and
  word-of-mouth win incumbents treat as a non-issue.

### Offline-first replay with content-addressed, idempotent spool
- Description: Harden the offline buffer into a proper **durable outbox**:
  every metric/script event is content-addressed (hash), locally persisted in
  LMDB, and replayed with server-side dedup by event ID. Guarantees at-least-
  once delivery with **no double-execution of remediation scripts** — the classic
  offline-buffer bug where a "restart service" script fires 4× when the link
  returns. This is what makes field offices / satellite sites actually viable.
- Effort: M — Impact: High
- Why it wins: Most RMM "offline buffers" are lossy and non-idempotent. Making
  replay provably safe is a real differentiator for connectivity-poor estates.

### Signed, verifiable agent identity (device attestation)
- Description: On first boot the agent derives a **device key** (attested on
  TPM/Secure Enclave where available, hardware-bound secret elsewhere) and the
  server issues a cert from the org root. Every subsequent enroll is
  cryptographically bound to the physical device — a stolen/cloned binary
  elsewhere can't enroll. Pair with a server-side "this device is new / this
  device moved" alert.
- Effort: M — Impact: High
- Why it wins: Directly feeds the zero-trust and compliance stories (see §4).
  Turns "we deployed an agent" into "we *attest* device identity."

---

## 2. Monitoring & Observability

### Signal RMMs are missing: application-layer + UX metrics
- Description: Beyond CPU/disk/net, capture signals that correlate to *user*
  complaints: app launch-to-ready time, browser/page load, network RTT to key
  SaaS (Microsoft 365, Google, Zoom), DNS resolution time, and "app hung
  >30s" from the event stream. These are the signals that actually explain
  "the computer is slow" tickets.
- Effort: M — Impact: High
- Why it wins: Turns the RMM from an infra monitor into a **user-experience
  monitor**, which is what justifies the seat/endpoint spend. Few incumbents
  do egress-latency-to-SaaS well.

### Log strategy: ship to Loki, don't rebuild ELK
- Description: **Do not build native long-term log storage.** The agent tails
  structured events and ships them (via the same NATS bus) to **Loki** for
  retention/search, with TimescaleDB holding only indexed, queryable log *events*
  (structured alerts/errors) for the last N days. This is exactly the "native
  open-source integrations" differentiator — Prometheus metrics, Loki logs,
  Grafana viz — and lets you offload the expensive log-retention problem to
  infrastructure the self-hoster already likes.
- Effort: M — Impact: High
- ⚑ Note: the trap is building a proprietary log DB to "own" the data. Resisting
  that and integrating Loki/Prometheus is the strategically correct call — it
  lowers self-hoster friction and makes you the *control plane* on top of the
  OSS observability stack rather than a competitor to it.

### AIOps without a data-science team: dynamic baselining + seasonal z-scores
- Description: Use TimescaleDB continuous aggregates + a lightweight, **deterministic**
  anomaly layer (per-metric rolling baseline by day-of-week/hour, robust z-score
  or MAD, plus EWMA for trends). No ML models to train/host; runs as a Go
  background job. Alert on "this CPU is 4σ above *this server's* 3pm-Friday
  norm," not on static thresholds. This is the "dynamic baselining" already on
  the roadmap — make it the concrete mechanism.
- Effort: M — Impact: High
- Why it wins: Kills the #1 RMM pain — alert noise / threshold tuning. An MSP
  that no longer tunes 10,000 static thresholds will not churn.

### Network topology auto-discovery from live data
- Description: Ingest SNMP (LLDP/CDP neighbor tables), ARP caches, DHCP
  leases, and the agent's own ARP route table to build a **live topology graph**
  (switch↔NIC, server↔VLAN, client↔gateway). Render as an interactive graph and
  flag "device vanished from its VLAN" or "new unmanaged device appeared on
  subnet X." Persist the graph in Meilisearch-backed graph + Timescale for
  drift-over-time.
- Effort: L — Impact: High
- Why it wins: Incumbents give you a flat device list. A *live, self-updating
  network map with change detection* is a genuine differentiator and a strong
  demo.

### Synthetic checks + "is it the user or the network?" triage
- Description: Let an MSP define synthetic checks (ping, HTTP, DNS, TCP to a
  port, RDP handshake) from an agent on a given VLAN, scheduled at low
  frequency. When an alert fires, auto-run a small battery of synthetics from
  2–3 vantage points so the tech can instantly tell "server down" vs "one
  user's LAN" vs "WAN path." 
- Effort: M — Impact: Medium

---

## 3. Automation Deep Cuts

### Self-healing playbooks (detect → verify → remediate → confirm)
- Description: Codify remediation as a state machine, not a one-shot script:
  *detect* (condition true) → *verify safe* (pre-check, e.g. "no one logged in")
  → *remediate* (action) → *confirm* (re-measure, N times) → *escalate* (if
  confirm fails, open ticket + notify). Every transition is audited and
  replay-safe (idempotent, per §1 outbox). Ship a starter library: disk full,
  service down, WSUS stuck, DNS stale.
- Effort: M — Impact: High
- Why it wins: One-shot "restart service" is the current state of the art in
  RMM. *Verified* self-healing (with confirmation + escalation) is what reduces
  tech touch-time — the actual ROI metric.

### Declarative desired-state enforcement (lightweight Puppet, no agent-rewrite)
- Description: A **convergent config layer**: a client declares desired state as
  simple YAML (services running, firewall rules, scheduled tasks, software
  installed, registry/group-policy parity). A lightweight "convergence" engine
  on the agent diffs actual vs. desired each cycle and reconciles + reports
  drift. This is the Puppet/Chef value without the heavyweight agent and
  server — and it's what gives you a real **compliance + remediation** loop
  (see §4) from the same primitive.
- Effort: L — Impact: High
- ⚑ Risk: full Puppet/Ansible feature parity is a quagmire. **Scope it to a
  bounded set of resource types** (services, software, firewall, scheduled
  tasks, files, registry keys) and lean on the Ansible bridge in §6 for the
  long tail. Do not try to re-implement Ansible's entire module ecosystem.

### Event-driven automation chains (the "graph" of automation)
- Description: Model automations as **DAGs of triggers→actions** wired over NATS
  (which you already have — this is cheap): `disk>90% → free space → if still >90%
  → snapshot → notify → open ticket → escalate to on-call`. Technicians compose
  these visually; every node is a signed, auditable action. Persist the chain
  definition in the same Git-backed library.
- Effort: M — Impact: High
- Why it wins: NATS makes event wiring near-free for you; incumbents bolt
  "workflows" on after the fact. Being event-native from day one is a real
  architectural advantage to productize.

### Community script marketplace (with a safety model)
- Description: A Git-backed, **signed** script library where the community
  contributes, with a two-tier trust model: (a) org-internal (auto-trusted,
  signed by your key) and (b) community (run in a sandboxed/default-deny mode,
  requires explicit tech approval, network-scoped). Every run writes to the
  audit trail. Version, fork, and rate scripts.
- Effort: L — Impact: Medium
- ⚑ Risk: an unvetted script marketplace is how an RMM becomes an
  **attack vector** (this is a real, historically documented class of RMM
  incident). The sandbox/default-deny + per-run approval + signed-only
  enforcement is non-negotiable. Ship it read-only/disabled for enterprises by
  default.

### GitOps for infrastructure through the RMM
- Description: The RMM becomes a **Git remote for your endpoints**: point it at a
  repo, and declared desired-state / automation / inventory-tags live as
  files. A pull request that changes desired-state → preview diff of which
  endpoints are affected → merge → staged convergence rollout. Full PR review,
  blame, and rollback for infrastructure changes — the way SREs already work.
- Effort: L — Impact: High
- Why it wins: This is the single strongest "modern stack" story you can tell.
  No incumbent RMM is a first-class GitOps tool. It converts the tech's "apply
  to 200 machines, pray" workflow into reviewable, reversible, audited changes
  and is the natural home for the §3 declarative layer.

---

## 4. Security Differentiators

### Zero-trust agent channel (mTLS + short-lived certs + per-action scoping)
- Description: Elevate the mTLS agent link into real zero-trust: **short-lived,
  auto-rotating certs** (leaf ~1h, root pinned), **per-action capability tokens**
  (an agent can't execute "remote control" unless a session token for it is
  minted and time-boxed), and per-client network egress scoping. The server
  never holds standing "do anything" credentials.
- Effort: L — Impact: High
- Why it wins: This is the headline feature for the data-sovereignty /
  security-sensitive buyer (healthcare, finance, gov). Make it a named,
  marketable capability, not an implementation detail.

### Runtime threat detection via Wazuh/Suricata bridge
- Description: Integrate with the OSS security stack rather than building a
  detection engine: ship a thin agent-side collector that forwards to **Wazuh**
  (endpoint/detect), and optionally ingest **Suricata** (network) alerts as
  first-class RMM events. A compromised endpoint then triggers *RMM
  remediation* (isolate host, kill process, quarantine file) via the
  automation engine — closing the detect→contain loop inside the tool the tech
  already has open.
- Effort: M — Impact: High
- Why it wins: MSPs are adding XDR/MDR on top of their RMM. If containment
  lives *in* the RMM, you capture that workflow. Wazuh integration is well-trodden
  and keeps it "native open-source" — on-brand.

### Secrets management for scripts (Vault / SOPS)
- Description: Scripts and automations should never embed credentials. Integrate
  **Vault** (or SOPS + age for the self-hosted lightweight case) so a script
  references a secret *path* and the agent/runner fetches it at run-time with a
  short-lived lease. Secrets are AES-256 at rest, never in the script source,
  and access is audit-logged. Provide a dead-simple "this script needs
  SECRET_aws" declaration.
- Effort: M — Impact: Medium
- Why it wins: "Where do I paste the customer's RDP password in the script?"
  is a daily RMM pain and a real security hole. Solving it is table-stakes
  that you can do well with Vault, and it's a big trust-builder for
  security-minded self-hosters.

### Compliance automation: CIS/STIG scans → gap report → one-click remediate
- Description: Ship signed scan profiles (CIS for Windows/Linux, STIG, plus
  customer-specific) that map to the §3 declarative resource types. Run a scan →
  produce a **gap report with evidence** (for auditors) → for any fixable item,
  one click generates the desired-state change and runs it through the staged
  rollout. Export to PDF/CSV for the compliance file.
- Effort: M — Impact: High
- Why it wins: Compliance reporting is a *recurring revenue* activity for MSPs
  and a top purchase driver for healthcare/gov. Automating scan→report→remediate
  is high-leverage and reuses the declarative + automation primitives you're
  already building.

### Supply-chain security for RMMWay itself (SBOM + signed releases + provenance)
- Description: Make the RMM *itself* supply-chain-clean: emit a **SPDX/CycloneDX
  SBOM** for every image, **sign all releases** (Go cosign / Sigstore for the
  server + agent binaries; minisign for installers), pin dependencies, and
  publish a reproducible-build statement. Agents verify the server's release
  signature before auto-updating.
- Effort: M — Impact: High
- Why it wins: The RMM is *the* high-value target (own the RMM, own every
  client machine). For a self-hosted product, **provable supply-chain integrity
  is the core trust product**. This is where "data sovereignty + no vendor
  lock-in" meets "I can verify you." Ship this early — it's cheap relative to
  the trust it buys and it's the strongest answer to "why should I let this
  into my estate."

---

## 5. UX & Product Experience

### Why existing RMM UIs are bad (and the fix)
- Description: Incumbent UIs are (a) *device-centric* — you find a machine,
  then hunt through tabs for the thing you need — rather than *task-centric*;
  (b) buried under 200 near-identical menu items; and (c) built for the
  mouse, not the "5 alerts at 6am" reality. Fix: **task-centric IA** (the home
  screen is "what needs attention right now, grouped and actionable"), a
  **global command palette** (Cmd-K) across the app, and progressive
  disclosure so the common 20% of actions are 1–2 clicks deep.
- Effort: M — Impact: High
- Why it wins: This is a defensible, demo-able difference. A tech can feel the
  difference in 10 seconds of your UI vs. N-Central — and they talk to each
  other.

### Command palette (Cmd-K) as the primary nav
- Description: A fuzzy, Meilisearch-backed command palette: type "restart
  svc on fileserver" or "disk under 10% on client Acme" and it executes or
  navigates. Actions, devices, scripts, and searches all live in one index.
  Meilisearch (already in the stack) makes this fast and cheap.
- Effort: S–M — Impact: High
- Why it wins: Turns a 200-menu app into "type and go." Huge keyboard-flow win
  for power users, and a memorable demo moment.

### Mobile-first alert triage (Tauri companion or PWA)
- Description: A lightweight mobile surface (Tauri-mobile or a PWA) tuned for
  *triage, not control*: the on-call tech sees the top alerts, one-tap
  acknowledge, runs a pre-approved self-heal playbook, and opens a ticket —
  from a phone in a parking lot. Full remote control stays on desktop; mobile
  is the "decide and dispatch" layer.
- Effort: M — Impact: High
- Why it wins: The 6am alert is *always* a phone moment today, currently
  handled by opening a full browser RMM on a tiny screen. Owning that moment
  is a retention differentiator.

### Natural-language querying over the live estate
- Description: "show me all servers with <10% disk" / "which Acme machines had
  failed patches this week." Back this with (a) a **semantic-to-query compiler**
  that maps NL→ your query DSL using a small local LLM or embeddings over the
  known fields, and (b) Meilisearch for fuzzy device/script lookup. Because you
  *know the schema* (unlike generic RAG), this can be deterministic and
  auditable, not hallucinated.
- Effort: M — Impact: Medium
- ⚑ Note: keep it a *query assistant* (it compiles to a reviewable query you
  can see), not a free-form "do anything" agent. The auditability is what makes
  it trustworthy in a security product.

### No-code dashboard builder with a query builder
- Description: Drag-and-drop dashboard from saved queries (metrics, logs,
  inventory, topology) with per-role views (executive / tech / client). Client
  white-label views are *the same builder* with a branding + data-scoping layer
  on top — no separate "portal" codebase. Ship a template gallery.
- Effort: M — Impact: Medium
- Why it wins: One builder for all three portal types (exec/tech/client)
  avoids the classic "the client portal is a whole separate app" sprawl.

### Zero-to-monitored in under 5 minutes
- Description: Design onboarding around a *single happy path*: `curl | sh` →
  agent reports → auto-inventory → auto-topology → first actionable dashboard,
  with zero configuration required. Everything else (SSO, compliance,
  marketplaces) is opt-in *after* the first device is green. Measure it as a
  product metric (median time-to-first-monitored-device) and drive it down.
- Effort: S–M — Impact: High
- Why it wins: Time-to-value is the strongest free-trial → paid converter.
  "Monitored a machine in 4 minutes" is the single best line in a sales deck.

---

## 6. Integrations Ecosystem

### Under-served integrations that unlock new segments
- Description: Prioritize the integrations that map to *underserved buyer
  segments*, not the obvious big four: **Google Workspace / M365 admin**
  (license + user state), **network gear via NETCONF/gNMI** (not just SNMP),
  **backup/restore (Veeam, Proxmox, Restic)** (a top co-purchase with RMM),
  **identity (Entra, Ping, Authelia)** beyond SSO, and **BISS/credit (billing
  for MSPs)**. Each one is a door into a segment incumbents treat as an afterthought.
- Effort: M (each) — Impact: High (collectively)

### Reverse ETL: push RMM data into the customer's warehouse
- Description: An out-of-the-box connector layer that syncs RMM data
  (inventory, ticket/asset linkage, compliance status, uptime) into
  **BigQuery / Snowflake / S3-Parquet / a Postgres the customer owns** on a
  schedule — the customer analyzes *their* estate with *their* BI tools.
  Because you're self-hosted, the customer already owns the DB; this just
  adds the "extract to my analytics" path.
- Effort: M — Impact: Medium
- Why it wins: For the larger self-hosters and data-sovereignty buyers,
  "our data lives in *our* Snowflake" is a hard requirement. Native reverse
  ETL is a strong enterprise unlock.

### First-class webhook + event-stream framework (DIY integrations)
- Description: Expose the NATS event bus as a **webhook + SSE/subscription
  framework**: any alert, inventory-change, or automation event can be pushed
  to an endpoint the user defines, with signed payloads (HMAC), retries, and
  replay. This lets *anyone* wire RMMWay into anything without waiting on you.
- Effort: S — Impact: High
- Why it wins: NATS makes this nearly free for you. It's the "if it has an API,
  we integrate with it" answer and it's what makes the "native integrations +
  no lock-in" promise *structural* rather than aspirational. Ship early.

### Docs sync: Notion / Obsidian / Confluence as a live knowledge base
- Description: Two-way (or RMM→doc) sync of runbooks / known-issues / client
  notes into the customer's existing knowledge tool, and pull in runbook
  *content* so an alert can surface the linked runbook inline. Obsidian/Notion
  specifically unlocks the homelab→prosumer segment who already live in those
  tools.
- Effort: M — Impact: Medium

### Ansible/Terraform bridge for existing IaC users
- Description: Rather than re-implementing provisioning, **emit and consume
  IaC**: (a) generate Ansible inventories / Terraform variables from your
  live inventory so the customer's *existing* IaC works against your data, and
  (b) accept a Terraform-managed inventory as a source of truth. This respects
  the modern-ops buyer who already runs Terraform and refuses to move IaC into
  the RMM.
- Effort: M — Impact: Medium
- ⚑ Strategic note: this is the *correct* posture — be a **source of truth for
  inventory**, not a competing IaC engine. Don't try to out-Terraform
  Terraform; be the live-data layer it consumes.

---

## 7. Pricing & Business Model

### "Free forever" self-host core + paid control-plane features (open-core)
- Description: The **agent, core monitoring, and a single-site self-host are
  free forever** (the hook + the community + the moat). Paid (the "way" tier):
  **multi-client MSP controls** (white-label portals, per-client RBAC, client
  billing metadata), **advanced automation/compliance**, and **the GitOps +
  marketplace surfaces**. This is open-core done *right*: the free tier is
  genuinely useful for a homelabber and a small in-house team, so you can't be
  accused of a "demoware" free tier.
- Effort: M (packaging/licensing) — Impact: High
- Why it wins: "No per-tech fees" + a free single-site tier is the exact wedge
  against Atera/Ninja/Datto. It also aligns the homelab segment (free users)
  with the enterprise segment (paid) as one funnel — see §10.

### Value-tiered pricing, not just per-agent
- Description: Move off a pure per-agent meter (which punishes your best
  customers and invites gaming) to **tiered by capability + a per-agent floor
  that's cheap**: e.g. a base platform fee + tiers that unlock modules
  (monitoring / automation / compliance / remote-control), with per-agent
  pricing only as a scaling floor above a generous free threshold. Transparent,
  published, no sales-call required.
- Effort: S (model) — Impact: Medium
- ⚑ Note: pure "capability subscription" can read as lock-in to a segment that
  values sovereignty; **keep the per-agent component cheap and visible** so the
  model is obviously fair. Hybrid (base + cheap per-agent + capability tiers)
  is the safest.

### Managed cloud as an *option*, not the default (multi-tenant "RMMWay Cloud")
- Description: Offer a **managed multi-tenant cloud** for those who don't want
  to self-host — but architect it as *the same binary* with a tenancy layer, so
  the self-host and cloud never diverge. Price the cloud as a convenience
  premium, and make it trivial to **export back to self-hosted** (no lock-in).
  The no-lock-in guarantee (one-click full export) *is* the marketing.
- Effort: L — Impact: High
- ⚑ Risk: multi-tenancy is the single hardest thing to get right in a
  sovereignty product — a tenancy isolation bug destroys the entire trust
  thesis. Do **hard isolation (per-tenant data planes), not soft row-level
  tenancy**, and never let cloud features create a dependency the self-host
  can't replicate. Sequence this *after* the self-host is excellent.

### Marketplace revenue share
- Description: Once the script/compliance/template marketplace (see §3) is
  live, take a **revenue share on paid community assets** (premium scripts,
  scan profiles, dashboard templates) and on the **plugin/add-on** ecosystem
  (§9). This monetizes the community and creates a reason for third parties to
  build *on* RMMWay — a real network effect and moat.
- Effort: M — Impact: Medium
- ⚑ Prerequisite: the marketplace security model (default-deny, signed,
  sandboxed) must be solid *first* or the revenue share funds a liability.

### MSP partner program (the real GTM engine)
- Description: An **MSP partner/ISV program** with: (a) white-label +
  co-brand, (b) a partner pricing tier, (c) a resell/implementation channel
  with revenue share, and (d) a "bring your own clients" onboarding concierge.
  MSPs are the *channel* — an MSP switching brings dozens of client estates at
  once. Incentivize *switching* specifically with migration tooling credits.
- Effort: M — Impact: High
- Why it wins: RMM is won MSP-by-MSP, not endpoint-by-endpoint. A structured
  partner program with migration incentives is the fastest path to the
  "10,000+ endpoints" target segment.

---

## 8. Niche Use Cases

### OT/ICS (operational technology) monitoring
- Description: A **read-only, out-of-band OT agent** (Modbus/OPC-UA/proprietary
  via plugins) that never sends control writes, is network-isolated on a
  dedicated segment, and surfaces only state/health + trend data. Pair with a
  strict "monitor-only, no remediate" mode that disables the automation engine
  on OT-tagged assets.
- Effort: L — Impact: Medium
- ⚑ Risk: **OT is a "you break it, a physical thing stops" environment.**
  The entire remediation surface must be hard-disabled on OT assets; a
  misfiring self-heal on a PLC is catastrophic. This is a *monitoring* wedge
  into an underserved, high-ACV segment, not a full RMM for OT.

### Healthcare / HIPAA compliance workflows
- Description: Package the §4 compliance + audit + SSO + encryption primitives
  into a **HIPAA-ready profile**: audit-log retention guarantees, access-review
  reports, a BA-friendly data-handling posture, and pre-built scan templates for
  the common healthcare baseline. Market the *evidence generation* for audits,
  not "we're HIPAA-compliant" (which is the customer's attestation to make).
- Effort: M — Impact: Medium
- ⚑ Note: you enable compliance; the *customer* bears the HIPAA obligation.
  The product's job is to make evidence collection and access control trivial
  so their compliance officer can actually do the attestation.

### Education / school-district fleet management
- Description: K-12 and higher-ed have huge, cheap, churning fleets (thin
  clients, 1:1 devices) with tight budgets and seasonal churn. Offer a
  **low-perf agent tier** (from §1), **classroom/room-based grouping** (VLAN +
  physical location as a first-class tag), **end-of-year device retirement
  workflows**, and a **flat, budget-friendly price**. Seasonal onboarding
  (August) is your sales cycle.
- Effort: M — Impact: Medium

### Gaming industry server fleet management
- Description: Game studios and server hosts run dense, churning fleets of
  headless Linux servers (game servers, build nodes, dev boxes) with
  short-lived machines. Offer **ephemeral-host-friendly enrollment** (agent
  survives container/VM teardown, re-binds on boot), **build/deploy pipeline
  hooks**, and **GPU/thermal monitoring** as first-class metrics. The
  "server lives 3 days" reality breaks normal RMM lifecycle assumptions.
- Effort: M — Impact: Low–Medium

### Air-gapped / disconnected environments
- Description: A genuine **air-gap mode**: a single signed, versioned "drop"
  bundle (server images + agent binaries + offline update repo + SBOM) that a
  customer can air-ship, with a local update server and zero outbound
  dependency. This is the *ultimate* data-sovereignty story — defense,
  finance, gov, and manufacturing all run air-gapped.
- Effort: M — Impact: High
- Why it wins: Almost no RMM supports *true* air-gap with a clean offline
  update story. For the sovereignty-focused buyer this is a differentiator that
  turns the "self-hostable" claim into a provable, extreme-environment
  capability. Pairs directly with the §4 supply-chain signing (verify the
  air-gap bundle's provenance).

---

## 9. Technical Architecture

### Multi-region / multi-site agent routing (edge gateways)
- Description: Because the agent link is mTLS+gRPC, deploy **regional edge
  gateways** that agents connect to locally (low latency, resilient WAN); the
  gateway relays to the central control plane and holds the offline outbox
  (from §1) as a regional cache. This makes a global MSP estate feel local,
  and it's the natural substrate for the air-gap mode (a gateway *is* the
  regional drop).
- Effort: L — Impact: High
- Why it wins: Incumbents are single-cloud, single-region. A self-hoster who
  runs sites in 3 countries needs this; few competitors offer it.

### Agent transport: gRPC over QUIC/HTTP3 with HTTP/2 fallback
- Description: Evolve the agent channel to **gRPC-over-QUIC (HTTP/3)** with a
  negotiated **HTTP/2 gRPC fallback**. QUIC gives connection migration
  (laptops moving WiFi→cell → the *same* stream survives), 0-RTT reconnection,
  and better NAT traversal — a huge win for the mobile/laptop fleet and
  connectivity-poor sites you're targeting. Feature-flag by client capability.
- Effort: L — Impact: Medium
- ⚑ Risk: QUIC/HTTP3 still has inconsistent middlebox/ISP support (some
  corporate/edge firewalls drop UDP). The **HTTP/2 fallback is mandatory** and
  must be seamless, or you'll create flaky agents. Worth it for connection
  migration; don't over-promise.

### Database partitioning strategy at 10k+ endpoints
- Description: TimescaleDB continuous aggregates + **hypertable partitioning by
  time** for metrics/events, and a **per-client logical partition** (schema or
  RLS) so one client's data is physically/logically separable — this is what
  makes the "full export per client" (no lock-in, §7) and the multi-tenant
  isolation story (§7) *architectural* rather than a data-engagement. Add
  **time-based tiering** (hot in Timescale, warm/cold to S3/Parquet via MinIO)
  for cost at scale.
- Effort: L — Impact: High
- Why it wins: Getting partitioning right *early* is what lets you hit 10k+
  endpoints without a rewrite, and it's what makes per-client export and
  tenancy cheap. This is a "boring but load-bearing" decision.

### Plugin system: WASM (wasmtime), not Go plugins
- Description: Build the extensibility layer on **WASM (wasmtime)** for
  collector / script / integrator plugins — deterministic, sandboxed,
  cross-platform, no cgo, and **the host can't be crashed by a bad plugin**.
  This also gives the marketplace (§3) and community a *safe* artifact format
  (a WASM module is inspectable + signable). Go plugins (shared libs) are the
  wrong tool: no sandbox, no cross-platform, glibc hell.
- Effort: L — Impact: High
- ⚑ Note: WASM has performance limits for heavy work (use it for collectors /
  glue, not for the core metrics hot path — keep that in Go). But the
  **sandboxing + signability + portability** make it the correct substrate for
  a *community* plugin marketplace. This is one of the few places the modern
  stack gives you a clean structural edge over C++/.NET incumbents.

### Edge compute on agents for distributed query processing
- Description: Push **query computation to the agents** for estate-wide
  aggregations: "average CPU across 5,000 hosts" is computed *on the edge* and
  only the reduction result ships up, instead of 5,000× raw series hitting the
  server. This is a massive bandwidth + server-CPU saver at 10k+ endpoints and
  it's a natural fit for the WASM plugin model (the "compute" is a WASM op that
  runs on each agent).
- Effort: L — Impact: Medium
- ⚑ Risk: adds distributed-query complexity (consistency, partial results).
  Worth it primarily for the *largest* estates; gate it behind estate size and
  make it a correctness-safe **optimization** (fall back to central query).

---

## 10. Community & Growth

### Documentation as a moat
- Description: Invest in docs as a *product*: versioned, searchable (Meilisearch
  over the docs), with **working examples for every feature**, an **air-gap
  install guide**, **API/developer docs for the webhook + plugin layer**, and
  **comparison/upgrade pages**. Great docs are a retention + trust moat for
  self-hosters (who will read every page) and the #1 "why not competitor X"
  decider. Treat docs quality as a release gate.
- Effort: M (ongoing) — Impact: High

### Community contribution incentives
- Description: A visible contributor program: **script/template marketplace
  listing with attribution + revenue share** (from §7), a **contributor badge
  + "built with RMMWay" badge** for the homelab segment, a public roadmap with
  upvotes, and a **"good first issue" pipeline** sourced from the docs gaps.
  The homelab contributors are *exactly* the future enterprise buyers.
- Effort: M — Impact: Medium

### Self-serve demo/sandbox (the conversion engine)
- Description: A **one-click, ephemeral demo environment** — either a hosted
  trial or (better, on-brand) a **single `docker compose up` / k3d script that
  spins up a full RMMWay + 10 fake devices generating live data in 2 minutes.**
  The self-hosted *demo* is the product demo. Let prospects evaluate in *their
  own environment*, which is what sovereignty buyers actually want to verify.
- Effort: M — Impact: High
- Why it wins: "docker compose up → live monitored fleet in 120 seconds" is the
  most convincing possible demo for your exact buyer and it's cheap to build
  because you're already containerized.

### Migration tooling from competing RMMs
- Description: Build **importers for the top incumbents**: pull inventory,
  groups, and (where possible) scripts/config from NinjaOne, Datto, Syncro,
  Atera, N-Central, Kaseya — via their APIs + CSV. This is the single highest-
  leverage *sales* feature: an MSP won't switch 10k endpoints unless switching
  is cheap. Pair with the partner-program migration credits (§7).
- Effort: L — Impact: High
- Why it wins: Migration friction is *the* switch-cost barrier in RMM.
  Removing it — "switch and your inventory/scripts/groups come along" — is the
  difference between a "maybe" and a "next quarter."

### Homelab → enterprise content pipeline
- Description: A deliberate **funnel of content** that starts at homelab and
  walks up: (a) homelab setup + automation content (the volume/SEO engine and
  the community), (b) "scaling from homelab to a real team" content, and (c)
  the enterprise / data-sovereignty / compliance story. The homelabbers who
  start at the top of the funnel *become* the IT leaders who buy at the bottom.
  Publish real architecture write-ups (WASM plugins, QUIC transport,
  TimescaleDB partitioning) — the technical depth *is* the enterprise
  credibility.
- Effort: M (ongoing) — Impact: Medium
- Why it wins: You can't be in two segments with two brands; you can be *one*
  product that a user grows up with. The self-hosted homelab segment is
  underserved by enterprise RMM marketing and is the cheapest possible
  acquisition channel for the segment that matters.

---

## Sequencing (what I'd actually do in Phase 1 MVP)

The MVP should be the **minimum coherent wedge that proves the thesis** —
"self-hosted, fast, low-noise, and I can verify it" — not a 10-module platform.

1. **Weeks 1–2 (the wedge):** Single static Go agent + `curl|sh` bootstrap
   (S1), core metrics to TimescaleDB, Meilisearch-backed device search +
   **command palette** (S5), **dynamic baselining** to kill alert noise (S2).
   → *Demo-able "monitored in 5 min, near-zero false alerts."*
2. **Weeks 3–4 (trust):** mTLS zero-trust channel (S4), **signed releases +
   SBOM** (S4), full per-client **export** (the no-lock-in promise, S7).
   → *The sovereignty/trust story, provable.*
3. **Weeks 5–6 (retention):** **Self-healing playbooks** + **event chains over
   NATS** (S3), **Loki** log integration (S2), **webhook/event framework** (S6).
   → *The "it fixes things and wires into everything" value.*
4. **After MVP (the enterprise unlocks):** declarative desired-state +
   compliance scan (S3/S4), GitOps (S3), marketplace + WASM plugins (S3/S9),
   **migration tooling** (S10), **air-gap mode** (S8), partner program (S7).

**Cut from the MVP** (do not start): agent mesh (S1, risky), remote-control
full RDP passthrough (S7 — heavy, defer), multi-tenant cloud (S7 — after the
self-host is excellent), reverse ETL, OT/ICS.
