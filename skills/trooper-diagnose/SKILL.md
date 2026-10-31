---
name: trooper-diagnose
description: Root-cause workflow for a production problem using the TaskTrooper MCP tools (tt_*). Use when the user says something is wrong, broken, slow, erroring, down, alerting or regressed in a service, endpoint, consumer or app ("something looks wrong with orders-api", "why is checkout slow", "what caused this incident", "did the last deploy break X"). Not for writing code or changing alert rules; the tools are read-only.
---

# trooper-diagnose

Find why a service/endpoint is unhealthy, using evidence from the `tt_*` MCP tools. The tools are
read-only and scoped to one project by the API key. Background on the tools: the `trooper-query` skill.

## Rules (apply to every step)

- **Cite or don't claim.** Every statement about a cause needs an id, value or timestamp from tool
  output (incident id, endpoint id, trace id, log ts, deploy version). No citation, no claim.
- **Truncated results.** If a result has `truncated: true`, read its `hint` and narrow the query
  (service, endpoint, `errorsOnly`, `minMs`, `level`, shorter `range`) and call again **before** concluding.
- **Never guess ids.** Service names come from `tt_list_services`, endpoint ids (slugs like
  `get-orders-id`, not routes) from `tt_list_endpoints`, trace ids from `tt_search_traces`, incident ids
  from `tt_list_incidents`. A 404 that says "call tt_list_endpoints" means the id was guessed.
- **Say "unknown"** when the evidence is thin or contradictory. A weak guess presented as a finding is worse.
- Timestamps are epoch **ms**. Convert to UTC when reporting.

## Workflow (in order; skip a step only when it cannot apply, and say so)

1. **Incidents.** `tt_list_incidents` with `{ service, state: "firing" }`; if empty, drop `state`
   and widen `range` (`6h`, `24h`). If the user did not name a service, call it without `service` and
   pick the most severe firing incident. If the service name may be wrong, check `tt_list_services`.
2. **Incident detail.** `tt_get_incident { id }` for the relevant incident(s). Note `startedAt`,
   `value`/`peakValue`, `endpoint`, and the `events` timeline. `rootCause` is unstructured JSON whose
   shape varies: treat it as a **hint** to verify in later steps, never as the conclusion.
3. **Service health.** `tt_get_service_kpis { service, range }`: error rate, latency, throughput,
   dependencies, version. Pick a `range` that covers the incident start (e.g. `6h` if it started 2h ago).
4. **Endpoints.** `tt_list_endpoints { service, range }`; rank by `errorRate` and `p95`. Take the endpoint
   `id` from this list, then `tt_endpoint_rca { service, endpoint: <id>, range, baseline }`
   (`baseline: "prev"` default; use `1d`/`7d` if the problem may be older than the window). Read
   `deltaAvgMs`, `ownMs` [current, baseline] and `children` (sorted by `deltaMsPerReq`): the top child is the
   dependency that got slower/errorier; a large `ownMs` delta means the endpoint's own code.
5. **Sample traces.** `tt_search_traces { service, endpoint, errorsOnly: true }` for errors, or
   `{ service, endpoint, sort: "slowest", minMs }` for latency. Keep `limit` small (5–10).
6. **One trace.** `tt_get_trace { traceId }` on 1–2 representative traces; find the failing or slow span,
   its dependency and attributes (status, `db.statement`, peer, exception).
7. **Logs.** `tt_search_logs { traceId }` first. Then the window: `{ service, level: "warn", range }`,
   optionally `q` with a term from the span/exception. To look further back, page with `before: <nextBefore>`.
8. **Deploys.** `tt_list_deploys { service, range }` (and for the dependency's service if step 4/6 points
   at one). A deploy shortly before `startedAt` or the RCA window is a correlation, not proof: say so.

Optional: `tt_list_monitors { service }` to see what else is configured/alerting;
`tt_list_loadtest_runs` if a load test may have caused the traffic.

## Output format

```
Summary: <one or two sentences: what is wrong, since when, impact>

Evidence:
- <fact> (<tool>: <id / value / ts>)
- ...

Most likely cause: <cause, or "unknown">
Confidence: high | medium | low — <why: how many independent signals agree>
Next step: <one concrete action: what to check in code/infra, or which query to run next>
```

Confidence guide: **high** = RCA attribution, traces and logs/deploy agree; **medium** = two signals
agree; **low** = one signal or only `rootCause`. If nothing points anywhere, cause is "unknown" and the
next step names the missing evidence.
