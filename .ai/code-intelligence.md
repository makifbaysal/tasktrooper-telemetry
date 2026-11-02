# Code intelligence

The service that reads the customer's code base, extracts an **inventory** and produces alert + load test suggestions from that inventory
(`services/codeintel`, ADR-014; contract: [contracts.md §13](contracts.md#13-code-intelligence-servicescodeintel-adr-014),
GitHub App: [§18](contracts.md#18-github-app-adr-017), GitLab/Bitbucket: [§18a](contracts.md#18a-gitlab-and-bitbucket-sources)).
Setup/env: `services/codeintel/README.md`.

## Connection

- **GitHub App** (ADR-017): api install flow + repo picker → `repositories(provider='github', installation_id)`.
  codeintel gets an installation token for cloning (`services/pkg/githubapp`: RS256 JWT, token cache).
- Alternatives: `credentials_ref` (PAT; `secret:<id>|env:|file:`), public repo (no token), `provider='local'` +
  `local_path` (dev/demo, read in place).
- **GitLab** (`provider='gitlab'`): gitlab.com or self-managed. `full_name` = `group/subgroup/project` (2+ segments),
  for self-managed `repositories.base_url` (`https://gitlab.example.com[/path]`; https only, no userinfo/query/fragment;
  NULL → `https://gitlab.com`). Token: personal / project / group access token (`read_repository` scope is enough) →
  `credentials_ref`; sent to git as `Authorization: Basic base64("oauth2:<token>")`.
- **Bitbucket Cloud** (`provider='bitbucket'`): `full_name` = `workspace/repo` (exactly 2 segments), host fixed to
  `https://bitbucket.org` (`base_url` not accepted). Token: repository/project/workspace access token →
  `Basic base64("x-token-auth:<token>")`; app password / API token is entered as `user:password` →
  `Basic base64(user:password)`. Bitbucket Server / Data Center are not supported.
- Shared rules live in `services/pkg/vcs` (api + codeintel): full name format, base URL validation, clone URL, auth header.
  For all providers the token is only in the `GIT_CONFIG_*` env (`http.extraHeader`); never in argv/URL/log/`repositories.error`
  (raw token, password and base64 form become `***` in error messages). No token → public clone.
- Webhook secret: for GitLab, Bitbucket and GitHub repos without the App, api generates a random secret on connect, seals it into
  `secrets` (`webhook_secret_ref`) and returns it **once** together with the webhook URL (`POST /repos/{id}/webhook-secret` rotates it).
  The first verified delivery writes `repositories.webhook_received_at` → `webhookActive` (UI "webhook active").
- Monorepo: `.trooper/config.yaml` (`services: [{path, name}]`); otherwise SDK init calls (`TROOPER_SERVICE`,
  `service: "..."` + trooper), then the module root (go.mod/package.json/pyproject), finally the repo name.
- Triggers: first connection (full scan), push to the default branch (webhook file list → incremental), manual "re-
  analyze" (api `analyze_requested_at = now()`, `pending_paths = '{*}'`). `{*}` now means "list unknown": in a previously
  analyzed repo the changed files are found with `git diff last_analyzed_sha..HEAD` → still incremental (see
  [Incremental analysis](#incremental-analysis)). Forced full scan: `codeintel -once <id>` (or `last_analyzed_sha = NULL`).

## Pipeline

```
runner (15 s tick) → due repo (never analyzed | analyze_requested_at > analyzed_at) → debounce (30 s)
  → Postgres claim (status=analyzing, analysis_started_at; single analysis across replicas, stale claim reclaimed after 2×timeout)
  → fetch: git clone --depth 1 --single-branch (token only in GIT_CONFIG_* env as http.extraHeader; never in argv/URL/log)
  → changed files: webhook list | else git diff last_analyzed_sha..HEAD | else full scan (below)
  → extract (deterministic; Go: go/ast, JS/TS/Vue: tokenizer, Python/Java/Kotlin/C#/Swift/Dart: comment/string-blanking scanners) → raw inventory
  → if incremental: only items of changed files are refreshed, others stay as-is from the DB
  → alert-as-code sync (.trooper/monitors/*.yaml → monitors source='code')
  → observation matching (ClickHouse red_1m/consumer_1m, read-only) → observed / dead / shadow + stats
  → criticality: static rules → LLM (cache: same identity + code_hash) → traffic bump (≥100 rpm medium→high)
  → monitored flag (active/muted monitors) → inventory_items upsert (ids preserved by identity)
  → alert_suggestions (proposed; unique per item+kind) → load test drafts (generated/draft)
  → clone deleted (source code is not stored)
```

- Identity: `(kind, service, key)`; key format per contracts §13 (endpoint `METHOD /route/:param`, writer normalisation).
- Timeout `TT_CODEINTEL_ANALYZE_TIMEOUT` (10 min), panic → `status='error'`; 5 min backoff after an error (immediately if a new request arrives).
- Successful analysis: `status='ready'`, `last_analyzed_sha`, `languages`, `services`, `analyzed_at` (if a new request
  arrived while analysing, `analyzed_at = start` → the repo stays due). Error: `status='error'`, `error`, `pending_paths` written back.

### Incremental analysis

The changed file set (`analyze.changedFiles`) is determined in this order:

1. Repo never analyzed (no `analyzed_at` / `last_analyzed_sha`) → **full scan**.
2. `pending_paths` is a usable list (no `*`, ≤ 200 files) → webhook list (`diff_source=webhook`).
3. Otherwise (manual request `{*}`, webhook overflow `{*}`, empty list) → **git diff** (`diff_source=git`,
   `source.Fetcher.ChangedSince`): the old commit is fetched into the shallow clone with `git fetch --depth=1 --no-tags origin <last_analyzed_sha>`
   (same `GIT_CONFIG_*` credentials as the clone; GitHub/GitLab/Bitbucket allow fetch by commit id in protocol v2),
   then `git diff --name-status -z --no-renames <last_sha> HEAD`. Since the diff is tree-to-tree it is correct after a force push
   too (if the old commit still exists on the server). Rename = old + new path; items of deleted files
   drop out of the inventory as on the webhook path (the changed file no longer yields items in extract).
4. HEAD == `last_analyzed_sha` → incremental with zero files: extract refreshes no items, but alert-as-code, traffic matching,
   monitored flag, suggestions and load test drafts are still refreshed.

Fallback to full scan (the cause is written as `reason` in the `full scan` log):

- the old commit cannot be fetched (`source.ErrNoBase`: history rewritten + GC, invalid sha, fetch/auth error);
- git diff has more than 200 files (`store.MaxPendingPaths`); any other git error;
- a change that may alter service mapping: `.trooper/config.yaml|yml` (in both sources), a module manifest added/deleted
  in git diff (`go.mod`, `package.json`, `pyproject.toml`, `requirements.txt`, `setup.py`) — because items of unchanged files
  would stay with the service in the DB;
- `provider='local'`: read in place, uncommitted changes included → the commit diff does not describe what the previous analysis
  saw; every `{*}` is a full scan.

Note: when extractors improve, unchanged files are not re-extracted in incremental analysis; to refresh the whole inventory
a full scan must be forced (above).

## GitHub webhook (`POST /v1/webhooks/github`)

- Signature: `X-Hub-Signature-256`, the App secret (`TT_GITHUB_WEBHOOK_SECRET`) for every event; a repo's `webhook_secret_ref`
  authorises only that repo's `push`. Unsigned/wrong → 401.
- Idempotency: `X-GitHub-Delivery` → `codeintel_webhook_deliveries` (7 days); repeat → 200 "duplicate"; processing error →
  record deleted (GitHub redelivery is processed).
- `push` to default branch: `analyze_requested_at = now()`, `pending_paths ∪= added+modified+removed`. 20+ commits (GitHub truncates
  the list), push without commits or >200 files → `{*}` (git diff, else full scan). Branch deletion / other branches are ignored.
- Debounce: analysis does not start until 30 s after the last request (push bursts merge into a single analysis); if the burst lasts longer
  than 10× debounce it runs anyway. Two analyses never run at the same time for the same repo.
- `installation_repositories` removed → repo `status='disconnected'` (runner skips); added → `pending` + full scan.
- `installation` deleted/suspend → github integration `status='error'` + repos disconnected; unsuspend → restored.

## GitLab / Bitbucket webhook (`POST /v1/webhooks/gitlab`, `POST /v1/webhooks/bitbucket`)

Per-repo webhook (no App), on codeintel's public address; api provides the URL based on `TT_PUBLIC_WEBHOOK_URL`. Candidate
repos are found by `provider` + `lower(full_name)` (all projects); only repos whose secret matches are affected; if none
match → 401.

| | GitLab | Bitbucket Cloud |
|---|---|---|
| Verification | `X-Gitlab-Token` = the repo's webhook secret (constant-time comparison) | `X-Hub-Signature: sha256=<hex>` = HMAC-SHA256(body, secret) |
| Event | `X-Gitlab-Event: Push Hook`; others 200 "ignored" | `X-Event-Key: repo:push`; others 200 "ignored" |
| Repo | `project.path_with_namespace` | `repository.full_name` |
| Idempotency | `Idempotency-Key`, else `X-Gitlab-Event-UUID` (together with `X-Gitlab-Webhook-UUID`) | `X-Request-UUID` (together with `X-Hook-UUID`) |
| Changed files | `commits[].added/modified/removed`; GitLab lists at most 20 commits → `total_commits_count > len(commits)`, push without commits or >200 files → `{*}` | the push payload does not list files → always `{*}` |
| Branch deletion | `after` zero / `checkout_sha` null → ignored | `changes[].new == null` → ignored |

- Default branch push (the repo's `default_branch`; if empty, the one in the payload): `analyze_requested_at = now()`,
  `analyze_requested_by = NULL`, `pending_paths ∪= files`; other branches "ignored". `{*}` is turned into a
  `last_analyzed_sha` diff in the incremental pipeline (see Pipeline).
- Delivery ids are written to `codeintel_webhook_deliveries` with a provider prefix (`gitlab:<hook>:<id>`, `bitbucket:<hook>:<id>`;
  no collision with GitHub ids or with hooks of other projects that connected the same repo). Repeat → 200 "duplicate"; processing
  error → record deleted (the provider's retry is processed). Body limit 25 MB. No id header → no idempotency.
- Every verified delivery (other branches included) writes `webhook_received_at = now()`.
- Metric: `codeintel_webhooks_total{event="gitlab:push"|"gitlab:other"|"bitbucket:repo:push"|"bitbucket:other", result}` (event label is bounded; unverified header values do not create cardinality).

## Extractors (framework-based, deterministic)

| Language | Framework | What is extracted |
|---|---|---|
| Go | net/http (including 1.22 patterns), chi (Route/Group/Mount), gin/echo/fiber (group), gorilla/mux (PathPrefix/Subrouter/Methods), gRPC (`*_ServiceDesc`) | route + method + handler file:line, handler source + signatures of called functions |
| Go | sarama, franz-go, kafka-go, asynq | consumer (process = group/queue, source = topic) |
| Go | robfig/cron | cron |
| TS/JS | express, fastify (route objects, register prefix), koa, hono, Nest (Controller/Get…/setGlobalPrefix), Next.js (app `route.ts` / pages `api`) | route + method + handler; `use`/`register` mounts across files |
| TS/JS | kafkajs, bullmq, amqplib, sqs-consumer / ReceiveMessage, Nest EventPattern/Processor | consumer |
| TS/JS | node-cron, `cron` CronJob, Nest `@Cron` | cron |
| TS/JS | react-router (object + `<Route>`), Next.js page/app router, react-navigation, vue-router (`createRouter`/`RouteRecordRaw[]`, nested `children` joined), Nuxt `pages/` file routing (`nuxt.config.*` root), Angular router (`Routes`, nested `children`, lazy `loadChildren` mounted under the parent, `**` → `:wildcard`) | view |
| Vue SFC | `.vue` files: only the `<script>` / `<script setup>` blocks are tokenized (template/style blanked, line numbers kept) | as TS/JS; language `typescript` for `lang="ts"`, else `javascript` |
| Python | FastAPI (APIRouter prefix, include_router), Flask (Blueprint, register_blueprint), Django (path/re_path/include) | route |
| Python | Celery task + beat_schedule (crontab → cron), kafka-python, confluent-kafka | consumer / cron |
| Java/Kotlin | Spring MVC/WebFlux (class `@RequestMapping` prefix, `@*Mapping`, functional `route()`/`coRouter` nest), JAX-RS/Quarkus (`@Path` + `@GET`…), Micronaut (`@Controller` + `@Get`…), Ktor (nested `route {}`, `Route.x()` extensions mounted across files) | route + method + handler (`Class.method`) + source; same-file string constants and Kotlin templates resolved |
| Java/Kotlin | spring-kafka/amqp/jms, `@SqsListener`, SmallRye `@Incoming`, Micronaut `@KafkaListener`+`@Topic`; `@Scheduled` (Spring/Quarkus/Micronaut); Android Activity/Fragment subclasses, Compose Navigation `composable()` | consumer (process = group/listener id, source = topics/queues), cron (cron or `every <interval>`), view (class name / SDK-style route) |
| C# (.NET) | ASP.NET Core controllers (`[Route]` incl. inherited/const, `[controller]`/`[action]`/`[area]`, `Http*`/`AcceptVerbs`, absolute `/`·`~/` templates, conventional routes with `MapControllerRoute`), minimal APIs (`Map*`, `MapGroup` chains, group variables, `this RouteGroupBuilder` mounts across files), gRPC (`X.XBase` overrides) | route + method + handler (`Class.Method` / method group / lambda) source |
| C# (.NET) | MassTransit `IConsumer<T>`, NServiceBus, Confluent.Kafka `Subscribe` (+ `GroupId`), RabbitMQ.Client `BasicConsume`, Azure Service Bus `CreateProcessor`, queue-reading `BackgroundService`; Hangfire `RecurringJob` (`Cron.*` → cron), Quartz `WithCronSchedule`, Coravel `Schedule<T>()` | consumer (process = consumer class / group / queue, source = message type / topic / queue), cron |
| Swift | SwiftUI (`View` structs named `*Screen`/`*Page` or used as NavigationLink / navigationDestination / sheet / TabView / WindowGroup targets, `.trooperView` name), UIKit (`UIViewController` / `UITableViewController` / `UICollectionViewController` subclasses + in-repo subclasses; containers skipped), Vapor (`get/post/…/on`, `grouped`, `group {}`, `RouteCollection` + `register(collection:)`) | view (key = type name, `meta.rumName`) / route + method + handler |
| Dart (Flutter) | go_router (`GoRoute` / `@TypedGoRoute`, nested `routes:` joined, ShellRoute transparent), MaterialApp `routes:` map + `onGenerateRoute` `case`, auto_route (`AutoRoute(page:, path:, children:)`, `@RoutePage`), unrouted `*Screen`/`*Page` widgets | view (key = route path / route name / class name; `meta.rumName` = what the SDK observer reports) |
| YAML | k8s CronJob (+ `TROOPER_SERVICE` env), helm (regex fallback) | cron |
| All | HTTP client URL literals, DB/cache/queue driver imports / `using` / `Use*` calls / connection strings (Go, TS/JS, Python, Java/Kotlin, C#, Swift, Dart) | dependency (`<kind> · <name>`) |

Extractors stay deterministic; the LLM is used for **interpretation**, not **extraction**. Route parameters are normalised to a single
form (`{id}`, `<int:id>`, `[id]`, `:id(\d+)` → `:id`; wildcard → `:wildcard`). Limits: 50,000 files, 1 MB per file,
`node_modules`/`vendor`/`dist`/hidden directories and mobile/JVM build output (`build`, `target`, `Pods`, `DerivedData`, `generated`, …) are
skipped. Test sources are skipped by name (`*Test`, `*Tests`, `*Spec`, `*IT`, `*_test`) and by directory (`src/test`, `androidTest`,
`Tests`, `*.Tests`, `test`, `integration_test`); so are generated files (`*.g.dart`, `*.freezed.dart`, `*.Designer.cs`, `*.g.cs`). Module
roots for the service name also include `pom.xml`, `build.gradle[.kts]`, `*.csproj`, `Package.swift`, `pubspec.yaml`.

## LLM usage

- The platform's single LLM endpoint (ADR-034, `services/pkg/llmapi`): an **OpenAI-compatible Chat Completions** API from
  `TT_ASSISTANT_API_URL` (default `https://api.meta.ai/v1`, Meta Muse Spark), `TT_ASSISTANT_API_KEY`, `TT_ASSISTANT_MODEL` — the same
  variables as the dashboard assistant. Enabled only when key **and** model are both set (model ID never in code); otherwise static rules.
- Plain `net/http` (no vendor SDK), non-streaming, `max_tokens` 16000, up to 3 retries on network errors / 429 / 5xx.
- Two tiers: **full** (items the static rules rate medium or above, 15 per request) asks every field; **lite** (static `low`,
  30 per request) asks only `criticality`, `business_domain`, `summary` — suggestions start at `high` and load test drafts only use
  write endpoints of critical services, so monitor templates / load test hints of low items were unused output. Trivial endpoints
  (health, metrics, static assets — `enrich.Trivial`) are not sent at all.
- Batches are grouped by tier, then by service (services in order of their most critical item), so the AI quota still goes to the
  most critical items first.
  JSON output: `response_format: {type: json_schema, strict: true}` **and** the schema in the system prompt; if the endpoint answers HTTP
  400 to `response_format`, the request is repeated without it and later batches send only the prompt schema. The answer's JSON object is
  taken from between the outermost braces (Markdown fences tolerated). `finish_reason` `length` (truncated) or undecodable JSON →
  the batch is retried once per half (usage of every request recorded); `content_filter` → refused (batch skipped, usage recorded).
- System message = tier instructions + schema only: identical for every request of a tier (a stable prefix for providers that cache
  prompt prefixes automatically; no explicit cache control). User message = repository context (name, languages, service names, the
  **inventory rows of the batch's service** only, sorted, ≤300) + the items as compact JSON.
- Self telemetry: the HTTP client goes through the trooper `llm` transport with the endpoint host registered under `llmapi.Provider`
  (`api.meta.ai` → `meta`, `*.baseten.co` → `baseten`, unknown OpenAI-compatible hosts → `openai`) → GenAI client spans
  (`gen_ai.provider.name`, `gen_ai.request.model`, `gen_ai.usage.*`; content only with `TROOPER_LLM_CAPTURE_CONTENT=true`).
- Input: item + handler source + signatures of called functions. The source is compacted first (trailing whitespace, blank lines and
  common indentation removed; comment runs of 3+ lines keep 2 lines + `…`), then capped at 6000 characters. The code hash uses the
  original snippet. Output: `criticality`, `businessDomain`, `summary`,
  `failureModes[]`, `suggestedMonitors[]` (template + rationale), `loadtestHint` (sample payload, idempotent, note) → `inventory_items.llm`.
- Cache: same identity + `code_hash` (sha256 of key + snippet) → the previous result is reused; on an incremental push unchanged
  handlers are not sent to the LLM. Per-analysis cap `TT_CODEINTEL_LLM_MAX_ITEMS` (300; critical ones first). Dependencies/views are not sent.
- A batch error does not fail the analysis (warning + static criticality).

### AI quota (ADR-018, contracts §19)

- **Before** each LLM batch, `services/pkg/aiquota` `Reserve` (per org `pg_advisory_xact_lock(7140, org_id)`, short transaction):
  estimate = `ceil(promptChars/4) × in + min(16000, 600 × items) × out` credits (output estimated at 600 tokens per item; estimating with `max_tokens`
  would reject far below the limit). `used + estimate > limit` → rejected. **After** the call, an `ai_usage` row with the real tokens
  (`feature=codeintel.enrich`, `model` = `TT_ASSISTANT_MODEL`, `ref=repo:<id>`, `input_tokens` = `prompt_tokens` (cached tokens included);
  credits: `prompt_tokens_details.cached_tokens` count as cache reads at 0.1× the input price, the rest at the input price). The record is
  written even if the response arrives but cannot be used (decode error, `finish_reason: length`).
- User = `repositories.analyze_requested_by` (api fills it on manual analysis). Webhooks (push, installation) and other
  automatic requests set this column to `NULL` → only the org limit applies; the member cap (`memberships.ai_monthly_credits`) only applies
  to analyses started by the member.
- Rejected → remaining items without LLM (static criticality, previous cache results kept), the analysis finishes successfully; the rejection
  persists for the whole analysis (not asked again). If the quota check fails (DB) it fails closed: LLM is skipped, `reason: check_failed`.
- Analysis summary `repositories.analysis` (jsonb, `FinishOK`): `{sha, incremental, items, llmItems, llm: {status: ok|disabled|quota_exceeded,
  reason?: org_limit|user_limit|check_failed, credits, skipped?}}`. `disabled` = LLM not configured; `credits` = credits spent by this
  analysis (4 decimals). api passes it through as `inventory.analysis.llm`.
- Prices `TT_AI_PRICES` (`<model>=<in>/<out>`, USD / million tokens; exact id, else longest prefix), `TT_AI_DEFAULT_PRICE` (`3/15`),
  plans `TT_AI_PLAN_CREDITS` (`free=100,team=5000,enterprise=-1`).
- Suggestion generation (`suggest`) does not call the LLM; the `codeintel.suggest` feature is reserved for future LLM-based suggestions.

## Static criticality rules

`critical | high | medium | low`: critical keyword such as payment/checkout/auth/order + write → critical, critical + read → high, write → medium,
read/health → low; on an `ANY` method a DB write statement counts as a write; consumer DLQ/critical → high, others → medium; critical cron → high;
payment provider dependencies → high, DB/queue → medium. Traffic ≥100 rpm → medium → high.

## Outputs

- **Inventory** (`inventory_items`, Code insights screen): item, service, file:line, criticality, `observed`, `monitored`, `dead`
  (exists in code, no traffic for 30 days; not flagged if the service is not instrumented at all), `shadow` (has traffic, not in code; `repository_id` NULL),
  `summary`, `llm`, `meta.stats` (7-day count/errors/p95/rpm/errorRate).
- **Alert suggestions** (`alert_suggestions`, `state='proposed'`, unique on `(inventory_item_id, kind)`; accepted/rejected rows are
  not touched): for critical/high items without a monitor that are not dead/shadow. Endpoint → error rate (`metric_threshold`,
  threshold calibrated from observation if there are ≥500 requests: max(2×rate, 1%), capped at 50%), if critical also latency (p95×1.5) and error_new;
  consumer → `consumer_lag` if there is exactly one suitable broker cluster, else `job_missed` ("consumer stalled"); cron → `job_missed`
  (interval from the cron expression). `monitor_draft` = api `POST /monitors` body. Details: [alerting.md](alerting.md#code-aware-automatic-alerts).
- **Load test drafts** (`loadtest_scenarios`, `source='generated'`, `state='draft'`, per service "<svc> · critical paths
  (generated)"): critical+high HTTP endpoints, weights from production traffic, levels 1×/2×/3× production RPS (VUs if no traffic),
  LLM payload example, note on non-idempotent ones, thresholds `{p95Ms, errorRate}`. A draft moved to `ready` is not updated;
  drafts no longer generated and never run are deleted. Details: [load-testing.md](load-testing.md#scenario-generation).
- **Alert-as-code**: `.trooper/monitors/*.yaml` (alerting.md § Rule schema; single document, list, `monitors:` or `---`) →
  `monitors(source='code', code_ref='<repo>:<path>')`, key code_ref + name; a monitor removed from the file is deleted, monitors of a file
  with parse errors are kept (warning). api shows these rows read-only (409 "managed in repository").
