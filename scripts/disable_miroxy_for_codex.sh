#!/bin/sh
# disable_miroxy_for_codex.sh
#
# Removes miroxy integration from Codex:
#   - Deletes ~/.codex/config.toml
#   - Deletes ~/.codex/miroxy-models.json
#   - Restores the most recent config.toml backup (if one exists)
#
# Usage:
#   ./disable_miroxy_for_codex.sh

set -eu

CODEX_DIR="${HOME}/.codex"
TOML="${CODEX_DIR}/config.toml"
CATALOG="${CODEX_DIR}/miroxy-models.json"

[ -d "$CODEX_DIR" ] || { echo "error: $CODEX_DIR not found — is Codex installed?" >&2; exit 1; }

echo "disabling miroxy for Codex"
echo ""

if [ -f "$CATALOG" ]; then
    rm "$CATALOG"
    echo "  removed miroxy-models.json"
else
    echo "  skip: miroxy-models.json not found"
fi

if [ -f "$TOML" ]; then
    rm "$TOML"
    echo "  removed config.toml"
else
    echo "  skip: config.toml not found"
fi

LATEST_BAK="$(ls -t "${CODEX_DIR}/config.toml.bak."* 2>/dev/null | head -1 || true)"

if [ -n "$LATEST_BAK" ]; then
    cp "$LATEST_BAK" "$TOML"
    echo "  restored $(basename "$LATEST_BAK") -> config.toml"
else
    echo "  no backup found — config.toml left absent (Codex will use defaults)"
fi

echo ""
echo "Done. Restart Codex for the changes to take effect."
