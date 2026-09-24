#!/usr/bin/env bash
#
# local-emu.sh — local end-to-end emulation of the freebuff CLI lifecycle:
#   device-code login (browser) -> local token store -> model select ->
#   session admission -> one chat turn -> DELETE cleanup.
#
# Static sources: devdocs/re-kit/LOGIN-TUI.md (device-code + poll + persist),
#   MODEL-SELECT.md (picker hero + admission POST + chat envelope),
#   ACCOUNT-REQUEST.md (credential -> admission -> chat -> release),
#   SESSION.md (single seat + DELETE release), HEADERS.md (Bun vs ai-sdk UA),
#   capture/01-auth.sh + 02-session.sh + 03-chat.sh, scripts/gen-freebuff-token.sh
#   (auth_code -> onboard rewrite, BASE_URL/API_BASE_URL split).
#
# Routing: auth legs hit $WEB_URL (/api/auth/cli/*, default https://freebuff.com);
#   session/chat legs hit $API_URL (/api/v1/..., default https://www.codebuff.com).
#   Override with FREEBUFF_WEB_URL / FREEBUFF_API_URL (FREEBUFF_BASE_URL is read
#   as a legacy alias for the web origin only).
#
# Token storage: C:/tmp/fb-local-emu/credentials.json (0600), created by this
#   script. Tokens are NEVER written to the repo, logs, or output — all prints
#   show first2/last2 only. Re-runs reuse a stored non-empty token (skip login).
#
# Deps: curl, jq, openssl (+ a browser for the login step). tzutil is optional
#   (Windows IANA guess, else UTC).
#
set -euo pipefail

WEB_URL="${FREEBUFF_WEB_URL:-${FREEBUFF_BASE_URL:-https://freebuff.com}}"
API_URL="${FREEBUFF_API_URL:-https://www.codebuff.com}"
CRED_FILE="C:/tmp/fb-local-emu/credentials.json"
POLL_INTERVAL_S=5
POLL_TIMEOUT_S=600
BUN_UA="Bun/1.3.14"
AI_UA="ai-sdk/openai-compatible/1.0.0/codebuff"
DRY_RUN=0

# Static default free-model list (MODEL-SELECT.md catalog snapshot):
# [0] is the recommended hero = upstream DEFAULT_FREEBUFF_MODEL_ID.
# Extend from devdocs/re-kit/MODEL-SELECT.md when the served set moves.
MODELS=(
  "z-ai/glm-5.3-flash"
  "deepseek/deepseek-v4-flash"
  "mimo/mimo-v2.5"
)
MODEL_NOTES=(
  "recommended hero (DEFAULT_FREEBUFF_MODEL_ID)"
  "DeepSeek V4.1 Flash"
  "always-joinable fallback (FALLBACK_FREEBUFF_MODEL_ID)"
)

AUTH_TOKEN=""
FINGERPRINT_ID=""
FINGERPRINT_HASH=""
MODEL=""
INSTANCE_ID=""
TZ_SEL=""
SSE_FILE=""

info() { printf '%s\n' "$*"; }
dim()  { printf '%s\n' "$*"; }
warn() { printf 'WARN: %s\n' "$*" >&2; }
err()  { printf 'ERROR: %s\n' "$*" >&2; }

usage() {
  cat <<'EOF'
Usage: local-emu.sh [--dry-run] [--logout]
  (no flags)  LIVE: login (browser) -> pick model -> admit -> one ping -> DELETE.
  --dry-run   Print every curl + body shape, send nothing, create nothing.
  --logout    Delete C:/tmp/fb-local-emu/credentials.json and exit.
Env: FREEBUFF_WEB_URL (default https://freebuff.com),
     FREEBUFF_API_URL (default https://www.codebuff.com),
     FREEBUFF_TZ (override x-fb-timezone; default tzutil guess else UTC).
EOF
}

mask() { # mask <secret>: first2...last2, never the middle
  local s="${1:-}"
  if [ "${#s}" -le 6 ]; then echo "<REDACTED len=${#s}>";
  else echo "${s:0:2}...${s: -2} (len ${#s})"; fi
}

need() {
  command -v "$1" >/dev/null 2>&1 || { err "$1 is required"; exit 1; }
}

url_encode() { # percent-encode one value (server-supplied triple, never eval'd)
  local out
  out="$(jq -rn --arg v "$1" '$v | @uri' 2>/dev/null)" || out=""
  if [ -z "$out" ]; then out="$1"; fi
  printf '%s' "$out"
}

detect_tz() { # x-fb-timezone: env override, else tzutil Windows->IANA map, else UTC
  if [ -n "${FREEBUFF_TZ:-}" ]; then printf '%s' "$FREEBUFF_TZ"; return; fi
  if command -v tzutil >/dev/null 2>&1; then
    case "$(tzutil /g 2>/dev/null)" in
      "China Standard Time")        printf 'Asia/Shanghai' ;;
      "Tokyo Standard Time")        printf 'Asia/Tokyo' ;;
      "Korea Standard Time")        printf 'Asia/Seoul' ;;
      "Singapore Standard Time"|"Malay Peninsula Standard Time") printf 'Asia/Singapore' ;;
      "Taipei Standard Time")       printf 'Asia/Taipei' ;;
      "India Standard Time")        printf 'Asia/Kolkata' ;;
      "Pacific Standard Time")      printf 'America/Los_Angeles' ;;
      "Mountain Standard Time")     printf 'America/Denver' ;;
      "Central Standard Time")      printf 'America/Chicago' ;;
      "Eastern Standard Time")      printf 'America/New_York' ;;
      "GMT Standard Time")          printf 'Europe/London' ;;
      "W. Europe Standard Time")    printf 'Europe/Berlin' ;;
      "Romance Standard Time")      printf 'Europe/Paris' ;;
      "Central European Standard Time") printf 'Europe/Warsaw' ;;
      "E. Europe Standard Time")    printf 'Europe/Bucharest' ;;
      "Russian Standard Time")      printf 'Europe/Moscow' ;;
      "AUS Eastern Standard Time")  printf 'Australia/Sydney' ;;
      "UTC")                        printf 'UTC' ;;
      *)                            printf 'UTC' ;;
    esac
    return
  fi
  printf 'UTC'
}

cleanup() { # single-session rule: DELETE the SAME instance on exit (404 tolerated)
  rm -f "$SSE_FILE"
  if [ "$DRY_RUN" -eq 0 ] && [ -n "$INSTANCE_ID" ] && [ -n "$AUTH_TOKEN" ]; then
    info "cleanup: DELETE instance $(printf '%s' "$INSTANCE_ID" | cut -c1-8)... (404 tolerated)"
    code=$(curl -sS -o /dev/null -w '%{http_code}' -X DELETE "$API_URL/api/v1/freebuff/session" \
      -H "Authorization: Bearer $AUTH_TOKEN" \
      -H "User-Agent: $BUN_UA" \
      -H "x-fb-timezone: $TZ_SEL" \
      -H "x-freebuff-first-tab-discount: 0" \
      -H "x-freebuff-instance-id: $INSTANCE_ID" 2>/dev/null || echo "000")
    info "cleanup: DELETE http=$code (404/000 tolerated)"
  fi
}
trap cleanup EXIT

print_dry_run() { # every curl the LIVE run would send, with secrets redacted
  cat <<EOF
--- DRY-RUN: login code POST (not sent) ---
curl -sS -X POST "$WEB_URL/api/auth/cli/code" \\
  -H 'Content-Type: application/json' \\
  -H 'User-Agent: $BUN_UA' \\
  --data-raw '{"fingerprintId":"<FINGERPRINT_REDACTED>"}'
--- DRY-RUN: login status poll every ${POLL_INTERVAL_S}s up to ${POLL_TIMEOUT_S}s (not sent) ---
curl -sS "$WEB_URL/api/auth/cli/status?fingerprintId=<FP>&fingerprintHash=<HASH>&expiresAt=<EXP>" \\
  -H 'User-Agent: $BUN_UA'
# 401 = pending (silent); success when JSON .user.authToken is set.
--- DRY-RUN: admission POST, no body, no Content-Type (not sent) ---
curl -sS -X POST "$API_URL/api/v1/freebuff/session/admission" \\
  -H 'Authorization: Bearer <REDACTED>' \\
  -H 'User-Agent: $BUN_UA' \\
  -H 'x-freebuff-model: ${MODEL:-<MODEL>}' \\
  -H 'x-fb-timezone: $TZ_SEL' \\
  -H 'x-freebuff-wallet-spend-limit: 0' \\
  -H 'x-freebuff-first-tab-discount: 0'
--- DRY-RUN: chat POST (not sent) ---
curl -sS -N -X POST "$API_URL/api/v1/chat/completions" \\
  -H 'Authorization: Bearer <REDACTED>' \\
  -H 'User-Agent: $AI_UA' \\
  -H 'Content-Type: application/json' \\
  --data-raw '<BODY below>'
--- DRY-RUN: body envelope (ids ephemeral per run) ---
$(jq -n --arg m "${MODEL:-<MODEL>}" \
  '{model: $m, stream: true,
    messages: [{role: "user", content: "ping"}],
    codebuff_metadata: {run_id: "<RUN>", client_id: "<CLIENT>",
      trace_session_id: "<TRACE>", freebuff_instance_id: "<INSTANCE>",
      llm_step_number: "1", cost_mode: "free"}}')
# No x-freebuff-* headers on chat: model + instance ride the body only.
--- DRY-RUN: cleanup DELETE (not sent) ---
curl -sS -X DELETE "$API_URL/api/v1/freebuff/session" \\
  -H 'Authorization: Bearer <REDACTED>' \\
  -H 'User-Agent: $BUN_UA' \\
  -H 'x-fb-timezone: $TZ_SEL' \\
  -H 'x-freebuff-first-tab-discount: 0' \\
  -H 'x-freebuff-instance-id: <INSTANCE>'
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run) DRY_RUN=1; shift ;;
    --logout) rm -f "$CRED_FILE"; info "logged out: removed $CRED_FILE"; exit 0 ;;
    -h|--help) usage; exit 0 ;;
    *) err "unknown arg: $1"; usage; exit 2 ;;
  esac
done

need curl
need jq
need openssl

TZ_SEL="$(detect_tz)"
info "== local-emu (dry-run=$([ "$DRY_RUN" -eq 1 ] && echo yes || echo no)) =="
info "web=$WEB_URL api=$API_URL tz=$TZ_SEL"

# --- (b) MODEL (local, no network): numbered menu, default = hero [1] --------
info "--- MODEL: numbered free-model menu (static list; extend via MODEL-SELECT.md) ---"
for i in "${!MODELS[@]}"; do
  info "  [$((i+1))] ${MODELS[$i]}  (${MODEL_NOTES[$i]})"
done
CHOICE=""
if [ -t 0 ]; then
  printf 'Pick a model [1]: '
  read -r CHOICE || CHOICE=""
else
  info "(non-interactive stdin: using default [1])"
fi
CHOICE="${CHOICE:-1}"
if ! [[ "$CHOICE" =~ ^[1-9][0-9]*$ ]] || [ "$CHOICE" -lt 1 ] || [ "$CHOICE" -gt "${#MODELS[@]}" ]; then
  err "choice '$CHOICE' out of range 1-${#MODELS[@]}"; exit 2
fi
MODEL="${MODELS[$((CHOICE-1))]}"
export MODEL
info "MODEL=$MODEL"
info "SEND-SHAPE: x-freebuff-model header (admission) + .model (chat body)."

if [ "$DRY_RUN" -eq 1 ]; then
  print_dry_run
  info "DRY-RUN done: nothing sent, nothing stored."
  exit 0
fi

# --- (a) LOGIN: reuse stored token, else device-code via browser -------------
if [ -f "$CRED_FILE" ]; then
  STORED="$(jq -r '.authToken // empty' "$CRED_FILE" 2>/dev/null || true)"
  if [ -n "$STORED" ]; then
    AUTH_TOKEN="$STORED"
    FINGERPRINT_ID="$(jq -r '.fingerprintId // empty' "$CRED_FILE" 2>/dev/null || true)"
    FINGERPRINT_HASH="$(jq -r '.fingerprintHash // empty' "$CRED_FILE" 2>/dev/null || true)"
    info "--- LOGIN: reusing stored token (skip browser) ---"
    info "SEND-SHAPE: no auth request sent. file=$CRED_FILE token=$(mask "$AUTH_TOKEN")"
  fi
fi

if [ -z "$AUTH_TOKEN" ]; then
  # Isolated variant: random-only, never hardware (LOGIN-TUI.md fingerprint shape).
  FINGERPRINT_ID="enhanced-$(openssl rand -base64 32 | tr '+/' '-_' | tr -d '=')"
  info "--- LOGIN: POST {fingerprintId} (Bun UA, unauthenticated) ---"
  info "SEND-SHAPE: POST $WEB_URL/api/auth/cli/code body keys {fingerprintId}; value redacted (len ${#FINGERPRINT_ID})."
  CODE_RESP="$(curl -sS -X POST "$WEB_URL/api/auth/cli/code" \
    -H 'Content-Type: application/json' \
    -H "User-Agent: $BUN_UA" \
    -d "{\"fingerprintId\":\"$FINGERPRINT_ID\"}")"
  LOGIN_URL="$(printf '%s' "$CODE_RESP" | jq -r '.loginUrl // empty')"
  FINGERPRINT_HASH="$(printf '%s' "$CODE_RESP" | jq -r '.fingerprintHash // empty')"
  EXPIRES_AT="$(printf '%s' "$CODE_RESP" | jq -r '.expiresAt // empty')"
  if [ -z "$LOGIN_URL" ] || [ -z "$FINGERPRINT_HASH" ] || [ -z "$EXPIRES_AT" ]; then
    err "code response missing loginUrl/fingerprintHash/expiresAt (link may have expired upstream)"
    exit 1
  fi
  AUTH_CODE="$(printf '%s' "$LOGIN_URL" | sed -n 's/.*auth_code=\([^&]*\).*/\1/p')"
  if [ -n "$AUTH_CODE" ]; then
    LOGIN_URL="https://freebuff.com/onboard?auth_code=$AUTH_CODE"
  fi
  info ""
  info "Open this URL in your browser and complete the login:"
  info "  $LOGIN_URL"
  info "(If it shows an expired/stale page, re-run: each run mints a fresh code.)"
  info ""
  ENC_FP="$(url_encode "$FINGERPRINT_ID")"
  ENC_HASH="$(url_encode "$FINGERPRINT_HASH")"
  ENC_EXP="$(url_encode "$EXPIRES_AT")"
  info "--- LOGIN: polling GET status every ${POLL_INTERVAL_S}s up to ${POLL_TIMEOUT_S}s (401=pending) ---"
  info "SEND-SHAPE: GET $WEB_URL/api/auth/cli/status?fingerprintId=<FP>&fingerprintHash=<HASH>&expiresAt=<EXP> (Bun UA, unauthenticated)."
  START="$(date +%s)"
  ATTEMPTS=0
  USER_JSON=""
  while true; do
    ELAPSED=$(( $(date +%s) - START ))
    if [ "$ELAPSED" -ge "$POLL_TIMEOUT_S" ]; then
      err "login timed out after ${POLL_TIMEOUT_S}s; re-run for a fresh code."
      exit 1
    fi
    ATTEMPTS=$((ATTEMPTS + 1))
    sleep "$POLL_INTERVAL_S"
    STATUS_BODY="$(mktemp)"
    HTTP_CODE="$(curl -sS -o "$STATUS_BODY" -w '%{http_code}' \
      -H "User-Agent: $BUN_UA" \
      "$WEB_URL/api/auth/cli/status?fingerprintId=$ENC_FP&fingerprintHash=$ENC_HASH&expiresAt=$ENC_EXP" 2>/dev/null || echo "000")"
    if [ "$HTTP_CODE" = "401" ] || [ "$HTTP_CODE" = "000" ]; then
      if [ $((ATTEMPTS % 6)) -eq 0 ]; then dim "  ... still waiting (${ELAPSED}s elapsed, 401=pending)"; fi
      rm -f "$STATUS_BODY"
      continue
    fi
    CANDIDATE="$(jq -r '.user.authToken // empty' "$STATUS_BODY" 2>/dev/null || true)"
    if [ -n "$CANDIDATE" ]; then
      USER_JSON="$(cat "$STATUS_BODY")"
      rm -f "$STATUS_BODY"
      break
    fi
    dim "  HTTP $HTTP_CODE without user.authToken yet; retrying..."
    rm -f "$STATUS_BODY"
  done
  AUTH_TOKEN="$(printf '%s' "$USER_JSON" | jq -r '.user.authToken // empty')"
  mkdir -p "$(dirname "$CRED_FILE")"
  chmod 700 "$(dirname "$CRED_FILE")"
  TMP_CRED="$(mktemp)"
  jq -n --arg t "$AUTH_TOKEN" --arg f "$FINGERPRINT_ID" --arg h "$FINGERPRINT_HASH" \
    --arg o "$(date -u +%FT%TZ)" \
    '{authToken: $t, fingerprintId: $f, fingerprintHash: $h, obtainedAt: $o}' > "$TMP_CRED"
  mv "$TMP_CRED" "$CRED_FILE"
  chmod 600 "$CRED_FILE"
  info "LOGIN success: saved {authToken,fingerprintId,fingerprintHash,obtainedAt} to $CRED_FILE (0600)."
  info "token=$(mask "$AUTH_TOKEN")"
fi

# --- (c) SESSION: admission POST, no body ------------------------------------
info "--- SESSION: POST admission (Bun UA, Bearer, no body, no Content-Type) ---"
info "SEND-SHAPE: headers {Authorization: Bearer $(mask "$AUTH_TOKEN"), User-Agent: $BUN_UA, x-freebuff-model: $MODEL, x-fb-timezone: $TZ_SEL, x-freebuff-wallet-spend-limit: 0, x-freebuff-first-tab-discount: 0}."
SESS_BODY="$(mktemp)"
HTTP_CODE="$(curl -sS -o "$SESS_BODY" -w '%{http_code}' -X POST "$API_URL/api/v1/freebuff/session/admission" \
  -H "Authorization: Bearer $AUTH_TOKEN" \
  -H "User-Agent: $BUN_UA" \
  -H "x-freebuff-model: $MODEL" \
  -H "x-fb-timezone: $TZ_SEL" \
  -H "x-freebuff-wallet-spend-limit: 0" \
  -H "x-freebuff-first-tab-discount: 0" || echo "000")"
STATUS="$(jq -r '.status // empty' "$SESS_BODY" 2>/dev/null || true)"
info "admission: http=$HTTP_CODE status=${STATUS:-<non-JSON>}"
case "$STATUS" in
  active) ;;
  model_locked|model_unavailable)
    err "admission refused ($STATUS): pick another model (re-run, choose a different number)."
    rm -f "$SESS_BODY"; exit 1 ;;
  rate_limited|spend_limited|ip_capped)
    err "admission throttled ($STATUS): wait for the quota window, then re-run."
    rm -f "$SESS_BODY"; exit 1 ;;
  country_blocked|banned)
    err "admission terminal ($STATUS)."
    rm -f "$SESS_BODY"; exit 1 ;;
  consent_required|first_tab_discount_changed|"")
    err "admission gate ($STATUS, http=$HTTP_CODE): shape keys: $(jq -r 'keys_unsorted | join(",")' "$SESS_BODY" 2>/dev/null || echo '<non-JSON>')"
    rm -f "$SESS_BODY"; exit 1 ;;
  *)
    err "unexpected admission status '$STATUS' (http=$HTTP_CODE)."
    rm -f "$SESS_BODY"; exit 1 ;;
esac
INSTANCE_ID="$(jq -r '.instanceId // empty' "$SESS_BODY" 2>/dev/null || true)"
rm -f "$SESS_BODY"
if [ -z "$INSTANCE_ID" ]; then err "active session without instanceId; exiting."; exit 1; fi
info "seat held: single session, instance $(printf '%s' "$INSTANCE_ID" | cut -c1-8)... (auto-DELETE on exit)"

# --- (d) CHAT: one ping turn, SSE chunk count + first bytes only --------------
info "--- CHAT: POST one turn {model, messages:[{role:user,content:ping}], stream:true} (ai-sdk UA) ---"
RUN_ID="$(openssl rand -hex 8)"
CLIENT_ID="$(openssl rand -hex 8)"
TRACE_ID="$(openssl rand -hex 8)"
info "SEND-SHAPE: codebuff_metadata keys {run_id,client_id,trace_session_id,freebuff_instance_id,llm_step_number:\"1\",cost_mode:free} (ephemeral ids, values redacted); NO x-freebuff-* headers on chat."
SSE_FILE="$(mktemp)"
jq -n --arg m "$MODEL" --arg run "$RUN_ID" --arg cli "$CLIENT_ID" --arg tr "$TRACE_ID" \
  --arg inst "$INSTANCE_ID" \
  '{model: $m, stream: true,
    messages: [{role: "user", content: "ping"}],
    codebuff_metadata: {run_id: $run, client_id: $cli, trace_session_id: $tr,
      freebuff_instance_id: $inst, llm_step_number: "1", cost_mode: "free"}}' \
| curl -sS -N -X POST "$API_URL/api/v1/chat/completions" \
  -H "Authorization: Bearer $AUTH_TOKEN" \
  -H "User-Agent: $AI_UA" \
  -H 'Content-Type: application/json' \
  --data-raw @- > "$SSE_FILE" || { err "chat POST failed (transport)."; exit 1; }
CHUNKS="$(grep -c '^data:' "$SSE_FILE" 2>/dev/null || true)"
FIRST_TEXT="$(grep '^data: {' "$SSE_FILE" 2>/dev/null | sed 's/^data: //' \
  | jq -rs '[.[] | (.choices // [])[] | .delta.content? // empty] | join("")' 2>/dev/null | head -c 300 || true)"
info "chat: SSE data chunks: ${CHUNKS:-0}"
info "chat: first text bytes: ${FIRST_TEXT:-<none parsed>}"
info "done: exiting (trap DELETEs the session)."
