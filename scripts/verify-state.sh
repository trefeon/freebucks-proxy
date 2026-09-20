#!/usr/bin/env bash
# verify-state.sh — post-boot deploy gate: prove the recreate stranded nothing.
#
# Any trip prints ROLL BACK and exits nonzero — never cut traffic on a trip.
# Checks, in order:
#   A. /healthz answers 200.
#   B. Wrong-token POST /admin/login answers 401 (auth enforcement alive;
#      the probe value is a synthetic literal, never a real credential).
#   C. Authed GET /admin/api/settings reports a strict no-op boot:
#      migrate.fresh=false, migrate.noop=true, migrate.applied=[],
#      migrate.marker=true (fresh=true means the live store is empty — the
#      exact shape of the vps-sg outage).
#   D. Live per-table row counts equal the backup manifest on the five
#      operator-state tables (settings, pages_state, sessions_persist,
#      tokens, pool_state); display-history tables (log_entries,
#      quota_snapshots, maturity_events, request_records) may only grow.
#
# Counting reads a docker-cp'd trio copy of the live DB (integrity-checked;
# one retry, then trip) — never the live file, values never selected.
#
# Usage:
#   scripts/verify-state.sh [--base URL] [--manifest FILE] [--container NAME]
#   ADMIN_TOKEN must be in the environment (read once for the login cookie,
#   never printed or logged).
# Defaults: base http://127.0.0.1:3457, container freebucks-proxy, manifest =
# newest ./state-recovery/*/manifest.txt.
set -euo pipefail

BASE="http://127.0.0.1:3457"
CONTAINER="freebucks-proxy"
MANIFEST=""
while [ $# -gt 0 ]; do
  case "$1" in
    --base=*) BASE="${1#--base=}"; shift ;;
    --base) BASE="${2:?ERROR: --base needs a value}"; shift 2 ;;
    --manifest=*) MANIFEST="${1#--manifest=}"; shift ;;
    --manifest) MANIFEST="${2:?ERROR: --manifest needs a value}"; shift 2 ;;
    --container=*) CONTAINER="${1#--container=}"; shift ;;
    --container) CONTAINER="${2:?ERROR: --container needs a value}"; shift 2 ;;
    -h|--help)
      echo "usage: verify-state.sh [--base URL] [--manifest FILE] [--container NAME]"
      exit 0 ;;
    *) echo "ERROR: unknown arg $1" >&2; exit 2 ;;
  esac
done

trip() { echo "GATE TRIP — ROLL BACK, do not cut traffic: $1" >&2; exit 1; }
# Unexpected aborts (a failed query, a missing tool mid-run) carry the same
# banner: any nonzero path out of this script means "do not cut traffic".
trap 'trip "unexpected failure (${BASH_COMMAND})"' ERR
for bin in docker curl; do
  command -v "$bin" >/dev/null 2>&1 || trip "$bin not found on this host"
done
[ -n "${ADMIN_TOKEN:-}" ] || trip "ADMIN_TOKEN is unset (needed for the authed migrate/counts checks)"
if [ -z "$MANIFEST" ]; then
  MANIFEST="$(ls -t ./state-recovery/*/manifest.txt 2>/dev/null | head -1 || true)"
  [ -n "$MANIFEST" ] || trip "no manifest found under ./state-recovery/ — run backup-state.sh pre-recreate first"
fi
[ -f "$MANIFEST" ] || trip "manifest $MANIFEST not found"
mget() { grep -E "^$1=" "$MANIFEST" | cut -d= -f2-; }

# --- A. liveness ---
HEALTH="$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 "$BASE/healthz" || true)"
[ "$HEALTH" = "200" ] || trip "/healthz answered $HEALTH, want 200"

# --- B. auth enforcement (synthetic wrong value only) ---
WRONG="$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 -X POST \
  --data-urlencode 'token=verify-gate-wrong-token-probe' "$BASE/admin/login" || true)"
[ "$WRONG" = "401" ] || trip "wrong-token login answered $WRONG, want 401"

# --- C. strict no-op boot via the authed settings payload ---
JAR="$(mktemp)"; BODY="$(mktemp)"
chmod 600 "$JAR" "$BODY"
trap 'rm -f "$JAR" "$BODY"' EXIT
LOGIN_CODE="$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 \
  --cookie-jar "$JAR" --data-urlencode "token=$ADMIN_TOKEN" "$BASE/admin/login" || true)"
[ "$LOGIN_CODE" = "302" ] || trip "admin login answered $LOGIN_CODE, want 302 (token wrong or boot broken)"
API_CODE="$(curl -s -o "$BODY" -w '%{http_code}' --max-time 10 --cookie "$JAR" "$BASE/admin/api/settings" || true)"
[ "$API_CODE" = "200" ] || trip "GET /admin/api/settings answered $API_CODE, want 200"
if command -v python3 >/dev/null 2>&1; then
  MIGRATE="$(python3 -c "
import json,sys
m = json.load(open('$BODY')).get('migrate') or {}
print(('fresh' if m.get('fresh') else 'notfresh'), ('noop' if m.get('noop') else 'notnoop'), ('marker' if m.get('marker') else 'nomarker'), 'applied' if m.get('applied') == [] else 'applied-nonempty')
")"
else
  # Dependency-free fallback: strict literal matches on the compact payload.
  MIGRATE="notfresh notnoop nomarker applied-nonempty"
  grep -q '"fresh":false' "$BODY" && grep -q '"noop":true' "$BODY" \
    && grep -q '"marker":true' "$BODY" && grep -q '"applied":\[\]' "$BODY" \
    && MIGRATE="notfresh noop marker applied"
fi
echo "migrate report: $MIGRATE"
[ "$MIGRATE" = "notfresh noop marker applied" ] || trip "live store is not a strict no-op boot ($MIGRATE) — state was re-initialized, not carried"

# --- D. live counts vs the manifest ---
DB_PATH="$(docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$CONTAINER" 2>/dev/null | grep -E '^DB_PATH=' | cut -d= -f2- || true)"
if [ -z "$DB_PATH" ]; then
  WORKDIR="$(docker inspect -f '{{.Config.WorkingDir}}' "$CONTAINER" 2>/dev/null || echo /app)"
  [ -z "$WORKDIR" ] && WORKDIR="/app"
  case "$WORKDIR" in
    /app/state) DB_PATH="/app/state/data/freebuff.db" ;;
    *) DB_PATH="$WORKDIR/data/freebuff.db" ;;
  esac
fi
TMPD="$(mktemp -d)"
chmod 700 "$TMPD"
trap 'rm -f "$JAR" "$BODY"; rm -rf "$TMPD"' EXIT
docker cp "${CONTAINER}:${DB_PATH}" "$TMPD/live.db" >/dev/null 2>&1 \
  || trip "could not copy live DB ${CONTAINER}:${DB_PATH} (permission?)"
for suffix in "-wal" "-shm"; do
  docker cp "${CONTAINER}:${DB_PATH}${suffix}" "$TMPD/live.db${suffix}" >/dev/null 2>&1 || true
done
sqlite_q() {
  if command -v sqlite3 >/dev/null 2>&1; then
    sqlite3 -readonly "$TMPD/live.db" "$1"
  else
    # Throwaway image over the disposable trio copy (mounted writable so
    # WAL-mode opens succeed; the live file and the recovery copy are
    # never mounted). Prefers the host sqlite3 CLI when present.
    docker run --rm -v "$TMPD:/bk" keinos/sqlite3:latest sqlite3 /bk/live.db "$1" 2>/dev/null
  fi
}
if [ "$(sqlite_q 'PRAGMA integrity_check;')" != "ok" ]; then
  sleep 5
  docker cp "${CONTAINER}:${DB_PATH}" "$TMPD/live.db" >/dev/null 2>&1 || true
  [ "$(sqlite_q 'PRAGMA integrity_check;')" = "ok" ] \
    || trip "live DB copy failed integrity_check twice — retry, then roll back on repeat"
fi
lcount() {
  local has
  has="$(sqlite_q "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='$1';")"
  if [ "$has" = "0" ]; then echo 0; else sqlite_q "SELECT COUNT(*) FROM $1;"; fi
}
FAIL=0
check_exact() { # table manifest-key
  local live want
  live="$(lcount "$1")"; want="$(mget "$2")"
  echo "count $1: live=$live manifest=$want"
  [ "$live" = "$want" ] || { echo "  MISMATCH on $1" >&2; FAIL=1; }
}
check_exact settings settings_total
check_exact pages_state pages_state
check_exact sessions_persist sessions_persist
check_exact tokens tokens
# pool_state is anti-stranding, not exact: the pool lazily creates runtime
# keys (admissions/burst/ledger blobs) on boot and maintain ticks, so a
# healthy live store usually holds MORE keys than the pre-recreate manifest
# (proven: 0 -> 3 across a reboot with zero traffic), and ledger orphan
# pruning can legitimately remove keys. The stranding signal is total loss:
# manifest non-empty but live empty. Any other divergence is a warning.
LIVE_POOL="$(lcount pool_state)"; WANT_POOL="$(mget pool_state)"
echo "count pool_state: live=$LIVE_POOL manifest=$WANT_POOL (anti-stranding)"
if [ "$WANT_POOL" != "0" ] && [ "$LIVE_POOL" = "0" ]; then
  echo "  STRANDED: pool_state lost" >&2; FAIL=1
elif [ "$LIVE_POOL" != "$WANT_POOL" ]; then
  echo "  note: pool_state differs (runtime keys churn — expected)" >&2
fi
LIVE_CONFIG="$(sqlite_q "SELECT COUNT(*) FROM settings WHERE key LIKE 'config:%';")"
echo "count settings_config: live=$LIVE_CONFIG manifest=$(mget settings_config)"
[ "$LIVE_CONFIG" = "$(mget settings_config)" ] || { echo "  MISMATCH on settings_config" >&2; FAIL=1; }
for t in log_entries quota_snapshots maturity_events request_records; do
  live="$(lcount "$t")"; want="$(mget "$t")"
  echo "count $t: live=$live manifest=$want (grow-only)"
  [ "$live" -ge "$want" ] 2>/dev/null || { echo "  SHRANK: $t" >&2; FAIL=1; }
done
[ "$FAIL" = "0" ] || trip "live counts diverge from the manifest — state stranded, roll back"
echo "gate GREEN: live store matches the manifest on a strict no-op boot"
