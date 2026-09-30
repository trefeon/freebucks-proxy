#!/usr/bin/env bash
# setup-warp-egress.sh — put upstream egress behind local Cloudflare WARP
# (proxy mode ONLY: no routes change, SSH/Tailscale unaffected).
# NEVER run warp full-tunnel on a remote box: it grabs the default route.
#
# Usage: sudo ./scripts/setup-warp-egress.sh [--port 40000] [--relay-port 40001]
#   --port        host-local SOCKS port warp listens on (default 40000)
#   --relay-port  bridge-visible relay port for the container (default 40001)
#
# What it does:
#   1. installs cloudflare-warp (apt, idempotent)
#   2. ensures warp-svc, registers, sets proxy mode, connects
#   3. starts a socat relay 0.0.0.0:<relay> -> 127.0.0.1:<port> (containers
#      cannot see host-localhost, so the relay must sit on the bridge)
#   4. prints the exit IP + the exact UPSTREAM_EGRESS_PROXY line to set
#
# It does NOT flip the proxy knob or restart anything: flipping is a separate
# deliberate step (see docs/EGRESS.md "Flipping the proxy").
set -euo pipefail

PORT=40000
RELAY=40001
while [[ $# -gt 0 ]]; do
  case "$1" in
    --port) PORT="$2"; shift 2 ;;
    --relay-port) RELAY="$2"; shift 2 ;;
    *) echo "unknown arg: $1" >&2; exit 1 ;;
  esac
done

echo "==> [1/4] installing cloudflare-warp"
if ! command -v warp-cli >/dev/null 2>&1; then
  curl -fsSL https://pkg.cloudflareclient.com/pubkey.gpg \
    | gpg --yes --dearmor -o /usr/share/keyrings/cloudflare-warp-archive-keyring.gpg
  CODENAME="$(lsb_release -cs 2>/dev/null || echo bookworm)"
  echo "deb [signed-by=/usr/share/keyrings/cloudflare-warp-archive-keyring.gpg] https://pkg.cloudflareclient.com/ $CODENAME main" \
    > /etc/apt/sources.list.d/cloudflare-client.list
  apt-get update -qq
  apt-get install -y -qq cloudflare-warp socat
else
  echo "    warp-cli present, skipping install"
  command -v socat >/dev/null 2>&1 || apt-get install -y -qq socat
fi

echo "==> [2/4] warp-svc + register + proxy mode"
pgrep -x warp-svc >/dev/null || {
  if command -v systemctl >/dev/null 2>&1 && systemctl list-unit-files 2>/dev/null | grep -q warp-svc; then
    systemctl start warp-svc
  else
    nohup warp-svc >/tmp/warp-svc.log 2>&1 &
  fi
  sleep 3
}
warp-cli --accept-tos registration new >/dev/null 2>&1 || true
warp-cli --accept-tos mode proxy
warp-cli --accept-tos proxy port "$PORT"
warp-cli --accept-tos connect
sleep 8

echo "==> [3/4] socat relay 0.0.0.0:$RELAY -> 127.0.0.1:$PORT"
if ! pgrep -f "socat.*LISTEN.*:$RELAY" >/dev/null 2>&1; then
  nohup socat "TCP-LISTEN:$RELAY,bind=0.0.0.0,reuseaddr,fork" "TCP:127.0.0.1:$PORT" \
    >/tmp/socat-warp.log 2>&1 &
  sleep 2
fi
ss -tln | grep -E ":$RELAY" || { echo "relay failed to listen" >&2; exit 1; }

echo "==> [4/4] exit IP via relay"
GATEWAY="$(docker network inspect freebuff-proxy_default \
  --format '{{(index .IPAM.Config 0).Gateway}}' 2>/dev/null || echo 172.18.0.1)"
EXIT_IP="$(curl -s --max-time 15 --socks5-hostname "$GATEWAY:$RELAY" ifconfig.me || true)"
echo "    gateway:        $GATEWAY"
echo "    warp exit IP:   ${EXIT_IP:-UNREACHABLE}"
echo
echo "Next (deliberate, see docs/EGRESS.md):"
echo "  printf '%s\n' 'UPSTREAM_EGRESS_PROXY=socks5://$GATEWAY:$RELAY' >> ~/freebuff-proxy/.env"
echo "  cd ~/freebuff-proxy && docker compose restart"
