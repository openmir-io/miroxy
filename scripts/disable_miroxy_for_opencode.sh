#!/bin/sh
# disable_miroxy_for_opencode.sh
#
# Removes the miroxy provider from ~/.config/opencode/opencode.json
# and restores the most recent backup if one exists.
#
# Usage:
#   ./disable_miroxy_for_opencode.sh

set -eu

OPENCODE_DIR="${HOME}/.config/opencode"
OPENCODE_CFG="${OPENCODE_DIR}/opencode.json"

[ -d "$OPENCODE_DIR" ] || { echo "error: $OPENCODE_DIR not found — is OpenCode installed?" >&2; exit 1; }

if ! command -v python3 >/dev/null 2>&1; then
    echo "error: python3 is required for JSON manipulation" >&2
    exit 1
fi

echo "disabling miroxy for OpenCode"
echo ""

if [ ! -f "$OPENCODE_CFG" ]; then
    echo "  skip: opencode.json not found"
else
    python3 - "$OPENCODE_CFG" <<'PYEOF'
import sys, json

path = sys.argv[1]
with open(path) as f:
    cfg = json.load(f)

removed = False
if "provider" in cfg and "miroxy" in cfg["provider"]:
    del cfg["provider"]["miroxy"]
    if not cfg["provider"]:          # remove empty providers object
        del cfg["provider"]
    removed = True

# If default model was miroxy/*, clear it
model = cfg.get("model", "")
if model.startswith("miroxy/"):
    del cfg["model"]
    print(f"  cleared default model: {model}")

with open(path, "w") as f:
    json.dump(cfg, f, indent=2)
    f.write("\n")

if removed:
    print("  removed provider.miroxy from opencode.json")
else:
    print("  skip: provider.miroxy not found in opencode.json")
PYEOF
fi

# ── Restore most recent backup ────────────────────────────────────────────────

LATEST_BAK="$(ls -t "${OPENCODE_DIR}/opencode.json.bak."* 2>/dev/null | head -1 || true)"

if [ -n "$LATEST_BAK" ]; then
    cp "$LATEST_BAK" "$OPENCODE_CFG"
    echo "  restored $(basename "$LATEST_BAK") -> opencode.json"
else
    echo "  no backup found — current opencode.json kept as-is"
fi

echo ""
echo "Done. Restart OpenCode for the changes to take effect."
