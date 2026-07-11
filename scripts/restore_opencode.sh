#!/bin/sh
# restore_opencode.sh
#
# Removes the miroxy provider from ~/.config/opencode/opencode.json and
# restores whatever state existed before enable_miroxy_for_opencode.sh ran,
# using the backup record in ~/.config/opencode/.miroxy.tmp.
#
# Usage:
#   ./restore_opencode.sh

set -eu

OPENCODE_DIR="${HOME}/.config/opencode"
OPENCODE_CFG="${OPENCODE_DIR}/opencode.json"
TRACK_FILE="${OPENCODE_DIR}/.miroxy.tmp"

[ -d "$OPENCODE_DIR" ] || { echo "error: $OPENCODE_DIR not found — is OpenCode installed?" >&2; exit 1; }

echo "disabling miroxy for OpenCode"
echo ""

# ── No backup record: fall back to a surgical removal of provider.miroxy ─────
# (covers the case where enable was never run via the current script, or the
# tracking file was removed manually)

if [ ! -f "$TRACK_FILE" ]; then
    echo "  no miroxy backup record found ($(basename "$TRACK_FILE"))"

    if [ -f "$OPENCODE_CFG" ] && command -v python3 >/dev/null 2>&1; then
        python3 - "$OPENCODE_CFG" <<'PYEOF'
import sys, json

path = sys.argv[1]
with open(path) as f:
    cfg = json.load(f)

removed = False
if "provider" in cfg and "miroxy" in cfg["provider"]:
    del cfg["provider"]["miroxy"]
    if not cfg["provider"]:
        del cfg["provider"]
    removed = True

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
    else
        echo "  skip: opencode.json not found or python3 unavailable"
    fi

    echo ""
    echo "Done. Restart OpenCode for the changes to take effect."
    exit 0
fi

# ── Backup record present: restore exact pre-enable state ────────────────────

CFG_ENTRY="$(awk -F: '/^config:/{print $2; exit}' "$TRACK_FILE")"

if [ -n "${CFG_ENTRY:-}" ] && [ "$CFG_ENTRY" != "none" ] && [ -f "${OPENCODE_DIR}/${CFG_ENTRY}" ]; then
    cp "${OPENCODE_DIR}/${CFG_ENTRY}" "$OPENCODE_CFG"
    rm -f "${OPENCODE_DIR}/${CFG_ENTRY}"
    echo "  restored ${CFG_ENTRY} -> opencode.json"
else
    rm -f "$OPENCODE_CFG"
    echo "  opencode.json: no prior file existed — removed"
fi

rm -f "$TRACK_FILE"
echo "  cleared backup record ($(basename "$TRACK_FILE"))"

echo ""
echo "Done. Restart OpenCode for the changes to take effect."
