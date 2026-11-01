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
if [ -n "${TT_SELF_OWNER:-}" ]; then
  # Self-observability (ADR-027): idempotent `api bootstrap` of TaskTrooper / tasktrooper owned by TT_SELF_OWNER. The
  # ingest key stays in $self_dir on this machine (reused on every deploy) and is never printed.
  step "self project"
  self_dir="$HOME/trooper-demo/self"
  mkdir -p "$self_dir" && chmod 700 "$self_dir"
  "${dc[@]}" --profile app build api
  out="$("${dc[@]}" --profile app run --rm --no-deps -T -u 0 -v "$self_dir:/out" api \
    bootstrap --org TaskTrooper --project tasktrooper --owner "$TT_SELF_OWNER" --ingest-key-out /out/ingest-key --json)"
  dsn="$(sed -nE 's/.*"dsn":"([^"]*)".*/\1/p' <<<"$out")"
  project="$(sed -nE 's/.*"projectId":([0-9]+).*/\1/p' <<<"$out")"
  key="$(sed -nE 's#^[a-z]+://([^@]+)@.*#\1#p' <<<"$dsn")"
  if [ -z "$key" ] || [ -z "$project" ]; then echo "api bootstrap returned no dsn" >&2; exit 1; fi
  [ "${GITHUB_ACTIONS:-}" = true ] && echo "::add-mask::$key"
  # containers reach ingest-gw directly; the browser uses the public ingest host
  export TROOPER_SELF_DSN="http://$key@ingest-gw:4318/$project" TT_SELF_PROJECT_ID="$project"
  web_dsn="$dsn"
  profiles+=(--profile self)
  agent=(trooper-agent)
  echo "self project $project (owner $TT_SELF_OWNER)"
else
  echo "TT_SELF_OWNER unset: self-observability off"
fi

step "dashboard"
pnpm install --frozen-lockfile
pnpm -r --filter "./sdks/js/packages/**" build
(cd apps/web && VITE_API_URL="$TT_PUBLIC_API_URL" VITE_INGEST_URL="$TT_PUBLIC_INGEST_URL" \
  VITE_TROOPER_DSN="$web_dsn" VITE_TROOPER_ENV=demo VITE_TROOPER_VERSION="$(git rev-parse --short HEAD)" pnpm exec vite build)
rsync -a --delete apps/web/dist/ "$TT_DEMO_WEB_DIR/"

step "services"
"${dc[@]}" "${profiles[@]}" up -d --build ingest-gw ingest-writer api alert-engine notifier web "${agent[@]}"

step "health"
wait_ok http://127.0.0.1:8080/healthz api
wait_ok http://127.0.0.1:4318/healthz ingest-gw
wait_ok http://127.0.0.1:8081/ web
"${dc[@]}" "${profiles[@]}" ps --format 'table {{.Service}}\t{{.Status}}'
echo "deployed $(git rev-parse --short HEAD)"
