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
