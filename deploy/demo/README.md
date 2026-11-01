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
web. Not in the demo: codeintel, loadtest, trooper-cloud.

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
cloudflared service install
```

## Deploy
Actions → **deploy-demo** → Run workflow (branch `main`). The job checks out the commit, writes `deploy/.env` from
`DEMO_ENV`, runs [`deploy.sh`](deploy.sh) and removes the env file. `deploy.sh`:
infra up → `make migrate` → dashboard build (copied to `~/trooper-demo/web`) → services `up -d --build` → health checks
on api, ingest-gw and web. Data volumes (compose project `tasktrooper`) survive every deploy.

By hand on the machine (from a checkout with `deploy/.env`): `deploy/demo/deploy.sh`.

## First sign-in
Open `https://telemetry.tasktrooper.ai`, sign in with Google, onboarding creates the org, project and ingest key (DSN).
