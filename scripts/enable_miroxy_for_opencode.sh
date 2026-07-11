#!/bin/sh
# enable_miroxy_for_opencode.sh
#
# Adds or updates a miroxy provider in ~/.config/opencode/opencode.json,
# listing model_routes from miroxy as selectable models.
#
# Model source (tried in order):
#   1. Path given as $1
#   2. ../config/config.yaml  (relative to this script)
#   3. ./config/config.yaml   (current working directory)
#   4. miroxy admin API at localhost:9001
#   5. Prompt for admin URL:port or config.yaml path

set -eu

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
OPENCODE_DIR="${HOME}/.config/opencode"
OPENCODE_CFG="${OPENCODE_DIR}/opencode.json"
TRACK_FILE="${OPENCODE_DIR}/.miroxy.tmp"
TIMESTAMP="$(date +%Y%m%d_%H%M%S)"
ROUTES_TMP="$(mktemp)"
API_TMP="$(mktemp)"
PARSE_PY="$(mktemp)"
TAB="$(printf '\t')"

trap 'rm -f "$ROUTES_TMP" "$API_TMP" "$PARSE_PY"' EXIT

CONFIG=""
SOURCE_TYPE=""
PROXY_PORT=""
DEFAULT_MODEL=""

# ── Early checks ──────────────────────────────────────────────────────────────

if ! command -v python3 >/dev/null 2>&1; then
    echo "error: python3 is required for JSON manipulation" >&2
    exit 1
fi

if [ ! -d "$OPENCODE_DIR" ]; then
    echo "error: $OPENCODE_DIR not found — is OpenCode installed?" >&2
    exit 1
fi

# ── API parse helper ──────────────────────────────────────────────────────────

cat > "$PARSE_PY" << 'PYEOF'
import sys, json

api_file    = sys.argv[1]
routes_file = sys.argv[2]

try:
    with open(api_file) as f:
        data = json.load(f)
except Exception:
    sys.exit(1)

routes = data.get("model_routes", [])
if not routes:
    sys.exit(1)

with open(routes_file, "w") as f:
    for r in routes:
        slug = r.get("model_name", "")
        if not slug:
            continue
        dname = r.get("display_name") or slug
        desc  = r.get("description") or "Routed via miroxy"
        f.write("{}\t{}\t{}\n".format(slug, dname, desc))

server    = data.get("server", {})
port      = str(server.get("port") or "9000")
def_model = str(server.get("default_model") or "miroxy")
print("{} {}".format(port, def_model))
PYEOF

# ── Auth preflight ────────────────────────────────────────────────────────────
# OpenCode uses {env:MIROXY_AUTH_TOKEN} as the bearer token to miroxy.
# It must match one of miroxy's auth.allowed_keys.

auth_preflight() {
    CUR_KEY="${MIROXY_AUTH_TOKEN:-}"

    echo "──────────────────────────────────────────────────────────────"
    echo "Pre-flight: OpenCode auth token (MIROXY_AUTH_TOKEN)"
    echo "──────────────────────────────────────────────────────────────"
    echo ""

    if [ -z "$CUR_KEY" ]; then
        echo "  MIROXY_AUTH_TOKEN is not set in your environment."
        echo ""
        echo "  OpenCode sends this as a bearer token to miroxy."
        echo "  It must match one of miroxy's auth.allowed_keys."
        echo "  Tip: check your miroxy secrets file for an allowed key."
        echo ""
        printf "  Set MIROXY_AUTH_TOKEN now? (yes/no): "
        read -r _ans
        case "$_ans" in
            yes|y|YES|Y)
                printf "  AUTH KEY> "
                IFS= read -r _key
                [ -n "$_key" ] || { echo "No key entered. Aborted."; exit 1; }
                export MIROXY_AUTH_TOKEN="$_key"
                echo ""
                ;;
            *)
                echo "Aborted."
                exit 0
                ;;
        esac
    else
        _masked="****${CUR_KEY#"${CUR_KEY%????}"}"
        echo "  MIROXY_AUTH_TOKEN: ${_masked}"
        echo ""
        echo "  This must match one of miroxy's auth.allowed_keys."
        echo "  (env vars in config like \${MY_KEY} are expanded on the miroxy server;"
        echo "   this script cannot verify them.)"
        echo ""
        printf "  Continue? (yes/no): "
        read -r _ans
        case "$_ans" in
            yes|y|YES|Y) echo "" ;;
            *) echo "Aborted."; exit 0 ;;
        esac
    fi
}

# ── Admin API probe ───────────────────────────────────────────────────────────

_try_admin_api() {
    _h="$1"
    command -v curl >/dev/null 2>&1 || return 1
    _tok="${MIROXY_AUTH_TOKEN:-}"
    [ -n "$_tok" ] || return 1

    curl -fsS --max-time 5 \
        -H "Authorization: Bearer ${_tok}" \
        -o "$API_TMP" "http://${_h}/v1/config" 2>/dev/null || return 1

    _out="$(python3 "$PARSE_PY" "$API_TMP" "$ROUTES_TMP" 2>/dev/null)" || return 1
    [ -n "$_out" ] || return 1

    PROXY_PORT="${_out%% *}"
    DEFAULT_MODEL="${_out#* }"
    SOURCE_TYPE="api"
    return 0
}

# ── YAML helpers ──────────────────────────────────────────────────────────────

_parse_yaml_meta() {
    PROXY_PORT="$(awk '
        /^server:/{in_s=1; next}
        in_s && /^[a-z]/ && !/^server:/{in_s=0}
        in_s && /^ *port:/{gsub(/.*port: */,""); gsub(/ *$/,""); print; exit}
    ' "$CONFIG")"
    PROXY_PORT="${PROXY_PORT:-9000}"

    DEFAULT_MODEL="$(awk '
        /^server:/{in_s=1; next}
        in_s && /^[a-z]/ && !/^server:/{in_s=0}
        in_s && /default_model:/{
            val=$0; sub(/.*default_model: *"?/,"",val); sub(/"? *$/,"",val); print; exit
        }
    ' "$CONFIG")"
    DEFAULT_MODEL="${DEFAULT_MODEL:-miroxy}"
}

_parse_yaml_routes() {
    awk '
    BEGIN { in_routes=0; slug=""; dname=""; desc="" }
    /^model_routes:/ { in_routes=1; next }
    in_routes && /^[a-zA-Z_]/ {
        if (slug != "") print slug "\t" (dname!=""?dname:slug) "\t" (desc!=""?desc:"Routed via miroxy")
        in_routes=0; slug=""
    }
    in_routes && /^  - model_name:/ {
        if (slug != "") print slug "\t" (dname!=""?dname:slug) "\t" (desc!=""?desc:"Routed via miroxy")
        slug=$0; sub(/^.*model_name: *"?/,"",slug); sub(/"? *$/,"",slug)
        dname=""; desc=""; next
    }
    in_routes && slug != "" && /^ +display_name:/ {
        dname=$0; sub(/^.*display_name: *"?/,"",dname); sub(/"? *$/,"",dname); next
    }
    in_routes && slug != "" && /^ +description:/ {
        desc=$0; sub(/^.*description: *"?/,"",desc); sub(/"? *$/,"",desc); next
    }
    END { if (slug != "") print slug "\t" (dname!=""?dname:slug) "\t" (desc!=""?desc:"Routed via miroxy") }
    ' "$CONFIG" > "$ROUTES_TMP"
}

# ── Resolve model source ──────────────────────────────────────────────────────

resolve_model_source() {
    # 1. CLI arg
    if [ -n "${1:-}" ]; then
        [ -f "$1" ] || { echo "error: not found: $1" >&2; exit 1; }
        CONFIG="$1"; SOURCE_TYPE="yaml"
        _parse_yaml_meta; _parse_yaml_routes; return
    fi

    # 2. Auto-discover config.yaml
    if [ -f "${SCRIPT_DIR}/../config/config.yaml" ]; then
        CONFIG="$(cd "${SCRIPT_DIR}/../config" && pwd)/config.yaml"
        SOURCE_TYPE="yaml"; _parse_yaml_meta; _parse_yaml_routes; return
    fi
    if [ -f "./config/config.yaml" ]; then
        CONFIG="$(pwd)/config/config.yaml"
        SOURCE_TYPE="yaml"; _parse_yaml_meta; _parse_yaml_routes; return
    fi

    # 3. Try admin API on localhost defaults
    echo "  config.yaml not found; probing miroxy admin API..."
    for _addr in "localhost:9001" "127.0.0.1:9001"; do
        printf "    http://${_addr}/v1/config ... "
        if _try_admin_api "$_addr"; then
            echo "ok"; return
        fi
        echo "failed"
    done

    # 4. Prompt: admin URL:port or config.yaml path
    echo ""
    echo "  Could not reach miroxy admin API on localhost:9001."
    echo "  If miroxy is running on a different host/port, enter its admin URL:port."
    echo "  If the instance is not running, enter the path to config.yaml."
    echo ""
    printf "  Admin URL:port or config.yaml path> "
    read -r _input
    [ -n "$_input" ] || { echo "error: no input provided" >&2; exit 1; }

    # Detect host:port vs file path
    _h=""
    case "$_input" in
        http://*|https://*)
            _h="${_input#http://}"; _h="${_h#https://}"; _h="${_h%%/*}"
            ;;
        *:*)
            _h="$_input"
            ;;
    esac

    if [ -n "$_h" ]; then
        printf "    http://${_h}/v1/config ... "
        if _try_admin_api "$_h"; then
            echo "ok"; return
        fi
        echo "failed"
        printf "  Enter path to config.yaml: "
        read -r _input
    fi

    [ -f "$_input" ] || { echo "error: not found: ${_input}" >&2; exit 1; }
    CONFIG="$_input"; SOURCE_TYPE="yaml"
    _parse_yaml_meta; _parse_yaml_routes
}

# ── Main ──────────────────────────────────────────────────────────────────────

auth_preflight
resolve_model_source "${1:-}"

echo "enabling miroxy for OpenCode"
if [ "$SOURCE_TYPE" = "yaml" ]; then
    echo "  source:   $CONFIG"
else
    echo "  source:   miroxy admin API"
fi
echo "  opencode: $OPENCODE_CFG"
echo "  port:     $PROXY_PORT"
echo "  model:    $DEFAULT_MODEL"
echo ""

# ── Backup pre-existing opencode.json (once) ──────────────────────────────────
# A backup is taken only the first time; while .miroxy.tmp exists, later
# enable runs overwrite opencode.json directly instead of piling up
# timestamped backups. restore_opencode.sh consumes and clears this record.

if [ -f "$TRACK_FILE" ]; then
    echo "  existing backup record found -> $(basename "$TRACK_FILE") (skipping new backup)"
else
    CFG_ENTRY="none"
    if [ -f "$OPENCODE_CFG" ]; then
        CFG_BAK="opencode.json.backup${TIMESTAMP}"
        cp "$OPENCODE_CFG" "${OPENCODE_DIR}/${CFG_BAK}"
        CFG_ENTRY="$CFG_BAK"
        echo "  backed up opencode.json -> ${CFG_BAK}"
    fi

    {
        echo "[backup - ${TIMESTAMP}]"
        echo "config:${CFG_ENTRY}"
    } > "$TRACK_FILE"
    echo "  recorded backup state -> $(basename "$TRACK_FILE")"
fi

# ── Add/update miroxy provider in opencode.json ───────────────────────────────

python3 - \
    "$OPENCODE_CFG" \
    "$ROUTES_TMP" \
    "$PROXY_PORT" \
    "$DEFAULT_MODEL" \
    "$OPENCODE_DIR" \
    <<'PYEOF'
import sys, json, os

cfg_path    = sys.argv[1]
routes_path = sys.argv[2]
port        = sys.argv[3]
def_model   = sys.argv[4]
cfg_dir     = sys.argv[5]

if os.path.exists(cfg_path):
    with open(cfg_path) as f:
        cfg = json.load(f)
else:
    cfg = {"$schema": "https://opencode.ai/config.json"}

models = {}
with open(routes_path) as f:
    for line in f:
        parts = line.rstrip('\n').split('\t')
        if len(parts) >= 2:
            slug, name = parts[0], parts[1]
            desc = parts[2] if len(parts) > 2 else ""
            entry = {"name": name}
            if desc:
                entry["description"] = desc
            models[slug] = entry

if not models:
    print("  warning: no model_routes found", file=sys.stderr)

miroxy_provider = {
    "npm": "@ai-sdk/openai-compatible",
    "name": "Miroxy",
    "options": {
        "baseURL": "http://localhost:{}/v1".format(port),
        "apiKey":  "{env:MIROXY_AUTH_TOKEN}"
    },
    "models": models
}

cfg.setdefault("provider", {})
cfg["provider"]["miroxy"] = miroxy_provider

if "model" not in cfg:
    cfg["model"] = "miroxy/{}".format(def_model)

with open(cfg_path, "w") as f:
    json.dump(cfg, f, indent=2)
    f.write("\n")

print("  wrote {} model(s) to opencode.json provider.miroxy".format(len(models)))
for slug, m in models.items():
    print("    miroxy/{:<28}  {}".format(slug, m.get("name", "")))
print("  default model: {}".format(cfg.get("model", "")))
PYEOF

echo ""
echo "Done. Restart OpenCode for the changes to take effect."
echo ""
echo "Make sure MIROXY_AUTH_TOKEN is set in your shell before starting OpenCode:"
echo "  export MIROXY_AUTH_TOKEN=<your-miroxy-auth-key>"
echo "  (add to ~/.bashrc or ~/.zshrc for a permanent setting)"
