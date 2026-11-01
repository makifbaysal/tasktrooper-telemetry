#!/usr/bin/env bash
# Sets the same version on every JS SDK package (sdks/js/packages/*/package.json).
# Usage: sdks/js/scripts/version.sh 0.2.0   → then commit and tag js-v0.2.0
set -euo pipefail
v="${1:-}"
if ! [[ "$v" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
  echo "usage: $0 <semver>  (e.g. 0.2.0)" >&2
  exit 1
fi
cd "$(dirname "$0")/../packages"
for f in */package.json; do
  node -e '
    const fs = require("fs"); const [f, v] = process.argv.slice(1);
    const p = JSON.parse(fs.readFileSync(f, "utf8")); p.version = v;
    fs.writeFileSync(f, JSON.stringify(p, null, 2) + "\n");
    console.log(`${p.name} -> ${v}`);
  ' "$f" "$v"
done
echo "next: git commit -am \"chore(sdk-js): release $v\" && git tag js-v$v && git push && git push origin js-v$v"
