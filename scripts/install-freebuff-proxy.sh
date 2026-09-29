#!/usr/bin/env bash
# install-freebuff-proxy.sh - Backward compatibility wrapper for install.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
exec "$SCRIPT_DIR/install.sh" "$@"
