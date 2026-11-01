# Demo: diagnose a slow endpoint from a coding agent

End-to-end rehearsal of `tt-mcp` + the `trooper-diagnose` skill against the live demo world. About 30 minutes of
wall-clock, mostly waiting for the incident. Setup details: [README § Quickstart](../README.md#quickstart).

## 1. Platform, data and the incident (terminal 1)

```bash
make infra migrate
make demo DEMO_ARGS="--scenario payments-provider-errors --incident-after 15m"
```

The demo starts healthy, so the RCA gets a baseline, and switches the scenario on after 15 minutes.
`payments-provider-errors` makes the fake iyzico provider slower, with 502s and timeouts
([tools/demo § Scenarios](../tools/demo/README.md#scenarios-default-all-on)). Monitor *Charges p95 latency*
(`p95 > 600 ms for 10m`) goes **pending** 1–2 minutes after the switch and **firing** about 10 minutes later.

Ready check, before you start talking:

```bash
curl -s -H 'Authorization: Bearer tt_dev_api_key' 'localhost:8080/api/v1/incidents?service=payments-api' \
  | jq -c '.[] | {id, state, endpoint, title, value}'
# {"id":"…","state":"firing","endpoint":"POST /v1/charges","title":"Charges p95 latency","value":"944.4"}
```

## 2. Agent (terminal 2, repo root)

```bash
(cd services/mcp && go install ./cmd/tt-mcp)
claude mcp add tasktrooper \
  -e TT_API_URL=http://localhost:8080 -e TT_API_KEY=tt_dev_api_key \
  -e TT_ORG_ID=1 -e TT_PROJECT_ID=1 \
  -- tt-mcp
claude mcp list        # tasktrooper: tt-mcp - ✔ Connected  (ENOENT → $(go env GOPATH)/bin is not on PATH)
claude
```

Prompt:

> Something looks wrong with payments-api in the last 15 minutes. Find out why. Use only the TaskTrooper tools.

Keep "Use only the TaskTrooper tools". Without it, the agent may open `tools/demo` in this repo and quote the
simulator as the cause.

## 3. What to expect

Tool calls from a rehearsal (Claude Code, about 20 turns and 50 s; the order can vary slightly):

| # | Call | Shows |
|---|---|---|
| 1 | `Skill trooper-diagnose` | the skill loads from `.claude/skills` |
| 2 | `tt_list_incidents {service: payments-api, range: 24h}` | incident *Charges p95 latency* on `POST /v1/charges` |
| 3 | `tt_get_service_kpis`, `tt_list_endpoints` | `post-v1-charges` has the worst p95 (~880 ms) |
| 4 | `tt_get_incident {id}` | pending → firing timeline, `rootCause` hint |
| 5 | `tt_endpoint_rca {endpoint: post-v1-charges}` | ~96 % of the latency is the `iyzico` dependency, ~0 ms own code |
| 6 | `tt_search_traces {endpoint: "POST /v1/charges", sort: slowest}` then `errorsOnly` | slow/failed trace ids |
| 7 | `tt_get_trace` ×1–2 | one ~2 s trace: ~2,077 ms in `POST /payment/auth` (iyzico), DB ~7 ms |
| 8 | `tt_search_logs {level: error}`, then `{level: warn}` | `charge failed: provider error status=502 provider=iyzico` |
| 9 | `tt_list_deploys` | no deploy near the start: not a release |

The answer follows the skill's format:

```
Summary: POST /v1/charges on payments-api is slow because its payment provider iyzico got slower …
Evidence: incident <id> (p95 944 ms > 600 ms) · RCA: iyzico ≈ 96 % of time · trace <id> 2,077 ms in iyzico ·
          log "charge failed: provider error status=502 provider=iyzico" · no deploy before the start
Most likely cause: iyzico provider latency and errors
Confidence: high — RCA, traces and logs agree
Next step: check the provider's status; review the 6 s client timeout / fallback
```

Side note the agent may raise, which is correct: Stripe webhook errors (`charge not found`) on
`POST /v1/webhooks/stripe` are a separate, always-on stream, unrelated to the alert.

## Reset

Ctrl-C in terminal 1 stops the demo and removes its `ttdemo-*` containers. Running `make demo` again starts it
fresh. `make infra-down` stops the infra and keeps its volumes.
