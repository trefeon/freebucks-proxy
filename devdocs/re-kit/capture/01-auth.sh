#!/usr/bin/env bash
# 01-auth.sh — device-code login capture (DRY-RUN by default, prints curl, sends nothing).
# Static source: upstream/freebuff/cli/src/login/login-flow.ts (generateLoginUrl + poll loop),
#   cli/src/utils/codebuff-api.ts (POST /api/auth/cli/code, GET /api/auth/cli/status),
#   cli/src/utils/fingerprint.ts (enhanced- hardware hash shape), plain-login.ts.
# Vendor pin: repo 0.0.193; live CLI 0.0.194 gap noted in README (UNVERIFIED delta).
set -euo pipefail

API_URL="${FREEBUFF_API_URL:-https://www.codebuff.com}"  # UNVERIFIED default: prod web/API origin; override per run.
INTERVAL="${POLL_INTERVAL_S:-5}"    # login-flow.ts:122-123 default 5000ms
TIMEOUT="${POLL_TIMEOUT_S:-300}"    # login-flow.ts:122-123 default 5*60*1000ms
FINGERPRINT_ID="${FREEBUFF_FINGERPRINT_ID:-}"  # ephemeral unless caller exports a stable one
SEND=0

usage() {
  cat <<'EOF'
Usage: 01-auth.sh [--send] [--fingerprint-id ID]
  Default (no flags): DRY-RUN — print the curl commands, send nothing, create nothing.
  --send              Actually POST /api/auth/cli/code and poll GET status. No token needed.
                      Requires explicit per-use approval (tokens env-only; this step uses none).
Env: FREEBUFF_API_URL (default shown above, UNVERIFIED), FREEBUFF_FINGERPRINT_ID,
     POLL_INTERVAL_S (default 5), POLL_TIMEOUT_S (default 300).
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --send) SEND=1; shift ;;
    --fingerprint-id) FINGERPRINT_ID="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown arg: $1" >&2; usage; exit 2 ;;
  esac
done

if [ -z "$FINGERPRINT_ID" ]; then
  # Isolated/ephemeral variant (cf. GenerateIsolatedFingerprintID): random, NOT the hardware hash.
  # Prod CLI shape is `enhanced-<base64url sha256>` (fingerprint.ts:84-129); kit never reads hardware.
  if command -v openssl >/dev/null 2>&1; then
    FINGERPRINT_ID="enhanced-$(openssl rand -hex 16)"
  else
    FINGERPRINT_ID="enhanced-ephemeral-$RANDOM$RANDOM"
  fi
fi

CODE_URL="$API_URL/api/auth/cli/code"
echo "== 01-auth (dry-run=$([ "$SEND" -eq 1 ] && echo no || echo yes)) =="
echo "POST body carries ONLY {fingerprintId}; raw hardware fields never leave the host."
echo "Fingerprint shape: ${FINGERPRINT_ID:0:9}... (value redacted, length ${#FINGERPRINT_ID})"

print_code_curl() {
  cat <<EOF
curl -sS -X POST "$CODE_URL" \\
  -H 'Content-Type: application/json' \\
  --data-raw '{"fingerprintId":"<FINGERPRINT_REDACTED>"}'
EOF
}
print_status_curl() {
  cat <<'EOF'
curl -sS -D - -o /tmp/fb-status.json \
  "$API_URL/api/auth/cli/status?fingerprintId=<FP>&fingerprintHash=<HASH>&expiresAt=<EXP>"
# 401 + empty user = pending (silent); success when JSON .user is an object.
EOF
}

if [ "$SEND" -eq 0 ]; then
  echo "--- DRY-RUN: code request (not sent) ---"
  print_code_curl
  echo "--- DRY-RUN: status poll (not sent): every ${INTERVAL}s up to ${TIMEOUT}s ---"
  print_status_curl
  echo "--- DRY-RUN: on success, report loginUrl SHAPE only ---"
  echo 'jq check: jq -e ".loginUrl | contains(\"auth_code\")" /tmp/fb-code.json && echo "loginUrl carries auth_code (value NEVER logged)"'
  echo "NOTE: open loginUrl verbatim in a browser (CLI does not rewrite it); complete OAuth server-side, then poll."
  exit 0
fi

# ---------------- LIVE (explicit --send only) ----------------
echo "LIVE: POST $CODE_URL"
ESCAPED_FP=$(printf '%s' "$FINGERPRINT_ID" | sed 's/"/\\"/g')
RESP=$(curl -sS -X POST "$CODE_URL" -H 'Content-Type: application/json' \
  --data-raw "{\"fingerprintId\":\"$ESCAPED_FP\"}")
# Shape-only report: presence of keys, NEVER values.
echo "$RESP" | jq -e '{has_loginUrl: has("loginUrl"), has_fingerprintHash: has("fingerprintHash"), has_expiresAt: has("expiresAt"), loginUrl_has_auth_code: ((.loginUrl // "") | contains("auth_code"))}' \
  || { echo "ERROR: unexpected code-response shape (see UNVERIFIED gap note)" >&2; exit 1; }
echo "Open the loginUrl (printed ONLY by the server response holder, not logged here) in a browser."
HASH=$(echo "$RESP" | jq -r '.fingerprintHash // empty')
EXP=$(echo "$RESP" | jq -r '.expiresAt // empty')
if [ -z "$HASH" ] || [ -z "$EXP" ]; then echo "ERROR: missing fingerprintHash/expiresAt" >&2; exit 1; fi

echo "Polling status every ${INTERVAL}s, deadline ${TIMEOUT}s (401=pending)..."
deadline=$(( SECONDS + TIMEOUT ))
while [ $SECONDS -lt $deadline ]; do
  sleep "$INTERVAL"
  code=$(curl -sS -o /tmp/fb-status.json -w '%{http_code}' \
    "$API_URL/api/auth/cli/status?fingerprintId=$FINGERPRINT_ID&fingerprintHash=$HASH&expiresAt=$EXP" || echo "000")
  if [ "$code" = "401" ]; then continue; fi
  if [ "$code" = "000" ]; then echo "network error, retrying..." >&2; continue; fi
  if jq -e '.user | type == "object"' /tmp/fb-status.json >/dev/null 2>&1; then
    echo "SUCCESS: server attached user object."
    jq '{id: .user.id, email_set: ((.user.email // "") != ""), fingerprint_echo: true} | del(.email)' /tmp/fb-status.json 2>/dev/null || echo "SUCCESS (shape changed; inspect keys only, not values)."
    echo "Persist per CLI: <configDir>/credentials.json {default:{id,email,authToken,fingerprintId,fingerprintHash}} mode 0600 (manual step, not done here)."
    exit 0
  fi
  echo "HTTP $code without user object; retrying..." >&2
done
echo "TIMEOUT after ${TIMEOUT}s (LOGIN_TIMEOUT). Re-run 01-auth.sh for a fresh code." >&2
exit 1
