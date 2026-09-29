#!/usr/bin/env bash
# setup-proxy-docker.sh — build + start the freebuff-proxy Docker container on
# Linux, then print the EXACT 9router configuration, including the Docker
# gateway IP to use when 9router itself runs in a container.
#
# Usage:
#   ./setup-proxy-docker.sh                # clone/build/start + print config
#   ./setup-proxy-docker.sh --skip-token   # don't touch the token (already set)
#   ./setup-proxy-docker.sh --no-start     # build image only, don't start
#
# Requirements: docker + docker compose (v2); for token generation: curl + jq +
# openssl (only if you don't already have a FreeBuff token).
set -euo pipefail

SKIP_TOKEN=0
NO_START=0
for arg in "$@"; do
  case "$arg" in
    --skip-token) SKIP_TOKEN=1 ;;
    --no-start) NO_START=1 ;;
    -h|--help) grep '^#' "$0" | head -25; exit 0 ;;
    *) echo "unknown arg: $arg (see header)" >&2; exit 1 ;;
  esac
done

c() { printf '\033[36m%s\033[0m\n' "$*"; }
ok() { printf '\033[32m%s\033[0m\n' "$*"; }
warn() { printf '\033[33m%s\033[0m\n' "$*"; }

# --- 0. requirements --------------------------------------------------------
for bin in docker; do
  command -v "$bin" >/dev/null 2>&1 || { echo "ERROR: $bin not found" >&2; exit 1; }
done
docker compose version >/dev/null 2>&1 || { echo "ERROR: docker compose v2 not found" >&2; exit 1; }

# --- 1. repo ----------------------------------------------------------------
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
if [ -f "$SCRIPT_DIR/../docker-compose.yml" ]; then
  # Running from inside the repo's scripts/ dir — use the repo root.
  REPO_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
else
  # Running from anywhere else — default to ~/freebuff-proxy.
  REPO_DIR="$HOME/freebuff-proxy"
fi
if [ ! -f "$REPO_DIR/docker-compose.yml" ]; then
  c "Cloning freebuff-proxy into $REPO_DIR..."
  git clone --quiet https://github.com/trefeon/freebuff-proxy.git "$REPO_DIR"
fi
cd "$REPO_DIR"

# --- 2. token ---------------------------------------------------------------
if [ "$SKIP_TOKEN" = "0" ] && ! grep -q '^AUTH_TOKENS=' .env 2>/dev/null; then
  c "No AUTH_TOKENS in .env — running the token helper..."
  if [ -x scripts/gen-token.sh ]; then
    ./scripts/gen-token.sh --append
  elif [ -x scripts/gen-freebuff-token.sh ]; then
    ./scripts/gen-freebuff-token.sh --append
  else
    cp .env.example .env
    warn "Set AUTH_TOKENS in .env manually (see README → Getting a token), then re-run."
    exit 1
  fi
fi
grep -q '^AUTH_TOKENS=' .env 2>/dev/null || { echo "ERROR: AUTH_TOKENS missing in .env" >&2; exit 1; }
if grep -Eq '^AUTH_TOKENS=[[:space:]]*$' .env 2>/dev/null; then
  warn "AUTH_TOKENS is empty — bridge mode: clients must send their own FreeBuff token as Authorization: Bearer <token>."
else
  ok "Token configured (AUTH_TOKENS present in .env)"
fi

# --- 3. build + start -------------------------------------------------------
c "Building the proxy image (this takes a minute the first time)..."
if [ "$NO_START" = "1" ]; then
  docker compose build
  ok "Image built. Start it later with: docker compose up -d"
else
  docker compose up --build -d
  # --- 4. wait for health ---------------------------------------------------
  c "Waiting for the container to become healthy..."
  for i in $(seq 1 30); do
    STATUS="$(docker compose ps --format '{{.Status}}' 2>/dev/null | head -1)"
    case "$STATUS" in
      *healthy*) ok "Container healthy: $STATUS"; break ;;
      *) sleep 2 ;;
    esac
    [ "$i" = "30" ] && { echo "ERROR: container not healthy after 60s" >&2; docker compose logs --tail 20; exit 1; }
  done
fi

# --- 5. detect the real Docker gateway (host IP as seen from the container) -
# The host is reached from inside a container via the gateway of the network
# that container is on. Inspect the ACTUAL proxy container instead of guessing
# the network name: compose projects prefix networks with the project name
# (e.g. freebuff-proxy_default), and this script may run from any directory.
BRIDGE_GW="$(docker network inspect bridge --format '{{(index .IPAM.Config 0).Gateway}}' 2>/dev/null || echo 172.17.0.1)"
GATEWAY=""
# 1) the compose project we just started (correct even with a project-prefixed
#    network name); 2) the well-known container name; 3) the image name.
CONTAINER="$(docker compose ps -q 2>/dev/null | head -1 || true)"
[ -z "$CONTAINER" ] && CONTAINER="$(docker ps -q --filter name=^/freebuff-proxy$ 2>/dev/null | head -1 || true)"
[ -z "$CONTAINER" ] && CONTAINER="$(docker ps -q --filter ancestor=freebuff-proxy:latest 2>/dev/null | head -1 || true)"
if [ -n "$CONTAINER" ]; then
  GATEWAY="$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.Gateway}} {{end}}' "$CONTAINER" 2>/dev/null | awk '{print $1}' || true)"
fi
# Fallback: the classic docker0 bridge gateway (172.17.0.1). Only correct for
# default-bridge networking; kept so the config below stays printable when the
# container isn't running (e.g. --no-start).
[ -z "$GATEWAY" ] && GATEWAY="$BRIDGE_GW"

# --- 6. print the 9router configuration -------------------------------------
cat <<EOF

============================================================
  9router → freebuff-proxy — fill the "Add OpenAI Compatible"
  form with these values (Dashboard → Providers → Add)
============================================================

  Name          : freebuff
  Prefix        : freebuff
  API Type      : Chat Completions          (NOT Responses API)
  Base URL      : see below — depends on where 9router runs
  API Key       : any non-empty value (the proxy has no API_KEYS set)
  Model ID      : (leave empty — the proxy has /v1/models)

  Base URL — pick ONE:

  A) 9router runs as a plain process on this host:
        http://127.0.0.1:3457/v1

  B) 9router runs in a Docker container on this host — the host is reached
     via the gateway of the network the ROUTER container is on. Detected:
        • router on the default bridge network:  http://${BRIDGE_GW}:3457/v1
        • router on any compose network:         http://${GATEWAY}:3457/v1
     (published ports are reachable via ANY host-interface IP, so both work
     from any container; use the router's own network gateway.)

  Verify before creating the node:
        curl http://${GATEWAY}:3457/v1/models
     (or http://127.0.0.1:3457/v1/models) — should list the model catalog (~15 models).

  After Create, add models to the node, e.g.:
        deepseek/deepseek-v4-flash
        deepseek/deepseek-v4-pro
        minimax/minimax-m3
        openai/gpt-5.6-luna
        mimo/mimo-v2.5
        z-ai/glm-5.2
     They are addressed as freebuff/<model-id> (e.g. freebuff/deepseek-v4-flash).
============================================================
EOF
