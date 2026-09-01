SHELL := /bin/bash
COMPOSE ?= docker compose
# Go + buf + node live in these dirs on the dev box; put them on PATH once.
PATH := /root/go/bin:/usr/local/go/bin:/root/.nvm/versions/node/v24.19.0/bin:$(PATH)
export PATH

.PHONY: help
help: ## Show available targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

## ---- Local dev stack -----------------------------------------------------

.PHONY: pull
pull: ## Pull all service images
	$(COMPOSE) pull

.PHONY: up
up: ## Start the backing services (Timescale, NATS, Redis, MinIO, Meilisearch)
	$(COMPOSE) up -d

.PHONY: down
down: ## Stop and remove the backing services (data volumes are kept)
	$(COMPOSE) down

.PHONY: clean
clean: ## Stop the stack AND remove data volumes (destructive)
	$(COMPOSE) down -v

.PHONY: logs
logs: ## Tail logs from the stack
	$(COMPOSE) logs -f --tail=100

.PHONY: health
health: ## Block until every service reports healthy
	@scripts/wait-for-health.sh

.PHONY: dev
dev: ## make dev — boot the full local stack and wait for health (idempotent)
	@echo "==> W0-1 local dev stack"
	$(MAKE) pull
	$(MAKE) up
	$(MAKE) health

## ---- Production (A-1) ---------------------------------------------------
# Default stack includes the bundled Caddy TLS edge (the "edge" profile).
COMPOSE_PROD = $(COMPOSE) --env-file .env.prod --profile edge -f docker-compose.prod.yml
# BYO-reverse-proxy stack: bundled Caddy stays off (no "edge" profile) and the
# operator API + SPA are published on the host for your own proxy to front.
COMPOSE_PROD_BYO = $(COMPOSE) --env-file .env.prod -f docker-compose.prod.yml -f docker-compose.byoproxy.yml

.PHONY: prod
prod: ## Build + boot the hardened production stack (Caddy TLS edge + mTLS agent port). Needs .env.prod
	@test -f .env.prod || { echo "no .env.prod — first: cp .env.prod.example .env.prod and set the secrets"; exit 1; }
	$(COMPOSE_PROD) up -d --build
	@echo "==> up. see .env.prod for RMMWAY_DOMAIN + RMMWAY_AGENT_MTLS_PORT"
	@echo "==> operator UI/API: https://<RMMWAY_DOMAIN>/  (health: .../healthz)"
	@echo "==> agent mTLS gRPC:   <host>:<RMMWAY_AGENT_MTLS_PORT>  (default 50052)"

.PHONY: prod-down
prod-down: ## Stop the production stack (data volumes are kept)
	$(COMPOSE_PROD) down

.PHONY: prod-clean
prod-clean: ## Stop the production stack AND remove its data volumes (destructive)
	$(COMPOSE_PROD) down -v

.PHONY: prod-logs
prod-logs: ## Tail production stack logs
	$(COMPOSE_PROD) logs -f --tail=100

## ---- Production, bring-your-own reverse proxy (A-1) ----------------------
.PHONY: prod-byoproxy
prod-byoproxy: ## Boot the stack WITHOUT the bundled Caddy, publishing the API (RMMWAY_HTTP_PORT, def 8080) + SPA (RMMWAY_FRONTEND_PORT, def 8081) for your own proxy
	@test -f .env.prod || { echo "no .env.prod — first: cp .env.prod.example .env.prod and set the secrets"; exit 1; }
	$(COMPOSE_PROD_BYO) up -d --build
	@echo "==> up WITHOUT the bundled Caddy (edge profile off)."
	@echo "==> operator API (HTTP): http://<host>:8080 (RMMWAY_HTTP_PORT)   <- your proxy forwards /api,/agent,/healthz here"
	@echo "==> operator SPA (HTTP): http://<host>:8081 (RMMWAY_FRONTEND_PORT)   <- your proxy forwards the rest here"
	@echo "==> agent mTLS gRPC:   <host>:<RMMWAY_AGENT_MTLS_PORT>  (default 50052, unchanged)"

.PHONY: prod-byoproxy-down
prod-byoproxy-down: ## Stop the BYO-proxy production stack (data volumes are kept)
	$(COMPOSE_PROD_BYO) down

.PHONY: prod-byoproxy-clean
prod-byoproxy-clean: ## Stop the BYO-proxy production stack AND remove its data volumes (destructive)
	$(COMPOSE_PROD_BYO) down -v

.PHONY: prod-byoproxy-logs
prod-byoproxy-logs: ## Tail BYO-proxy production stack logs
	$(COMPOSE_PROD_BYO) logs -f --tail=100

## ---- Go ------------------------------------------------------------------

.PHONY: build
build: ## Build the Go server and agent
	cd server && go build ./...
	cd agent && go build ./...

.PHONY: test
test: ## Run Go tests
	cd server && go test ./...
	cd agent && go test ./...

.PHONY: run-server
run-server: ## Run the backend server locally (needs the stack up)
	cd server && go run ./cmd/server

.PHONY: migrate
migrate: ## Apply pending SQL migrations from server/migrations
	cd server && go run ./cmd/server --migrate-only

.PHONY: check
check: ## W0-1 DoD: build everything + verify /healthz reports all services ok
	$(MAKE) build
	@scripts/wait-for-health.sh
	@echo "==> /healthz"
	@curl -fsS http://localhost:8080/healthz || \
		{ echo "server not running — try: make run-server (background)"; exit 1; }

.PHONY: agent
agent: ## Cross-compile static agent binaries (linux/darwin/windows × amd64/arm64)
	@scripts/build-agent.sh $(VERSION)

.PHONY: verify-agent
verify-agent: ## W1-1 DoD: confirm the built binaries are static (no shared libs)
	@echo "== file (linux/amd64)"
	@file agent/dist/rmmway-agent-linux-amd64
	@echo "== ldd (should report not a dynamic executable)"
	@ldd agent/dist/rmmway-agent-linux-amd64 2>&1 | grep -Ei 'not a dynamic|statically linked' || \
		{ echo "FAIL: linux binary is dynamically linked"; exit 1; }
	@echo "== version (runs on this host)"
	@./agent/dist/rmmway-agent-linux-amd64 --version

.PHONY: sign
sign: ## W3-4: sign all release artifacts (minisign) + verify. Needs MINISIGN_PASS (or keys/minisign.key on disk)
	@scripts/sign.sh $(VERSION)

.PHONY: verify-sigs
verify-sigs: ## W3-4: verify all signed artifacts against keys/minisign.pub
	@scripts/verify-sigs.sh

.PHONY: signer-test
signer-test: ## Run the tools/signer unit tests (keygen/sign/verify + CLI-fixture interop)
	go -C tools/signer test ./...

## ---- Signed agent auto-update (W4-2) ---------------------------------------

.PHONY: update-e2e
update-e2e: ## W4-2 DoD: a validly signed release is applied; a tampered/unsigned build is refused (in-process server + real agent binary + real signer)
	@cd server && go run ./cmd/e2e/update

.PHONY: logs-e2e
logs-e2e: ## W6-1 DoD: real agent's log lines are queryable in Loki AND surfaced per-device in the RMM (needs the stack up: make dev)
	@cd server && go run ./cmd/e2e/logs

.PHONY: release-dir
release-dir: ## W4-2: assemble a releases dir (RMMWAY_RELEASES_DIR) from agent/dist. Needs: make agent && make sign. $(DIR) [default releases-local]
	@cd server && go run ./cmd/publish-release -dir $(or $(DIR),releases-local)

.PHONY: pin-release-key
pin-release-key: ## W4-2: re-pin the release public key into the agent (run after a minisign key rotation), then rebuild
	@cp keys/minisign.pub agent/internal/update/minisign.pub
	@echo "re-pinned agent/internal/update/minisign.pub — commit it and rebuild the agent"

## ---- Per-client full export (W4-3) -------------------------------------------

.PHONY: export-e2e
export-e2e: ## W4-3 DoD: one-click client export -> self-describing Parquet+JSON bundle (verify + tamper + window + re-read + re-import). Needs a Timescale PG where the user can CREATE DATABASE
	@cd server && go run ./cmd/e2e/export

## ---- Provable trust milestone (W4-4) -------------------------------------------

.PHONY: trust-e2e
trust-e2e: ## W4-4 DoD (closes Block 2): a skeptic (a) verifies a signed release + reads the SBOM, (b) exports a client and confirms the data is theirs. Self-contained; Part B needs a Timescale PG (CREATEDB user)
	@cd server && go run ./cmd/e2e/trust

## ---- Webhook + event-stream framework (W6-2) --------------------------------

.PHONY: webhook-e2e
webhook-e2e: ## W6-2 DoD: a user-defined endpoint receives signed (HMAC) alert/inventory/automation events, with retries + replay, and the bus is exposed as SSE. Needs Timescale PG (CREATEDB user) + NATS (JetStream)
	@cd server && go run ./cmd/e2e/webhook

## ---- Full automation E2E (W6-3 MILESTONE) ---------------------------------

.PHONY: automation-e2e
automation-e2e: ## W6-3 DoD (closes Block 3 = Phase 1 MVP): ONE condition (disk 95%) drives alert -> self-heal confirm -> ticket -> webhook, all audited, on a real NATS bus. Needs Timescale PG (CREATEDB user) + NATS (JetStream)
	@cd server && go run ./cmd/e2e/automation

## ---- First-boot setup wizard (A-2) ------------------------------------------

.PHONY: setup-e2e
setup-e2e: ## A-2 DoD: a fresh database triggers the first-boot wizard (mint root admin, define the org CA, configure + verify the SMTP outbox) and every subsequent boot bypasses it. Needs Timescale PG (CREATEDB user)
	@cd server && go run ./cmd/e2e/setup

## ---- Simplified "Add a device" (agent install) --------------------------------

.PHONY: adddevice-e2e
adddevice-e2e: ## Add-device DoD: the operator mints a token (auth-gated UI action), a fresh agent enrolls over the operator's HTTPS origin (leaf signed by the pinned org root, token one-time), and the REAL agent — with the plain gRPC bootstrap port DEAD — still enrolls (via HTTP) and comes online over the mTLS port. Self-contained (no backing services)
	@cd server && go run ./cmd/e2e/adddevice

## ---- SBOM (W4-1) -----------------------------------------------------------

.PHONY: image
image: ## Build the server Docker image locally (rmmway-server:local)
	docker build -t rmmway-server:local -f server/Dockerfile .

.PHONY: sbom
sbom: ## W4-1: generate CycloneDX SBOMs (5 agent binaries + server image)
	@scripts/sbom.sh

.PHONY: sbom-agent
sbom-agent: ## W4-1: agent binary SBOMs only (no docker needed)
	@scripts/sbom.sh --skip-image

## ---- Protos ----------------------------------------------------------------

.PHONY: proto
proto: ## Regenerate gRPC stubs from proto/ into proto/gen/
	buf generate
	cd proto/gen && go mod tidy

.PHONY: proto-lint
proto-lint: ## Lint the protos (add --against <ref> for breaking checks vs a baseline)
	buf lint

## ---- Frontend ------------------------------------------------------------

.PHONY: frontend-deps
frontend-deps: ## Install frontend npm dependencies
	cd frontend && npm install

.PHONY: setup-ui-smoke
setup-ui-smoke: ## A-2 UI DoD: the real <App/> redirects a fresh database to the setup wizard, completes + auto-signs-in, and skips the wizard afterwards (jsdom, no browser needed). Needs: make frontend-deps
	@cd frontend && bash scripts/setup-wizard.smoke.sh

.PHONY: adddevice-ui-smoke
adddevice-ui-smoke: ## Add-device UI DoD: the real <App/> — sign in -> Devices -> "Add a device" mints a one-time token (POST /api/bootstrap) and renders copy-paste install commands with the origin + token + device id (jsdom, no browser needed). Needs: make frontend-deps
	@cd frontend && bash scripts/adddevice.smoke.sh

.PHONY: sse-ui-smoke
sse-ui-smoke: ## B-1 UI DoD: the real <App/> in TWO operator sessions — a device going offline flips both sessions' status badge and a new alert bumps both sessions' nav badge + open inbox, instantly off the live SSE stream (jsdom, no browser needed). Needs: make frontend-deps
	@cd frontend && bash scripts/sse.smoke.sh

.PHONY: groups-e2e
groups-e2e: ## B-2 DoD (server half): the operator tags a cohort, filters to the tag group, and ONE command fans out to every matched agent — per-device capability tokens, offline devices reported, capability-gated (in-process server + real mTLS identity). Self-contained
	@cd server && go run ./cmd/e2e/groups

.PHONY: groups-ui-smoke
groups-ui-smoke: ## B-2 UI DoD: the real <App/> — tag a cohort through the per-device tag editor, filter to the exact group with `tag:web`, then ONE bulk command fans out to the whole group (jsdom, no browser needed). Needs: make frontend-deps
	@cd frontend && bash scripts/groups.smoke.sh

.PHONY: metrics-ui-smoke
metrics-ui-smoke: ## Per-device metrics viewer UI DoD: the real <App/> — the device detail shows the Metrics panel (series picker + bucketed SVG chart), the range selector re-requests with the new range, per-source series send name+source (jsdom, no browser needed). Needs: make frontend-deps
	@cd frontend && bash scripts/metrics.smoke.sh

.PHONY: commands-ui-smoke
commands-ui-smoke: ## D-1 UI DoD: the real <App/> — the device detail shows the Commands panel (full dispatch history, newest first, PENDING/RUNNING/SUCCEEDED statuses, expandable agent output); a command-category SSE event re-fetches the list live and the manual refresh is the fallback (jsdom, no browser needed). Needs: make frontend-deps
	@cd frontend && bash scripts/commands.smoke.sh

.PHONY: frontend
frontend: ## Run the frontend dev server (http://localhost:5173)
	cd frontend && npm run dev
