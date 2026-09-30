#!/usr/bin/env bash
# revert-warp-egress.sh — take upstream egress off WARP, back to direct.
# Reversible complement of setup-warp-egress.sh. Restarts the proxy.
#
# Usage: sudo ./scripts/revert-warp-egress.sh [--keep-warp]
#   --keep-warp   leave warp connected + relay running (default: disconnect
#                 warp and kill the relay)
set -euo pipefail

KEEP=0
[[ "${1:-}" == "--keep-warp" ]] && KEEP=1

ENV_FILE="$HOME/freebuff-proxy/.env"
[[ -f "$ENV_FILE" ]] || ENV_FILE="./.env"

echo "==> [1/3] removing UPSTREAM_EGRESS_PROXY"
if [[ -f "$ENV_FILE" ]] && grep -q "^UPSTREAM_EGRESS_PROXY=" "$ENV_FILE"; then
  sed -i '/^UPSTREAM_EGRESS_PROXY=/d' "$ENV_FILE"
  echo "    knob removed from $ENV_FILE"
else
  echo "    knob not set, nothing to remove"
fi

if ((KEEP == 0)); then
  echo "==> [2/3] disconnecting warp + stopping relay"
  warp-cli --accept-tos disconnect >/dev/null 2>&1 || true
  pkill -f "socat.*LISTEN.*:4000" 2>/dev/null || true
  echo "    warp disconnected, relay stopped"
else
  echo "==> [2/3] keeping warp connected (--keep-warp)"
fi

echo "==> [3/3] restarting proxy on direct egress"
cd ~/freebuff-proxy 2>/dev/null || cd .
docker compose restart | tail -n 1
sleep 12
curl -s --max-time 5 http://127.0.0.1:3457/healthz -o /dev/null -w "healthz: %{http_code}\n"
