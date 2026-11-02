# MCP server (`services/mcp`, ADR-025)

Model Context Protocol server for local coding agents (OpenCode, Claude Code, Cursor …): **read-only** by default, approved write tools with `TT_MCP_ALLOW_WRITE` (ADR-032).
Wraps `services/api` (`openapi.yaml`); does **not touch** ClickHouse/Postgres. ADR-025, contract: contracts.md §26.

## Architecture

- New Go module `services/mcp` (`cmd/tt-mcp`), added to `services/go.work` → the CI module matrix expands automatically (ADR-026).
- Official Go MCP SDK (`github.com/modelcontextprotocol/go-sdk`); transport **stdio**. The server is started by the
  agent as a process, tool call = JSON-RPC over stdin/stdout.
- Extension target (not designed in v1): streamable-HTTP transport → remote agents connecting to a shared
  server. v1 is limited to stdio; the tool surface is designed independently of the transport.
- API client: `net/http`, a single `apiBaseURL` + `Authorization: Bearer <TT_API_KEY>`; all tenant
  filtering (org/project) in the API comes from the key itself (ADR-016, identity.go: api key → `(org, project)`,
  role `admin`).
- In-process use (ADR-033): the exported package `toolset` builds the same server for the api's dashboard assistant
  (`.ai/assistant.md`) and connects it with `mcp.NewInMemoryTransports`; `apiclient.WithAuth` replaces the bearer key with the
  user's session headers and the api serves the calls in process. Keep tool behaviour transport and caller independent.

## Configuration (`services/pkg/config` with TT_* env)

| Env | Required | Meaning |
|---|---|---|
| `TT_API_URL` | ✓ | API base (default `http://localhost:8080`) — same name as the collector |
| `TT_API_KEY` | ✓ | Project-scoped api key (no seed key, ADR-028: dashboard Settings → API keys or `POST /api/v1/api-keys`) |
| `TT_ORG_ID`, `TT_PROJECT_ID` | | Verified at startup via `GET /me`; on mismatch the server **does not start** (reading the wrong project with the wrong key). With API key auth the tenant comes from the key, not from a request header |
| `TT_MCP_ALLOW_WRITE` | | `true` → 11 write tools are registered (default `false`, ADR-032). The API key already has the `admin` role: this flag is the only gate for the agent being able to write |
| `TT_LOG_LEVEL` | | `logx` (JSON slog) |
| `TT_MCP_ENV_FILE` | | Env file to read (otherwise the default search order, see Install § Env file) |

**Key secrecy:** `TT_API_KEY` is only written to the outbound `Authorization` header. It **never** goes into a tool
definition, tool output, error message or log; the logx middleware does not log the `Authorization` header.

## Scope: read-only by default, opt-in write (ADR-032)

The default install is **read-only**: 27 tools, all GET (the only exception is `tt_preview_monitor`: POST but saves nothing).
With `TT_MCP_ALLOW_WRITE=true`, 11 more write tools are registered (38 total). Rules for write tools:

- **Approval:** every write tool takes a `confirm` parameter. If `confirm` is missing / `false`, nothing is written to the API; the result is
  `{dryRun: true, wouldSend: {method, path, body}, next}` — the agent shows this to the user, and once the user approves it repeats
  the same call with `confirm: true`. The only exception is `tt_stop_loadtest` (it only reduces load, emergency stop).
- **Source:** monitors, KPIs and load test scenarios created by the agent carry `source: "agent"` (migration
  `014_agent_source.sql`); "Agent" badge in the UI. A `source` supplied by the user is ignored.
- **Ownership:** `tt_delete_monitor` / `tt_delete_kpi` only delete `source = agent` records; others are rejected
  (the user deletes them from the UI or the agent mutes them). Monitors/KPIs managed from the repo (`source = code`) are not updated.
- **Load test:** `tt_run_loadtest` generates real traffic; it is rejected if the scenario is `draft`, and the target must be a verified domain
  (adding/verifying domains is up to the user). If a scenario the agent created has no `gate`, it adds
  `{abortErrorRate: 0.5, abortP95Ms: 10000}`.
- Still not wrapped: incident ack/resolve, triggering repo analysis, error status changes, adding channels/integrations,
  adding domains, API key operations.

## Tool surface (27 read + 11 write)

Names are prefixed with `tt_`; definitions are written so the LLM can **choose** a tool (when to call it, when not to).
Tools that take a time window accept `{ range?, env? }` (default `1h`, enum `15m|30m|1h|6h|24h|7d` — contracts §8);
if `env` is not given, all environments. Result shapes are the schemas in contracts.md (one-to-one passthrough from the api).

| # | Tool | Wrapped endpoint (operationId) |
|---|---|---|
| 1 | `tt_list_services` | `GET /api/v1/services` (`getServices`) |
| 2 | `tt_get_service_kpis` | `GET /api/v1/services/{service}` (`getServicesService`) |
| 3 | `tt_list_endpoints` | `GET /api/v1/services/{service}/endpoints` (`getServicesServiceEndpoints`) |
| 4 | `tt_endpoint_rca` | `GET /api/v1/services/{service}/endpoints/{endpoint}/rca` (`getServicesServiceEndpointsEndpointRca`) |
| 5 | `tt_search_traces` | `GET /api/v1/traces` (`getTraces`) |
| 6 | `tt_get_trace` | `GET /api/v1/traces/{traceId}` (`getTracesTraceId`) |
| 7 | `tt_search_logs` | `GET /api/v1/logs` (`getLogs`) |
| 8 | `tt_list_monitors` | `GET /api/v1/monitors` (`getMonitors`) |
| 9 | `tt_list_incidents` | `GET /api/v1/incidents` (`getIncidents`) |
| 10 | `tt_get_incident` | `GET /api/v1/incidents/{id}` (`getIncidentsId`) + `GET /api/v1/incidents/{id}/events` (`getIncidentsIdEvents`) |
| 11 | `tt_list_loadtest_runs` | `GET /api/v1/loadtests/runs` (`getLoadtestsRuns`) |
| 12 | `tt_list_deploys` | `GET /api/v1/deploys` (`getDeploys`) |
| 13 | `tt_list_errors` | `GET /api/v1/errors` (`getErrors`) — messages pass through `redact` |
| 14 | `tt_get_error` | `GET /api/v1/errors/{fingerprint}` (`getErrorsFingerprint`) |
| 15 | `tt_list_kpis` | `GET /api/v1/kpis` (`getKpis`) |
| 16 | `tt_list_alert_suggestions` | `GET /api/v1/suggestions` (`getSuggestions`) |
| 17 | `tt_list_inventory` | `GET /api/v1/inventory` (`getInventory`) |
| 18 | `tt_list_monitor_templates` | `GET /api/v1/monitor-templates` |
| 19 | `tt_list_notify_channels` | `GET /api/v1/integrations/slack` + `/integrations/slack/channels` (`refresh`) + `GET /api/v1/notification-channels` (name + type only; never config) |
| 20 | `tt_get_app_summary` | `GET /api/v1/apps/{app}/summary` |
| 21 | `tt_get_ai_overview` | `GET /api/v1/ai/overview` |
| 22 | `tt_list_hosts` | `GET /api/v1/infra/hosts` |
| 23 | `tt_list_databases` | `GET /api/v1/infra/databases` |
| 24 | `tt_list_consumer_groups` | `GET /api/v1/infra/groups` |
| 25 | `tt_preview_monitor` | `POST /api/v1/monitors/preview` (does not save; series trimmed to the last 60 buckets) |
| 26 | `tt_list_loadtest_scenarios` | `GET /api/v1/loadtests/scenarios` (spec stripped) + `GET /api/v1/loadtests/domains` |
| 27 | `tt_get_loadtest_run` | `GET /api/v1/loadtests/runs/{id}` |

**Write tools** (`TT_MCP_ALLOW_WRITE=true`; all with `confirm`, see Scope):

| Tool | Wrapped endpoint | Note |
|---|---|---|
| `tt_create_monitor` | `POST /api/v1/monitors` | `source: agent`; `channels` required (empty list = incident only) |
| `tt_update_monitor` | `GET` + `PATCH /api/v1/monitors/{id}` | name, severity, channels, status, condition, mutedUntil; `source = code` rejected |
| `tt_delete_monitor` | `GET` + `DELETE /api/v1/monitors/{id}` | `source = agent` only |
| `tt_accept_suggestion` | `POST /api/v1/suggestions/{id}/accept` | threshold / severity / channels override |
| `tt_create_kpi` | `POST /api/v1/kpis` | `source: agent`; endpoint `ref` = `"METHOD /route"`, `good.thresholdMs` required for a latency SLI |
| `tt_update_kpi` | `GET` + `PUT /api/v1/kpis/{id}` | full KPI; existing `source` is preserved, `code` rejected |
| `tt_delete_kpi` | `GET` + `DELETE /api/v1/kpis/{id}` | `source = agent` only |
| `tt_send_slack_test` | `POST /api/v1/integrations/slack/test` | reduces a `slack:<id>#name` target to the id; a 502 message (bot not invited etc.) is passed to the agent |
| `tt_create_loadtest_scenario` | `POST /api/v1/loadtests/scenarios` | `source: agent`, `state: ready`; `target` http(s) URL, `steps` + `levels` (or `script`) required |
| `tt_run_loadtest` | `GET` scenario + `POST /api/v1/loadtests/scenarios/{id}/runs` | dry run shows target / peak VUs / duration; draft rejected |
| `tt_stop_loadtest` | `POST /api/v1/loadtests/runs/{id}/stop` | no confirm |

Write errors: 400/409 → the API's `error` message (≤ 300 characters) is passed through ("the API rejected the input (HTTP 400): …");
501/502 → the API's readable reason (Slack App not configured, Slack rejected the message); a 5xx body is never passed through; timeout
→ "may or may not have been applied: check with a list call before retrying".

**Tool details** (v1 tools; name → LLM definition → input → result):

1. **`tt_list_services`** — "List services in the project with throughput, error rate and latency
   summary; start here to discover service names." Input: `{ range?, env?, type? }` (`type` = `http|worker|web|mobile`,
   comma list). Result: `Service[]` (contracts §8; RED summary + sparkline).
2. **`tt_get_service_kpis`** — "Health, RED metrics, dependencies and version info of ONE service;
   use after tt_list_services, not for discovery." Input: `{ service: string, range?, env? }`.
   Result: `ServiceDetail` (+ `hasAi`).
3. **`tt_list_endpoints`** — "Endpoints (routes, consumers) of one service with error rate and p95.
   Returns each endpoint's id (slug, for tt_endpoint_rca) and method + route ("POST /v1/charges", for the
   endpoint filter of tt_search_traces, tt_list_incidents, tt_list_monitors)." Input: `{ service: string, range?, env? }`.
   Result: `Endpoint[]` (id = slug, e.g. `get-orders-id`; `method` + `route`).
4. **`tt_endpoint_rca`** — "Explain WHY one endpoint got slower or errorier: per-dependency delta vs a
   baseline window (root-cause attribution). Requires an endpoint id from tt_list_endpoints." Input:
   `{ service, endpoint, range?, baseline?: prev|1d|7d }` (default `prev`). Result: `EndpointRca` (ADR-009).
5. **`tt_search_traces`** — "Find recent traces by service/endpoint/duration/error; returns trace ids
   for tt_get_trace." Input: `{ range?, env?, service?, endpoint? ("METHOD /route" or process name — not a slug),
   errorsOnly?: bool, minMs?: number,
   sort?: string, limit? }` (limit default 20, max 100). Result: `TraceSummary[]`.
6. **`tt_get_trace`** — "Full span tree of ONE trace (waterfall, attributes, dependencies). Use a trace
   id from tt_search_traces or an error issue." Input: `{ traceId: string }` (32 hex; format is validated).
   Result: `Trace` (if span count > 200 the span list is truncated + `truncated`/`total`/`hint`, rule 3).
7. **`tt_search_logs`** — "Search log lines (newest first) by query text, level, service, route or trace
   id; cursor-paginate with `before`/`nextBefore`." Input: `{ range?, env?, service?, level?, q?, traceId?,
   route?, before?, limit? }` (limit default 50, max 200). Result: `LogPage { lines, nextBefore, ... }`.
8. **`tt_list_monitors`** — "Alert rules (monitors) with kind, query and state; use to find what is
   alerting or what is configured for a service." Input: `{ service?, endpoint? ("METHOD /route"), kind?, clusterId?, group? }`.
   Result: `Monitor[]`.
9. **`tt_list_incidents`** — "Open or recent incidents with severity, state (pending, firing, acknowledged,
   resolved) and current value; start here for 'what is broken right now'. Omit state first: pending incidents
   are often the live problem." Input: `{ state?, service?, endpoint? ("METHOD /route"), monitorId?, kpiId?, since?, limit? }`
   (limit default 20, max 100). Result: `Incident[]`.
10. **`tt_get_incident`** — "Everything about ONE incident: alert detail, root cause and timeline.
    rootCause is unstructured and its shape may vary; treat it as opaque JSON." Input: `{ id: string }`.
    Result: `{ incident: Incident (+ rootCause), events: IncidentEvent[] }` (rootCause has no schema —
    see "Known flags").
11. **`tt_list_loadtest_runs`** — "Load test runs with state, thresholds and summary; use to check a
    scenario's latest result." Input: `{ scenarioId? }`. Result: `LoadTestRun[]`.
12. **`tt_list_deploys`** — "Deploy markers (newest versions first seen, Vercel/CI markers); use to
    correlate an incident or latency change with a deployment." Input: `{ range?, env?, service?, source? }`.
    Result: `DeployMarker[]`.

**Still out of scope:** profiling (`/profiles/*`), session replay, `POST /api/v1/ai/incident-summary` (LLM quota).

## Overlap with code intelligence

`services/codeintel` (:8096) is **only a webhook + analysis** service; it has no query API. Its data
(inventory, suggestions, repo status) is already read from `services/api` endpoints (`GET /repos`, `GET /inventory`,
`GET /suggestions`). Rule: MCP tools do **not** connect to codeintel **directly**; inventory / suggestion tools read through the api. The only write is
`tt_accept_suggestion` (ADR-032, approved); `POST /repos/{id}/analyze` and `suggestions/{id}/reject` are not wrapped.

## Cross-cutting rules (for every tool)

1. **Tenant (AGENTS.md rule 7):** org/project comes from the key; MCP additionally enforces the
   `TT_ORG_ID`/`TT_PROJECT_ID` ↔ `/me` match at startup. No tool takes an org/project parameter.
2. **Input validation:** enums (`range`, `baseline`, `type`, `level`), hex trace id, numeric `limit`/`minMs`
   caps; invalid input → tool error (without calling the API).
3. **Result cap:** tool output is at most **64 KB** JSON (a trace > 200 spans is also truncated separately). If exceeded, the array
   is truncated and `truncated: true`, the total of the truncated source as `total`, and a `hint` on how the agent can narrow
   the query are added to the result. `limit` is clamped in lists: trace ≤ 100 (default 20), log ≤ 200 (default 50),
   incident ≤ 100 (default 20); logs are paginated with the `nextBefore` cursor.
4. **PII:** MCP collects no extra data; no tool collects headers/bodies. Returned log messages and trace attribute
   values are masked with `internal/redact` (email, IPv4/IPv6, keys starting with `tt_api_`/`tt_ingest_`/`tt_dev_` (old dev keys)
   — tool names (`tt_list_*`) are intentionally not matched — the `Authorization: Bearer`
   value, 16-digit card-like numbers). The `q` filter and headers are not written to logs. The masks do **not conflict**
   with the SDK masking values in the dashboard: MCP does not collect data the API does not collect, it only masks on the incoming
   surface.
5. **Clean errors:** a tool error is short and action-oriented; the upstream body, stack trace,
   `Authorization` value and internal URL are never carried. 401/403 → "check TT_API_KEY", 404 (in tools that require a valid resource
   id) → "call tt_list_endpoints for valid endpoint ids", 5xx/timeout → API
   unavailable. The `/me` check at startup (config) may show at most 512 characters of body — this is
   written to stderr, it is not tool output.

## Install (one command)

```bash
curl -fsSL https://telemetry.tasktrooper.ai/install-agent.sh | bash
```

Script `apps/web/public/install-agent.sh` (served as `/install-agent.sh` by the web build; bash 3.2 compatible):

1. Finds the coding tools installed on the machine (command, home directory, `/Applications/*.app`) and asks which ones to install into.
2. Asks for the scope: **global** (user configs) or **project** (git root).
3. Takes the API URL (default `https://telemetry-api.tasktrooper.ai`) and api key; the key is read with a hidden prompt (or from the `TT_API_KEY` env),
   verified against `GET /api/v1/me` with `curl -H @-` (never goes into argv). Then asks whether to enable write tools (y/N).
4. Downloads the `tt-mcp` binary from the latest `mcp/v*` GitHub Release, verifies it with `checksums.txt`, puts it in `~/.local/bin`.
   If there is no release it builds from source with Go (`git clone` + `go build`); `--source DIR` builds from a local checkout.
5. Writes the env file (umask 077, mode 600): global `~/.config/tasktrooper/mcp.env`, project `<root>/.tasktrooper/mcp.env`
   (`.tasktrooper/` is added to `.gitignore`). The key is **not written** to tool configs, it lives only in this file.
6. Copies the skills (`trooper-diagnose`, `trooper-query`, `trooper-alerts`), adds the MCP server to each tool's config
   under the name `tasktrooper`. JSON merge is done with python3 / node, a `.tasktrooper.bak` backup is taken on the first change;
   for a file that cannot be parsed, such as JSONC, the script does not touch the file and prints a snippet to add manually.

Flags: `--tools claude,cursor|all`, `--scope global|project`, `--project-dir`, `--api-url`, `--write|--read-only`,
`--bin-dir`, `--source`, `-y` (no prompts). Supported tools and where they are written:

| Tool | MCP (global / project) | Skills (global / project) |
|---|---|---|
| Claude Code | `claude mcp add --scope user` / `.mcp.json` | `~/.claude/skills` (symlink) / `.claude/skills` (symlink) |
| Cursor | `~/.cursor/mcp.json` / `.cursor/mcp.json` | `~/.agents/skills` / `.agents/skills` |
| Antigravity | `~/.gemini/config/mcp_config.json` (+ legacy `~/.gemini/antigravity/`) / `.agents/mcp_config.json` | `~/.gemini/config/skills` (symlink) / `.agents/skills` |
| Codex | `codex mcp add` or `~/.codex/config.toml` / `.codex/config.toml` | `~/.agents/skills` / `.agents/skills` |
| OpenCode | `~/.config/opencode/opencode.json(c)` / `opencode.json` | `~/.agents/skills` / `.agents/skills` |
| Gemini CLI | `gemini mcp add -s user` or `~/.gemini/settings.json` / `.gemini/settings.json` | `~/.agents/skills` / `.agents/skills` |
| Muse Code (Meta) | `~/.config/muse/settings.json` (`schema_version: 1`, `mcp_servers`) — no project config, always global | `~/.agents/skills` |

Global configs use the binary's absolute path; project configs write it by name if `tt-mcp` is on PATH (portable to the repo).
**Known gap:** Muse Code's MCP key (`mcp_servers` vs `mcpServers`) is contradictory across sources, not verified.

**Env file** (`internal/config/envfile.go`): `tt-mcp` reads the first file it finds at startup; the order is
`$TT_MCP_ENV_FILE` → `.tasktrooper/mcp.env` from cwd up to the git root → `$XDG_CONFIG_HOME/tasktrooper/mcp.env`
(default `~/.config/tasktrooper/mcp.env`). Only `TT_API_URL`, `TT_API_KEY`, `TT_MCP_ALLOW_WRITE`, `TT_ORG_ID`,
`TT_PROJECT_ID`, `TT_LOG_LEVEL` are read; a variable already set in the environment wins. If any of the `077` mode bits is set, a
warning is logged (`chmod 600`). If `TT_MCP_ENV_FILE` is given and the file does not exist, the server does not start; missing default files are fine.
Project scope relies on the tool starting the MCP server in the project folder.

**Release:** `.github/workflows/release-mcp.yml`, on an `mcp/vX.Y.Z` tag, puts darwin/linux (arm64, amd64) + windows/amd64
binaries into a GitHub Release as `tt-mcp_<os>_<arch>.tar.gz` with `checksums.txt`
(`git tag mcp/v0.1.0 && git push origin mcp/v0.1.0`). Because the Go module path does not match the repo and there is a `replace`,
`go install github.com/...` does not work; distribution is via binary.

## Connecting (manual)

Binary: download from a release or `cd services/mcp && go build -o ~/.local/bin/tt-mcp ./cmd/tt-mcp` (`~/.local/bin` must be on PATH;
otherwise write the absolute path in the `command` below). The API must be up (compose `--profile app` or run the services locally) and there must be a project + api key: onboarding
(UI "create org + project", contracts §11) or `api bootstrap --org <name> --project <name> --owner <email>`, then
dashboard Settings → API keys. For the platform's own data, `make self` (ADR-027/028). `tt-mcp`
calls `GET /me` at startup and does not start if the API is down. Env variables can also go into the env file instead of the config
(see Install § Env file); then only `command` remains in the config.

**OpenCode** (`~/.config/opencode/opencode.json` or project `opencode.json`):

```json
{
  "mcp": {
    "tasktrooper": {
      "type": "local",
      "command": ["tt-mcp"],
      "environment": {
        "TT_API_URL": "http://localhost:8080",
        "TT_API_KEY": "<api-key>",
        "TT_ORG_ID": "<org-id>",
        "TT_PROJECT_ID": "<project-id>"
      },
      "enabled": true
    }
  }
}
```

**Claude Code** (CLI):

```bash
claude mcp add tasktrooper \
  -e TT_API_URL=http://localhost:8080 -e TT_API_KEY=<api-key> \
  -e TT_ORG_ID=<org-id> -e TT_PROJECT_ID=<project-id> \
  -- tt-mcp
```

To have it define alerts / KPIs / load tests, write is enabled (ADR-032; the agent still asks for approval on every change). The demo API is
`https://telemetry-api.tasktrooper.ai`; the key comes from dashboard Settings → API keys, the user pastes it themselves:

```bash
claude mcp add tasktrooper \
  -e TT_API_URL=https://telemetry-api.tasktrooper.ai -e TT_API_KEY=<api-key> \
  -e TT_MCP_ALLOW_WRITE=true \
  -- tt-mcp
```

or `.mcp.json` in the project root:

```json
{
  "mcpServers": {
    "tasktrooper": {
      "command": "tt-mcp",
      "env": {
        "TT_API_URL": "http://localhost:8080",
        "TT_API_KEY": "<api-key>",
        "TT_ORG_ID": "<org-id>",
        "TT_PROJECT_ID": "<project-id>"
      }
    }
  }
}
```

## Agent Skills (`skills/`)

Agent Skills that describe **in which order** to use the tools (`skills/<name>/SKILL.md`, frontmatter `name` +
`description`; the description tells the agent **when** to load the skill). The canonical location is `skills/` at the repo
root; skills are edited only there.

| Skill | When |
|---|---|
| [`trooper-diagnose`](../skills/trooper-diagnose/SKILL.md) | "Something is wrong with X, why?" — incident → KPI → endpoint RCA → trace → log → deploy; no cause without evidence, "unknown" on weak evidence |
| [`trooper-query`](../skills/trooper-query/SKILL.md) | Telemetry question; scope (`range`/`service`/`limit`), log pagination (`before`/`nextBefore`), reading `truncated` |
| [`trooper-alerts`](../skills/trooper-alerts/SKILL.md) | "Set up alerts / KPIs for this project" — inventory + existing monitors/KPIs/suggestions → thresholds from data → `tt_preview_monitor` → approval as a list → `confirm: true`; if data is insufficient, suggests a load test (scenario + run with approval) |
| [`trooper-contribute`](../skills/trooper-contribute/SKILL.md) | Code changes in this repo; checklist pointing to AGENTS.md + `.ai/` rules |

`trooper-diagnose`, `trooper-query` and `trooper-alerts` require the `tasktrooper` MCP server registered via § Install (or § Connecting) above. `trooper-alerts`
needs `TT_MCP_ALLOW_WRITE=true` for its write steps; if it is off, it gives the suggestions as a list and the user creates them from the UI.

**Discovery:** the single link is the committed `.claude/skills -> ../skills` symlink, no copies.
- Claude Code reads project skills from `.claude/skills/<name>/SKILL.md`.
- OpenCode (2.0.x), besides `.opencode/skill(s)/`, also scans `.claude/skills/` and `.agents/skills/` for compatibility
  (upward from cwd, symlinks are followed). Scanning is asynchronous: if the background service is running, a file change
  shows up after a few seconds.
- **Do not put non-skill `.md` files under `skills/`:** OpenCode scans with the `{*.md,**/SKILL.md}` pattern; e.g. a
  `skills/README.md` is listed as a skill named "README". That is why the install note is here, not in `skills/`.
- Checkout without symlinks (`core.symlinks=false`): for OpenCode `opencode.json` → `"skills": ["./skills"]`; for Claude
  Code `rm .claude/skills && cp -r skills .claude/skills` (the copy is not committed).
- Check: `claude -p "list skills starting with trooper-"`; in OpenCode the `/skills` command or
  `opencode api skill.list --param "location[directory]=$PWD"`.

Skill text uses tool names verbatim (`tt_*`); if a tool is added/renamed, the skills are updated in the same
PR.

## Known flags / gaps

- `POST /api/v1/ai/incident-summary` (`postAIIncidentSummary`) **just** landed on main (0764ad4, "offline mock
  integrations"); the default provider is **mock** and it is a paid model call. Not wrapped in v1 (POST + LLM quota);
  a v2 candidate once the real provider is verified.
- `Incident.rootCause` is `rootCause: {}` in openapi — **schema undocumented**, passthrough opaque JSON.
- The `endpoint` path param is not a route but a **slug id** (`get-orders-id`); agents must call `tt_list_endpoints`
  first (the tool definition says so).
- **Two different endpoint identities** (verified with demo data, 2026-10-31): the `endpoint` of `tt_endpoint_rca`
  (path param, `ResolveEndpoint`) is a **slug**; whereas the `endpoint` filter of `tt_search_traces`, `tt_list_incidents`, `tt_list_monitors`
  (query param, api `refParam` / `i.endpoint = $?`) is **`"METHOD /route"`** (process
  name for a consumer). A slug in these filters returns an **empty list**, not an error — the agent thinks "no events". The tool input definitions
  say this explicitly; `Incident.endpoint` is also in route form (`POST /v1/orders`). If the API converges on a single form, the definitions are updated.
- **Validation ↔ API mismatches** (verified with the demo, 2026-10-31; the code fix is open work, for now the tool definitions
  point to the values that work): `tt_list_incidents.state` is validated in MCP as `firing|resolved|acked`, while the API
  expects `open|pending|firing|acknowledged|resolved` → `acked` returns 400 from the API, `pending`/`open`/`acknowledged`
  are rejected in MCP. `tt_search_logs.level` is an **exact match** in the API (accepts a comma list `warn,error`), not a
  "minimum"; MCP accepts a single value (a comma list is rejected).
- Auth today: `Authorization: Bearer <api key>` (dev flow). If we move to the OIDC session flow, the key stays for MCP;
  the `X-Trooper-Project` header is only meaningful in the cookie flow.
