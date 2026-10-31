# mcp

Read-only MCP server for local coding agents (Claude Code, OpenCode, …) — ADR-025, design in
[`.ai/mcp.md`](../../.ai/mcp.md), tool contract in [`.ai/contracts.md`](../../.ai/contracts.md) §26.
Module `github.com/tasktrooper/telemetry/services/mcp`, binary `cmd/tt-mcp`, **no port**: it speaks MCP
over stdin/stdout (stdio transport), spawned by the agent.

**The stdout guarantee:** stdout carries nothing but newline-delimited JSON-RPC. All logging goes to
stderr — do not import `services/pkg/logx` here (it logs to `os.Stdout`) and do not add `fmt.Print*`.
`cmd/tt-mcp/main_test.go` builds the real binary and fails if a single stdout line is not a JSON-RPC
message; keep it green.

## Surface

12 `tt_*` tools, every one a GET over `services/api` with the shared key-derived tenant (no org/project
tool params, no write tools):

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

## Cross-cutting rules

- **Validation before the API:** `range` (15m…7d), `baseline` (prev/1d/7d), `type`
  (http/worker/web/mobile), `level`, `state`, `sort`, `minMs >= 0`, 32-hex trace ids, `limit` clamped
  (trace ≤ 100 default 20, log ≤ 200 default 50, incident ≤ 100 default 20).
- **Result cap:** 64 KB JSON; a trimmed result carries `truncated: true`, `total` (original count) and a
  `hint` on how to narrow the query. Trace span lists additionally cap at 200.
- **PII:** log messages, span attrs/resource/events and incident event data pass through
  `internal/redact` (the single masking helper of this module): email, IPv4/IPv6, `Bearer …`,
  `tt_api_`/`tt_ingest_`/`tt_dev_` keys, 16-digit card-like numbers. Tool names (`tt_list_*`) are
  deliberately not matched.
- **Errors:** short and actionable, never a stack, secret or raw upstream body — `check TT_API_KEY`
  (401/403), `call tt_list_endpoints for valid endpoint ids` (404), API unavailable (5xx/timeout).
- **Startup:** `Check` calls `GET /me` once — a bad key or unreachable API fails fast before any tool
  call; `TT_ORG_ID`/`TT_PROJECT_ID` are cross-checked when set. The key is only ever on the wire.

## Config

| Env | Default |
|---|---|
| `TT_API_KEY` | **required** (dev: `tt_dev_api_key`) |
| `TT_API_URL` | `http://localhost:8080` |
| `TT_ORG_ID` / `TT_PROJECT_ID` | unset (optional cross-checks) |
| `TT_LOG_LEVEL` | `info` (stderr JSON) |

Connection recipes (OpenCode, Claude Code, `.mcp.json`): `.ai/mcp.md` § Bağlanma.

## Development

```bash
go test ./...                                     # units + in-memory e2e + stdio purity
docker build -f mcp/Dockerfile -t tasktrooper/mcp services/   # distroless, no ports
```

Layout: `cmd/tt-mcp` (binary, stdout guard) · `internal/config` (TT_* load + validate) ·
`internal/apiclient` (the one HTTP client, error mapping, result shaping) · `internal/redact` (PII) ·
`internal/server` (one file per tool group). No DB, no cache: the module only calls `services/api`.
