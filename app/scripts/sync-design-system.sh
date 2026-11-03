#!/usr/bin/env bash
# Copy the design system's two stylesheets into the app.
#
# The design system is the single definition of every component and is FROZEN as
# built (113 specimens, lint clean). The app consumes it; it does not fork it.
# These files are therefore vendored by copy and never hand-edited here — a
# change belongs upstream, after which this script is re-run.
#
#   usage: app/scripts/sync-design-system.sh [path-to-design-system-project]
set -euo pipefail

DS="${1:-$HOME/codebases/2026/dockito/design-system/projects/ibeji}"
SRC="$DS/preview"
DEST="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/src/styles"

if [[ ! -d "$SRC" ]]; then
  echo "error: design system not found at $SRC" >&2
  echo "       pass its path: $0 /path/to/design-system/projects/ibeji" >&2
  exit 1
fi

mkdir -p "$DEST"
for f in _foundation.css _components.css; do
  if [[ ! -f "$SRC/$f" ]]; then
    echo "error: $SRC/$f is missing" >&2
    exit 1
  fi
  {
    echo "/* VENDORED from design-system/projects/ibeji/preview/$f"
    echo "   Do not edit here. Change it upstream, then re-run"
    echo "   app/scripts/sync-design-system.sh"
    echo "   Synced: $(date -u +%Y-%m-%dT%H:%M:%SZ) */"
    cat "$SRC/$f"
  } > "$DEST/$f"
  printf '  %-20s %7d bytes\n' "$f" "$(wc -c < "$DEST/$f" | tr -d ' ')"
done

echo "design system synced from $SRC"
