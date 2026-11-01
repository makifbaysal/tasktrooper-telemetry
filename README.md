# TaskTrooper Telemetry

Observability platform of the TaskTrooper family: SDKs collect APM, RUM, logs and profiles from backends, workers,
web and mobile apps; the platform visualises them, alerts on them and runs load tests. It also reads the customer's
code to propose alerts and load-test scenarios.

- **Start here:** [`AGENTS.md`](AGENTS.md) — repo map, stack, working rules (for humans and agents).
- **Design docs:** [`.ai/`](.ai/README.md) (Turkish, technical terms in English).
- **Local stack with live data:** `make infra migrate && make demo` — see [`tools/demo/README.md`](tools/demo/README.md).

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
`make demo` starts the platform services (api on `:8080`) plus a demo world with incidents, and runs until Ctrl-C:

```bash
make infra migrate && make demo          # terminal 1, leave it running
```

**2. Build the server.**

```bash
(cd services/mcp && go install ./cmd/tt-mcp)
command -v tt-mcp || echo "not on PATH: use $(go env GOPATH)/bin/tt-mcp as the command below"
```

**3. Configuration** (`TT_*` env, set in the agent's MCP config below):

| Env | |
|---|---|
| `TT_API_KEY` | required; project API key (dev: `tt_dev_api_key`). Org and project come from the key |
| `TT_API_URL` | default `http://localhost:8080` |
| `TT_ORG_ID`, `TT_PROJECT_ID` | optional; checked against `/me` at startup, the server refuses to start on a mismatch |
| `TT_LOG_LEVEL` | default `info` (JSON on stderr) |

**4. Register it.** Claude Code:

```bash
claude mcp add tasktrooper \
  -e TT_API_URL=http://localhost:8080 -e TT_API_KEY=tt_dev_api_key \
  -e TT_ORG_ID=1 -e TT_PROJECT_ID=1 \
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
        "TT_API_KEY": "tt_dev_api_key",
        "TT_ORG_ID": "1",
        "TT_PROJECT_ID": "1"
      },
      "enabled": true
    }
  }
}
```

A project `.mcp.json` variant is in [`.ai/mcp.md` § Bağlanma](.ai/mcp.md#bağlanma).

**5. Skills.** Nothing to install when the agent runs in this repo: the committed `.claude/skills -> ../skills` symlink
is read by Claude Code and by OpenCode. Check with `claude -p "list skills starting with trooper-"`. Checkouts without
symlinks: see [`.ai/mcp.md` § Agent Skills](.ai/mcp.md#agent-skills-skills).

**6. Try it.**

> Something looks wrong with payments-api in the last 15 minutes. Find out why.

The agent should load `trooper-diagnose` and walk incidents → service KPIs → endpoint RCA → traces → logs → deploys.
A rehearsable end-to-end run with the expected tool calls: [`docs/demo-mcp.md`](docs/demo-mcp.md).

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
