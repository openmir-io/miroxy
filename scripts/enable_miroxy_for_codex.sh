#!/bin/sh
# enable_miroxy_for_codex.sh
#
# Generates ~/.codex/miroxy-models.json merging Codex default models with
# miroxy model_routes, then registers it in ~/.codex/config.toml.
#
# Model source (tried in order):
#   1. Path given as $1
#   2. ../config/config.yaml  (relative to this script)
#   3. ./config/config.yaml   (current working directory)
#   4. miroxy admin API at localhost:9001 (requires python3 + curl)
#   5. Prompt for admin URL:port or config.yaml path

set -eu

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
CODEX_DIR="${HOME}/.codex"
CATALOG="${CODEX_DIR}/miroxy-models.json"
TOML="${CODEX_DIR}/config.toml"
TRACK_FILE="${CODEX_DIR}/.miroxy.tmp"
BASE_MODELS="${SCRIPT_DIR}/template/codex-default-models.json"
CODEX_MODELS_URL="https://raw.githubusercontent.com/openai/codex/main/codex-rs/models-manager/models.json"
TIMESTAMP="$(date +%Y%m%d_%H%M%S)"
ROUTES_TMP="$(mktemp)"
MODELS_TMP="$(mktemp)"
API_TMP="$(mktemp)"
PARSE_PY="$(mktemp)"
TAB="$(printf '\t')"

trap 'rm -f "$ROUTES_TMP" "$MODELS_TMP" "$API_TMP" "$PARSE_PY"' EXIT

CONFIG=""
SOURCE_TYPE=""
PROXY_PORT=""
DEFAULT_MODEL=""

# ── Early checks ──────────────────────────────────────────────────────────────

[ -d "$CODEX_DIR" ] || { echo "error: $CODEX_DIR not found — is Codex installed?" >&2; exit 1; }

# ── API parse helper (used only when admin API path is taken) ─────────────────

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
# Codex uses env_key = "OPENAI_API_KEY" in config.toml; miroxy receives it as
# the bearer token. It must match one of miroxy's auth.allowed_keys.

auth_preflight() {
    CUR_KEY="${OPENAI_API_KEY:-}"

    echo "──────────────────────────────────────────────────────────────"
    echo "Pre-flight: Codex auth token (OPENAI_API_KEY)"
    echo "──────────────────────────────────────────────────────────────"
    echo ""

    if [ -z "$CUR_KEY" ]; then
        echo "  OPENAI_API_KEY is not set in your environment."
        echo ""
        echo "  Codex sends this as a bearer token to miroxy."
        echo "  It must match one of miroxy's auth.allowed_keys."
        echo "  Tip: check your miroxy secrets file for an allowed key."
        echo ""
        printf "  Set OPENAI_API_KEY now? (yes/no): "
        read -r _ans
        case "$_ans" in
            yes|y|YES|Y)
                printf "  AUTH KEY> "
                IFS= read -r _key
                [ -n "$_key" ] || { echo "No key entered. Aborted."; exit 1; }
                export OPENAI_API_KEY="$_key"
                echo ""
                ;;
            *)
                echo "Aborted."
                exit 0
                ;;
        esac
    else
        _masked="****${CUR_KEY#"${CUR_KEY%????}"}"
        echo "  OPENAI_API_KEY: ${_masked}"
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
    command -v python3 >/dev/null 2>&1 || return 1
    command -v curl    >/dev/null 2>&1 || return 1
    _tok="${OPENAI_API_KEY:-}"
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
        if (slug != "") print slug "\t" (dname != "" ? dname : slug) "\t" (desc != "" ? desc : "Routed via miroxy")
        in_routes=0; slug=""
    }
    in_routes && /^  - model_name:/ {
        if (slug != "") print slug "\t" (dname != "" ? dname : slug) "\t" (desc != "" ? desc : "Routed via miroxy")
        slug=$0; sub(/^.*model_name: *"?/,"",slug); sub(/"? *$/,"",slug)
        dname=""; desc=""; next
    }
    in_routes && slug != "" && /^ +display_name:/ {
        dname=$0; sub(/^.*display_name: *"?/,"",dname); sub(/"? *$/,"",dname); next
    }
    in_routes && slug != "" && /^ +description:/ {
        desc=$0; sub(/^.*description: *"?/,"",desc); sub(/"? *$/,"",desc); next
    }
    END { if (slug != "") print slug "\t" (dname != "" ? dname : slug) "\t" (desc != "" ? desc : "Routed via miroxy") }
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

echo "enabling miroxy for Codex"
if [ "$SOURCE_TYPE" = "yaml" ]; then
    echo "  source:  $CONFIG"
else
    echo "  source:  miroxy admin API"
fi
echo "  catalog: $CATALOG"
echo "  port:    $PROXY_PORT"
echo ""

# ── Backup pre-existing config.toml / miroxy-models.json (once) ──────────────
# A backup is taken only the first time; while .miroxy.tmp exists, later
# enable runs overwrite the current files directly instead of piling up
# timestamped backups. restore_codex.sh consumes and clears this record.

if [ -f "$TRACK_FILE" ]; then
    echo "  existing backup record found -> $(basename "$TRACK_FILE") (skipping new backup)"
else
    CFG_ENTRY="none"
    if [ -f "$TOML" ]; then
        CFG_BAK="config.toml.backup${TIMESTAMP}"
        cp "$TOML" "${CODEX_DIR}/${CFG_BAK}"
        CFG_ENTRY="$CFG_BAK"
        echo "  backed up config.toml -> ${CFG_BAK}"
    fi

    MODELS_ENTRY="none"
    if [ -f "$CATALOG" ]; then
        MODELS_BAK="miroxy-models.json.backup${TIMESTAMP}"
        cp "$CATALOG" "${CODEX_DIR}/${MODELS_BAK}"
        MODELS_ENTRY="$MODELS_BAK"
        echo "  backed up miroxy-models.json -> ${MODELS_BAK}"
    fi

    {
        echo "[backup - ${TIMESTAMP}]"
        echo "config:${CFG_ENTRY}"
        echo "models:${MODELS_ENTRY}"
    } > "$TRACK_FILE"
    echo "  recorded backup state -> $(basename "$TRACK_FILE")"
fi

# ── Build catalog JSON ────────────────────────────────────────────────────────

json_str() {
    printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g'
}

DOWNLOAD_OK=0
if command -v curl >/dev/null 2>&1; then
    printf "  fetching latest Codex models from GitHub... "
    if curl -fsSL --max-time 10 "$CODEX_MODELS_URL" -o "$MODELS_TMP" 2>/dev/null; then
        if grep -q '"models"' "$MODELS_TMP"; then
            DOWNLOAD_OK=1; echo "ok"
        else
            echo "invalid JSON, using template"
        fi
    else
        echo "failed, using template"
    fi
elif command -v wget >/dev/null 2>&1; then
    printf "  fetching latest Codex models from GitHub... "
    if wget -qO "$MODELS_TMP" --timeout=10 "$CODEX_MODELS_URL" 2>/dev/null; then
        if grep -q '"models"' "$MODELS_TMP"; then
            DOWNLOAD_OK=1; echo "ok"
        else
            echo "invalid JSON, using template"
        fi
    else
        echo "failed, using template"
    fi
fi

if [ "$DOWNLOAD_OK" = "1" ]; then
    ACTIVE_BASE="$MODELS_TMP"
elif [ -f "$BASE_MODELS" ]; then
    echo "  using bundled template: $(basename "$BASE_MODELS")"
    ACTIVE_BASE="$BASE_MODELS"
else
    echo "warning: no base models found; catalog will contain only miroxy models" >&2
    ACTIVE_BASE=""
fi

if [ -n "$ACTIVE_BASE" ]; then
    NLINES="$(wc -l < "$ACTIVE_BASE")"
    head -n "$((NLINES - 2))" "$ACTIVE_BASE" | \
        sed '$s/^    }$/    },/' > "$CATALOG"
else
    printf '{\n  "models": [\n' > "$CATALOG"
fi

ENTRY_COUNT=0
while IFS="$TAB" read -r slug dname desc; do
    [ -n "$slug" ] || continue

    cat >> "$CATALOG" << ENTRY
    {
      "slug":         "$(json_str "$slug")",
      "display_name": "$(json_str "$dname")",
      "description":  "$(json_str "$desc")",
      "visibility":   "list",
      "supported_in_api": true,
      "priority": $ENTRY_COUNT,
      "shell_type": "shell_command",
      "base_instructions": "You are a helpful AI assistant routed via miroxy.",
      "supported_reasoning_levels": [
        {"effort": "low",    "description": "Fast"},
        {"effort": "medium", "description": "Balanced"},
        {"effort": "high",   "description": "Thorough"}
      ],
      "default_reasoning_level": "medium",
      "supports_reasoning_summaries": false,
      "support_verbosity": false,
      "default_verbosity": null,
      "apply_patch_tool_type": null,
      "truncation_policy": {"mode": "tokens", "limit": 10000},
      "supports_parallel_tool_calls": false,
      "experimental_supported_tools": [],
      "availability_nux": null,
      "upgrade": null,
      "context_window": 1000000,
      "max_context_window": 1000000,
      "input_modalities": ["text"]
    },
ENTRY

    ENTRY_COUNT="$((ENTRY_COUNT + 1))"
    echo "  + $slug ($dname)"
done < "$ROUTES_TMP"

TMP_CAT="$(mktemp)"
sed '$s/,$//' "$CATALOG" > "$TMP_CAT"
mv "$TMP_CAT" "$CATALOG"
printf '  ]\n}\n' >> "$CATALOG"

echo ""
echo "  wrote $ENTRY_COUNT miroxy model(s) to catalog"

# ── Update config.toml ────────────────────────────────────────────────────────

set_toml_toplevel() {
    local key="$1" value="$2" file="$3"
    local line="${key} = \"${value}\""
    local tmp
    if grep -q "^${key}" "$file"; then
        tmp="$(mktemp)"
        sed "s|^${key}.*|${line}|" "$file" > "$tmp" && mv "$tmp" "$file"
    elif grep -q "^\[" "$file"; then
        tmp="$(mktemp)"
        awk -v l="$line" 'BEGIN{d=0} !d && /^\[/{print l; print ""; d=1} {print}' \
            "$file" > "$tmp" && mv "$tmp" "$file"
    else
        printf '\n%s\n' "$line" >> "$file"
    fi
}

ensure_provider_section() {
    local file="$1"
    if grep -q '^\[model_providers\.miroxy\]' "$file" 2>/dev/null; then
        echo "  [model_providers.miroxy] already present in config.toml"
        return
    fi
    cat >> "$file" << SECTION

[model_providers.miroxy]
name = "miroxy"
base_url = "http://localhost:${PROXY_PORT}/v1"
env_key = "OPENAI_API_KEY"
wire_api = "responses"
SECTION
    echo "  added [model_providers.miroxy] (base_url=localhost:${PROXY_PORT}, env_key=OPENAI_API_KEY)"
}

if [ ! -f "$TOML" ]; then
    cat > "$TOML" << TOMLCONTENT
model_catalog_json = "${CATALOG}"
model = "${DEFAULT_MODEL}"
model_provider = "miroxy"

[model_providers.miroxy]
name = "miroxy"
base_url = "http://localhost:${PROXY_PORT}/v1"
env_key = "OPENAI_API_KEY"
wire_api = "responses"
TOMLCONTENT
    echo "  created config.toml"
    echo "    model_catalog_json → ${CATALOG}"
    echo "    model              → ${DEFAULT_MODEL}"
    echo "    model_provider     → miroxy"
    echo "    base_url           → http://localhost:${PROXY_PORT}/v1"
else
    set_toml_toplevel "model_catalog_json" "$CATALOG"       "$TOML"
    echo "  set model_catalog_json"
    set_toml_toplevel "model"              "$DEFAULT_MODEL"  "$TOML"
    echo "  set model = ${DEFAULT_MODEL}"
    set_toml_toplevel "model_provider"     "miroxy"          "$TOML"
    echo "  set model_provider = miroxy"
    ensure_provider_section "$TOML"
fi

echo ""
echo "Done. Restart Codex to load the new model catalog."
