SHELL := /bin/bash
COMPOSE ?= docker compose
# Go + buf + node live in these dirs on the dev box; put them on PATH once.
PATH := /root/go/bin:/root/.nvm/versions/node/v24.19.0/bin:$(PATH)
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

.PHONY: frontend
frontend: ## Run the frontend dev server (http://localhost:5173)
	cd frontend && npm run dev
