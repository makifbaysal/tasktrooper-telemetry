# AGENTS.md — TaskTrooper Telemetry

This file is the entry point for every AI agent (Claude Code, Cursor, Codex, etc.) and developer working on the project.
Read the relevant `.ai/` doc before writing code. If you make a decision that contradicts a doc, update the doc in the same PR.

## Project summary

TaskTrooper Telemetry (part of the TaskTrooper product family; repo `Task Trooper/tasktrooper-telemetry`) is an observability platform that collects data through **SDKs** added to backend services, workers/consumers, and web and mobile apps,
visualizes that data, lets you set up **alerts** on it, and can run **load tests**.

Distinguishing feature: the platform **reads the customer's codebase** (repo connection), extracts the endpoint/consumer/page inventory,
**automatically suggests/defines alerts** based on that inventory, and automatically generates load test scenarios.

Four core capabilities:
1. **APM (backend):** per-endpoint throughput, error rate, latency (p50/p95/p99), traces, dependency map; queue consumer metrics (lag, processing time, retry/DLQ).
2. **RUM (frontend/mobile):** page/screen views, who visited (anonymous/identified user), session duration, load times (Web Vitals, TTFB, app start), JS/crash errors, network calls.
3. **Logs & Alerting:** log ingestion + search; metric, log pattern and anomaly based alerts; code-aware automatic alert suggestions.
4. **Load testing:** define/run scenarios from within the platform, correlate results with the same telemetry.

## Doc map (`.ai/`)

| File | When to read |
|---|---|
| [`.ai/README.md`](.ai/README.md) | Doc index, glossary |
| [`.ai/product.md`](.ai/product.md) | Personas, use cases, screen list, scope |
| [`.ai/architecture.md`](.ai/architecture.md) | System architecture, services, data flow, monorepo layout |
| [`.ai/sdk.md`](.ai/sdk.md) | SDK design, API surface, wire protocol (OTLP) |
| [`.ai/data-model.md`](.ai/data-model.md) | ClickHouse/Postgres schemas, retention, cardinality rules |
| [`.ai/alerting.md`](.ai/alerting.md) | Alert engine, rule types, automatic alerts from code analysis |
| [`.ai/code-intelligence.md`](.ai/code-intelligence.md) | Repo reading, inventory extraction, LLM usage |
| [`.ai/mcp.md`](.ai/mcp.md) | MCP server (`services/mcp`), tool surface, config |
| [`.ai/assistant.md`](.ai/assistant.md) | Dashboard AI chat (`/assistant`): agent loop, MCP tools in process, approvals, widgets, SSE API |
| [`skills/`](skills/) | Agent Skills (`trooper-diagnose`, `trooper-query`, `trooper-alerts`, `trooper-contribute`); discovery + install: [`.ai/mcp.md`](.ai/mcp.md) § Agent Skills |
| [`.ai/load-testing.md`](.ai/load-testing.md) | Load test infrastructure, runners, scenario format |
| [`.ai/design-system.md`](.ai/design-system.md) | UI tokens, components, chart rules |
| [`.ai/contracts.md`](.ai/contracts.md) | Contract between services/SDKs: ports, auth, Kafka topics, `trooper.*` attributes, collector metrics |
| [`.ai/decisions.md`](.ai/decisions.md) | Architecture decision records (ADR) |
| [`.ai/coding-standards.md`](.ai/coding-standards.md) | Repo-wide coding rules + links to each app's `<app>/.ai/coding-standards.md` (Go services, web, every SDK) |
| [`.ai/testing.md`](.ai/testing.md) | Repo-wide testing rules + links to each app's `<app>/.ai/testing.md` |
| [`.ai/roadmap.md`](.ai/roadmap.md) | Phases and next work |

## Monorepo layout

```
apps/web/               Dashboard (React 19 + Vite + TypeScript)
services/               Go services: pkg, ingest (gw + writer), api, alerting (engine + notifier), collector, integrations (trooper-cloud), symbolicator, codeintel, loadtest (ctl + runner)
sdks/                   go, jvm, dotnet, js/packages (browser, react, vue, angular, react-native, node), android, ios, flutter
tools/                  trooper-cli (sourcemap/dSYM/mapping upload)
proto/                  RUM wire schema
deploy/                 docker-compose + ClickHouse/Postgres schemas
skills/                 Agent Skills (for coding agents; .claude/skills symlink)
.ai/                    Architecture and product docs (contract between services: .ai/contracts.md)
```

Details: [`.ai/architecture.md`](.ai/architecture.md#monorepo).

## Stack

- **Frontend:** React 19, Vite, TypeScript (strict), react-router, plain CSS + design tokens (CSS custom properties). Charts: our own SVG chart components (`apps/web/src/components/charts`).
- **Backend:** Go 1.26. `net/http` + chi for HTTP, `go.opentelemetry.io/collector` components for gRPC/OTLP.
- **Data:** ClickHouse (telemetry: metrics, spans, logs, RUM events), PostgreSQL (metadata: org, project, alert, user), Redpanda (Kafka API, ingest buffer), Redis (rate limit, cache).
- **Load test engine:** our own Go engine (ADR-015), runners as local process or Kubernetes Job.
- **LLM:** Claude API (code analysis and alert suggestions). Model IDs are not hard-coded; they come from config.
- **Package manager:** pnpm (JS), Go modules.

## Commands

```bash
# Web dashboard
cd apps/web
pnpm install
pnpm dev          # http://localhost:5173
pnpm build        # typecheck + production build
pnpm typecheck
```

Backend: `docker compose -f deploy/docker-compose.yml up -d` (infrastructure), `make migrate`, `make build`, `make test`. No seed data
(ADR-028): on first login, the org/project/key is created through dashboard onboarding or `api bootstrap`; `make self` for the platform's own data. Details: `services/*/README.md`, `sdks/*/README.md`.
Demo deployment (single Mac, self-hosted runner, Cloudflare Tunnel): Actions → `deploy-demo` → Run workflow; setup in `deploy/demo/README.md` (ADR-031).

Full check (exactly the same gates as CI): `make check` → gofmt, `go vet`, unit tests for all Go modules, web typecheck + build, JS SDK build + vitest, workflow lint.

## Working rules

1. **Docs first.** New service, table, SDK API or alert type → update the relevant `.ai/*.md` first, then the code.
2. **Stay within the design system.** The theme follows TaskTrooper (see design-system.md § Theme source). No raw hex, raw px spacing or inline fonts in the UI; use the tokens in `apps/web/src/styles/tokens.css`. New component → under `apps/web/src/components/ui/`, add it to design-system.md.
3. **Chart rules:** single y-axis (dual axis forbidden), categorical colors in a fixed order (`--series-1..8`), status colors (`--status-*`) only for status and always with icon + label. Details: design-system.md § Data viz.
4. **Cardinality discipline.** Do not put user id, request id or raw URLs in metric labels. Endpoints are normalized as route templates (`/users/:id`). See data-model.md.
5. **SDKs never crash the host app.** All SDK errors are swallowed and written to an internal metric; network calls are async + batch + backoff.
6. **PII:** By default the SDK does not collect headers/body/query params; masking rules are applied on the SDK side.
7. **Multi-tenancy:** Every query is filtered by `org_id` + `project_id`. No query without a tenant filter is written in the query service.
8. **Tests:** Go → table-driven tests; web → vitest for component logic (to be added). Benchmarks are mandatory for the ingest path.
9. **Commit:** Conventional Commits (`feat(web): ...`, `fix(ingest): ...`).
10. **Language:** Code, identifiers, commits and `.ai/` docs are all English.
11. **`make check` at the end of the session.** Before a change is committed / a PR is opened, run `make check` at the repo root and always wait for it to finish with `check OK`. If the app's core flow (Go services compile + tests pass, web builds, JS SDKs build + test, workflow lint clean) is broken, don't settle for just checking: find and fix the cause, then run `make check` again. Do not hand off work until it is green; no temporarily skipping/excluding anything.

## Current status

- [x] Architecture docs (`.ai/`)
- [x] Design system tokens + base components (`apps/web`)
- [x] First screens: Overview, Services (including web/mobile apps), Service/Endpoint detail (KPIs, alerts and logs under the service/endpoint), KPI detail, Load tests, Code insights, Infrastructure (Databases, Message queues — Kafka/RabbitMQ lag)
- [x] KPI (SLO) + burn-rate alerts and root cause analysis UI (`src/lib/rca.ts`)
- [x] Infrastructure (compose), ClickHouse/Postgres schemas, RUM proto, contract (`contracts.md`)
- [x] Backend services: ingest (gw + writer), api (90 endpoints, `services/api/openapi.yaml`), alerting (engine + notifier), collector (`trooper-agent`)
- [x] SDKs: Go, JVM (agent, Spring Boot, Quarkus, Ktor), .NET, JS (browser/react/vue/angular/RN/node), Android, iOS, Flutter
- [x] The UI's `src/data/*` functions read only from the api (no mock/seed/demo data, onboarding + `api bootstrap`) — ADR-013, ADR-028
- [x] Cloud integrations: AWS, Google Cloud, Neon, Vercel (`services/integrations`, Integrations screen) — ADR-012
- [x] Symbolication service + `trooper-cli`
- [x] OIDC auth + team management (ADR-016), code intelligence (Phase 4, ADR-014), load test runner (Phase 5, ADR-015)
- [x] Slack App (alert + root cause/confidence, ack/mute) and GitHub App (adding repos, push webhook → incremental analysis) — ADR-017
- [x] AI usage quota (ADR-018), host/K8s/container monitoring + optional eBPF (ADR-019), AWS Lambda + Azure Functions (ADR-020)
- [x] Session replay (web rrweb + Android/iOS wireframe), continuous profiling (Go/JVM/Node, flame graph + diff), AI/LLM monitoring (GenAI spans, tokens/cost) — ADR-021/022/023
- [x] Self-observability: the platform monitors itself with its own SDKs (`make self`) — ADR-027
- [x] Package publishing: npm, PyPI, Maven Central, NuGet, pub.dev, CocoaPods/SwiftPM, Go — ADR-029
- [x] Service map (`/service-map`, `GET /service-map`): graph of all clients/services/dependencies, problems highlighted, hover + detail panel, zoom/pan; Map tab in service detail (the service's neighborhood)
- [x] Public site (`telemetry.tasktrooper.ai`): landing (animated service map), `/docs` (SDK installs, MCP server, agent skills), separate `/login`; no sign-up
- [x] Demo deploy: `deploy-demo` workflow (manual, main) → Mac mini self-hosted runner, Cloudflare Tunnel, Google OIDC — ADR-031
- [x] MCP v2: 27 read tools + opt-in, approval-gated write tools (monitor, KPI, Slack test, load test; `source=agent`) and the `trooper-alerts` skill — ADR-032
- [x] Assistant (`/assistant`): AI chat with history, @ mentions, images, MCP tools in process, approval-gated writes, widgets — ADR-033 (replaces Overview "Ask telemetry")

Next work: [`.ai/roadmap.md`](.ai/roadmap.md).
