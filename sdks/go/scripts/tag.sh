#!/usr/bin/env bash
# Create (but do not push) annotated release tags for the Go SDK:
#   sdks/go/v<version>
#   sdks/go/integrations/<name>/v<version>   (one per integration module)
# Usage: sdks/go/scripts/tag.sh 0.1.0
# Before tagging, make sure every integration's go.mod requires the root SDK (and sibling
# integrations) at v<version>. Pushing the tags triggers .github/workflows/release-go.yml.
set -euo pipefail

if [ $# -ne 1 ]; then
  echo "usage: $0 <version>   (e.g. 0.1.0)" >&2
  exit 2
fi
version="${1#v}"
if ! [[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
  echo "invalid version: $1 (want X.Y.Z or X.Y.Z-pre)" >&2
  exit 2
fi

root="$(git -C "$(dirname "$0")" rev-parse --show-toplevel)"
cd "$root"

if [ -n "$(git status --porcelain)" ]; then
  echo "working tree is dirty; commit or stash first" >&2
  exit 1
fi

tags=("sdks/go/v$version")
while IFS= read -r mod; do
  tags+=("$(dirname "$mod")/v$version")
done < <(find sdks/go/integrations -mindepth 2 -maxdepth 2 -name go.mod | sort)

for t in "${tags[@]}"; do
  if git rev-parse -q --verify "refs/tags/$t" >/dev/null; then
    echo "tag already exists: $t" >&2
    exit 1
  fi
done

# Integrations must require the version being tagged, or consumers get a mismatched core.
stale=$(grep -lE "github\.com/makifbaysal/tasktrooper-telemetry/sdks/go(/integrations/[a-z0-9]+)? v" sdks/go/integrations/*/go.mod |
  xargs grep -hE "^\s*(require\s+)?github\.com/makifbaysal/tasktrooper-telemetry/sdks/go(/integrations/[a-z0-9]+)? v" |
  grep -v " v$version\b" || true)
if [ -n "$stale" ]; then
  echo "integration go.mod files require a different SDK version than v$version:" >&2
  echo "$stale" >&2
  exit 1
fi

for t in "${tags[@]}"; do
  git tag -a "$t" -m "Go SDK $t"
  echo "created $t"
done

echo
echo "Push with:"
echo "  git push origin ${tags[*]}"
