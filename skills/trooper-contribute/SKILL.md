---
name: trooper-contribute
description: Repo rules for changing the TaskTrooper Telemetry codebase (this monorepo: services/, apps/web, sdks/, deploy/, .ai/). Use before editing or adding code, schemas, SDK APIs, alert types or UI here, and before committing or opening a PR. Not for diagnosing production telemetry (use trooper-diagnose / trooper-query).
---

# trooper-contribute

`AGENTS.md` (= `CLAUDE.md`) at the repo root is the source of truth. This skill is a checklist that points
into it and `.ai/`; **if AGENTS.md says something different, follow AGENTS.md**. Re-read the relevant
section rather than relying on this summary.

## Before writing code

1. **Docs first** (AGENTS.md rule 1). New service, table, SDK API, alert type or tool → update the matching
   `.ai/*.md` first (doc map in AGENTS.md § Doküman haritası), in the same PR. Typical pairs:
   - schema / table / retention → `.ai/data-model.md` + `deploy/clickhouse|postgres/*.sql`
   - API endpoint → `services/api/openapi.yaml` + `.ai/contracts.md` § 8
   - port, Kafka topic, `trooper.*` attribute, env var → `.ai/contracts.md`
   - SDK API → `.ai/sdk.md`; alert/monitor kind → `.ai/alerting.md`; MCP tool → `.ai/mcp.md` + contracts § 26
   - architectural decision → new ADR in `.ai/decisions.md`
2. **Ownership** (`.ai/contracts.md` § 1): each module has one owner; do not edit another module's
   `go.mod`. Keep `services/pkg` additions small and note them in contracts § 1.
3. Docs in `.ai/` are Turkish (technical terms English); code, identifiers and commits are English (rule 10).

## Invariants to keep

- **Multi-tenancy** (rule 7, `.ai/architecture.md` § Multi-tenancy): every query filters by `org_id` +
  `project_id`. No query without a tenant filter in the API/query layer.
- **Cardinality** (rule 4, `.ai/data-model.md` § Kardinalite kuralları): no user id, request id or raw URL
  in metric labels; routes are templates (`/users/:id`).
- **PII** (rule 6, `.ai/sdk.md` § İlkeler): SDKs do not collect headers/body/query params by default;
  masking happens in the SDK.
- **SDKs never break the host** (rule 5, `.ai/sdk.md` § İlkeler): swallow all SDK errors into internal
  metrics; network calls async + batched + backoff.
- **Web UI** (rules 2–3, `.ai/design-system.md`): no raw hex, px spacing or inline fonts — use tokens from
  `apps/web/src/styles/tokens.css`; new components go in `apps/web/src/components/ui/` and get listed in
  design-system.md. Charts: single y-axis, categorical colors `--series-1..8` in order, `--status-*` only for
  status and always with icon + label (design-system.md § Data viz kuralları).
- **LLM model ids** come from config, never hard-coded (AGENTS.md § Stack).
- **Tests** (rule 8): Go table-driven; ingest hot path needs benchmarks (`make bench-ingest`).

## Make targets are the contract (ADR-026)

CI (`.github/workflows/ci.yml`) runs the same gates as `make check`; the Go module list comes from
`services/go.work` (adding a module there extends CI automatically).

| Target | Use |
|---|---|
| `make check` | gofmt, `go vet`, all Go unit tests, web typecheck + build, JS SDK build + vitest, actionlint |
| `make build` / `make test` | Go modules only |
| `make infra` / `make migrate` | local ClickHouse, Postgres, Redpanda, Redis + schemas |
| `make demo` | live demo data (`tools/demo/README.md`) |
| `make e2e-ingest` | local-only ingest e2e (not in CI) |

## Finishing

1. Run `make check` from the repo root and wait for the final line `check OK` (rule 11). Needs Node ≥ 22,
   pnpm, Go 1.26 on PATH. If it fails, find and fix the cause, then rerun; do not skip or remove a gate.
2. Commit with Conventional Commits: `feat(web): …`, `fix(ingest): …`, `docs(mcp): …` (rule 9).
3. If a code change contradicts a `.ai/` doc, update the doc in the same PR.
