#!/bin/sh
# restore_codex.sh
#
# Removes miroxy integration from Codex and restores whatever config.toml /
# miroxy-models.json state existed before enable_miroxy_for_codex.sh ran,
# using the backup record in ~/.codex/.miroxy.tmp.
#
# Usage:
#   ./restore_codex.sh

set -eu

CODEX_DIR="${HOME}/.codex"
TOML="${CODEX_DIR}/config.toml"
CATALOG="${CODEX_DIR}/miroxy-models.json"
TRACK_FILE="${CODEX_DIR}/.miroxy.tmp"

[ -d "$CODEX_DIR" ] || { echo "error: $CODEX_DIR not found — is Codex installed?" >&2; exit 1; }

echo "disabling miroxy for Codex"
echo ""

rm -f "$CATALOG" "$TOML"
echo "  removed config.toml and miroxy-models.json (if present)"

if [ ! -f "$TRACK_FILE" ]; then
    echo "  no miroxy backup record found ($(basename "$TRACK_FILE")) — nothing to restore"
    echo ""
    echo "Done. Restart Codex for the changes to take effect."
    exit 0
fi

CFG_ENTRY="$(awk -F: '/^config:/{print $2; exit}' "$TRACK_FILE")"
MODELS_ENTRY="$(awk -F: '/^models:/{print $2; exit}' "$TRACK_FILE")"

if [ -n "${CFG_ENTRY:-}" ] && [ "$CFG_ENTRY" != "none" ] && [ -f "${CODEX_DIR}/${CFG_ENTRY}" ]; then
    cp "${CODEX_DIR}/${CFG_ENTRY}" "$TOML"
    rm -f "${CODEX_DIR}/${CFG_ENTRY}"
    echo "  restored ${CFG_ENTRY} -> config.toml"
else
    echo "  config.toml: no prior file existed — left absent"
fi

if [ -n "${MODELS_ENTRY:-}" ] && [ "$MODELS_ENTRY" != "none" ] && [ -f "${CODEX_DIR}/${MODELS_ENTRY}" ]; then
    cp "${CODEX_DIR}/${MODELS_ENTRY}" "$CATALOG"
    rm -f "${CODEX_DIR}/${MODELS_ENTRY}"
    echo "  restored ${MODELS_ENTRY} -> miroxy-models.json"
else
    echo "  miroxy-models.json: no prior file existed — left absent"
fi

rm -f "$TRACK_FILE"
echo "  cleared backup record ($(basename "$TRACK_FILE"))"

echo ""
echo "Done. Restart Codex for the changes to take effect."
