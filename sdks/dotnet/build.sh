#!/usr/bin/env bash
# Restore, build and test the .NET SDK inside Docker (no local dotnet needed).
# Usage: ./build.sh [test|build|pack|shell]   (default: test)
set -euo pipefail
cd "$(dirname "$0")"

IMAGE="${DOTNET_SDK_IMAGE:-mcr.microsoft.com/dotnet/sdk:10.0}"
CMD="${1:-test}"

run() {
  docker run --rm \
    -v "$PWD":/src -w /src \
    -v ttnuget:/root/.nuget/packages \
    -e DOTNET_CLI_TELEMETRY_OPTOUT=1 -e DOTNET_NOLOGO=1 -e DOTNET_SKIP_FIRST_TIME_EXPERIENCE=1 \
    "$IMAGE" bash -c "$1"
}

case "$CMD" in
  build) run "dotnet restore TaskTrooper.sln && dotnet build TaskTrooper.sln -c Release --no-restore" ;;
  test)  run "dotnet restore TaskTrooper.sln && dotnet build TaskTrooper.sln -c Release --no-restore && dotnet test TaskTrooper.sln -c Release --no-build --logger 'console;verbosity=normal'" ;;
  pack)  run "dotnet pack TaskTrooper.sln -c Release -o artifacts" ;;
  shell) docker run --rm -it -v "$PWD":/src -w /src -v ttnuget:/root/.nuget/packages "$IMAGE" bash ;;
  *) echo "unknown command: $CMD" >&2; exit 2 ;;
esac
