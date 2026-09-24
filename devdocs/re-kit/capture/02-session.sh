#!/usr/bin/env bash
# 02-session.sh — single session admission + poll + DELETE cleanup (DRY-RUN by default).
# Static source: cli/src/utils/freebuff-session-api.ts (callFreebuffSession route select + headers),
#   cli/src/hooks/use-freebuff-session.ts (30s poll, compact GET, unmount DELETE),
#   common/src/util/freebucks-timezone.ts (x-fb-timezone), proxy backend/internal/upstream/session.go.
set -euo pipefail

API_URL="${FREEBUFF_API_URL:-https://www.codebuff.com}"  # UNVERIFIED default; override per run.
MODEL="${FREEBUFF_MODEL:-}"            # x-freebuff-model (POST only, when picked)
TZ="${FREEBUFF_TZ:-UTC}"               # x-fb-timezone IANA zone
WALLET="${FREEBUFF_WALLET_LIMIT:-0}"   # x-freebuff-wallet-spend-limit (default '0' = no-consent path)
FIRST_TAB="${FREEBUFF_FIRST_TAB:-0}"   # x-freebuff-first-tab-discount 0|1
TOKEN="${FREEBUFF_TOKEN:-}"            # Bearer; env only, NEVER arg/logged
SEND=0

usage() {
  cat <<'EOF'
Usage: 02-session.sh [--send] [--model ID] [--tz ZONE]
  Default (no flags): DRY-RUN — print curl commands, send nothing, hold no seat.
  --send  Actually POST admission, GET-poll once, then DELETE the SAME instance.
          Single session only. Requires FREEBUFF_TOKEN in env + explicit per-use approval.
Env: FREEBUFF_API_URL, FREEBUFF_TOKEN, FREEBUFF_MODEL, FREEBUFF_TZ (default UTC),
     FREEBUFF_WALLET_LIMIT (default 0), FREEBUFF_FIRST_TAB (default 0).
Logs: token always redacted (first 2 / last 2 chars only).
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --send) SEND=1; shift ;;
    --model) MODEL="$2"; shift 2 ;;
    --tz) TZ="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown arg: $1" >&2; usage; exit 2 ;;
  esac
done

redact() { # redact <secret>: show 2+...+2, never the middle
  local s="$1"
  if [ ${#s} -le 6 ]; then echo "<REDACTED len=${#s}>"; else echo "${s:0:2}...${s: -2} (len ${#s})"; fi
}
auth_header_note() {
  if [ -n "$TOKEN" ]; then echo "Authorization: Bearer $(redact "$TOKEN")"; else echo "Authorization: Bearer <MISSING — export FREEBUFF_TOKEN to --send>"; fi
}

INSTANCE_ID=""  # held seat; exactly one per run
cleanup() {
  if [ "$SEND" -eq 1 ] && [ -n "$INSTANCE_ID" ] && [ -n "$TOKEN" ]; then
    echo "cleanup: DELETE instance $(echo "$INSTANCE_ID" | cut -c1-8)... (404 tolerated)" >&2
    curl -sS -o /dev/null -w 'DELETE http=%{http_code}\n' -X DELETE "$API_URL/api/v1/freebuff/session" \
      -H "Authorization: Bearer $TOKEN" \
      -H "x-fb-timezone: $TZ" \
      -H "x-freebuff-first-tab-discount: $FIRST_TAB" \
      -H "x-freebuff-instance-id: $INSTANCE_ID" || true
  fi
}
trap cleanup EXIT

BUN_UA="Bun"  # non-chat calls use plain Bun default UA (no override); chat uses ai-sdk UA (03-chat.sh)
echo "== 02-session (dry-run=$([ "$SEND" -eq 1 ] && echo no || echo yes)) =="
echo "$(auth_header_note)"
echo "model=${MODEL:-<unset>} tz=$TZ wallet=$WALLET first_tab=$FIRST_TAB"

if [ "$SEND" -eq 0 ]; then
  cat <<EOF
--- DRY-RUN: admission POST (no body, no Content-Type; not sent) ---
curl -sS -X POST "$API_URL/api/v1/freebuff/session/admission" \\
  -H 'Authorization: Bearer <REDACTED>' \\
  -H 'User-Agent: $BUN_UA' \\${MODEL:+
  -H 'x-freebuff-model: $MODEL' \\}
  -H 'x-fb-timezone: $TZ' \\
  -H 'x-freebuff-wallet-spend-limit: $WALLET' \\
  -H 'x-freebuff-first-tab-discount: $FIRST_TAB'
# Responses: active | model_locked | model_unavailable | consent_required |
#   first_tab_discount_changed | rate_limited | spend_limited | ip_capped | country_blocked | banned
--- DRY-RUN: poll GET (instance + compact; not sent) ---
curl -sS "$API_URL/api/v1/freebuff/session" \\
  -H 'Authorization: Bearer <REDACTED>' \\
  -H 'x-fb-timezone: $TZ' \\
  -H 'x-freebuff-first-tab-discount: $FIRST_TAB' \\
  -H 'x-freebuff-instance-id: <INSTANCE>' \\
  -H 'x-freebuff-compact-session: 1'
# CLI cadence 30s +/-20% jitter; single poll here. Quota/balance refresh from THIS poll, not the stream.
--- DRY-RUN: cleanup DELETE (instance header; 404 tolerated; not sent) ---
curl -sS -X DELETE "$API_URL/api/v1/freebuff/session" \\
  -H 'Authorization: Bearer <REDACTED>' \\
  -H 'x-fb-timezone: $TZ' \\
  -H 'x-freebuff-first-tab-discount: $FIRST_TAB' \\
  -H 'x-freebuff-instance-id: <INSTANCE>'
# -> {status:'ended', freebucksRefund?, freebucksRefundPending?}
EOF
  exit 0
fi

# ---------------- LIVE (explicit --send only) ----------------
if [ -z "$TOKEN" ]; then echo "ERROR: FREEBUFF_TOKEN required for --send (env only)." >&2; exit 2; fi
ARGS=(-sS -X POST "$API_URL/api/v1/freebuff/session/admission"
  -H "Authorization: Bearer $TOKEN" -H "User-Agent: $BUN_UA"
  -H "x-fb-timezone: $TZ" -H "x-freebuff-wallet-spend-limit: $WALLET"
  -H "x-freebuff-first-tab-discount: $FIRST_TAB")
if [ -n "$MODEL" ]; then ARGS+=(-H "x-freebuff-model: $MODEL"); fi
echo "LIVE: POST admission (token $(redact "$TOKEN"))"
RESP=$(curl "${ARGS[@]}")
echo "$RESP" | jq '{status, instanceId_set: ((.instanceId // "") != ""), model, keys: (keys_unsorted)}' 2>/dev/null \
  || { echo "non-JSON or changed shape; keys only:"; echo "$RESP" | head -c 300; echo; }
INSTANCE_ID=$(echo "$RESP" | jq -r '.instanceId // empty' 2>/dev/null || true)
if [ -z "$INSTANCE_ID" ]; then echo "No instanceId granted (gate response above); exiting without seat." >&2; exit 1; fi
echo "Seat held: instance $(echo "$INSTANCE_ID" | cut -c1-8)... — exactly ONE session this run."

echo "LIVE: GET poll (compact)..."
curl -sS "$API_URL/api/v1/freebuff/session" \
  -H "Authorization: Bearer $TOKEN" -H "x-fb-timezone: $TZ" \
  -H "x-freebuff-first-tab-discount: $FIRST_TAB" \
  -H "x-freebuff-instance-id: $INSTANCE_ID" -H "x-freebuff-compact-session: 1" \
  | jq '{status, model, remainingMs, rateLimit, balance: (.freebucks.balance // null)}' 2>/dev/null
echo "Done. Trap will DELETE the instance on exit (single-session rule)."
