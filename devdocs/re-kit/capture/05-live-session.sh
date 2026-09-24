#!/usr/bin/env bash
# 05-live-session.sh -- live reverse-engineering session: passive monitoring,
# device-code login handover, then ONE free-model session (admit -> ping -> DELETE).
# DRY-RUN by default (prints curls, sends nothing). PROD: light touch only.
#
# Static sources: capture/01-auth.sh (code POST + status poll, 5s/5min CLI
#   defaults), capture/02-session.sh (admission/poll/DELETE routes + headers),
#   capture/03-chat.sh (chat envelope + ai-sdk UA), kit docs ENDPOINTS.md /
#   HEADERS.md / SESSION.md / LOGIN-TUI.md / MODEL-SELECT.md.
# Live reference: CLI 0.0.194 on vps-us; repo pin 0.0.193 (delta UNVERIFIED).
#
# Usage:
#   05-live-session.sh                 DRY-RUN: print everything, send nothing.
#   05-live-session.sh --live          Full live run (needs USER browser login).
#   05-live-session.sh --live --no-session
#                                      Login + monitoring only, no session even
#                                      after auth (link-expiry path rehearsal).
# Env: FREEBUFF_API_URL (default https://freebuff.com -- VERIFIED live 2026-09-24:
#        code POST 200 there; www.codebuff.com fallback untried),
#      POLL_INTERVAL_S (default 5), POLL_TIMEOUT_S (default 600 = 10 min),
#      FREEBUFF_TZ (default UTC), FREEBUFF_PROMPT (default "ping").
#
# Rules (binding):
# - Monitoring is PASSIVE only: tcpdump SNI-only when permitted, else 1s
#   ss snapshots + /proc/<pid>/fd + cmdline. No installs, no restarts.
# - Auth token (arrives inside the status-success body) lives in a shell
#   variable ONLY: never argv-logged, never file-written, never echoed.
#   Report shapes (key names, id presence, chunk counts, timings) only.
# - loginUrl is the ONE value printed in full (user must open it in a browser).
# - At most ONE session per run; trap DELETEs it (404 tolerated).
# - Raw logs stay under OUTDIR (default /tmp/fb-live) on the target host;
#   the script removes OUTDIR on exit. Only the redacted summary leaves.
set -euo pipefail

API_URL="${FREEBUFF_API_URL:-https://freebuff.com}"
INTERVAL="${POLL_INTERVAL_S:-5}"
TIMEOUT="${POLL_TIMEOUT_S:-600}"
TZ="${FREEBUFF_TZ:-UTC}"
PROMPT="${FREEBUFF_PROMPT:-ping}"
OUTDIR="${FB_LIVE_OUTDIR:-/tmp/fb-live}"
LIVE=0
NO_SESSION=0

usage() {
  sed -n '2,32p' "$0"
  echo "Flags: --live [--no-session] [--outdir DIR] [-h|--help]"
}

while [ $# -gt 0 ]; do
  case "$1" in
    --live) LIVE=1; shift ;;
    --no-session) NO_SESSION=1; shift ;;
    --outdir) OUTDIR="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown arg: $1" >&2; usage; exit 2 ;;
  esac
done

# ---------------- redaction helpers (same contract as 01-03) ----------------
redact_token() { # first2...last2, never the middle
  local s="$1"
  if [ "${#s}" -le 6 ]; then echo "<REDACTED len=${#s}>"
  else echo "${s:0:2}...${s: -2} (len ${#s})"; fi
}
redact_ips() { # stdin->stdout: mask IPv4/IPv6 literals (counts/ports kept)
  sed -E -e 's/[0-9]{1,3}(\.[0-9]{1,3}){3}/<IP>/g' \
         -e 's/([0-9a-fA-F]{0,4}:){2,}[0-9a-fA-F:.]+/<IP6>/g'
}
have() { command -v "$1" >/dev/null 2>&1; }

TLOG="$OUTDIR/timing.log"
MON_PID=""

t_start() { T0=$(date +%s); echo "[$(date -u +%FT%TZ)] START $1" | tee -a "$TLOG"; }
t_end() { local n; n=$(date +%s); echo "[$(date -u +%FT%TZ)] END $1 ($((n - T0))s)" | tee -a "$TLOG"; }

# ---------------- passive monitoring ----------------
mon_start() {
  mkdir -p "$OUTDIR"
  : > "$TLOG"
  t_start "monitoring"
  if have tcpdump && timeout 2s tcpdump -i any tcp port 443 -c 1 >/dev/null 2>"$OUTDIR/tcpdump-err.log"; then
    echo "monitor: tcpdump permitted; SNI-only capture (no -w pcap, no payloads)"
    ( timeout "${TIMEOUT}"s tcpdump -n -nn -s 120 -l 'tcp port 443' >"$OUTDIR/tcpdump.log" 2>/dev/null & echo $! >"$OUTDIR/mon.pid" )
  else
    echo "monitor: tcpdump unavailable/denied; fallback ss 1s snapshots + /proc (no payloads)"
    ( while true; do
        echo "--- $(date -u +%FT%TZ) ---" >>"$OUTDIR/ss.log"
        ss -tnp state all 2>/dev/null | grep -a ':443' >>"$OUTDIR/ss.log" 2>&1 || echo '(no :443 flows)' >>"$OUTDIR/ss.log"
        pgrep -a -f freebuff >>"$OUTDIR/ss.log" 2>&1 || echo '(no freebuff pid)' >>"$OUTDIR/ss.log"
        sleep 1
      done & echo $! >"$OUTDIR/mon.pid" )
  fi
  MON_PID=$(cat "$OUTDIR/mon.pid")
  echo "monitor pid $MON_PID logging under $OUTDIR"
  t_end "monitoring-start"
}
mon_stop() {
  if [ -n "${MON_PID:-}" ] && kill -0 "$MON_PID" 2>/dev/null; then kill "$MON_PID" 2>/dev/null || true; fi
  if [ -f "$OUTDIR/mon.pid" ]; then
    if kill -0 "$(cat "$OUTDIR/mon.pid")" 2>/dev/null; then kill "$(cat "$OUTDIR/mon.pid")" 2>/dev/null || true; fi
  fi
  echo "monitor stopped"
}
mon_summary() { # redacted counts only; SNI names only if tcpdump parsed any
  echo "--- monitoring summary (redacted) ---"
  if [ -f "$OUTDIR/ss.log" ]; then
    echo "ss snapshots: $(grep -ac '^--- ' "$OUTDIR/ss.log" || echo 0)"
    echo "flows :443 observed: $(grep -ac ':443' "$OUTDIR/ss.log" || echo 0)"
  fi
  if [ -f "$OUTDIR/tcpdump.log" ]; then
    echo "tcpdump header lines: $(grep -ac . "$OUTDIR/tcpdump.log" || echo 0)"
    grep -a . "$OUTDIR/tcpdump.log" 2>/dev/null | redact_ips | head -5 || true
  fi
}

# ---------------- dry-run ----------------
if [ "$LIVE" -eq 0 ]; then
  cat <<EOF
== 05-live-session DRY-RUN (nothing sent, no seat, no auth) ==
[1] monitoring: tcpdump SNI-only if permitted, else 'ss -tnp' 1s snapshots
    + pgrep freebuff + /proc/<pid>/fd+cmdline, all under $OUTDIR
[2] code mint (no token):
curl -sS -X POST "$API_URL/api/auth/cli/code" \\
  -H 'Content-Type: application/json' \\
  --data-raw '{"fingerprintId":"<FINGERPRINT_REDACTED>"}'
# 200 {loginUrl, fingerprintHash, expiresAt, expiresInMs}; loginUrl carries auth_code.
# Print loginUrl IN FULL (only secret-adjacent value ever shown); user opens it verbatim.
[3] status poll every ${INTERVAL}s up to ${TIMEOUT}s (401 = pending, silent):
curl -sS -o \$OUTDIR/status.json -w '%{http_code}' \\
  "$API_URL/api/auth/cli/status?fingerprintId=<FP>&fingerprintHash=<HASH>&expiresAt=<EXP>"
# success when .user is an object; token arrives in-body -> shell var ONLY.
[4] ONE free-model session (default model: header omitted, server default):
    POST $API_URL/api/v1/freebuff/session/admission (no body, Bun UA,
      x-fb-timezone, wallet-0, first-tab-0) -> GET poll (compact) ->
    POST $API_URL/api/v1/chat/completions {model, stream:true,
      messages:[{role:user, content:"$PROMPT"}],
      codebuff_metadata{run_id, client_id, trace_session_id,
      freebuff_instance_id, llm_step_number, cost_mode:free}} (ai-sdk UA) ->
    SSE chunk count only -> DELETE session (404 tolerated).
[5] mon_stop; print redacted summary (status/shape/timings/counts);
    rm -rf \$OUTDIR. Token never leaves the shell variable.
Re-run with --live for the real pass (needs the user at their browser).
EOF
  exit 0
fi

# ---------------- LIVE ----------------
mkdir -p "$OUTDIR"
chmod 700 "$OUTDIR"
TOKEN=""        # in-memory only; never logged, never written
INSTANCE_ID=""  # exactly one seat per run
FP=""

cleanup() {
  if [ -n "$INSTANCE_ID" ] && [ -n "$TOKEN" ]; then
    echo "cleanup: DELETE seat (404 tolerated)" >&2
    curl -sS -o /dev/null -w 'DELETE http=%{http_code} total=%{time_total}s\n' -X DELETE \
      "$API_URL/api/v1/freebuff/session" \
      -H "Authorization: Bearer $TOKEN" \
      -H "x-fb-timezone: $TZ" \
      -H "x-freebuff-first-tab-discount: 0" \
      -H "x-freebuff-instance-id: $INSTANCE_ID" || true
  fi
  mon_stop || true
  TOKEN="unset-after-run"
}
trap cleanup EXIT

mon_start

t_start "code-mint"
if have openssl; then FP="enhanced-$(openssl rand -hex 16)"
else FP="enhanced-ephemeral-$RANDOM$RANDOM"; fi
curl -sS -m 20 -X POST "$API_URL/api/auth/cli/code" \
  -H 'Content-Type: application/json' \
  --data-raw "{\"fingerprintId\":\"$FP\"}" \
  -o "$OUTDIR/code.json" -w 'code http=%{http_code} total=%{time_total}s\n' | tee -a "$TLOG"
jq -e '{has_loginUrl: has("loginUrl"), has_fingerprintHash: has("fingerprintHash"), has_expiresAt: has("expiresAt"), loginUrl_has_auth_code: ((.loginUrl // "") | contains("auth_code"))}' \
  "$OUTDIR/code.json" || { echo "ERROR: unexpected code shape" >&2; exit 1; }
t_end "code-mint"

echo "================ USER ACTION ================"
jq -r '.loginUrl' "$OUTDIR/code.json"   # FULL URL: the one printed value
echo "Open the URL above in your browser and complete login."
echo "============================================="
HASH=$(jq -r '.fingerprintHash // empty' "$OUTDIR/code.json")
EXP=$(jq -r '.expiresAt // empty' "$OUTDIR/code.json")

t_start "login-poll"
deadline=$(( SECONDS + TIMEOUT ))
while [ $SECONDS -lt $deadline ]; do
  sleep "$INTERVAL"
  code=$(curl -sS -m 15 -o "$OUTDIR/status.json" -w '%{http_code}' \
    "$API_URL/api/auth/cli/status?fingerprintId=$FP&fingerprintHash=$HASH&expiresAt=$EXP" || echo "000")
  if [ "$code" = "401" ]; then continue; fi
  if [ "$code" = "000" ]; then echo "network error, retrying..." >&2; continue; fi
  if jq -e '.user | type == "object"' "$OUTDIR/status.json" >/dev/null 2>&1; then
    echo "SUCCESS: server attached user object."
    jq '{id_set: ((.user.id // "") != ""), email_set: ((.user.email // "") != ""), top_keys: keys_unsorted}' \
      "$OUTDIR/status.json"
    # Token: whichever in-body field carries it; value NEVER logged.
    TOKEN=$(jq -r '.authToken // .auth_token // .token // empty' "$OUTDIR/status.json")
    break
  fi
  echo "HTTP $code without user object; retrying..." >&2
done
t_end "login-poll"

if [ -z "$TOKEN" ]; then
  echo "TIMEOUT after ${TIMEOUT}s: link expired, no session created." >&2
  exit 1
fi
echo "authenticated (token $(redact_token "$TOKEN")); account id masked above."

if [ "$NO_SESSION" -eq 1 ]; then
  echo "--no-session: stopping before admission; no seat held."
  TOKEN=""; exit 0
fi

BUN_UA="Bun"
AI_UA="ai-sdk/openai-compatible/1.0.0/codebuff"  # pinned shape; CLI uses live VERSION (drift note)

t_start "admission"
curl -sS -m 20 -D "$OUTDIR/adm-hdrs.log" -o "$OUTDIR/adm-body.json" \
  -w 'admission http=%{http_code} total=%{time_total}s connect=%{time_connect}s tls=%{time_appconnect}s\n' \
  -X POST "$API_URL/api/v1/freebuff/session/admission" \
  -H "Authorization: Bearer $TOKEN" -H "User-Agent: $BUN_UA" \
  -H "x-fb-timezone: $TZ" \
  -H "x-freebuff-wallet-spend-limit: 0" \
  -H "x-freebuff-first-tab-discount: 0" | tee -a "$TLOG"
jq '{status, instanceId_set: ((.instanceId // "") != ""), model, keys: keys_unsorted}' "$OUTDIR/adm-body.json" \
  || { echo "non-JSON admission body (control-stripped head):"; tr -dc '[:print:]\n\t' <"$OUTDIR/adm-body.json" | head -c 200; echo; }
INSTANCE_ID=$(jq -r '.instanceId // empty' "$OUTDIR/adm-body.json" 2>/dev/null || true)
if [ -z "$INSTANCE_ID" ]; then echo "No instanceId granted (gate above); exiting without seat." >&2; exit 1; fi
echo "seat held: exactly ONE session this run."
t_end "admission"

t_start "poll"
curl -sS -m 20 -o "$OUTDIR/poll-body.json" \
  -w 'poll http=%{http_code} total=%{time_total}s\n' \
  "$API_URL/api/v1/freebuff/session" \
  -H "Authorization: Bearer $TOKEN" \
  -H "x-fb-timezone: $TZ" \
  -H "x-freebuff-first-tab-discount: 0" \
  -H "x-freebuff-instance-id: $INSTANCE_ID" \
  -H "x-freebuff-compact-session: 1" | tee -a "$TLOG"
jq '{status, model, keys: keys_unsorted}' "$OUTDIR/poll-body.json" \
  || { echo "non-JSON poll body (control-stripped head):"; tr -dc '[:print:]\n\t' <"$OUTDIR/poll-body.json" | head -c 200; echo; }
MODEL_USED=$(jq -r '.model // empty' "$OUTDIR/poll-body.json" 2>/dev/null || true)
t_end "poll"

t_start "chat-ping"
jq -n --arg m "$MODEL_USED" --arg p "$PROMPT" \
  --arg inst "$INSTANCE_ID" \
  '{model: $m, stream: true,
    messages: [{role: "user", content: $p}],
    codebuff_metadata: {run_id: "re-ping", client_id: "re-client",
      trace_session_id: "re-trace", freebuff_instance_id: $inst,
      llm_step_number: "1", cost_mode: "free"},
    provider: {data_collection: "deny"}}' >"$OUTDIR/chat-body.json"
curl -sS -N -m 60 -X POST "$API_URL/api/v1/chat/completions" \
  -H "Authorization: Bearer $TOKEN" -H "User-Agent: $AI_UA" \
  -H 'Content-Type: application/json' \
  --data @"$OUTDIR/chat-body.json" \
  -o "$OUTDIR/chat-sse.log" -w 'chat http=%{http_code} total=%{time_total}s ttfb=%{time_starttransfer}s\n' | tee -a "$TLOG"
echo "SSE chunks (data: lines): $(grep -ac '^data:' "$OUTDIR/chat-sse.log" || echo 0)"
echo "stream errors (error token, shape only): $(grep -aci 'error' "$OUTDIR/chat-sse.log" || echo 0)"
t_end "chat-ping"

echo "== redacted session trace =="
echo "admission: see timing log; poll model: ${MODEL_USED:-<unset>} (server default);"
echo "chat: one '$PROMPT' turn, counts above; DELETE follows via trap."
mon_summary
echo "timing:"; cat "$TLOG"
echo "== LIVE COMPLETE (trap DELETEs the seat; OUTDIR removed by owner after review) =="
