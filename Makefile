# TaskTrooper Telemetry — local development.
#   make infra && make migrate && make build && make test
#   make run-writer   (terminal 1)     make run-gw   (terminal 2)

COMPOSE  := docker compose -f deploy/docker-compose.yml
CH       := $(COMPOSE) exec -T clickhouse clickhouse-client --user trooper --password trooper --multiquery
PSQL     := $(COMPOSE) exec -T -e PGOPTIONS=--client-min-messages=warning postgres psql -U trooper -d trooper -v ON_ERROR_STOP=1 -q
MODULES  := pkg ingest api alerting collector integrations symbolicator

.PHONY: infra infra-down migrate build test run-gw run-writer e2e-ingest bench-ingest

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

run-gw: ## ingest-gw on :4318 (HTTP) / :4317 (gRPC)
	cd services/ingest && go run ./cmd/ingest-gw

run-writer: ## ingest-writer (consumer group tt-ingest-writer)
	cd services/ingest && go run ./cmd/ingest-writer

e2e-ingest: ## ingest end-to-end test against the running infra
	cd services/ingest && go test -tags e2e -count=1 -v ./e2e/

bench-ingest: ## ingest hot-path benchmarks
	cd services/ingest && go test -run '^$$' -bench . -benchmem ./internal/...

.PHONY: demo demo-seed
DEMO_ARGS ?=

demo: ## live demo world (tools/demo): platform services if not running + seed + run until Ctrl-C (DEMO_ARGS="--rps 40 --keep")
	cd tools/demo && GOWORK=off go build -o .run/demo ./cmd/demo && cd ../.. && tools/demo/.run/demo up $(DEMO_ARGS)

demo-seed: ## ttdemo containers + schemas + topics + api metadata (idempotent; needs the api on :8080)
	cd tools/demo && GOWORK=off go build -o .run/demo ./cmd/demo && cd ../.. && tools/demo/.run/demo seed $(DEMO_ARGS)
