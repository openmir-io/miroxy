#!/bin/sh
# enable_miroxy_for_opencode.sh
#
# Adds or updates a miroxy provider in ~/.config/opencode/opencode.json,
# listing model_routes from miroxy's config.yaml as selectable models.
#
# Usage:
#   ./enable_miroxy_for_opencode.sh [path/to/config.yaml]

set -eu

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
OPENCODE_DIR="${HOME}/.config/opencode"
OPENCODE_CFG="${OPENCODE_DIR}/opencode.json"
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

if ! command -v python3 >/dev/null 2>&1; then
    echo "error: python3 is required for JSON manipulation" >&2
    exit 1
fi

if [ ! -d "$OPENCODE_DIR" ]; then
    echo "error: $OPENCODE_DIR not found — is OpenCode installed?" >&2
    exit 1
fi

# ── Parse proxy port and default model from config.yaml ──────────────────────

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

# ── Pre-flight: auth token check ──────────────────────────────────────────────

echo "──────────────────────────────────────────────────────────────"
echo "Pre-flight check: OpenCode auth token"
echo "──────────────────────────────────────────────────────────────"
echo ""
echo "OpenCode will call miroxy at http://localhost:${PROXY_PORT}/v1"
echo "using MIROXY_AUTH_TOKEN as the bearer token (via {env:MIROXY_AUTH_TOKEN})."
echo ""

CUR_KEY="${MIROXY_AUTH_TOKEN:-}"
if [ -z "$CUR_KEY" ]; then
    echo "  MIROXY_AUTH_TOKEN: (not set)"
else
    masked="****${CUR_KEY#"${CUR_KEY%????}"}"
    echo "  MIROXY_AUTH_TOKEN: $masked"
fi
echo ""
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
echo "  MIROXY_AUTH_TOKEN must match one of the above after env expansion."
echo "  OpenCode config stores it as {env:MIROXY_AUTH_TOKEN} — never plain text."
echo ""
printf "Continue? (yes/no): "
read -r _ans
case "$_ans" in
    yes|y|YES|Y) echo "" ;;
    *) echo "Aborted."; exit 0 ;;
esac

echo "enabling miroxy for OpenCode"
echo "  config:     $CONFIG"
echo "  opencode:   $OPENCODE_CFG"
echo "  port:       $PROXY_PORT"
echo "  def model:  $DEFAULT_MODEL"
echo ""

# ── Backup existing opencode.json ─────────────────────────────────────────────

if [ -f "$OPENCODE_CFG" ]; then
    BAK="${OPENCODE_CFG}.bak.${TIMESTAMP}"
    cp "$OPENCODE_CFG" "$BAK"
    echo "  backed up opencode.json -> $(basename "$BAK")"
fi

# ── Parse model_routes → tab-separated: slug TAB display_name TAB description ─

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

# ── Add/update miroxy provider in opencode.json ───────────────────────────────

python3 - \
    "$OPENCODE_CFG" \
    "$ROUTES_TMP" \
    "$PROXY_PORT" \
    "$DEFAULT_MODEL" \
    "$TIMESTAMP" \
    "$OPENCODE_DIR" \
    <<'PYEOF'
import sys, json, os

cfg_path    = sys.argv[1]
routes_path = sys.argv[2]
port        = sys.argv[3]
def_model   = sys.argv[4]
ts          = sys.argv[5]
cfg_dir     = sys.argv[6]

# Load existing config or start fresh
if os.path.exists(cfg_path):
    with open(cfg_path) as f:
        cfg = json.load(f)
else:
    cfg = {"$schema": "https://opencode.ai/config.json"}

# Parse model_routes
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
    print("  warning: no model_routes found in config.yaml", file=sys.stderr)

# Build miroxy provider entry
miroxy_provider = {
    "npm": "@ai-sdk/openai-compatible",
    "name": "Miroxy",
    "options": {
        "baseURL": f"http://localhost:{port}/v1",
        "apiKey":  "{env:MIROXY_AUTH_TOKEN}"
    },
    "models": models
}

# Inject into config
cfg.setdefault("provider", {})
cfg["provider"]["miroxy"] = miroxy_provider

# Set default model if not already set
if "model" not in cfg:
    cfg["model"] = f"miroxy/{def_model}"

with open(cfg_path, "w") as f:
    json.dump(cfg, f, indent=2)
    f.write("\n")

print(f"  wrote {len(models)} model(s) to opencode.json provider.miroxy")
for slug, m in models.items():
    print(f"    miroxy/{slug:28s}  {m.get('name','')}")
print(f"  default model: {cfg.get('model','')}")
PYEOF

echo ""
echo "Done. Restart OpenCode for the changes to take effect."
echo ""
echo "Make sure MIROXY_AUTH_TOKEN is set in your shell:"
echo "  export MIROXY_AUTH_TOKEN=<your-miroxy-auth-key>"
