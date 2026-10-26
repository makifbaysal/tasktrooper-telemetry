#!/bin/bash
# Uploads dSYMs to TaskTrooper symbolication (services/symbolicator, contracts.md §12):
#   POST $TROOPER_API_URL/v1/artifacts  (multipart: kind=dsym, app, release, file=<zipped .dSYM>)
#   Authorization: Bearer $TROOPER_API_KEY
#
# Xcode "Run Script" build phase (Release; add ${DWARF_DSYM_FOLDER_PATH}/${DWARF_DSYM_FILE_NAME} as input file):
#   "${SRCROOT}/path/to/sdks/ios/Scripts/upload-dsyms.sh"
# fastlane / CI (explicit paths: .dSYM bundles, folders containing them, or .zip archives):
#   TROOPER_APP=shop-ios TROOPER_RELEASE="1.4.0 (812)" upload-dsyms.sh build/Shop.app.dSYM.zip
#
# Environment:
#   TROOPER_API_URL   symbolicator base URL (e.g. https://symbols.example.com or http://localhost:8095)
#   TROOPER_API_KEY   api key
#   TROOPER_APP       app name = TrooperConfig.app (required)
#   TROOPER_RELEASE   release = RUM resource.version. Default: "<CFBundleShortVersionString> (<CFBundleVersion>)"
#                     from the built Info.plist — the same string as TrooperConfig.bundleVersion
#                     (the default `version` of TrooperConfig).
#   TROOPER_STRICT=1  fail (exit 1) on errors; otherwise errors are printed as warnings and the build continues.
#   TROOPER_SKIP=1    do nothing (e.g. for Debug builds).
set -u

warn() {
  if [ -n "${XCODE_VERSION_ACTUAL:-}" ]; then echo "warning: TaskTrooper dSYM upload: $*"; else echo "TaskTrooper dSYM upload: $*" >&2; fi
}
fail() {
  warn "$*"
  if [ "${TROOPER_STRICT:-0}" = "1" ]; then exit 1; fi
  exit 0
}

[ "${TROOPER_SKIP:-0}" = "1" ] && { echo "TaskTrooper dSYM upload skipped (TROOPER_SKIP=1)"; exit 0; }

API_URL="${TROOPER_API_URL:-}"
API_KEY="${TROOPER_API_KEY:-}"
APP="${TROOPER_APP:-}"
[ -n "$API_URL" ] || fail "TROOPER_API_URL is not set"
[ -n "$API_KEY" ] || fail "TROOPER_API_KEY is not set"
[ -n "$APP" ] || fail "TROOPER_APP is not set"
API_URL="${API_URL%/}"

RELEASE="${TROOPER_RELEASE:-}"
if [ -z "$RELEASE" ]; then
  PLIST=""
  if [ -n "${TARGET_BUILD_DIR:-}" ] && [ -n "${INFOPLIST_PATH:-}" ] && [ -f "${TARGET_BUILD_DIR}/${INFOPLIST_PATH}" ]; then
    PLIST="${TARGET_BUILD_DIR}/${INFOPLIST_PATH}"
  elif [ -n "${TROOPER_INFO_PLIST:-}" ]; then
    PLIST="$TROOPER_INFO_PLIST"
  fi
  if [ -n "$PLIST" ]; then
    SHORT=$(/usr/libexec/PlistBuddy -c "Print :CFBundleShortVersionString" "$PLIST" 2>/dev/null || true)
    BUILD=$(/usr/libexec/PlistBuddy -c "Print :CFBundleVersion" "$PLIST" 2>/dev/null || true)
  else
    SHORT="${MARKETING_VERSION:-}"
    BUILD="${CURRENT_PROJECT_VERSION:-}"
  fi
  if [ -n "$SHORT" ] && [ -n "$BUILD" ]; then RELEASE="$SHORT ($BUILD)"
  else RELEASE="${SHORT}${BUILD}"; fi
fi
[ -n "$RELEASE" ] || fail "cannot determine release; set TROOPER_RELEASE (must equal TrooperConfig.version)"

# Inputs: arguments, else the Xcode dSYM folder.
INPUTS=("$@")
if [ ${#INPUTS[@]} -eq 0 ]; then
  [ -n "${DWARF_DSYM_FOLDER_PATH:-}" ] || fail "no paths given and DWARF_DSYM_FOLDER_PATH is not set"
  INPUTS=("$DWARF_DSYM_FOLDER_PATH")
fi

TMP=$(mktemp -d "${TMPDIR:-/tmp}/trooper-dsym.XXXXXX") || fail "mktemp failed"
trap 'rm -rf "$TMP"' EXIT

FILES=()
add_dsym() {  # zip one .dSYM bundle
  local dsym="$1" name
  name=$(basename "$dsym")
  local zip="$TMP/${#FILES[@]}/${name}.zip"
  mkdir -p "$TMP/${#FILES[@]}"
  (cd "$(dirname "$dsym")" && /usr/bin/zip -qry "$zip" "$name") || { warn "cannot zip $dsym"; return; }
  FILES+=("$zip")
}
for input in "${INPUTS[@]}"; do
  if [ -d "$input" ] && [[ "$input" == *.dSYM ]]; then
    add_dsym "$input"
  elif [ -d "$input" ]; then
    while IFS= read -r -d '' d; do add_dsym "$d"; done < <(find "$input" -type d -name "*.dSYM" -prune -print0)
  elif [ -f "$input" ] && [[ "$input" == *.zip ]]; then
    FILES+=("$input")
  else
    warn "skipping $input (not a .dSYM, folder or .zip)"
  fi
done
[ ${#FILES[@]} -gt 0 ] || fail "no dSYMs found in: ${INPUTS[*]}"

ARGS=(-sS --fail-with-body --retry 2 --max-time 300 -X POST "$API_URL/v1/artifacts"
      -H "Authorization: Bearer $API_KEY"
      -F "kind=dsym" -F "app=$APP" -F "release=$RELEASE")
for f in "${FILES[@]}"; do ARGS+=(-F "file=@$f;type=application/zip"); done

echo "TaskTrooper: uploading ${#FILES[@]} dSYM archive(s) for $APP $RELEASE to $API_URL"
if ! OUT=$(/usr/bin/curl "${ARGS[@]}" 2>&1); then
  fail "upload failed: $OUT"
fi
echo "TaskTrooper: dSYM upload done"
exit 0
