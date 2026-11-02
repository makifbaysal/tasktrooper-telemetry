# Demo deployment (one Mac, GitHub Actions, Cloudflare Tunnel)

ADR-031. The whole platform runs on one machine (here a Mac mini with Docker Desktop). A manual **deploy-demo**
workflow runs on a self-hosted runner on that machine; Cloudflare Tunnel publishes it without open ports.

| Host | Local target |
|---|---|
| `telemetry.tasktrooper.ai` | dashboard, `web` (Caddy) on `127.0.0.1:8081` |
| `telemetry-api.tasktrooper.ai` | `api` on `:8080` |
| `telemetry-ingest.tasktrooper.ai` | `ingest-gw` OTLP/HTTP on `:4318` (no gRPC through the tunnel) |

One level under the zone on purpose: Cloudflare's free certificate does not cover `api.telemetry.tasktrooper.ai`.

Runs: ClickHouse, Postgres, Redpanda, Redis, s3 (replay blobs), ingest-gw, ingest-writer, api, alert-engine, notifier,
loadtest, web. Not in the demo: codeintel, trooper-cloud.

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

## Deploy
Actions → **deploy-demo** → Run workflow (branch `main`). The job checks out the commit, writes `deploy/.env` from
`DEMO_ENV`, runs [`deploy.sh`](deploy.sh) and removes the env file. `deploy.sh`:
infra up → `make migrate` → dashboard build (copied to `~/trooper-demo/web`) → services `up -d --build` → health checks
on api, ingest-gw and web. Data volumes (compose project `tasktrooper`) survive every deploy.
Every deploy stamps `TROOPER_VERSION` with the short commit sha (service version, deploy markers). The containers that
send self-telemetry resolve with 1.1.1.1 / 8.8.8.8, so a stale router DNS cache cannot point the public ingest host elsewhere.

By hand on the machine (from a checkout with `deploy/.env`): `deploy/demo/deploy.sh`.

## Self-observability (ADR-027)
Same as any customer: sign in, create a project (e.g. org **TaskTrooper**, project **tasktrooper**) in the dashboard and
copy its DSN (`https://<ingest-key>@telemetry-ingest.tasktrooper.ai/<project-id>`). Store it as the `demo` environment
secret **`DEMO_TROOPER_DSN`** and run deploy-demo. Then:
- the Go services export traces, logs and metrics as `tt-ingest-gw`, `tt-ingest-writer`, `tt-api`, `tt-alert-engine`,
  `tt-notifier` (env `demo`) through the public ingest host; ingest-gw/writer skip that project's own requests (loop guard,
  `TT_SELF_PROJECT_ID` = the DSN's project id),
- the dashboard sends RUM as `tt-web`,
- `trooper-agent` (profile `self`, `deploy/demo/agent.yaml`) reports Postgres, Redis, Redpanda, the Docker VM and every
  container.

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
   codeintel (analysis, push webhooks) is not part of the demo, so repositories are connected but not analyzed.
4. `demo` environment: variables **`DEMO_GITHUB_APP_ID`** (App ID) and **`DEMO_GITHUB_APP_SLUG`** (`github.com/apps/<slug>`),
   secret **`DEMO_GITHUB_APP_PRIVATE_KEY`** (General → Private keys → Generate a private key; the whole `.pem`):
   `gh secret set DEMO_GITHUB_APP_PRIVATE_KEY --env demo < task-trooper.*.private-key.pem`, then run deploy-demo.

## LLM model (ADR-033, ADR-034)
Every LLM call goes through one OpenAI-compatible Chat Completions endpoint (Meta Muse Spark by default): the assistant, the
incident summary and — where it runs — codeintel enrichment. `demo` environment: variable **`DEMO_ASSISTANT_MODEL`**
(e.g. a Muse Spark model id), optional variable **`DEMO_ASSISTANT_API_URL`** (default `https://api.meta.ai/v1`), secret
**`DEMO_ASSISTANT_API_KEY`**; the workflow passes them as `TT_ASSISTANT_*`. Without key and model the assistant and the incident
summary run the offline mock.

## First sign-in
Open `https://telemetry.tasktrooper.ai`, sign in with Google, onboarding creates the org, project and ingest key (DSN).
