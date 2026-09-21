#!/bin/sh
# collect-debug.sh — one-command debug bundle for a live freebuff-proxy.
#
# Usage:
#   scripts/collect-debug.sh [--container NAME] [--since DURATION]
#                            [--out DIR] [--base URL]
#
#   --container  docker container to read (default: freebucks-proxy)
#   --since      docker logs window (default: 24h, docker --since syntax)
#   --out        bundle directory (default: ./debug-YYYYMMDD-HHMMSS)
#   --base       dashboard base URL (default: http://127.0.0.1:3457)
#
# Collects docker logs (ANSI-stripped), GET /healthz, and the dashboard
# /admin/api/logs/export + /admin/api/logs/rollup documents via the
# dashboard login. ADMIN_TOKEN comes from the environment ONLY (never a
# flag or literal). The bundle dir is chmod 0600; the cookie jar lives in
# mktemp and is deleted on exit.
#
# Remote gateway: point docker at it (e.g. DOCKER_HOST=ssh://user@host)
# and pass --base for the dashboard URL; needs only curl/jq/docker/ssh.
#
# Windows: run under Git Bash, e.g.
#   "C:\Program Files\Git\bin\bash.exe" scripts/collect-debug.sh
# Requires curl, jq and docker on PATH.

set -eu

CONTAINER="freebucks-proxy"
SINCE="24h"
OUT="./debug-$(date +%Y%m%d-%H%M%S)"
BASE="http://127.0.0.1:3457"

die() { echo "collect-debug: error: $1" >&2; exit "${2:-1}"; }

while [ $# -gt 0 ]; do
	case "$1" in
		--container) CONTAINER="${2:?--container needs a value}"; shift 2;;
		--since) SINCE="${2:?--since needs a value}"; shift 2;;
		--out) OUT="${2:?--out needs a value}"; shift 2;;
		--base) BASE="${2:?--base needs a value}"; shift 2;;
		--help|-h) sed -n '2,/^$/p' "$0" | sed 's/^# \{0,1\}//'; exit 0;;
		--) shift; break;;
		-*) die "unknown flag: $1";;
		*) die "unexpected argument: $1";;
	esac
done

[ -n "${ADMIN_TOKEN:-}" ] || die "ADMIN_TOKEN is not set (export it; never pass it as a flag)"
command -v docker >/dev/null 2>&1 || die "docker not found on PATH"
command -v curl >/dev/null 2>&1 || die "curl not found on PATH"
command -v jq >/dev/null 2>&1 || die "jq not found on PATH"

mkdir -p "$OUT"
JAR="$(mktemp)" || die "mktemp failed"
trap 'rm -f "$JAR"' EXIT INT TERM

warn() { echo "collect-debug: warning: $1" >&2; }

# 1. Container logs, ANSI-stripped (level=\x1b[32mINFO broke grep once).
if docker logs --since "$SINCE" "$CONTAINER" >"$OUT/docker-raw.log" 2>&1; then
	sed -e 's/\x1b\[[0-9;]*[a-zA-Z]//g' "$OUT/docker-raw.log" >"$OUT/docker.log"
	rm -f "$OUT/docker-raw.log"
else
	warn "docker logs failed for container $CONTAINER"
fi

# 2. Health (no auth).
curl -sS -m 15 "$BASE/healthz" -o "$OUT/healthz.json" || warn "GET /healthz failed"

# 3. Dashboard login (fresh jar: no fb_csrf cookie yet, so the login POST
#    is origin-checked only and curl's missing Origin header passes), then
#    the GET endpoints (no CSRF needed) with the fb_admin session cookie.
TOKEN_JSON="$(jq -n --arg t "$ADMIN_TOKEN" '{token:$t}')"
CODE="$(curl -sS -m 15 -c "$JAR" -b "$JAR" -X POST "$BASE/admin/login" \
	-H 'Content-Type: application/json' -d "$TOKEN_JSON" \
	-o "$OUT/login.json" -w '%{http_code}')" || die "dashboard login request failed"
[ "$CODE" = "200" ] || [ "$CODE" = "302" ] || die "dashboard login -> HTTP $CODE (see $OUT/login.json)"
curl -sS -m 30 -b "$JAR" "$BASE/admin/api/logs/export" -o "$OUT/logs-export.json" \
	|| warn "GET /admin/api/logs/export failed"
curl -sS -m 30 -b "$JAR" "$BASE/admin/api/logs/rollup" -o "$OUT/logs-rollup.json" \
	|| warn "GET /admin/api/logs/rollup failed"

# 4. Grep signatures to watch (counts over the collected logs).
sig() { c=0; for f in "$OUT"/docker.log "$OUT"/logs-export.json; do [ -f "$f" ] || continue; n=$(grep -c "$1" "$f" 2>/dev/null || true); c=$((c + n)); done; echo "$c"; }
echo "bundle: $OUT"
echo "signatures to watch:"
echo "  request failed:           $(sig 'request failed')"
echo "  attempts=2 retried=true:  $(sig 'attempts=2 retried=true')"
echo "  run resumed from store:   $(sig 'run resumed from store')"

chmod 600 "$OUT"/* 2>/dev/null || true
chmod 0600 "$OUT"
echo "note: bundle dir is mode 0600 (chmod u+X to inspect)"
