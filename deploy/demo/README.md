# Demo deployment (one Mac, GitHub Actions, Cloudflare Tunnel)

ADR-031. The whole platform runs on one machine (here a Mac mini with Docker Desktop). A manual **deploy-demo**
workflow runs on a self-hosted runner on that machine; Cloudflare Tunnel publishes it without open ports.

| Host | Local target |
|---|---|
| `telemetry.tasktrooper.ai` | dashboard, `web` (Caddy) on `127.0.0.1:8081` |
| `telemetry-api.tasktrooper.ai` | `api` on `:8080` |
| `telemetry-ingest.tasktrooper.ai` | `ingest-gw` OTLP/HTTP on `:4318` (no gRPC through the tunnel) |

One level under the zone on purpose: Cloudflare's free certificate does not cover `api.telemetry.tasktrooper.ai`.

Runs: ClickHouse, Postgres, Redpanda, Redis, s3 (replay blobs, symbol files), ingest-gw, ingest-writer, api, alert-engine, notifier,
loadtest, codeintel, symbolicator, web. Not in the demo: trooper-cloud.

Load tests run on this machine with small caps (`docker-compose.demo.yml`: 50 VUs, 15 min, one run at a time, 3000
VU-minutes a month) and only against verified domains: `TT_LOADTEST_ALLOW_LOCAL=false`, so a scenario can never target
the demo host itself. Add and verify a target under Load tests → Domains first.

## One-time setup

### Machine
- Docker Desktop: Resources ≥ 10 GB memory, 6 CPU, 100 GB disk; General → "Start Docker Desktop when you sign in".
- `brew install node pnpm cloudflared` (Node ≥ 22; with asdf: `corepack enable && asdf reshim nodejs`).
- No sleep, restart after a power cut: `sudo pmset -a sleep 0 disksleep 0 autorestart 1`.
- The runner and Docker Desktop run in the user session: after a restart that user must be logged in.

### Repository settings (GitHub)
1. Settings → Actions → General → Approval for running fork pull request workflows → **Require approval for all external contributors**.
   The repo is public; this keeps fork PRs off the self-hosted runner.
2. Settings → Environments → **New environment `demo`**. Deployment branches: `main` only. Optionally add yourself as
   required reviewer.
3. In `demo`, add the secret **`DEMO_ENV`** with the full contents of [`.env.example`](.env.example), filled in.

### Runner
Settings → Actions → Runners → **New self-hosted runner** → macOS / ARM64 shows the download and a one-time token.
On the machine, in `~/actions-runner`:

```bash
./config.sh --url https://github.com/makifbaysal/tasktrooper-telemetry --token <token> \
  --name mac-mini --labels trooper-demo --unattended
./svc.sh install && ./svc.sh start      # LaunchAgent: starts at login
```

### Tunnel
`tasktrooper.ai` DNS must be on Cloudflare.

```bash
cloudflared tunnel login
cloudflared tunnel create trooper-demo
cp deploy/demo/cloudflared.example.yml ~/.cloudflared/config.yml   # fill in the tunnel id and user
for h in telemetry telemetry-api telemetry-ingest; do cloudflared tunnel route dns trooper-demo $h.tasktrooper.ai; done
sudo cloudflared service install       # system LaunchDaemon: runs at boot without a login
```

The daemon that `sudo cloudflared service install` writes starts `cloudflared` without arguments and exits ("use `cloudflared
tunnel run`"). Copy `config.yml` and `<TUNNEL_ID>.json` to `/etc/cloudflared/` (credentials-file pointing there), then add
the arguments and reload:

```bash
PL=/Library/LaunchDaemons/com.cloudflare.cloudflared.plist
sudo launchctl bootout system/com.cloudflare.cloudflared
for a in --config /etc/cloudflared/config.yml tunnel run; do sudo /usr/libexec/PlistBuddy -c "Add :ProgramArguments: string $a" "$PL"; done
sudo launchctl bootstrap system "$PL"
cloudflared tunnel info trooper-demo     # a CONNECTOR ID line = connected
```

`protocol: http2` (in the example config) keeps the tunnel on TCP. On QUIC the home network dropped the idle UDP flows:
every minute `timeout: no recent network activity` in `/Library/Logs/com.cloudflare.cloudflared.err.log` and 530 on all
hostnames until the reconnect. Existing machine: add the line to `/etc/cloudflared/config.yml`, then
`sudo launchctl kickstart -k system/com.cloudflare.cloudflared`. `edge-ip-version: "4"` too: on HTTP/2 over IPv6 the
edge connections still dropped every ~2 minutes (`Error shutting down control stream: client disconnected`).

The machine must never sleep (a sleeping Mac drops the tunnel; `pmset -g` showed `sleep 1`):
`sudo pmset -a sleep 0 disksleep 0 powernap 0`.

## Deploy
Actions → **deploy-demo** → Run workflow (branch `main`). The job checks out the commit, writes `deploy/.env` from
`DEMO_ENV`, runs [`deploy.sh`](deploy.sh) and removes the env file. `deploy.sh`:
infra up → `make migrate` → dashboard build (copied to `~/trooper-demo/web`) → services `up -d --build` → health checks
on api, ingest-gw, codeintel, symbolicator and web. Data volumes (compose project `tasktrooper`) survive every deploy.
Every deploy stamps `TROOPER_VERSION` with the short commit sha (service version, deploy markers). The service containers
resolve with 1.1.1.1 / 8.8.8.8, so a stale router DNS cache cannot point their outbound calls elsewhere.

By hand on the machine (from a checkout with `deploy/.env`): `deploy/demo/deploy.sh`.

## Self-observability (ADR-027)
Same as any customer: sign in, create a project (e.g. org **TaskTrooper**, project **tasktrooper**) in the dashboard and
copy its DSN (`https://<ingest-key>@telemetry-ingest.tasktrooper.ai/<project-id>`). Store it as the `demo` environment
secret **`DEMO_TROOPER_DSN`** and run deploy-demo. Then:
- the Go services export traces, logs, metrics and continuous profiles (`TROOPER_PROFILING=true`) as `tt-ingest-gw`,
  `tt-ingest-writer`, `tt-api`, `tt-alert-engine`, `tt-notifier`, `tt-loadtest-ctl` / `tt-loadtest-runner`, `tt-codeintel`,
  `tt-symbolicator` (env `demo`) straight to `ingest-gw:4318` on the compose network (deploy.sh rewrites the DSN host; the
  browser uses the public one); ingest-gw/writer skip that project's own requests (loop guard, `TT_SELF_PROJECT_ID` = the
  DSN's project id),
- the dashboard sends RUM as `tt-web`, with session replay for every session (text and inputs masked),
- `trooper-agent` (profile `self`, `deploy/demo/agent.yaml`) reports Postgres (incl. top queries via `pg_stat_statements`),
  ClickHouse, Redis, Redpanda, the Docker VM and every container.

Optional secret **`DEMO_TROOPER_API_KEY`**: an API key (Team → API keys, kind `api`) of the same project. With it every
deploy uploads the `tt-web` sourcemaps to the symbolicator (readable JS error stacks) and writes a deploy marker (commit,
author, run URL) for each `tt-*` service: `gh secret set DEMO_TROOPER_API_KEY --env demo`.

No secret = all of this is off.

## Slack (ADR-017)
"Connect Slack" needs one Slack App registered by whoever runs the server (users then just click "Add to Slack").
Without it the Slack endpoints answer 501 and the dashboard says Slack isn't set up.
1. https://api.slack.com/apps → **Create New App** → From scratch (name e.g. *TaskTrooper*, any workspace).
2. **OAuth & Permissions** → Redirect URL `https://telemetry-api.tasktrooper.ai/api/v1/integrations/slack/callback`;
   Bot Token Scopes `chat:write`, `chat:write.public`, `channels:read`, `groups:read`, `channels:join`, `users:read`,
   `users:read.email`.
3. **Interactivity & Shortcuts** → on, Request URL `https://telemetry-api.tasktrooper.ai/api/v1/slack/interactions`
   (ack / mute buttons).
4. **Manage Distribution** → Activate Public Distribution (only if workspaces other than the app's own will connect).
5. **Basic Information** → App Credentials: store Client ID, Client Secret and Signing Secret as the `demo` environment
   secrets **`DEMO_SLACK_CLIENT_ID`**, **`DEMO_SLACK_CLIENT_SECRET`**, **`DEMO_SLACK_SIGNING_SECRET`**, then run
   deploy-demo. Kept out of `DEMO_ENV` on purpose: regenerating that would rotate `TT_SECRETS_KEY`.

## GitHub App (ADR-017)
"Connect GitHub" (Settings, Code insights) needs one GitHub App; without it the install endpoints answer 501.
1. https://github.com/settings/apps → New GitHub App (or reuse one that no other product depends on).
2. **Setup URL** `https://telemetry-api.tasktrooper.ai/api/v1/integrations/github/callback`, "Redirect on update" on.
3. **Permissions & events** → Repository: Contents read-only, Metadata read-only; event **Push**. Webhook can stay inactive:
   codeintel analyzes a repository when it is connected; push webhooks (incremental re-analysis) need a tunnel route to
   codeintel `:8096`, which the demo does not have.
4. `demo` environment: variables **`DEMO_GITHUB_APP_ID`** (App ID) and **`DEMO_GITHUB_APP_SLUG`** (`github.com/apps/<slug>`),
   secret **`DEMO_GITHUB_APP_PRIVATE_KEY`** (General → Private keys → Generate a private key; the whole `.pem`):
   `gh secret set DEMO_GITHUB_APP_PRIVATE_KEY --env demo < task-trooper.*.private-key.pem`, then run deploy-demo.

## LLM model (ADR-033, ADR-034)
Every LLM call goes through one OpenAI-compatible Chat Completions endpoint (Meta Muse Spark by default): the assistant, the
incident summary and codeintel enrichment. `demo` environment: variable **`DEMO_ASSISTANT_MODEL`**
(e.g. a Muse Spark model id), optional variable **`DEMO_ASSISTANT_API_URL`** (default `https://api.meta.ai/v1`), secret
**`DEMO_ASSISTANT_API_KEY`**; the workflow passes them as `TT_ASSISTANT_*`. Without key and model the assistant and the incident
summary run the offline mock and codeintel uses static rules only.

## First sign-in
Open `https://telemetry.tasktrooper.ai`, sign in with Google, onboarding creates the org, project and ingest key (DSN).

## Shared demo login (ADR-035)
One email + password account for people who want to look inside the demo without a Google account. It is a **member** of
the org that owns the self project, so the jury can try everything: create / edit monitors, KPIs and load tests, approve
the assistant's write tools. Members, API keys, integrations, AI quota and org settings stay admin-only, and the account
cannot create orgs. Anything it changes is audited as that one user. `demo`
environment: variable **`DEMO_LOGIN_EMAIL`** (e.g. `demo@tasktrooper.ai`), secret **`DEMO_LOGIN_PASSWORD`** (12–72 bytes):

```bash
gh variable set DEMO_LOGIN_EMAIL --env demo --body demo@tasktrooper.ai
gh secret set DEMO_LOGIN_PASSWORD --env demo          # paste the password
```

Then run deploy-demo. After the health checks `deploy.sh` runs `api set-password --project "$TT_SELF_PROJECT_ID" --role member`
inside the api container (password over stdin). Idempotent: same password = no change; a new password signs out everyone
using the old one. Skipped (with a log line) when either value is empty or self-observability (`DEMO_TROOPER_DSN`) is off.
The api runs with `TT_AUTH_PASSWORD=true` and `TT_CLIENT_IP_HEADER=CF-Connecting-IP` (failed logins: 10 per visitor IP, 30
per account per 15 min). To lock people out, change `DEMO_LOGIN_PASSWORD` and redeploy (old sessions are signed out).

Visitors share the one user: they share its AI assistant credits (the member quota, else the org quota: set a member quota
in Settings → AI usage) and see each other's assistant threads.
