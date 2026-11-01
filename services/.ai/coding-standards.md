# Coding standards: Go services (`services/`)

Covers every module in `services/go.work`: `pkg`, `ingest`, `api`, `alerting`, `collector`, `integrations`,
`symbolicator`, `codeintel`, `loadtest`, `mcp`. `tools/trooper-cli` is a small stdlib-only Go CLI (its own module,
not in `go.work`) and follows these same rules; it has no separate standards file.

Repo-wide rules (docs first, multi-tenancy, cardinality, PII, commits, `make check`): [`../../.ai/coding-standards.md`](../../.ai/coding-standards.md).
Testing: [`testing.md`](testing.md). Cross-service contracts (ports, topics, headers, `trooper.*` attributes): [`../../.ai/contracts.md`](../../.ai/contracts.md).

## Toolchain and gates

- Go version comes from `services/go.work` (`go 1.26.3`); each module's `go.mod` uses the same `go` line.
- Formatting is `gofmt` only (no goimports/golangci config in the repo). `testdata/` is excluded from the gofmt gate.
- Static checks: `go vet ./...` per module. Both run in CI (`go / <module>` matrix in `.github/workflows/ci.yml`) and in `make check`.
- Adding a module: add it to `services/go.work`. CI and the Makefile derive the module list from there; nothing else to update.
- Every module must also build alone: Dockerfiles and integration tests run with `GOWORK=off`. Cross-module code goes
  through `services/pkg` with `replace github.com/tasktrooper/telemetry/services/pkg => ../pkg` in `go.mod`.

```bash
make build            # go build ./... in every module
make test             # go test ./... in every module
make check            # gofmt + vet + tests + web + JS SDKs + actionlint → must print "check OK"
```

## Layout

- `cmd/<binary>/main.go` per binary (`api/cmd/api`, `ingest/cmd/ingest-gw`, `alerting/cmd/alert-engine`, ...).
  Everything else lives under `internal/<pkg>`. Shared code across modules lives only in `services/pkg/*`.
- `main` stays thin: load config, build logger, wire dependencies, then `run(cfg, log) error`; `main` logs and
  `os.Exit(1)` on error. Reference: `api/cmd/api/main.go`.
- Shutdown via `signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)`; resources closed with `defer`.
- Package names: short, lowercase, one word (`tq`, `norm`, `flatten`, `burn`, `leader`). No `util`/`common` packages.
- Test helper packages are named `<pkg>test` (`ingest/internal/selfobs/selfobstest`).
- Long files may be split with a section rule: `// ---------------------------------------------------------------- helpers`.

## Configuration

- All config is env vars prefixed `TT_`, read with `services/pkg/config` (`config.String/Int/Bool/Duration/List`) with a
  dev default that matches `deploy/docker-compose.yml`. Reference: `pkg/config/config.go`.
- Shared settings come from `config.Load()` (`TT_POSTGRES_URL`, `TT_CLICKHOUSE_URL`, `TT_KAFKA_BROKERS`, `TT_REDIS_URL`, `TT_LOG_LEVEL`).
- Production-only guards key off `config.Production()` (`TT_ENV=production`) and **fail closed**: see `pkg/secrets/secrets.go`
  (dev key allowed locally, rejected in production).
- Invalid config returns an error from `run`, it does not panic: `return fmt.Errorf("AI configuration: %w", err)`.
- Document every new `TT_*` variable in the service's `README.md`. LLM model IDs come from config, never hard-coded.
- `TROOPER_*` variables are reserved for the platform's own SDK (self-observability, ADR-027).

## Logging

- Use the logger from `logx.New(service, level)` (JSON `slog`, `service` attribute). Pass `*slog.Logger` explicitly
  (struct field `Log` or constructor arg); do not use `log.Printf`/`fmt.Println` outside `cmd/`.
- Message is a short lowercase phrase, attributes are key/value pairs in `snake_case`; the error key is always `"err"`.

```go
s.Log.Error("request failed", "path", r.URL.Path, "err", err, "request_id", middleware.GetReqID(r.Context()))
```

- Never log secrets, API keys, raw request bodies or PII. Log route templates, not raw URLs.

## Errors

- Wrap with context using `%w`; the prefix is a short lowercase noun phrase naming what failed, no "failed to":
  `fmt.Errorf("postgres: %w", err)`, `fmt.Errorf("create bucket %s: %w", name, err)`.
- Inspect with `errors.Is` / `errors.As`; never compare error strings. No `github.com/pkg/errors`.
- Sentinels are package-level `var ErrX = errors.New("pkg: ...")` (`tenant.ErrNoTenant`, `tq.ErrNoTenantFilter`).
- Domain errors that map to HTTP statuses are typed in the store layer: `store.ErrNotFound`, `store.ErrInvalid`
  (`store.Invalid(msg)` → 400), `store.ErrConflict`, `store.ConflictMsg`. Reference: `api/internal/store/store.go`.
- Handlers do not pick statuses ad hoc: they call `s.fail` / `s.reply` / `s.created` / `s.noContent`
  (`api/internal/server/server.go`). Unknown errors become `500 {"error":"internal error"}` and are logged; the
  client never sees internal error text. `context.Canceled` → 499, not logged.
- No `panic` in request or consumer paths (only `regexp.MustCompile` at package init and impossible-state guards).
- Tolerate a not-yet-applied migration where the feature is optional: catch Postgres `42P01` (undefined_table) and
  return empty (`alerting/internal/store/serverless.go`).

## HTTP

- `api` (and parts of `codeintel`) use chi; the other services use stdlib `http.ServeMux`. Do not add a third router.
- JSON responses through `writeJSON(w, status, v)`; it marshals before writing headers so a NaN never yields a 200
  with an empty body. Error body shape: `{"error": "<message>"}`.
- Floats from ClickHouse go through `fin()` (NaN/Inf → 0) before JSON encoding.
- Every `api` route must be in `services/api/openapi.yaml`; `TestOpenAPICoversRoutes` enforces it.
- Shared request parsing goes in small wrappers (`withQ`, `withEndpoint` in `api/internal/server/routes.go`) rather
  than being repeated per handler.
- CORS echoes the exact allowed origin, never `*` with credentials (`OriginAllowed`).
- Outgoing HTTP clients are package variables or struct fields (`cloudcheck.HTTPClient`, `sym.HTTP`) so tests and
  self-telemetry can swap them. Always set timeouts.

## Context and concurrency

- `ctx context.Context` is the first parameter of anything that does I/O. Use `r.Context()` in handlers.
- `context.Background()` only in `main`, long-lived workers/leaders and self-telemetry setup, never inside a request path.
- Guard shared state with `sync.Mutex` next to the fields it protects; prefer channels only for pipelines (ingest writer batching).
- Every goroutine has an owner that stops it (ctx cancel or close) on shutdown.

## Data access and multi-tenancy

- `api`: all ClickHouse/Postgres statements go through `api/internal/tq`. Write `{{tenant}}` (or `{{tenant:alias}}`,
  `{{project}}`, `{{org}}`) in the SQL; `tq` substitutes the tenant from ctx and **rejects** statements without a marker.
  Never build tenant filters by hand in `api`.

```go
d.pg.Query(ctx, `SELECT name, type, coalesce(language, '') FROM services WHERE {{tenant}}`)
```

- Tenant identity travels in context via `services/pkg/tenant` (`tenant.With`, `tenant.Must`) and between services
  in Kafka headers (`Tenant.Headers` / `tenant.Parse`).
- Other services (alerting, integrations, codeintel, loadtest) use pgx directly; every query still takes and filters by
  `project_id` (and `org_id` in ClickHouse) as parameters: `WHERE project_id = $1 AND ...`. Cross-tenant scans exist
  only in workers that iterate all projects and must select `org_id, project_id` per row.
- Always pass values as bind parameters (`?` for ClickHouse, `$n` for Postgres). String formatting into SQL is only
  allowed for identifiers/fragments chosen from a fixed set in code (table names like `redTable(w)`), never user input.
- Close `rows` with `defer rows.Close()` and return `rows.Err()`.
- Cardinality: metric labels and rollup keys use route templates (`ingest/internal/norm`). See [`../../.ai/data-model.md`](../../.ai/data-model.md).

## SQL migrations (`deploy/clickhouse`, `deploy/postgres`)

- Numbered files `NNN_topic.sql`, applied in order by `make migrate`; every statement is idempotent
  (`CREATE TABLE IF NOT EXISTS`, `ADD COLUMN IF NOT EXISTS`, `CREATE OR REPLACE FUNCTION`). Changes are additive.
- First line is a comment naming the owner/purpose and the ADR or doc section.
- Every telemetry table has `org_id` + `project_id`; Postgres tables are scoped by `project_id` (orgs via `projects`).
- Update [`../../.ai/data-model.md`](../../.ai/data-model.md) in the same PR.

## Naming and comments

- Exported identifiers and every package have a doc comment (`// Package tq is ...`). Comments explain *why* and
  link the governing doc: `// Self telemetry (ADR-027): a no-op unless TROOPER_DSN is set.`, `contracts §25`.
- Initialisms in caps (`OrgID`, `ProjectID`, `URL`, `HTTPClient`). Receivers are 1–2 letters (`s *Server`, `d *DB`, `st *Store`).
- Interfaces are defined by the consumer and named for what they provide (`server.Store` is everything handlers need,
  implemented by `*store.DB`) so handlers can be tested with a fake.
- Build-time version: `var version = ""` set by `-ldflags "-X main.version=..."`.

## Do / Don't

| Do | Don't |
|---|---|
| `config.Duration("TT_SESSION_TTL", 7*24*time.Hour)` | `os.Getenv` scattered through `internal/` |
| `fmt.Errorf("clickhouse dsn: %w", err)` | `fmt.Errorf("failed to parse dsn: %v", err)` |
| `s.fail(w, r, err)` | `http.Error(w, err.Error(), 500)` in `api` handlers |
| `WHERE {{tenant}} AND minute >= toDateTime(?)` | `WHERE project_id = ` + `fmt.Sprint(id)` |
| put shared code in `services/pkg` | import another service's `internal/` (Go forbids it; don't copy-paste around it either without reason) |
