#!/usr/bin/env bash
# Build, vet and test the core module and every integration/example module.
# Usage: scripts/test-all.sh [-race] [-tidy]
set -euo pipefail
cd "$(dirname "$0")/.."
export GOWORK=off   # each module must stand on its own (replace directives point at the local core)

race="" tidy=0
for a in "$@"; do
  case "$a" in
    -race) race="-race" ;;
    -tidy) tidy=1 ;;
  esac
done

mods=(.)
while IFS= read -r m; do mods+=("$(dirname "$m")"); done < <(find integrations examples -name go.mod -not -path '*/vendor/*' | sort)

failed=()
for m in "${mods[@]}"; do
  echo "==> $m"
  if ! (
    cd "$m"
    if [ "$tidy" = 1 ]; then go mod tidy; fi
    go build ./... && go vet ./... && go test $race -count=1 ./...
  ); then
    failed+=("$m")
  fi
done

echo
if [ ${#failed[@]} -gt 0 ]; then
  echo "FAILED: ${failed[*]}"
  exit 1
fi
echo "OK: ${#mods[@]} modules"
