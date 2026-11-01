# mcp

MCP server for local coding agents (Claude Code, OpenCode, …): read-only by default, opt-in confirmed writes (ADR-025,
ADR-032). Design in
[`.ai/mcp.md`](../../.ai/mcp.md), tool contract in [`.ai/contracts.md`](../../.ai/contracts.md) §26.
Module `github.com/tasktrooper/telemetry/services/mcp`, binary `cmd/tt-mcp`, **no port**: it speaks MCP
over stdin/stdout (stdio transport), spawned by the agent.

**The stdout guarantee:** stdout carries nothing but newline-delimited JSON-RPC. All logging goes to
stderr — do not import `services/pkg/logx` here (it logs to `os.Stdout`) and do not add `fmt.Print*`.
`cmd/tt-mcp/main_test.go` builds the real binary and fails if a single stdout line is not a JSON-RPC
message; keep it green.

## Surface

27 read tools over `services/api` with the shared key-derived tenant (no org/project tool params). The original 12:

| Tool | Upstream |
|---|---|
| `tt_list_services` | `GET /api/v1/services` |
| `tt_get_service_kpis` | `GET /api/v1/services/{service}` |
| `tt_list_endpoints` | `GET /api/v1/services/{service}/endpoints` |
| `tt_endpoint_rca` | `GET /api/v1/services/{service}/endpoints/{endpoint}/rca` |
| `tt_search_traces` | `GET /api/v1/traces` |
| `tt_get_trace` | `GET /api/v1/traces/{traceId}` |
| `tt_search_logs` | `GET /api/v1/logs` |
| `tt_list_monitors` | `GET /api/v1/monitors` |
| `tt_list_incidents` | `GET /api/v1/incidents` |
| `tt_get_incident` | `GET /api/v1/incidents/{id}` + `/events` (two calls, merged) |
| `tt_list_loadtest_runs` | `GET /api/v1/loadtests/runs` |
| `tt_list_deploys` | `GET /api/v1/deploys` |

Added by ADR-032 (`tools_insights.go`, `tools_infra.go`, `tools_loadtest.go`, `tools_monitors.go`): `tt_list_errors`,
`tt_get_error`, `tt_list_kpis`, `tt_list_alert_suggestions`, `tt_list_inventory`, `tt_list_monitor_templates`,
`tt_list_notify_channels` (Slack App channels + configured channels, each with its exact `target` string; never a
channel's config), `tt_get_app_summary`, `tt_get_ai_overview`, `tt_list_hosts`, `tt_list_databases`,
`tt_list_consumer_groups`, `tt_list_loadtest_scenarios`, `tt_get_loadtest_run` and `tt_preview_monitor` (a POST that
saves nothing).

### Writes (`TT_MCP_ALLOW_WRITE=true`)

11 more tools: `tt_create_monitor`, `tt_update_monitor`, `tt_delete_monitor`, `tt_accept_suggestion`, `tt_create_kpi`,
`tt_update_kpi`, `tt_delete_kpi`, `tt_send_slack_test`, `tt_create_loadtest_scenario`, `tt_run_loadtest`,
`tt_stop_loadtest`. Rules (tested in `e2e_test.go` `TestE2EWrite`):

- Without `confirm: true` nothing is sent; the result is `{dryRun, wouldSend, next}` for the agent to show the user.
  `tt_stop_loadtest` is the exception (it only reduces load).
- Created monitors, KPIs and scenarios get `source: "agent"` whatever the input says.
- Deletes only touch `source = agent`; `source = code` (repository managed) is never updated.
- `tt_run_loadtest` refuses draft scenarios; new scenarios get an abort `gate` when they have none.
- API 400/409/501/502 messages are passed on (≤ 300 chars); 5xx bodies never are.

## Cross-cutting rules

- **Validation before the API:** `range` (15m…7d), `baseline` (prev/1d/7d), `type`
  (http/worker/web/mobile), `level`, `state`, `sort`, `minMs >= 0`, 32-hex trace ids, `limit` clamped
  (trace ≤ 100 default 20, log ≤ 200 default 50, incident ≤ 100 default 20).
- **Result cap:** 64 KB JSON; a trimmed result carries `truncated: true`, `total` (original count) and a
  `hint` on how to narrow the query. Trace span lists additionally cap at 200.
- **PII:** log messages, span attrs/resource/events and incident event data pass through
  `internal/redact` (the single masking helper of this module): email, IPv4/IPv6, `Bearer …`,
  `tt_api_`/`tt_ingest_`/`tt_dev_` (legacy) keys, 16-digit card-like numbers. Tool names (`tt_list_*`) are
  deliberately not matched.
- **Errors:** short and actionable, never a stack, secret or raw upstream body — `check TT_API_KEY`
  (401/403), `call tt_list_endpoints for valid endpoint ids` (404), API unavailable (5xx/timeout).
- **Startup:** `Check` calls `GET /me` once — a bad key or unreachable API fails fast before any tool
  call; `TT_ORG_ID`/`TT_PROJECT_ID` are cross-checked when set. The key is only ever on the wire.

## Config

| Env | Default |
|---|---|
| `TT_API_KEY` | **required** — a project api key (`POST /api/v1/api-keys`, dashboard Settings → API keys) |
| `TT_API_URL` | `http://localhost:8080` |
| `TT_ORG_ID` / `TT_PROJECT_ID` | unset (optional cross-checks) |
| `TT_LOG_LEVEL` | `info` (stderr JSON) |
| `TT_MCP_ALLOW_WRITE` | `false` — `true` registers the 11 write tools (ADR-032) |
| `TT_MCP_ENV_FILE` | unset — env file to read; otherwise `.tasktrooper/mcp.env` (cwd up to the git root), then `~/.config/tasktrooper/mcp.env` |

Variables already in the environment win over the env file; only the `TT_*` keys above are read, and a file readable by group/others logs a `chmod 600` warning.

## Install

`curl -fsSL https://telemetry.tasktrooper.ai/install-agent.sh | bash` detects the installed coding tools (Claude Code, Cursor,
Antigravity, Codex, OpenCode, Gemini CLI, Muse Code), asks which ones and global vs project scope, verifies the key, writes the
env file and registers the server + skills. Binaries come from GitHub Releases tagged `mcp/vX.Y.Z`
(`.github/workflows/release-mcp.yml`). Details: `.ai/mcp.md` § Kurulum.

Connection recipes (OpenCode, Claude Code, `.mcp.json`): `.ai/mcp.md` § Bağlanma (elle).

## Self telemetry (ADR-027)

`internal/selftel` (contracts §27), service `tt-mcp`:

| Env | Default | |
|---|---|---|
| `TROOPER_DSN` | unset | self project ingest DSN (dev: the DSN `make self` prints, `http://<ingest-key>@localhost:4318/<self-project-id>`); unset = nothing is started, behaviour unchanged |
| `TROOPER_SERVICE` | `tt-mcp` | service name override |
| `TROOPER_ENV` | `TT_ENV`, else `development` | |
| `TROOPER_VERSION` | `-X main.version` build value | deploy marker |
| `TROOPER_SAMPLE_RATE`, `TROOPER_PROFILING`, `TROOPER_DEBUG` | SDK defaults | see `sdks/go/README.md` |

Traced: one server span per `tools/call`, named after the tool (`tt_list_services`, …; `trooper.route` = tool name,
`gen_ai.tool.name`, status Error on a failed call or an `isError` result; arguments and results are not recorded); the
api GETs as client spans carrying `traceparent` so the trace continues in `tt-api`; slog → OTel logs. The SDK writes its
diagnostics (`TROOPER_DEBUG`) to **stderr only** — `cmd/tt-mcp/main_test.go` runs the stdio session with self
telemetry on against a fake collector and asserts stdout stays pure JSON-RPC.

## Development

```bash
go test ./...                                     # units + in-memory e2e + stdio purity
docker build -f services/mcp/Dockerfile -t tasktrooper/mcp .   # repo root; distroless, no ports
```

Layout: `cmd/tt-mcp` (binary, stdout guard) · `internal/config` (TT_* load + validate) ·
`internal/apiclient` (the one HTTP client, error mapping, result shaping) · `internal/redact` (PII) ·
`internal/server` (one file per tool group). No DB, no cache: the module only calls `services/api`.
