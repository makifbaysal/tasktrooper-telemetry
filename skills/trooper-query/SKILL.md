---
name: trooper-query
description: How to query TaskTrooper telemetry efficiently with the tt_* MCP tools (services, endpoints, traces, logs, incidents, monitors, deploys, load test runs). Use when answering a question about production data from TaskTrooper, e.g. "which endpoints of payments-api are slowest", "show error logs for trace X", "what deployed in the last day", "is anything alerting". For "something is wrong, find out why" use trooper-diagnose instead.
---

# trooper-query

12 read-only tools over `services/api`, one project (from the API key). Full contract: `.ai/mcp.md`.

## Start here

1. `tt_list_services` → exact service names (+ RED summary). Every `service` argument must be one of
   these names; `type` filters `http,worker,web,mobile`.
2. Then go narrow: `tt_get_service_kpis` (one service), `tt_list_endpoints` (its endpoints + **ids**).
3. For "what is broken now": `tt_list_incidents { state: "firing" }` instead.

## Scoping

| Knob | Values | Notes |
|---|---|---|
| `range` | `15m 30m 1h 6h 24h 7d` | Relative to now; default `1h`. Must cover the event you look for |
| `env` | e.g. `production` | Omit = all environments |
| `service` | name from `tt_list_services` | On traces, logs, incidents, monitors, deploys |
| `endpoint` on `tt_endpoint_rca` | **`id` slug** from `tt_list_endpoints` (`post-v1-charges`) | Not the route |
| `endpoint` on traces, incidents, monitors | **`"METHOD /route"`** (`"POST /v1/charges"`) or consumer process name | Not the slug |
| `limit` | traces ≤100 (20), logs ≤200 (50), incidents/deploys/runs ≤100 (20) | Clamped server-side |
| `since` | epoch ms (incidents) | |
| `before` | epoch ms cursor (logs) | Pass the previous page's `nextBefore` |

Trace filters: `errorsOnly`, `minMs`, `sort: "slowest"`. Log filters: `level` (minimum:
`debug|info|warn|error`), `q` (full text), `traceId` (32 hex), `route` (template, e.g. `/orders/:id`).
RCA: `tt_endpoint_rca { service, endpoint, range, baseline: prev|1d|7d }`.

## Reading results

- **Truncation:** results are capped at 64 KB. A trimmed result has `truncated: true`, `total` (original
  count) and `hint`. Do what the hint says (narrow) — do not reason over a partial list as if complete.
  `tt_get_trace` also cuts at 200 spans.
- **Log paging:** lines are newest first. For older lines call again with `before: <nextBefore>`; stop
  when you have the window you need, not when pages run out.
- `tt_get_incident` returns `{ incident, events }`; `incident.rootCause` is unstructured — a hint only.
- Errors are short and actionable: "check TT_API_KEY" (auth), "call tt_list_endpoints for valid endpoint
  ids" (404 — you guessed an id), API unavailable (5xx). Input validation errors name the allowed values.
- Log messages and span attributes arrive with emails/IPs/keys masked; masked values are not bugs.

## Common mistakes

- Guessing an endpoint id from a route or URL → 404 on `tt_endpoint_rca`. Always `tt_list_endpoints` first.
- Passing the slug to `tt_search_traces`/`tt_list_incidents`/`tt_list_monitors` → silently **empty** list.
  Those filters take `"METHOD /route"`. Never read an empty filtered result as "no problem".
- Unfiltered `tt_search_logs` over `7d` → truncated noise. Add `service` + `level` + `q`/`traceId`.
- `tt_search_traces` with `limit: 100` to "see everything" → use `errorsOnly`/`minMs`/`sort` and limit 5–10.
- Calling `tt_get_service_kpis` to discover services (it needs a name; use `tt_list_services`).
- A `range` shorter than the age of the incident → empty results that look like "healthy".
- Expecting write actions: no tool acks incidents, creates monitors or starts load tests.

## Examples

**"Which endpoints of payments-api are slowest?"**
`tt_list_services { type: "http" }` → `tt_list_endpoints { service: "payments-api", range: "1h" }` → sort by `p95`.

**"Why did POST /v1/charges get slower since yesterday?"**
`tt_list_endpoints { service: "payments-api" }` (id for `POST /v1/charges` is `post-v1-charges`) →
`tt_endpoint_rca { service: "payments-api", endpoint: "post-v1-charges", range: "1h", baseline: "1d" }` → top
`children` → `tt_search_traces { service: "payments-api", endpoint: "POST /v1/charges", sort: "slowest", limit: 5 }`.

**"Show me the errors behind trace 4bf9…"**
`tt_get_trace { traceId }` → `tt_search_logs { traceId, level: "warn" }`.

**"Error logs for orders-api in the last 6h mentioning timeout"**
`tt_search_logs { service: "orders-api", level: "error", q: "timeout", range: "6h" }` → if more needed,
repeat with `before: <nextBefore>`.

**"Did anything deploy before the incident?"**
`tt_get_incident { id }` (note `startedAt`) → `tt_list_deploys { service, range: "24h" }` → compare `ts`.

**"Is anything alerting / what is configured for catalog-api?"**
`tt_list_incidents { state: "firing" }`; `tt_list_monitors { service: "catalog-api" }`.

**"Did the last checkout load test pass?"**
`tt_list_loadtest_runs { limit: 5 }` (or `{ scenarioId }`) → check state and thresholds.
