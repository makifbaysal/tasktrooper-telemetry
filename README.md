# TaskTrooper Telemetry

Observability platform of the TaskTrooper family: SDKs collect APM, RUM, logs and profiles from backends, workers,
web and mobile apps; the platform visualises them, alerts on them and runs load tests. It also reads the customer's
code to propose alerts and load-test scenarios.

- **Start here:** [`AGENTS.md`](AGENTS.md) — repo map, stack, working rules (for humans and agents).
- **Design docs:** [`.ai/`](.ai/README.md) (English).
- **Local stack:** `make infra migrate`, then the services (`docker compose -f deploy/docker-compose.yml --profile app up -d --build`).
  There is no seed or demo data (ADR-028): sign in on the dashboard (`cd apps/web && pnpm dev`, dev login), create an
  org + project in onboarding and connect an SDK. `make self` makes the platform report its own telemetry (ADR-027).

## Use TaskTrooper from your coding agent

`tt-mcp` ([`services/mcp`](services/mcp/README.md)) is a **read-only** MCP server for local coding agents (Claude Code,
OpenCode, …). It speaks stdio, wraps `services/api` with a project API key and exposes 12 `tt_*` tools: services,
endpoints, endpoint root-cause analysis, traces, logs, monitors, incidents, load-test runs and deploys. Three Agent
Skills in [`skills/`](skills/) teach the agent how to use them: `trooper-diagnose` ("something is wrong, find out
why"), `trooper-query` (answer a telemetry question efficiently) and `trooper-contribute` (rules for changing this repo).
Design, tool contract and known gaps: [`.ai/mcp.md`](.ai/mcp.md); skills setup: [`.ai/mcp.md` § Agent Skills](.ai/mcp.md#agent-skills-skills).

### Quickstart

Needs Go 1.26 and Docker. Run every command from the repo root.

**1. Platform with data.** `tt-mcp` calls `GET /api/v1/me` at startup and refuses to start without a reachable API.
There is no demo data (ADR-028); the quickest real data is the platform's own telemetry:

```bash
make infra migrate
docker compose -f deploy/docker-compose.yml --profile app up -d --build   # api on :8080, ingest on :4318
make self SELF_OWNER=you@example.com     # bootstraps the TaskTrooper project, prints TROOPER_DSN / TT_SELF_PROJECT_ID
```

For compose, set `TROOPER_SELF_DSN` to the printed DSN with host `ingest-gw` instead of `localhost`, plus `TT_SELF_PROJECT_ID`, and restart the
services so they report to that project, or connect your own app with an SDK. Then create a project API key in the dashboard
(Settings → API keys) for the project you want to query.

**2. Build the server.**

```bash
(cd services/mcp && go install ./cmd/tt-mcp)
command -v tt-mcp || echo "not on PATH: use $(go env GOPATH)/bin/tt-mcp as the command below"
```

**3. Configuration** (`TT_*` env, set in the agent's MCP config below):

| Env | |
|---|---|
| `TT_API_KEY` | required; project API key (dashboard Settings → API keys). Org and project come from the key |
| `TT_API_URL` | default `http://localhost:8080` |
| `TT_ORG_ID`, `TT_PROJECT_ID` | optional; checked against `/me` at startup, the server refuses to start on a mismatch |
| `TT_LOG_LEVEL` | default `info` (JSON on stderr) |

**4. Register it.** Claude Code:

```bash
claude mcp add tasktrooper \
  -e TT_API_URL=http://localhost:8080 -e TT_API_KEY=<api-key> \
  -- tt-mcp
claude mcp list                          # tasktrooper: tt-mcp - ✔ Connected
```

OpenCode (`~/.config/opencode/opencode.json` or a project `opencode.json`):

```json
{
  "mcp": {
    "tasktrooper": {
      "type": "local",
      "command": ["tt-mcp"],
      "environment": {
        "TT_API_URL": "http://localhost:8080",
        "TT_API_KEY": "<api-key>"
      },
      "enabled": true
    }
  }
}
```

A project `.mcp.json` variant is in [`.ai/mcp.md` § Connecting (manual)](.ai/mcp.md#connecting-manual).

**5. Skills.** Nothing to install when the agent runs in this repo: the committed `.claude/skills -> ../skills` symlink
is read by Claude Code and by OpenCode. Check with `claude -p "list skills starting with trooper-"`. Checkouts without
symlinks: see [`.ai/mcp.md` § Agent Skills](.ai/mcp.md#agent-skills-skills).

**6. Try it.**

> Something looks wrong with tt-api in the last 15 minutes. Find out why.

The agent should load `trooper-diagnose` and walk incidents → service KPIs → endpoint RCA → traces → logs → deploys.

## CI/CD

| Workflow | Runs on | What it does |
|---|---|---|
| [`ci.yml`](.github/workflows/ci.yml) | every PR, push to `main` | Per Go module in `services/go.work` (matrix derived from the file): gofmt (testdata excluded), `go vet`, unit tests. Web: `tsc -b` + `vite build`. JS SDKs: build + vitest. actionlint on the workflows |
| [`docker.yml`](.github/workflows/docker.yml) | PRs touching `services/**`, `deploy/**` or the workflow; push to `main` | One image per `services/*/Dockerfile` (one per `cmd/*` binary for `alerting`, `ingest`). PR: build + Trivy scan (CRITICAL, fixable), no push. `main`: build, scan, then push |

Run the same gates locally before a PR: `make check` (must end with `check OK`). Integration/e2e tests
(`make e2e-ingest`) stay local and are not in CI (ADR-026).

Images are published to GHCR as `ghcr.io/<owner>/<repo>/<image>:<sha>` and `:main`, e.g.
`ghcr.io/makifbaysal/tasktrooper-telemetry/api:main`. The Windows collector image (`Dockerfile.windows`) is not built.

**No deploy stage yet.** There is no environment to deploy the platform to: the repo has no deploy target
(no cluster, host or environment config for the platform services; `deploy/` holds the local compose stack and the
customer-side `trooper-agent` Helm chart). Images carry no `TT_ENV`; a production deployment must set
`TT_ENV=production` and a real `TT_SECRETS_KEY`; with the dev key, services that hold secrets fail closed
(ADR-026, contracts §10).
