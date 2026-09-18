#!/usr/bin/env bash
# monitor-control.sh — convenient runner for monitor-control.py
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

PYTHON_BIN="python3"
if ! command -v python3 >/dev/null 2>&1; then
  if command -v python >/dev/null 2>&1; then
    PYTHON_BIN="python"
  else
    echo "ERROR: python3 or python required" >&2
    exit 1
  fi
fi

exec "$PYTHON_BIN" "$SCRIPT_DIR/monitor-control.py" \
  --key-file "$REPO_ROOT/api-keys_freebuff_vps-sg" \
  "$@"
