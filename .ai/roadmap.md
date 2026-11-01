# Roadmap

## Phase 0 — Foundation ✅
- [x] Architecture and product docs
- [x] Design system (tokens + core components)
- [x] Dashboard screens (first version with mock data; mock removed with ADR-028, real data only)
- [x] `deploy/docker-compose.yml` (ClickHouse, Postgres, Redpanda, Redis) + schemas (`deploy/clickhouse`, `deploy/postgres`)
- [x] `services/pkg` shared packages, `proto/rum/v1`, inter-service contract (`contracts.md`)

## Phase 1 — Backend APM MVP ✅

Status: services (`ingest`, `api`, `alerting`, `collector`, `integrations` = `trooper-cloud`, `symbolicator`) and SDKs (Go, JVM,
.NET, JS, Android, iOS, Flutter, React Native) are ready; tests and the end-to-end flow (SDK → gw → Redpanda → writer → ClickHouse → api → UI)
work. The dashboard only shows real data from the api (ADR-013, ADR-028); if there is no data, screens show an empty state with SDK setup instructions.

- [x] UI `src/data/*` → api (APM, infra, alerting, KPI, load test, integrations)
- [x] `consumer_1m.messaging_system`, `broker_topics_1m`, `broker_lag_1m.oldest_age_s`; db metadata (version/zone/instance class/disk)
- [x] KPI headline `metric` + series, `degradedSince`; preview for all monitor types; incident `pending` + `quiet`
- [x] ingest: Vercel drain, GeoIP, tail sampling, Prometheus `/metrics`, RUM backfill (≤ 24 hours) fix
- [x] SDK: Android NDK crash + ApplicationExitInfo + R8 upload plugin, iOS all-thread stack + dSYM script, Flutter plugin, RN `TrooperRum`
  native module, JVM `tt-attempt` + tracestate ratio + `trooper.sdk.errors`
- [x] collector: MongoDB, Kafka topic metadata, MSK IAM / OAUTHBEARER / KIP-848, broker node metrics, MySQL prepared statements
- [x] Cloud integrations (ADR-012): AWS (RDS, ElastiCache, MSK, SQS), GCP (Cloud SQL, Memorystore, Pub/Sub), Neon, Vercel
  (deploy + drain) + Integrations screen
- [x] Symbolication: sourcemap, R8/ProGuard, dSYM, NDK, Dart + `trooper-cli`
- [x] Package publishing (npm, PyPI, Maven Central, NuGet, pub.dev, CocoaPods/SwiftPM, Go proxy) — ADR-029
- [x] Quarkus `tt-attempt` automatic reading; `@tasktrooper/node`
- [x] ingest-gw internal service credential (`TT_INTERNAL_TOKEN`, contracts §2)
- [ ] Verification with real AWS/GCP/Neon/Vercel accounts (currently tested against fake servers)
- [x] Auth (OIDC + dev login, ADR-016), org/project/member/API key management + audit log
- [x] api: ranges shorter than 1 hour (`15m`, `30m`)

## Phase 2 — RUM + Logs ✅
- [x] `@tasktrooper/browser`, `/v1/rum`, `rum_events` + MVs, frontend screens on real data
- [x] Log ingestion + patterns + Logs screen
- [x] `@tasktrooper/react-native`
- [x] Vercel log source (`vercel.source`) as a filter on the Logs screen

## Phase 3 — Alerting ✅
- [x] `alert-engine` (threshold, latency, log_match, error_new, anomaly, composite, job_missed, consumer_lag/oldest_age, kpi_burn) +
  `notifier` (Slack, PagerDuty, Opsgenie, email, webhook, Teams); stream rules; mute, maintenance windows, reminders
- [x] KPIs (SLO) + burn-rate alerts
- [x] Root cause analysis (`rca` endpoint, incident `rootCause`)
- [x] Alert-as-code (`.trooper/monitors`, synced by codeintel)
- [x] anomaly/composite/job_missed/error_new forms in AlertEditor
- [x] Slack App: alert message to the channel with root cause + confidence, thread reply, ack/mute buttons (ADR-017)
- [x] Reminders in KPI rules; stream rules with multiple shards

## Phase 4 — Code intelligence ✅
- [x] GitHub App installation, repo selection, push webhook → incremental analysis (30 s debounce)
- [x] TS/JS + Go extractors
- [x] LLM enrichment (optional), criticality score, alert suggestions, coverage metric
- [x] Incremental analysis with webhook paths
- [x] `last_analyzed_sha` git-diff based incremental (including manual requests and webhook overflow; fallback: full scan)
- [x] GitLab (cloud + self-managed) / Bitbucket Cloud sources (clone, webhook, api, UI)

## Phase 5 — Load testing ✅
- [x] Scenario/run CRUD (api + UI)
- [x] `loadtest-ctl` + `loadtest-runner` (own Go engine, ADR-015)
- [x] Domain verification, limits
- [x] Scenario generation from inventory/traffic, live run screen

## Phase 6 — AI quota, infrastructure and serverless ✅
- [x] AI usage quota: credit ledger, plan defaults, org + member limits, 80%/100% notifications, Usage screen (ADR-018)
- [x] trooper-agent `host` / `cluster` modes: host, container, process, K8s node/pod/workload, Warning events (ADR-019)
- [x] Helm chart (DaemonSet + Deployment + RBAC) + optional eBPF (OBI)
- [x] Monitor kind `infra`, pod/host signals in RCA
- [x] AWS Lambda discovery + CloudWatch metrics, Azure Functions provider, `faas` monitors, cost estimation (ADR-020)
- [x] `@tasktrooper/node` `wrapLambda`
- [x] UI: Hosts, Kubernetes, Serverless screens; infrastructure tab in service detail
- [x] Watch instead of list in K8s cluster mode (for thousands of pods and more)
- [x] containerd/Podman hosts, GPU and Windows node metrics
- [x] Azure: per-function metrics (App Insights), service mapping from app settings
- [x] Per-pod trace/log filter, trace detail page; merging the two Helm charts (`services/collector/deploy/helm` old DB chart)
- [x] Real K8s verification: kind cluster (host + cluster mode, OOMKilled pod, watch); Lambda discovery with LocalStack
- [ ] Verification with real EKS/AKS, Lambda CloudWatch metrics and an Azure account (requires accounts)
- [ ] Non-NVIDIA GPUs, container metrics without Docker/Podman on cgroup v1 hosts, real Windows node test

## Phase 7 — Session replay, continuous profiling, AI monitoring (New Relic parity) ✅
- [x] Contract: ADR-021/022/023, contracts §23–25, ClickHouse `005_replay_profiles_llm.sql`, Postgres `012_llm_prices.sql`
- [x] ingest: `/v1/replay` → `tt.replay` → S3 + `replay_segments`; `/v1/profiles` → `tt.profiles` → `profile_stacks`/`profiles`;
  GenAI spans → `dep_kind=ai`, `llm_1m`, `llm_content` (including OpenLLMetry and Vercel AI SDK names)
- [x] api: `/replays*`, `/profiles/*`, `/ai/*` + price table; sessions `hasReplay`, error `replay`, trace span `llm`
- [x] SDK: `@tasktrooper/replay` (rrweb), Android/iOS wireframe replay; Go/JVM (JFR)/Node profiling; Node/Go/JVM/.NET LLM spans
- [x] UI: Replays tab + player, Profiling tab + flame graph/diff, AI monitoring screen + service AI tab
- [x] Demo: replay (web rrweb + mobile wireframe), profile (two versions, diff stories), LLM traffic (Anthropic/OpenAI/Gemini)
- [ ] Mobile replay verification on a real device (main thread cost, Compose/SwiftUI output)

## Phase 7b — Complements
- [x] Flutter (Dart Element tree) and React Native (native SDK bridge) replay
- [x] .NET (`TaskTrooper.Telemetry.Profiling`, EventPipe) and Python profiling
- [x] Python SDK `tasktrooper` (ADR-024): OTel distribution + profiling + LLM
- [x] LLM alerts (monitor kind `llm`, templates, editor), price table `services/pkg/llmprice`
- [x] OTLP profiles signal (`/v1development/profiles`, gRPC)
- [x] Web replay: buffer tab `storage` event
- [x] Mobile replay emulator/simulator verification

## Phase 8 — MCP server (local coding agent integration) ✅
- [x] Contract: ADR-025/026, contracts §26, `.ai/mcp.md` (12 read-only `tt_*` tools)
- [x] `services/mcp` module: stdio transport (stdout = protocol), `TT_*` config + startup `/me` check,
  single shared HTTP client, 64 KB result cap (`truncated`/`total`/`hint`), `internal/redact` PII mask
- [x] Tests: validation/redact/error mapping (table-driven), in-memory e2e (12 tools + 3 calls),
  stdio stdout purity (real binary subprocess)
- [x] Dockerfile (distroless; build context `services/`, repo root with ADR-027); CI module matrix automatically from `go.work`
- [ ] Manual verification with a real MCP client on a live stack

## Phase 9 — Self-observability (ADR-027)
- [x] Contract: ADR-027, contracts §27; self project `make self` → `api bootstrap` (ADR-028)
- [x] Go services instrumented with `trooper.Start` (HTTP, pgx, go-redis, franz-go, ClickHouse, slog → OTel logs, runtime
  metrics, optional profiling; codeintel LLM spans) — no export if `TROOPER_DSN` is missing
- [x] Feedback loop protection: ingest-gw / ingest-writer do not produce spans/logs for `TT_SELF_PROJECT_ID` traffic
- [x] api CORS: `traceparent`, `tracestate`, `baggage` (RUM → api trace end to end)
- [x] Docker build context is the repo root (Dockerfiles copy `sdks/go`)
- [x] Dashboard RUM `tt-web` (`@tasktrooper/browser` + `@tasktrooper/react` + `@tasktrooper/replay`, the SDK chunk is not loaded if `VITE_TROOPER_DSN`
  is missing): route template view names, React root error handlers, Web Vitals, `traceparent` to the api, user `identify` only with
  the internal id, replay off by default + all text masked
- [x] Infrastructure: `deploy/self/agent.yaml` (Postgres, Redis, Redpanda `tt.*` topic / `tt-*` group + host + Docker containers),
  `make self` / `make self-down`
- [ ] `clickhouse` db engine in the collector for ClickHouse (for now only container metrics: CPU/memory/net)
- [x] No dummy data (ADR-028): `tools/demo`, web mock mode and Postgres seeds removed; onboarding (`POST /orgs`) +
  `api bootstrap`; tests set up their own fixtures
- [ ] Separate quota/sampling settings for the self project; enabling the self project by the operator in production (runbook)
