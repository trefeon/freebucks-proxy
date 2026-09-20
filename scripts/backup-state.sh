#!/usr/bin/env bash
# backup-state.sh — pre-deploy snapshot of the live dashboard store.
#
# Takes a consistent copy of the gateway's SQLite DB into a timestamped host
# recovery dir plus a manifest of per-table row COUNTS (never values), so a
# post-boot gate can prove the recreate stranded nothing.
#
# Runbook order: stop the container FIRST (a stopped DB checkpoints its WAL
# into the main file, so a plain copy is consistent), then this script, then
# recreate, then scripts/verify-state.sh. The script refuses to bless a
# running container unless --live is passed (copies the trio best-effort and
# marks the manifest accordingly).
#
# Credential rule: this script NEVER selects values — COUNT(*) only. The
# manifest holds file hashes and row counts; it is safe to keep alongside
# the recovery copy (which itself stays mode 0600 like the live file).
#
# Fail-closed: any lock/permission/copy/integrity failure exits nonzero with
# no manifest. A missing manifest must block the deploy gate downstream.
#
# Usage:
#   scripts/backup-state.sh [--container NAME] [--out-dir DIR] [--live]
CONTAINER="freebucks-proxy"
OUT_DIR=""
LIVE_OK=0
while [ $# -gt 0 ]; do
  case "$1" in
    --container=*) CONTAINER="${1#--container=}"; shift ;;
    --container) CONTAINER="${2:?ERROR: --container needs a value}"; shift 2 ;;
    --out-dir=*) OUT_DIR="${1#--out-dir=}"; shift ;;
    --out-dir) OUT_DIR="${2:?ERROR: --out-dir needs a value}"; shift 2 ;;
    --live) LIVE_OK=1; shift ;;
    -h|--help)
      echo "usage: backup-state.sh [--container NAME] [--out-dir DIR] [--live]"
      exit 0 ;;
    *) echo "ERROR: unknown arg $1" >&2; exit 2 ;;
  esac
done

for bin in docker; do
  command -v "$bin" >/dev/null 2>&1 || { echo "ERROR: $bin not found" >&2; exit 1; }
done
# Unexpected aborts fail closed with no manifest: a missing manifest must
# block the deploy gate downstream.
trap 'echo "ERROR: backup aborted during: ${BASH_COMMAND} — no manifest written" >&2' ERR

# The container must exist (running or stopped — docker cp serves both).
if ! docker inspect "$CONTAINER" >/dev/null 2>&1; then
  echo "ERROR: container $CONTAINER not found (docker inspect failed)" >&2
  exit 1
fi
RUNNING="$(docker inspect -f '{{.State.Running}}' "$CONTAINER")"
if [ "$RUNNING" = "true" ] && [ "$LIVE_OK" != "1" ]; then
  echo "ERROR: $CONTAINER is still running — stop it first so the WAL checkpoints" >&2
  echo "  (docker compose stop $CONTAINER), or pass --live for a best-effort trio copy." >&2
  exit 1
fi

# Resolve the live DB path from the container's process env (DB_PATH wins,
# else the ./data/freebuff.db default under the working dir). DB_PATH is
# process-env-only by construction, so inspect the container env — never a
# .env file or overlay row.
DB_PATH="$(docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$CONTAINER" | grep -E '^DB_PATH=' | cut -d= -f2- || true)"
if [ -z "$DB_PATH" ]; then
  WORKDIR="$(docker inspect -f '{{.Config.WorkingDir}}' "$CONTAINER")"
  [ -z "$WORKDIR" ] && WORKDIR="/app"
  # The default is cwd-relative (data/freebuff.db); resolve against workdir.
  case "$WORKDIR" in
    /app/state) DB_PATH="/app/state/data/freebuff.db" ;;
    *) DB_PATH="$WORKDIR/data/freebuff.db" ;;
  esac
fi

STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
if [ -z "$OUT_DIR" ]; then
  OUT_DIR="./state-recovery/${STAMP}-${CONTAINER}"
fi
mkdir -p "$OUT_DIR"
chmod 700 "$OUT_DIR"

# Copy the main file (hard fail) plus WAL sidecars when present (soft).
if ! docker cp "${CONTAINER}:${DB_PATH}" "$OUT_DIR/freebuff.db" >/dev/null 2>&1; then
  echo "ERROR: copy of ${CONTAINER}:${DB_PATH} failed (lock/permission?)" >&2
  echo "  Resolve the container access and re-run — no manifest written." >&2
  exit 1
fi
chmod 600 "$OUT_DIR/freebuff.db"
for suffix in "-wal" "-shm"; do
  docker cp "${CONTAINER}:${DB_PATH}${suffix}" "$OUT_DIR/freebuff.db${suffix}" >/dev/null 2>&1 || true
  [ -f "$OUT_DIR/freebuff.db${suffix}" ] && chmod 600 "$OUT_DIR/freebuff.db${suffix}" || true
done

# SQL runner for COUNT(*)-only inspection of the recovery COPY (never the
# live file): local sqlite3 CLI when present, else a throwaway sqlite image
# over the recovery dir mounted read-only. Anything else is fail-closed.
sqlite_count() { # $1=db $2=sql -> prints the count
  if command -v sqlite3 >/dev/null 2>&1; then
    sqlite3 -readonly "$1" "$2"
  elif docker image inspect keinos/sqlite3:latest >/dev/null 2>&1 || docker pull -q keinos/sqlite3:latest >/dev/null 2>&1; then
    docker run --rm -v "$OUT_DIR:/bk:ro" keinos/sqlite3:latest sqlite3 "file:/bk/$(basename "$1")?immutable=1" "$2"
  else
    echo "ERROR: no sqlite3 CLI and keinos/sqlite3 image unavailable — cannot count rows" >&2
    exit 1
  fi
}

DB="$OUT_DIR/freebuff.db"
# Integrity first: a torn copy must never become the blessed manifest.
INTEGRITY="$(sqlite_count "$DB" 'PRAGMA integrity_check;')"
if [ "$INTEGRITY" != "ok" ]; then
  echo "ERROR: recovery copy failed integrity_check ($INTEGRITY) — no manifest written" >&2
  exit 1
fi

count() { sqlite_count "$DB" "SELECT COUNT(*) FROM $1;"; }
# Tables the gateway owns; missing tables (older generations) count as zero.
tcount() {
  local has
  has="$(sqlite_count "$DB" "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='$1';")"
  if [ "$has" = "0" ]; then echo 0; else count "$1"; fi
}

SETTINGS_TOTAL="$(tcount settings)"
SETTINGS_CONFIG="$(sqlite_count "$DB" "SELECT COUNT(*) FROM settings WHERE key LIKE 'config:%';")"
MANIFEST="$OUT_DIR/manifest.txt"
{
  echo "# freebucks-proxy state backup manifest (counts only — never values)"
  echo "container=$CONTAINER"
  echo "db_path=$DB_PATH"
  echo "stamp_utc=$STAMP"
  echo "live_running_at_backup=$RUNNING"
  echo "db_sha256=$(sha256sum "$DB" | cut -d' ' -f1)"
  echo "db_bytes=$(wc -c <"$DB" | tr -d ' ')"
  echo "settings_total=$SETTINGS_TOTAL"
  echo "settings_config=$SETTINGS_CONFIG"
  echo "pages_state=$(tcount pages_state)"
  echo "sessions_persist=$(tcount sessions_persist)"
  echo "tokens=$(tcount tokens)"
  echo "pool_state=$(tcount pool_state)"
  echo "log_entries=$(tcount log_entries)"
  echo "quota_snapshots=$(tcount quota_snapshots)"
  echo "maturity_events=$(tcount maturity_events)"
  echo "request_records=$(tcount request_records)"
} >"$MANIFEST"
chmod 600 "$MANIFEST"

echo "backup ok: $OUT_DIR"
echo "--- manifest (expected post-boot counts) ---"
cat "$MANIFEST"
