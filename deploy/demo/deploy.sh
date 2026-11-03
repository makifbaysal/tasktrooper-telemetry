#!/usr/bin/env bash
# Demo deploy (ADR-031). Run from a checkout of the repo on the demo machine: by the deploy-demo workflow on the
# self-hosted runner, or by hand. Needs deploy/.env (the workflow writes it from the DEMO_ENV secret).
# Builds and (re)starts the core services, applies the schemas, builds the dashboard into a directory outside the
# checkout (TT_DEMO_WEB_DIR) so a clean checkout never empties the served site.
set -euo pipefail

cd "$(dirname "$0")/../.."
export PATH="/opt/homebrew/bin:/usr/local/bin:$HOME/.asdf/shims:$PATH"

step() { printf '\n==> %s\n' "$*"; }
wait_ok() {
  local url="$1" name="$2" _
  for _ in $(seq 1 30); do
    if curl -fsS -o /dev/null --max-time 3 "$url"; then echo "$name ok"; return 0; fi
    sleep 2
  done
  echo "$name is not healthy at $url" >&2
  "${dc[@]}" logs --tail 40 "$name" >&2 || true
  return 1
}

step "env"
[ -f deploy/.env ] || { echo "deploy/.env is missing (DEMO_ENV secret)" >&2; exit 1; }
grep -q '^TT_SECRETS_KEY=.' deploy/.env || { echo "deploy/.env has no TT_SECRETS_KEY" >&2; exit 1; }
set -a
# shellcheck disable=SC1091
. deploy/.env
set +a
export TT_DEMO_WEB_DIR="${TT_DEMO_WEB_DIR:-$HOME/trooper-demo/web}"
# service.version of the self-telemetry (deploy markers)
TT_DEMO_VERSION="$(git rev-parse --short HEAD)"
export TT_DEMO_VERSION
mkdir -p "$TT_DEMO_WEB_DIR"

# Docker Desktop's credential helper needs the login keychain, which a background runner may not have: use a config
# without credsStore that keeps the current context and the CLI plugins (public images only).
export DOCKER_CONFIG="$HOME/.docker-deploy"
mkdir -p "$DOCKER_CONFIG"
[ -e "$DOCKER_CONFIG/contexts" ] || ln -s "$HOME/.docker/contexts" "$DOCKER_CONFIG/contexts"
ctx="$(sed -nE 's/.*"currentContext": *"([^"]*)".*/\1/p' "$HOME/.docker/config.json" 2>/dev/null || true)"
printf '{"currentContext":"%s","cliPluginsExtraDirs":["%s/.docker/cli-plugins","/usr/local/lib/docker/cli-plugins","/Applications/Docker.app/Contents/Resources/cli-plugins"]}\n' \
  "${ctx:-desktop-linux}" "$HOME" >"$DOCKER_CONFIG/config.json"

dc=(docker compose -f deploy/docker-compose.yml -f deploy/demo/docker-compose.demo.yml)

step "infra"
"${dc[@]}" up -d --wait clickhouse postgres redpanda redis s3
"${dc[@]}" up redpanda-init

step "schemas"
make migrate

profiles=(--profile app)
agent=()
web_dsn=""
if [ -n "${TROOPER_DSN:-}" ]; then
  # Self-observability (ADR-027) like any customer: the DSN of a project created in the dashboard (DEMO_TROOPER_DSN
  # secret). The project id only feeds ingest-gw/writer's loop guard.
  step "self-observability"
  project="${TROOPER_DSN##*/}"
  [[ "$TROOPER_DSN" =~ ^https?://[^@/]+@[^/]+/[0-9]+$ ]] || { echo "TROOPER_DSN is not <scheme>://<key>@<host>/<project id>" >&2; exit 1; }
  # containers send straight to ingest-gw on the compose network (same key + project), never out and back in through
  # the tunnel; the browser (tt-web) keeps the public DSN
  key="${TROOPER_DSN#*://}"; key="${key%%@*}"
  export TROOPER_SELF_DSN="http://${key}@ingest-gw:4318/${project}" TT_SELF_PROJECT_ID="$project"
  web_dsn="$TROOPER_DSN"
  profiles+=(--profile self)
  agent=(trooper-agent)
  echo "on: project $project"
else
  echo "TROOPER_DSN unset: self-observability off"
fi

step "dashboard"
pnpm install --frozen-lockfile
pnpm -r --filter "./sdks/js/packages/**" build
# RUM of tt-web: every session recorded (masked text + inputs, low traffic), hidden sourcemaps for symbolicated stacks
(cd apps/web && VITE_API_URL="$TT_PUBLIC_API_URL" VITE_INGEST_URL="$TT_PUBLIC_INGEST_URL" \
  VITE_TROOPER_DSN="$web_dsn" VITE_TROOPER_ENV=demo VITE_TROOPER_VERSION="$TT_DEMO_VERSION" \
  VITE_TROOPER_REPLAY_SAMPLE_RATE=1 VITE_TROOPER_REPLAY_ON_ERROR_SAMPLE_RATE=1 TT_WEB_SOURCEMAP=hidden pnpm exec vite build)
# new files first, old ones removed at the end: index.html is never missing while the site is served; maps are never served
rsync -a --delete-after --exclude '*.map' apps/web/dist/ "$TT_DEMO_WEB_DIR/"

step "services"
"${dc[@]}" "${profiles[@]}" up -d --build ingest-gw ingest-writer api alert-engine notifier loadtest codeintel symbolicator web "${agent[@]}"

step "health"
wait_ok http://127.0.0.1:8080/healthz api
wait_ok http://127.0.0.1:4318/healthz ingest-gw
wait_ok http://127.0.0.1:8096/healthz codeintel
wait_ok http://127.0.0.1:8095/healthz symbolicator
wait_ok http://127.0.0.1:8081/ web
"${dc[@]}" "${profiles[@]}" ps --format 'table {{.Service}}\t{{.Status}}'

# Shared demo login (ADR-035): one email + password **member** account in the org of the self project
# (jury / visitors can create monitors, KPIs and load tests; members, keys and integrations stay admin-only). Idempotent; a new
# password signs out the old sessions. The password goes over stdin, never argv.
if [ -n "${TT_DEMO_LOGIN_EMAIL:-}" ] && [ -n "${TT_DEMO_LOGIN_PASSWORD:-}" ] && [ -n "${TT_SELF_PROJECT_ID:-}" ]; then
  step "demo login"
  printf '%s' "$TT_DEMO_LOGIN_PASSWORD" | "${dc[@]}" exec -T api /api set-password --email "$TT_DEMO_LOGIN_EMAIL" --name Demo \
    --project "$TT_SELF_PROJECT_ID" --role member --password-stdin
else
  echo "demo login skipped (needs DEMO_LOGIN_EMAIL, DEMO_LOGIN_PASSWORD and self-observability)"
fi

# Self project API key (kind api, DEMO_TROOPER_API_KEY): tt-web sourcemaps + one deploy marker per service
if [ -n "${TROOPER_DSN:-}" ] && [ -n "${TROOPER_API_KEY:-}" ]; then
  step "release"
  docker run --rm --network tasktrooper_default -v "$PWD":/src -v trooper-demo-gomod:/go/pkg/mod -w /src/tools/trooper-cli \
    -e TROOPER_API_KEY -e GOWORK=off golang:1.26-alpine \
    go run . upload-sourcemaps --app tt-web --release "$TT_DEMO_VERSION" --url http://symbolicator:8095 /src/apps/web/dist \
    || echo "sourcemap upload failed (stacks stay minified)" >&2
  sha="$(git rev-parse HEAD)"
  command -v jq >/dev/null || { echo "jq missing: no deploy marker" >&2; echo "deployed $(git rev-parse --short HEAD)"; exit 0; }
  body="$(jq -n --arg v "$TT_DEMO_VERSION" --arg sha "$sha" --arg msg "$(git log -1 --format=%s)" \
    --arg author "$(git log -1 --format=%an)" --arg url "${GITHUB_SERVER_URL:-https://github.com}/${GITHUB_REPOSITORY:-}/actions/runs/${GITHUB_RUN_ID:-}" \
    '{services: ["tt-web","tt-api","tt-ingest-gw","tt-ingest-writer","tt-alert-engine","tt-notifier","tt-loadtest-ctl","tt-codeintel","tt-symbolicator","tt-trooper-agent"],
      version: $v, commitSha: $sha, message: $msg, author: $author, url: $url, environment: "demo"}')"
  curl -fsS -o /dev/null -X POST -H "Authorization: Bearer $TROOPER_API_KEY" -H 'Content-Type: application/json' \
    --data "$body" http://127.0.0.1:8080/api/v1/deploys && echo "deploy marker sent" \
    || echo "deploy marker failed" >&2
fi
echo "deployed $(git rev-parse --short HEAD)"
