#!/bin/sh
# enable_miroxy_for_codex.sh
#
# Generates ~/.codex/miroxy-models.json by merging Codex default models with
# miroxy model_routes, then registers it in ~/.codex/config.toml.
#
# Usage:
#   ./enable_miroxy_for_codex.sh [path/to/config.yaml]
#
# If no path given, searches in order:
#   ../config/config.yaml  (relative to this script)
#   ./config/config.yaml   (current working directory)

set -eu

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
CODEX_DIR="${HOME}/.codex"
CATALOG="${CODEX_DIR}/miroxy-models.json"
TOML="${CODEX_DIR}/config.toml"
BASE_MODELS="${SCRIPT_DIR}/template/codex-default-models.json"
CODEX_MODELS_URL="https://raw.githubusercontent.com/openai/codex/main/codex-rs/models-manager/models.json"
TIMESTAMP="$(date +%Y%m%d_%H%M%S)"
ROUTES_TMP="$(mktemp)"
TAB="$(printf '\t')"

trap 'rm -f "$ROUTES_TMP"' EXIT

# ── Locate config.yaml ────────────────────────────────────────────────────────

if [ -n "${1:-}" ]; then
    CONFIG="$1"
elif [ -f "${SCRIPT_DIR}/../config/config.yaml" ]; then
    CONFIG="$(cd "${SCRIPT_DIR}/../config" && pwd)/config.yaml"
elif [ -f "./config/config.yaml" ]; then
    CONFIG="$(pwd)/config/config.yaml"
else
    echo "error: config.yaml not found." >&2
    echo "" >&2
    echo "usage: $0 [path/to/config.yaml]" >&2
    echo "  $0 /path/to/miroxy/config/config.yaml" >&2
    exit 1
fi

[ -f "$CONFIG" ] || { echo "error: not found: $CONFIG" >&2; exit 1; }
[ -d "$CODEX_DIR" ] || { echo "error: $CODEX_DIR not found — is Codex installed?" >&2; exit 1; }

# ── Parse proxy port from config.yaml ────────────────────────────────────────

PROXY_PORT="$(awk '
  /^server:/{in_s=1; next}
  in_s && /^[a-z]/ && !/^server:/{in_s=0}
  in_s && /^ *port:/{gsub(/.*port: */,""); gsub(/ *$/,""); print; exit}
' "$CONFIG")"
PROXY_PORT="${PROXY_PORT:-9000}"

# ── Pre-flight: OPENAI_API_KEY alignment ─────────────────────────────────────
# Codex uses OPENAI_API_KEY (from env_key in config.toml) as the bearer token
# for every request to miroxy. It must match one of auth.allowed_keys.

preflight_check() {
    echo "──────────────────────────────────────────────────────────────"
    echo "Pre-flight check: Codex auth token"
    echo "──────────────────────────────────────────────────────────────"
    echo ""
    echo "Codex config.toml uses  env_key = \"OPENAI_API_KEY\""
    echo "miroxy proxy address:   http://localhost:${PROXY_PORT}/v1"
    echo ""
    echo "Codex sends OPENAI_API_KEY as a bearer token to miroxy."
    echo "It must match one of the auth.allowed_keys in your config.yaml."
    echo ""

    # Show current OPENAI_API_KEY value (masked)
    CUR_KEY="${OPENAI_API_KEY:-}"
    if [ -z "$CUR_KEY" ]; then
        echo "  Your OPENAI_API_KEY: (not set)"
    else
        masked="****${CUR_KEY#"${CUR_KEY%????}"}"
        echo "  Your OPENAI_API_KEY: $masked"
    fi
    echo ""

    # Show raw auth.allowed_keys from config (no env expansion — miroxy may be remote)
    echo "  auth.allowed_keys in $(basename "$CONFIG"):"
    awk '
    /^auth:/{in_auth=1; next}
    in_auth && /^[a-z]/ && !/^  /{in_auth=0}
    in_auth && /allowed_keys:/{in_keys=1; next}
    in_keys && /^    -/{
        val=$0; sub(/^.*- */,"",val); sub(/ *$/,"",val)
        print "    - " val
    }
    in_keys && !/^    /{in_keys=0}
    ' "$CONFIG"
    echo ""
    echo "  Please verify OPENAI_API_KEY resolves to one of the above."
    echo "  (env vars like \${MIROXY_CLIENT_KEY_1} must be expanded"
    echo "   on the miroxy server — this script cannot check them.)"
    echo ""
    printf "Continue? (yes/no): "
    read -r _ans
    case "$_ans" in
        yes|y|YES|Y) echo "" ;;
        *) echo "Aborted."; exit 0 ;;
    esac
}

preflight_check

echo "enabling miroxy for Codex"
echo "  config:  $CONFIG"
echo "  catalog: $CATALOG"
echo "  port:    $PROXY_PORT"
echo ""

# ── Backup existing config.toml ───────────────────────────────────────────────

if [ -f "$TOML" ]; then
    BAK="${TOML}.bak.${TIMESTAMP}"
    cp "$TOML" "$BAK"
    echo "  backed up config.toml -> $(basename "$BAK")"
fi

# ── Parse model_routes → temp file (tab-separated: slug TAB display TAB desc) ─

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

# ── Build catalog JSON ────────────────────────────────────────────────────────

# json_str: escape backslash and double-quote for JSON strings
json_str() {
    printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g'
}

# ── Fetch latest Codex default models from GitHub (fall back to template) ─────

MODELS_TMP="$(mktemp)"
DOWNLOAD_OK=0
if command -v curl >/dev/null 2>&1; then
    printf "  fetching latest Codex models from GitHub... "
    if curl -fsSL --max-time 10 "$CODEX_MODELS_URL" -o "$MODELS_TMP" 2>/dev/null; then
        # basic sanity check: must contain "models" key
        if grep -q '"models"' "$MODELS_TMP"; then
            DOWNLOAD_OK=1
            echo "ok"
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
            DOWNLOAD_OK=1
            echo "ok"
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
rm -f "$MODELS_TMP"

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

# Remove trailing comma from the last entry, then close the JSON
TMP_CAT="$(mktemp)"
sed '$s/,$//' "$CATALOG" > "$TMP_CAT"
mv "$TMP_CAT" "$CATALOG"
printf '  ]\n}\n' >> "$CATALOG"

echo ""
echo "  wrote $ENTRY_COUNT miroxy model(s) to catalog"

# ── Parse default model from config.yaml ─────────────────────────────────────

DEFAULT_MODEL="$(awk '
    /^server:/{in_s=1; next}
    in_s && /^[a-z]/ && !/^server:/{in_s=0}
    in_s && /default_model:/{
        val=$0; sub(/.*default_model: *"?/,"",val); sub(/"? *$/,"",val); print; exit
    }
' "$CONFIG")"
DEFAULT_MODEL="${DEFAULT_MODEL:-miroxy}"

# ── Update config.toml ────────────────────────────────────────────────────────

# set_toml_toplevel: sets or updates a top-level TOML key (before any sections).
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

# ensure_provider_section: appends [model_providers.miroxy] if not present.
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
    # Create a minimal config.toml with all required settings
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
