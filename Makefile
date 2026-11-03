# TaskTrooper Telemetry — local development.
#   make infra && make migrate && make build && make test
#   make run-writer   (terminal 1)     make run-gw   (terminal 2)

COMPOSE  := docker compose -f deploy/docker-compose.yml
CH       := $(COMPOSE) exec -T clickhouse clickhouse-client --user trooper --password trooper --multiquery
PSQL     := $(COMPOSE) exec -T -e PGOPTIONS=--client-min-messages=warning postgres psql -U trooper -d trooper -v ON_ERROR_STOP=1 -q
MODULES  := $(shell grep -E '^[[:space:]]*\./' services/go.work | sed 's|.*\./||')

.PHONY: infra infra-down migrate build test check run-gw run-writer e2e-ingest bench-ingest

infra: ## start ClickHouse, Postgres, Redpanda (+ topics), Redis
	$(COMPOSE) up -d --wait clickhouse postgres redpanda redis
	$(COMPOSE) up redpanda-init

infra-down: ## stop infra (volumes kept)
	$(COMPOSE) down

migrate: ## (re)apply schemas to running containers; every statement is idempotent
	@for f in deploy/clickhouse/*.sql; do echo "clickhouse < $$f"; $(CH) < $$f || exit 1; done
	@for f in deploy/postgres/*.sql; do echo "postgres < $$f"; $(PSQL) < $$f > /dev/null || exit 1; done

build: ## build every Go service module (go.work)
	@for m in $(MODULES); do [ -d services/$$m ] || continue; echo "build services/$$m"; (cd services/$$m && go build ./...) || exit 1; done

test: ## unit tests of every Go service module
	@for m in $(MODULES); do [ -d services/$$m ] || continue; echo "test services/$$m"; (cd services/$$m && go test ./...) || exit 1; done

check: ## one-shot health check — same gates as .github/workflows/ci.yml
	@node --version  >/dev/null 2>&1 || { echo "check: node not in PATH (need Node.js >= 22)"; exit 1; }
	@pnpm --version  >/dev/null 2>&1 || { echo "check: pnpm not in PATH, or not runnable (needs node)"; exit 1; }
	@go version      >/dev/null 2>&1 || { echo "check: go not in PATH, or not runnable (Go 1.26; mise: run 'mise use -g go@1.26.3')"; exit 1; }
	@printf 'package p\n' | gofmt >/dev/null 2>&1 || { echo "check: gofmt not in PATH, or not runnable (ships with Go)"; exit 1; }
	@bad=$$(cd services && find . -path '*/testdata' -prune -o -name '*.go' -print0 | xargs -0 gofmt -l) || exit 1; \
	if [ -n "$$bad" ]; then echo "services/ needs gofmt:"; echo "$$bad"; (cd services && gofmt -d $$bad); exit 1; fi
	@for m in $(MODULES); do [ -d services/$$m ] || continue; echo "vet services/$$m"; (cd services/$$m && go vet ./...) || exit 1; done
	@$(MAKE) test
	@pnpm install --frozen-lockfile
	@echo "js sdk: build"
	@pnpm -r --filter "./sdks/js/packages/**" build
	@echo "web: typecheck, build"
	@cd apps/web && pnpm exec tsc -b && pnpm exec vite build
	@echo "js sdk: vitest"
	@pnpm -r --filter "./sdks/js/packages/**" test
	@go run github.com/rhysd/actionlint/cmd/actionlint@latest .github/workflows/*.yml
	@echo "check OK"

run-gw: ## ingest-gw on :4318 (HTTP) / :4317 (gRPC)
	cd services/ingest && go run ./cmd/ingest-gw

run-writer: ## ingest-writer (consumer group tt-ingest-writer)
	cd services/ingest && go run ./cmd/ingest-writer

e2e-ingest: ## ingest end-to-end test against the running infra
	cd services/ingest && go test -tags e2e -count=1 -v ./e2e/

bench-ingest: ## ingest hot-path benchmarks
	cd services/ingest && go test -run '^$$' -bench . -benchmem ./internal/...

# ---------------------------------------------------------------- self-observability (ADR-027, contracts §27)
.PHONY: self self-down
SELF_OWNER ?= $(shell git config user.email)
SELF_RUN := deploy/self/.run
SELF_PID := $(SELF_RUN)/trooper-agent.pid

self: ## self-observability: `api bootstrap` the TaskTrooper project (SELF_OWNER=<email>), (re)start trooper-agent on the compose infra, print the env for services + web
	@test -n "$(SELF_OWNER)" || { echo "set SELF_OWNER=<email> (owner of the TaskTrooper self project)"; exit 1; }
	@mkdir -p $(SELF_RUN)/bin $(SELF_RUN)/logs
	@cd services/api && GOWORK=off go run ./cmd/api bootstrap --org TaskTrooper --project tasktrooper --owner '$(SELF_OWNER)' \
		--ingest-key-out ../../$(SELF_RUN)/ingest-key --json > ../../$(SELF_RUN)/bootstrap.json
	@cd services/collector && GOWORK=off go build -o ../../$(SELF_RUN)/bin/trooper-agent ./cmd/trooper-agent
	@$(MAKE) --no-print-directory self-down > /dev/null
	@set -e; \
	dsn=$$(sed -E 's/.*"dsn":"([^"]*)".*/\1/' $(SELF_RUN)/bootstrap.json); \
	project=$$(sed -E 's/.*"projectId":([0-9]+).*/\1/' $(SELF_RUN)/bootstrap.json); \
	case "$$dsn" in http*) ;; *) echo "api bootstrap returned no dsn:"; cat $(SELF_RUN)/bootstrap.json; exit 1;; esac; \
	TT_SELF_PG_PASSWORD=$${TT_SELF_PG_PASSWORD:-trooper} TT_SELF_CH_PASSWORD=$${TT_SELF_CH_PASSWORD:-trooper} TROOPER_DSN="$$dsn" nohup $(SELF_RUN)/bin/trooper-agent \
		--config deploy/self/agent.yaml --agent-modes db,host --dsn "$$dsn" \
		> $(SELF_RUN)/logs/trooper-agent.log 2>&1 & echo $$! > $(SELF_PID); \
	sleep 1; kill -0 $$(cat $(SELF_PID)) 2>/dev/null || { echo "trooper-agent exited:"; tail -20 $(SELF_RUN)/logs/trooper-agent.log; exit 1; }; \
	echo "trooper-agent running (pid $$(cat $(SELF_PID)), log $(SELF_RUN)/logs/trooper-agent.log) → project $$project"; \
	curl -fsS -o /dev/null --max-time 2 http://localhost:4318/healthz 2>/dev/null || echo "note: ingest-gw is not up on :4318 yet (make run-gw); the agent retries"; \
	echo ""; \
	echo "# platform Go services (ingest-gw, ingest-writer, api, …):"; \
	echo "export TROOPER_DSN=$$dsn"; \
	echo "export TT_SELF_PROJECT_ID=$$project"; \
	echo "# dashboard (shell before pnpm dev, or without export in apps/web/.env.local):"; \
	echo "export VITE_TROOPER_DSN=$$dsn"

self-down: ## stop the self-observability trooper-agent started by `make self`
	@if [ -f $(SELF_PID) ] && kill -0 $$(cat $(SELF_PID)) 2>/dev/null; then \
		pid=$$(cat $(SELF_PID)); kill $$pid; \
		for i in 1 2 3 4 5 6 7 8 9 10; do kill -0 $$pid 2>/dev/null || break; sleep 1; done; \
		kill -0 $$pid 2>/dev/null && kill -9 $$pid; echo "trooper-agent stopped (pid $$pid)"; \
	else echo "trooper-agent not running"; fi
	@rm -f $(SELF_PID)
