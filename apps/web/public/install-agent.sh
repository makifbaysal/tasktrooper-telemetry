#!/usr/bin/env bash
# TaskTrooper for coding agents: installs the tt-mcp MCP server and the trooper-* agent skills into the coding tools
# found on this machine (Claude Code, Cursor, Antigravity, Codex, OpenCode, Gemini CLI, Muse Code).
#
#   curl -fsSL https://telemetry.tasktrooper.ai/install-agent.sh | bash
#
# It asks which tools to set up, whether for all projects (global) or only the current project, and for an API key
# (dashboard → Settings → API keys). The key goes to one file readable only by you (~/.config/tasktrooper/mcp.env, or
# .tasktrooper/mcp.env in the project, git-ignored); tool configs only reference the tt-mcp binary.
#
# Non-interactive: --tools claude,cursor|all  --scope global|project  --project-dir DIR  --api-url URL  --write|--read-only
# --yes; the key comes from $TT_API_KEY. Docs: https://telemetry.tasktrooper.ai/docs#install  (.ai/mcp.md § Kurulum)
set -euo pipefail

REPO="makifbaysal/tasktrooper-telemetry"
DEFAULT_API_URL="https://telemetry-api.tasktrooper.ai" # docsContent.ts API_HOST
SKILLS="trooper-diagnose trooper-query trooper-alerts"
ALL_TOOLS="claude muse cursor antigravity codex opencode gemini"
SERVER="tasktrooper"

TOOLS="" SCOPE="" PROJECT_DIR="" API_URL="" WRITE="" YES=0 BIN_DIR="${TT_INSTALL_DIR:-$HOME/.local/bin}" SOURCE=""

bold() { printf '\033[1m%s\033[0m' "$*"; }
info() { printf '  %s\n' "$*"; }
ok() { printf '  \033[32m✓\033[0m %s\n' "$*"; }
warn() { printf '  \033[33m!\033[0m %s\n' "$*" >&2; }
die() { printf '\033[31merror:\033[0m %s\n' "$*" >&2; exit 1; }

usage() {
  cat <<'TXT'
Installs the TaskTrooper MCP server (tt-mcp) and agent skills into your coding tools.
  --tools LIST        claude,cursor,antigravity,codex,opencode,gemini,muse or all (default: ask)
  --scope S           global | project (default: ask)
  --project-dir DIR   project root for --scope project (default: git root or current dir)
  --api-url URL       TaskTrooper API (default: https://telemetry-api.tasktrooper.ai)
  --write | --read-only  let agents create alerts / SLOs / load tests after your approval (default: ask)
  --bin-dir DIR       where tt-mcp goes (default: ~/.local/bin)
  --source DIR        a local checkout of the repository: build tt-mcp and take the skills from it
  -y, --yes           no questions; needs --tools, --scope and TT_API_KEY
TXT
  exit 0
}

while [ $# -gt 0 ]; do
  case "$1" in
    --tools) TOOLS="$(printf '%s' "$2" | tr ',' ' ')"; shift 2 ;;
    --scope) SCOPE="$2"; shift 2 ;;
    --project-dir) PROJECT_DIR="$2"; shift 2 ;;
    --api-url) API_URL="$2"; shift 2 ;;
    --write) WRITE=true; shift ;;
    --read-only) WRITE=false; shift ;;
    --bin-dir) BIN_DIR="$2"; shift 2 ;;
    --source) SOURCE="$(cd "$2" && pwd)"; shift 2 ;;
    -y|--yes) YES=1; shift ;;
    -h|--help) usage ;;
    *) die "unknown option $1 (see --help)" ;;
  esac
done

# Prompts read the terminal, not stdin: stdin is the script itself under `curl | bash`.
TTY=""
if [ "$YES" = 0 ] && { : </dev/tty; } 2>/dev/null; then TTY=/dev/tty; fi
ask() { # ask "question" default → answer
  local a=""
  if [ -n "$TTY" ]; then printf '%s ' "$1" >/dev/tty; IFS= read -r a </dev/tty || true; fi
  printf '%s' "${a:-$2}"
}
ask_secret() {
  local a=""
  if [ -n "$TTY" ]; then
    stty -echo </dev/tty 2>/dev/null || true # before the prompt, so nothing typed early is echoed
    printf '%s ' "$1" >/dev/tty
    IFS= read -r a </dev/tty || true
    stty echo </dev/tty 2>/dev/null || true
    printf '\n' >/dev/tty
  fi
  printf '%s' "$a"
}

need() { command -v "$1" >/dev/null 2>&1 || die "$1 is required"; }
need curl
need tar

# ---------------------------------------------------------------- detect tools

has() { command -v "$1" >/dev/null 2>&1; }
detected() { # tool → 0 when it looks installed
  case "$1" in
    claude) has claude || [ -d "$HOME/.claude" ] ;;
    cursor) has cursor || has cursor-agent || [ -d "$HOME/.cursor" ] || [ -d /Applications/Cursor.app ] ;;
    antigravity) has agy || has antigravity || [ -d "$HOME/.gemini/antigravity" ] || [ -d "$HOME/.gemini/config" ] || [ -d /Applications/Antigravity.app ] ;;
    codex) has codex || [ -d "$HOME/.codex" ] ;;
    opencode) has opencode || [ -d "$HOME/.config/opencode" ] ;;
    gemini) has gemini ;;
    muse) has muse || [ -d "${XDG_CONFIG_HOME:-$HOME/.config}/muse" ] ;;
  esac
}
label() {
  case "$1" in
    claude) echo "Claude Code" ;; cursor) echo "Cursor" ;; antigravity) echo "Antigravity" ;; codex) echo "Codex CLI" ;;
    opencode) echo "OpenCode" ;; gemini) echo "Gemini CLI" ;; muse) echo "Muse Code (Meta)" ;;
  esac
}

printf '\n%s\n\n' "$(bold 'TaskTrooper for coding agents')"

FOUND=""
for t in $ALL_TOOLS; do detected "$t" && FOUND="$FOUND $t"; done
FOUND="${FOUND# }"

if [ "${TOOLS# }" = all ]; then
  [ -n "$FOUND" ] || die "no supported coding tool found"
  TOOLS="$FOUND"
fi
if [ -z "$TOOLS" ]; then
  [ -n "$FOUND" ] || die "no supported coding tool found ($(for t in $ALL_TOOLS; do printf '%s, ' "$(label "$t")"; done | sed 's/, $//')); pass --tools to install anyway"
  [ -n "$TTY" ] || die "no terminal to ask on; pass --tools (found: $FOUND) and --yes"
  echo "Coding tools found on this machine:"
  i=0
  for t in $FOUND; do i=$((i + 1)); info "$i) $(label "$t")"; done
  pick="$(ask "Install for which? Numbers separated by spaces or commas, or 'all' [all]:" all)"
  if [ "$pick" = all ]; then TOOLS="$FOUND"
  else
    for n in $(printf '%s' "$pick" | tr ',' ' '); do
      case "$n" in ''|*[!0-9]*) die "not a number: $n" ;; esac
      t="$(printf '%s\n' $FOUND | sed -n "${n}p")"
      [ -n "$t" ] || die "no tool number $n"
      TOOLS="$TOOLS $t"
    done
  fi
fi
for t in $TOOLS; do case " $ALL_TOOLS " in *" $t "*) ;; *) die "unknown tool $t (one of: $ALL_TOOLS)" ;; esac; done
[ -n "${TOOLS// /}" ] || die "nothing selected"

if [ -z "$SCOPE" ]; then
  echo
  echo "Where?"
  info "1) Global: every project on this machine"
  info "2) This project only: $(git rev-parse --show-toplevel 2>/dev/null || pwd)"
  case "$(ask 'Choose [1]:' 1)" in 1|global|g) SCOPE=global ;; 2|project|p) SCOPE=project ;; *) die "choose 1 or 2" ;; esac
fi
case "$SCOPE" in
  global) ;;
  project) PROJECT_DIR="${PROJECT_DIR:-$(git rev-parse --show-toplevel 2>/dev/null || pwd)}"; PROJECT_DIR="$(cd "$PROJECT_DIR" && pwd)" ;;
  *) die "--scope must be global or project" ;;
esac

# ---------------------------------------------------------------- API URL + key

echo
API_URL="${API_URL:-$(ask "TaskTrooper API URL [$DEFAULT_API_URL]:" "$DEFAULT_API_URL")}"
API_URL="${API_URL%/}"
case "$API_URL" in http://*|https://*) ;; *) die "API URL must start with http:// or https://" ;; esac

check_key() { # 0 = valid; the key goes to curl on stdin, never on the command line
  local code
  code="$(printf 'Authorization: Bearer %s\n' "$1" | curl -sS -o /dev/null -w '%{http_code}' -H @- --max-time 15 "$API_URL/api/v1/me" || echo 000)"
  case "$code" in
    200) return 0 ;;
    401|403) warn "the API rejected this key (HTTP $code)"; return 1 ;;
    000) die "could not reach $API_URL" ;;
    *) warn "unexpected answer from $API_URL (HTTP $code)"; return 1 ;;
  esac
}
KEY="${TT_API_KEY:-}"
if [ -n "$KEY" ]; then check_key "$KEY" || die "TT_API_KEY is not valid for $API_URL"
else
  [ -n "$TTY" ] || die "set TT_API_KEY when running without a terminal"
  info "Create a key in the dashboard: Settings → API keys. It is stored only in a file readable by you."
  for _ in 1 2 3; do
    KEY="$(ask_secret 'API key (input hidden):')"
    [ -n "$KEY" ] && check_key "$KEY" && break
    KEY=""
  done
  [ -n "$KEY" ] || die "no valid API key"
fi
ok "API key accepted"

if [ -z "$WRITE" ]; then
  echo
  info "Agents can always read. They can also create alert rules, SLOs and load tests, but only after you approve each change."
  case "$(ask 'Allow that? [y/N]:' n)" in y|Y|yes) WRITE=true ;; *) WRITE=false ;; esac
fi

# ---------------------------------------------------------------- tt-mcp binary

os="$(uname -s | tr '[:upper:]' '[:lower:]')"; arch="$(uname -m)"
case "$arch" in x86_64|amd64) arch=amd64 ;; arm64|aarch64) arch=arm64 ;; *) die "unsupported CPU $arch" ;; esac
case "$os" in darwin|linux) ;; *) die "unsupported OS $os (Windows: use WSL)" ;; esac

TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
BIN="$BIN_DIR/tt-mcp"
mkdir -p "$BIN_DIR"

echo
tag=""
[ -n "$SOURCE" ] || tag="$(curl -fsSL "https://api.github.com/repos/$REPO/releases?per_page=30" 2>/dev/null \
  | grep -o '"tag_name": *"mcp/v[^"]*"' | head -n1 | sed 's/.*"\(mcp\/v[^"]*\)"/\1/' || true)"
if [ -n "$SOURCE" ]; then
  has go || die "--source needs Go 1.26+ to build tt-mcp"
  (cd "$SOURCE/services/mcp" && GOWORK=off CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$BIN" ./cmd/tt-mcp)
  ok "tt-mcp (built from $SOURCE) → $BIN"
elif [ -n "$tag" ]; then
  asset="tt-mcp_${os}_${arch}"
  base="https://github.com/$REPO/releases/download/$tag"
  curl -fsSL "$base/$asset.tar.gz" -o "$TMP/$asset.tar.gz" || die "download failed: $base/$asset.tar.gz"
  curl -fsSL "$base/checksums.txt" -o "$TMP/checksums.txt" || die "download failed: checksums.txt"
  want="$(grep " \./$asset.tar.gz\$\| $asset.tar.gz\$" "$TMP/checksums.txt" | awk '{print $1}')"
  if has sha256sum; then got="$(sha256sum "$TMP/$asset.tar.gz" | awk '{print $1}')"; else got="$(shasum -a 256 "$TMP/$asset.tar.gz" | awk '{print $1}')"; fi
  [ -n "$want" ] && [ "$want" = "$got" ] || die "checksum mismatch for $asset.tar.gz"
  tar -xzf "$TMP/$asset.tar.gz" -C "$TMP"
  install -m 0755 "$TMP/$asset/tt-mcp" "$BIN"
  ok "tt-mcp ${tag#mcp/} → $BIN"
elif has go && has git; then
  info "no tt-mcp release yet; building from source with $(go version | awk '{print $3}')"
  git clone -q --depth 1 "https://github.com/$REPO.git" "$TMP/src"
  (cd "$TMP/src/services/mcp" && GOWORK=off CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$BIN" ./cmd/tt-mcp)
  ok "tt-mcp (built from main) → $BIN"
else
  die "no tt-mcp release found and Go is not installed to build it; install Go 1.26+ and run again"
fi
case ":$PATH:" in *":$BIN_DIR:"*) ;; *) warn "$BIN_DIR is not on PATH (tool configs use the full path, so this is only for your shell)" ;; esac

# ---------------------------------------------------------------- key file

write_env() { # $1 = file
  mkdir -p "$(dirname "$1")"
  (umask 077 && printf '# tt-mcp settings, written by install-agent.sh. Keep private.\nTT_API_URL=%s\nTT_API_KEY=%s\nTT_MCP_ALLOW_WRITE=%s\n' \
    "$API_URL" "$KEY" "$WRITE" >"$1.tmp" && mv "$1.tmp" "$1")
  chmod 600 "$1"
}
if [ "$SCOPE" = global ]; then
  ENV_FILE="${XDG_CONFIG_HOME:-$HOME/.config}/tasktrooper/mcp.env"
  write_env "$ENV_FILE"
else
  ENV_FILE="$PROJECT_DIR/.tasktrooper/mcp.env"
  write_env "$ENV_FILE"
  if [ -d "$PROJECT_DIR/.git" ] || git -C "$PROJECT_DIR" rev-parse >/dev/null 2>&1; then
    gi="$PROJECT_DIR/.gitignore"
    grep -qxF '.tasktrooper/' "$gi" 2>/dev/null || printf '\n# TaskTrooper MCP key (install-agent.sh)\n.tasktrooper/\n' >>"$gi"
  fi
fi
ok "key saved to $ENV_FILE (mode 600)"

# ---------------------------------------------------------------- skills

if [ -n "$SOURCE" ]; then mkdir -p "$TMP/repo" && cp -R "$SOURCE/skills" "$TMP/repo/skills"
else
  curl -fsSL "https://codeload.github.com/$REPO/tar.gz/refs/heads/main" -o "$TMP/repo.tgz" || die "could not download the skills"
  mkdir -p "$TMP/repo" && tar -xzf "$TMP/repo.tgz" -C "$TMP/repo" --strip-components=1
fi
for s in $SKILLS; do [ -f "$TMP/repo/skills/$s/SKILL.md" ] || die "skill $s missing from the repository"; done

put_skills() { # $1 = skills dir: real copies
  mkdir -p "$1"
  for s in $SKILLS; do rm -rf "${1:?}/$s"; cp -R "$TMP/repo/skills/$s" "$1/$s"; done
}
link_skills() { # $1 = skills dir, $2 = shared dir: symlinks, so every tool sees the same files
  mkdir -p "$1"
  for s in $SKILLS; do rm -rf "${1:?}/$s"; ln -s "$2/$s" "$1/$s"; done
}
selected() { case " $TOOLS " in *" $1 "*) return 0 ;; esac; return 1; }

if [ "$SCOPE" = global ]; then
  SHARED="$HOME/.agents/skills" # Cursor, Codex, OpenCode, Gemini CLI, Muse Code
  put_skills "$SHARED"
  selected claude && link_skills "$HOME/.claude/skills" "$SHARED"
  selected antigravity && link_skills "$HOME/.gemini/config/skills" "$SHARED"
else
  SHARED="$PROJECT_DIR/.agents/skills" # Cursor, Antigravity, Codex, OpenCode, Gemini CLI, Muse Code
  put_skills "$SHARED"
  selected claude && link_skills "$PROJECT_DIR/.claude/skills" "../../.agents/skills"
fi
ok "skills ($SKILLS) → $SHARED"

# ---------------------------------------------------------------- MCP registration

# merge_json FILE KEY.PATH VALUE_JSON [DEFAULTS_JSON]: sets one key in a JSON config, keeps everything else, backs the
# file up once (FILE.tasktrooper.bak) and writes atomically. Fails on JSONC (comments) instead of rewriting it.
merge_json() {
  local file="$1" keypath="$2" value="$3" defaults="${4:-}"
  [ -n "$defaults" ] || defaults='{}'
  mkdir -p "$(dirname "$file")"
  [ -f "$file" ] && [ ! -f "$file.tasktrooper.bak" ] && cp "$file" "$file.tasktrooper.bak"
  if python3 -c 'import json' >/dev/null 2>&1; then
    python3 - "$file" "$keypath" "$value" "$defaults" 2>/dev/null <<'PY'
import json, os, sys
path, keypath, value, defaults = sys.argv[1], sys.argv[2].split("."), json.loads(sys.argv[3]), json.loads(sys.argv[4])
doc = {}
if os.path.exists(path) and os.path.getsize(path) > 0:
    with open(path) as f:
        doc = json.load(f)
for k, v in defaults.items():
    doc.setdefault(k, v)
cur = doc
for k in keypath[:-1]:
    cur = cur.setdefault(k, {})
cur[keypath[-1]] = value
tmp = path + ".tmp"
with open(tmp, "w") as f:
    json.dump(doc, f, indent=2)
    f.write("\n")
os.replace(tmp, path)
PY
  elif has node; then
    node - "$file" "$keypath" "$value" "$defaults" 2>/dev/null <<'JS'
const fs = require('fs');
const [path, keypath, value, defaults] = process.argv.slice(2);
let doc = {};
if (fs.existsSync(path) && fs.statSync(path).size > 0) doc = JSON.parse(fs.readFileSync(path, 'utf8'));
for (const [k, v] of Object.entries(JSON.parse(defaults))) if (!(k in doc)) doc[k] = v;
const keys = keypath.split('.'); let cur = doc;
for (const k of keys.slice(0, -1)) cur = cur[k] ??= {};
cur[keys.at(-1)] = JSON.parse(value);
fs.writeFileSync(path + '.tmp', JSON.stringify(doc, null, 2) + '\n'); fs.renameSync(path + '.tmp', path);
JS
  else
    return 2
  fi
}
json_str() { printf '"%s"' "$(printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g')"; }

# Global configs get the absolute binary path (GUI apps do not see your shell PATH). Project configs are often
# committed, so they name `tt-mcp` when it is on PATH.
CMD="$BIN"
if [ "$SCOPE" = project ] && [ "$(command -v tt-mcp 2>/dev/null)" = "$BIN" ]; then CMD="tt-mcp"; fi
C="$(json_str "$CMD")"

manual() { warn "$(label "$1"): could not edit $2 automatically; add this under $3:"; printf '%s\n' "$4" >&2; }
done_tool() { ok "$(label "$1") → $2"; }

append_toml() { # $1 file: Codex, adds the server table unless present
  mkdir -p "$(dirname "$1")"
  if grep -q "^\[mcp_servers\.$SERVER\]" "$1" 2>/dev/null; then
    warn "Codex: [mcp_servers.$SERVER] already in $1, left as is"
  else
    printf '\n[mcp_servers.%s]\ncommand = %s\nargs = []\n' "$SERVER" "$C" >>"$1"
  fi
}

register() { # $1 = tool
  local t="$1" f
  case "$SCOPE:$t" in
    global:claude)
      if has claude; then
        claude mcp remove --scope user "$SERVER" >/dev/null 2>&1 || true
        claude mcp add --scope user "$SERVER" -- "$BIN" >/dev/null && done_tool claude "user scope (claude mcp add)"
      else manual claude "$HOME/.claude.json" "mcpServers" "\"$SERVER\": { \"command\": $C, \"args\": [] }"; fi ;;
    project:claude)
      f="$PROJECT_DIR/.mcp.json"
      merge_json "$f" "mcpServers.$SERVER" "{\"command\": $C, \"args\": []}" && done_tool claude "$f" \
        || manual claude "$f" "mcpServers" "\"$SERVER\": { \"command\": $C, \"args\": [] }" ;;
    global:cursor|project:cursor)
      f="$HOME/.cursor/mcp.json"; [ "$SCOPE" = project ] && f="$PROJECT_DIR/.cursor/mcp.json"
      merge_json "$f" "mcpServers.$SERVER" "{\"type\": \"stdio\", \"command\": $C, \"args\": []}" && done_tool cursor "$f" \
        || manual cursor "$f" "mcpServers" "\"$SERVER\": { \"type\": \"stdio\", \"command\": $C, \"args\": [] }" ;;
    global:antigravity|project:antigravity)
      f="$HOME/.gemini/config/mcp_config.json"; [ "$SCOPE" = project ] && f="$PROJECT_DIR/.agents/mcp_config.json"
      merge_json "$f" "mcpServers.$SERVER" "{\"command\": $C, \"args\": []}" && done_tool antigravity "$f" \
        || manual antigravity "$f" "mcpServers" "\"$SERVER\": { \"command\": $C, \"args\": [] }"
      if [ "$SCOPE" = global ] && [ -f "$HOME/.gemini/antigravity/mcp_config.json" ]; then # older Antigravity IDE builds
        merge_json "$HOME/.gemini/antigravity/mcp_config.json" "mcpServers.$SERVER" "{\"command\": $C, \"args\": []}" || true
      fi ;;
    global:codex)
      if has codex; then
        codex mcp remove "$SERVER" >/dev/null 2>&1 || true
        codex mcp add "$SERVER" -- "$BIN" >/dev/null && done_tool codex "codex mcp add"
      else append_toml "$HOME/.codex/config.toml" && done_tool codex "$HOME/.codex/config.toml"; fi ;;
    project:codex)
      append_toml "$PROJECT_DIR/.codex/config.toml" && done_tool codex "$PROJECT_DIR/.codex/config.toml (trusted projects only)" ;;
    global:opencode|project:opencode)
      f="${XDG_CONFIG_HOME:-$HOME/.config}/opencode/opencode.json"; [ "$SCOPE" = project ] && f="$PROJECT_DIR/opencode.json"
      if [ ! -f "$f" ] && [ -f "${f%.json}.jsonc" ]; then f="${f%.json}.jsonc"; fi
      merge_json "$f" "mcp.$SERVER" "{\"type\": \"local\", \"command\": [$C], \"enabled\": true}" '{"$schema": "https://opencode.ai/config.json"}' \
        && done_tool opencode "$f" || manual opencode "$f" "mcp" "\"$SERVER\": { \"type\": \"local\", \"command\": [$C], \"enabled\": true }" ;;
    global:gemini)
      if has gemini; then
        gemini mcp remove -s user "$SERVER" >/dev/null 2>&1 || true
        gemini mcp add -s user "$SERVER" "$BIN" >/dev/null && done_tool gemini "user scope (gemini mcp add)"
      else f="$HOME/.gemini/settings.json"
        merge_json "$f" "mcpServers.$SERVER" "{\"command\": $C, \"args\": []}" && done_tool gemini "$f" \
          || manual gemini "$f" "mcpServers" "\"$SERVER\": { \"command\": $C, \"args\": [] }"; fi ;;
    project:gemini)
      f="$PROJECT_DIR/.gemini/settings.json"
      merge_json "$f" "mcpServers.$SERVER" "{\"command\": $C, \"args\": []}" && done_tool gemini "$f" \
        || manual gemini "$f" "mcpServers" "\"$SERVER\": { \"command\": $C, \"args\": [] }" ;;
    global:muse|project:muse)
      # Muse Code has no project-level MCP config: the server is registered for the user; skills stay per project.
      f="${XDG_CONFIG_HOME:-$HOME/.config}/muse/settings.json"
      local v
      v="{\"transport\": \"stdio\", \"command\": $(json_str "$BIN"), \"args\": [], \"mode\": \"optional\", \"enabled\": true}"
      merge_json "$f" "mcp_servers.$SERVER" "$v" '{"schema_version": 1}' && done_tool muse "$f" \
        || manual muse "$f" "mcp_servers" "\"$SERVER\": $v" ;;
  esac
}

echo
for t in $TOOLS; do register "$t" || warn "$(label "$t"): registration failed, see above"; done

# ---------------------------------------------------------------- done

echo
printf '%s\n' "$(bold 'Done.')"
info "Restart the tools (or start a new session) so they load the server and the skills."
info 'Try: "Set up alerts for this project" (trooper-alerts) or "why is checkout slow?" (trooper-diagnose).'
[ "$WRITE" = true ] && info "Writes are on: every change is shown as a dry run and needs your approval."
[ "$SCOPE" = project ] && info "Project install: the key is in .tasktrooper/mcp.env (git-ignored). Tools must start servers in the project folder."
info "Change settings later in $ENV_FILE. Docs: https://telemetry.tasktrooper.ai/docs#install"
