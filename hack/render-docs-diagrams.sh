#!/usr/bin/env bash
# Render tracked Mermaid sources; MMDC may point to a separately installed CLI.
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
mmdc=${MMDC:-mmdc}
args=()
if [[ -n ${PUPPETEER_CONFIG:-} ]]; then
  args+=(--puppeteerConfigFile "$PUPPETEER_CONFIG")
fi
for source in "$root"/docs/diagrams/*.mmd; do
  "$mmdc" -i "$source" -o "${source%.mmd}.svg" \
    --theme neutral --backgroundColor white ${args[@]+"${args[@]}"}
done
