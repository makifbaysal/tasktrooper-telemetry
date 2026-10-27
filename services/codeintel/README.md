# codeintel

Code intelligence (ADR-014, `.ai/code-intelligence.md`, contracts §13/§18): reads connected repositories, extracts an
inventory of endpoints / consumers / crons / views / dependencies, matches it with observed traffic, scores criticality
(static rules + optional Claude enrichment), and writes alert suggestions, generated load test drafts and
alert-as-code monitors to Postgres.

```bash
make infra && make migrate                     # Postgres + ClickHouse (005_codeintel.sql, 009_slack_github.sql)
cd services/codeintel
GOWORK=off go run ./cmd/codeintel              # :8096 — /healthz, /metrics, POST /v1/webhooks/github
GOWORK=off go run ./cmd/codeintel -once 12     # analyze repository 12 now (full scan) and print a report
GOWORK=off go test ./...                       # unit tests
GOWORK=off go test -tags integration ./internal/analyze/   # end-to-end against live Postgres + ClickHouse (project 9401)
```

Local repository for development:

```sql
INSERT INTO repositories (project_id, provider, full_name, default_branch, local_path)
VALUES (1, 'local', 'acme/shop', 'main', '/path/to/checkout');
```

## Configuration

| Env | Default | |
|---|---|---|
| `TT_CODEINTEL_ADDR` | `:8096` | HTTP listen address |
| `TT_POSTGRES_URL` | compose Postgres | metadata (read/write) |
| `TT_CLICKHOUSE_URL` | compose ClickHouse | observed traffic, read-only (`readonly=1`); unreachable → analysis continues without matching |
| `TT_SECRETS_KEY` | dev key | decrypts `secret:<id>` references |
| `TT_GITHUB_APP_ID`, `TT_GITHUB_APP_PRIVATE_KEY` | — | GitHub App (key: PEM or `env:`/`file:` ref); needed for installation repositories |
| `TT_GITHUB_API_URL` | `https://api.github.com` | GitHub Enterprise: `https://host/api/v3` (clone host derived from it) |
| `TT_GITHUB_WEBHOOK_SECRET` | — | App webhook secret (literal or `env:`/`file:` ref) |
| `ANTHROPIC_API_KEY` + `TT_CODEINTEL_MODEL` | — | both set → LLM enrichment; otherwise static rules only. No model id is hard-coded |
| `TT_CODEINTEL_LLM_MAX_ITEMS` | `300` | items sent to the LLM per analysis (most critical first) |
| `TT_AI_PRICES` | — | AI quota price table `<model>=<in>/<out>,…` (USD per million tokens; exact id, else longest prefix) |
| `TT_AI_DEFAULT_PRICE` | `3/15` | price of models missing from `TT_AI_PRICES` |
| `TT_AI_PLAN_CREDITS` | `free=100,team=5000,enterprise=-1` | monthly credits per `orgs.plan` (1 credit = 0.01 USD; -1 unlimited) |
| `TT_CODEINTEL_WORKDIR` | OS temp | where clones go (deleted after each analysis) |
| `TT_CODEINTEL_INTERVAL` | `15s` | scheduler tick |
| `TT_CODEINTEL_DEBOUNCE` | `30s` | quiet time after the last push before re-analyzing |
| `TT_CODEINTEL_ANALYZE_TIMEOUT` | `10m` | per analysis; a claim older than 2× is retaken |
| `TT_CODEINTEL_CONCURRENCY` | `2` | parallel analyses (one per repository, always) |
| `TT_LOG_LEVEL` | `info` | |

The runtime image needs the `git` CLI (`Dockerfile`: alpine + git; build context `services/`).

## Repository access

| provider | how |
|---|---|
| `github` + `installation_id` | GitHub App installation token (JWT RS256 → `/app/installations/{id}/access_tokens`, cached until 5 min before expiry) |
| `github` + `credentials_ref` | token from `secret:<id>` / `env:NAME` / `file:/path` |
| `github` (neither) | public clone |
| `local` | `local_path` read in place (dev/demo) |
| `gitlab`, `bitbucket` | not supported yet (analysis ends in `error`) |

`git clone --depth 1 --single-branch --no-tags --branch <default>`; the token goes only into `GIT_CONFIG_*` environment
variables as `http.extraHeader` (never argv, URLs, logs or stored errors). The clone is removed after the analysis.

## GitHub App setup

1. GitHub → Settings → Developer settings → GitHub Apps → New. Permissions: **Contents: read**, **Metadata: read**.
   Subscribe to events: **Push** (installation and installation_repositories events are always sent to Apps).
2. Webhook URL: `https://<codeintel host>/v1/webhooks/github`, webhook secret → `TT_GITHUB_WEBHOOK_SECRET`.
3. Generate a private key → `TT_GITHUB_APP_PRIVATE_KEY` (e.g. `file:/run/secrets/github-app.pem`), App id → `TT_GITHUB_APP_ID`.
   The api uses the same variables (+ `TT_GITHUB_APP_SLUG`) for the install flow and the repository picker.

Classic repository webhooks also work: set `repositories.webhook_secret_ref`; such a secret only authorizes `push` for
that repository.

## Webhook

| event | effect |
|---|---|
| `push` on the default branch | `analyze_requested_at = now()`, `pending_paths ∪= added+modified+removed`; ≥20 commits, no commits or >200 files → `{*}` (full scan) |
| `installation_repositories` removed / added | repository `disconnected` / back to `pending` with a full scan |
| `installation` deleted / suspend / unsuspend | github integration `error` + repositories `disconnected` / restored |
| `ping` | 200 |

Signature `X-Hub-Signature-256` is required (401 otherwise). Deliveries are idempotent by `X-GitHub-Delivery`
(`codeintel_webhook_deliveries`, 7 days); a delivery whose processing failed is forgotten so GitHub's redelivery works.
Pushes are debounced (`TT_CODEINTEL_DEBOUNCE`) and coalesced into one incremental analysis that re-extracts only the
changed files (other items stay as stored, cached LLM results are reused by `code_hash`).

## Extractor coverage

| Language | Endpoints | Consumers | Crons | Views | Dependencies |
|---|---|---|---|---|---|
| Go | net/http (incl. 1.22 patterns), chi, gin, echo, fiber, gorilla/mux, gRPC service descs | sarama, franz-go, kafka-go, asynq | robfig/cron | — | DB/cache/queue imports, HTTP URL literals |
| TS/JS | express, fastify, koa, hono, NestJS, Next.js route handlers / API routes | kafkajs, bullmq, amqplib, SQS, Nest EventPattern/Processor | node-cron, cron, Nest `@Cron` | react-router, Next.js pages, react-navigation | package imports, URL literals |
| Python | FastAPI, Flask, Django (path/re_path/include) | Celery tasks, kafka-python, confluent-kafka | Celery beat | — | imports, URL literals |
| YAML | — | — | Kubernetes CronJob, Helm templates | — | — |

Service mapping: `.trooper/config.yaml` (`services: [{path, name}]`) → SDK init calls → module root → repository name.

## Outputs

- `inventory_items` (per repository; ids are stable across analyses by `(kind, service, key)`), shadow items with
  `repository_id` NULL.
- `alert_suggestions` (`proposed`, one per item and monitor kind; accepted/rejected rows are never changed; `monitor_draft`
  is an api `POST /monitors` body without `source`).
- `loadtest_scenarios` (`source='generated'`, `state='draft'`).
- `monitors` (`source='code'`, `code_ref='<repo>:<path>'`) from `.trooper/monitors/*.yaml`.
- `repositories.status` `pending → analyzing → ready | error`, `analyzed_at`, `last_analyzed_sha`, `languages`, `services`, `error`.

Metrics: `codeintel_analyses_total{result}`, `codeintel_webhooks_total{event,result}`, `codeintel_repositories`,
`codeintel_errors_total{stage}`, `codeintel_items_total`, `codeintel_llm_items_total`, `codeintel_analysis_seconds_total`.

## AI quota

Every LLM batch is checked against the org's monthly AI credits (ADR-018, `services/pkg/aiquota`, `.ai/code-intelligence.md`
§ AI kotası) before it is sent and recorded in `ai_usage` (`feature=codeintel.enrich`, `ref=repo:<id>`) after it returns. The member
cap applies when `repositories.analyze_requested_by` is set (manual analysis from the dashboard); webhook-queued analyses clear it.
A denial keeps the remaining items on static criticality; the result lands in `repositories.analysis.llm`
(`{status: ok|disabled|quota_exceeded, reason?, credits, skipped?}`).
