#!/usr/bin/env bash
# 03-chat.sh — one chat turn on the session from 02 (DRY-RUN by default).
# Static source: sdk/src/impl/model-provider.ts (POST /api/v1/chat/completions, Bearer, ai-sdk UA),
#   sdk/src/impl/llm.ts + run-agent-step.ts (codebuff_metadata envelope), proxy upstream/chat.go.
set -euo pipefail

API_URL="${FREEBUFF_API_URL:-https://www.codebuff.com}"  # UNVERIFIED default; override per run.
MODEL="${FREEBUFF_MODEL:-}"                 # required for --send (selected model id)
TOKEN="${FREEBUFF_TOKEN:-}"                 # Bearer; env only
INSTANCE_ID="${FREEBUFF_INSTANCE_ID:-}"     # per-session binding (from 02)
RUN_ID="${FREEBUFF_RUN_ID:-ephemeral-run}"  # per-run id (agent-runs START in full flow; ephemeral here)
CLIENT_ID="${FREEBUFF_CLIENT_ID:-ephemeral-client}"
TRACE_ID="${FREEBUFF_TRACE_ID:-ephemeral-trace}"
STEP="${FREEBUFF_STEP:-1}"
PROMPT="${FREEBUFF_PROMPT:-ping}"           # single short turn only
SEND=0

usage() {
  cat <<'EOF'
Usage: 03-chat.sh [--send] [--model ID] [--prompt TEXT]
  Default (no flags): DRY-RUN — print curl + envelope, send nothing.
  --send  Actually POST one non-retry turn. Requires FREEBUFF_TOKEN + FREEBUFF_MODEL
          + explicit per-use approval. No auto-retry here (proxy/CLI retry policy excluded).
Env: FREEBUFF_API_URL, FREEBUFF_TOKEN, FREEBUFF_MODEL, FREEBUFF_INSTANCE_ID,
     FREEBUFF_RUN_ID, FREEBUFF_CLIENT_ID, FREEBUFF_TRACE_ID, FREEBUFF_STEP, FREEBUFF_PROMPT.
Notes: SSE stream relays verbatim; usage/credits arrive via callbacks, NOT the stream —
       quota/balance refresh from the NEXT session GET poll (02-session.sh), never parsed here.
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --send) SEND=1; shift ;;
    --model) MODEL="$2"; shift 2 ;;
    --prompt) PROMPT="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown arg: $1" >&2; usage; exit 2 ;;
  esac
done

AI_UA="ai-sdk/openai-compatible/1.0.0/codebuff"  # pinned shape; CLI uses live package VERSION (drift note)
echo "== 03-chat (dry-run=$([ "$SEND" -eq 1 ] && echo no || echo yes)) =="
echo "model=${MODEL:-<unset>} instance_set=$([ -n "$INSTANCE_ID" ] && echo yes || echo no) step=$STEP"

build_body() {
  jq -n --arg m "$MODEL" --arg p "$PROMPT" \
    --arg run "$RUN_ID" --arg cli "$CLIENT_ID" --arg tr "$TRACE_ID" \
    --arg inst "$INSTANCE_ID" --arg step "$STEP" \
    '{model: $m, stream: true,
      messages: [{role: "user", content: $p}],
      codebuff_metadata: {run_id: $run, client_id: $cli, trace_session_id: $tr,
        freebuff_instance_id: $inst, llm_step_number: $step, cost_mode: "free"},
      provider: {data_collection: "deny"}}'
}

if [ "$SEND" -eq 0 ]; then
  cat <<EOF
--- DRY-RUN: chat POST (not sent) ---
curl -sS -N -X POST "$API_URL/api/v1/chat/completions" \\
  -H 'Authorization: Bearer <REDACTED>' \\
  -H 'User-Agent: $AI_UA' \\
  -H 'Content-Type: application/json' \\
  --data-raw '<BODY below>'
--- DRY-RUN: body envelope ---
$(MODEL="${MODEL:-<MODEL>}" PROMPT="$PROMPT" build_body)
--- DRY-RUN: headers NOT sent here (session-scoped only) ---
  No x-freebuff-model / x-freebuff-instance-id headers on chat (chat.go scope).
  Optional x-freebuff-acting-user-id carries ONLY the token owner's own /me id.
--- DRY-RUN: stream + quota notes ---
  Relay SSE verbatim to stdout; do NOT parse quota from chunks.
  Refresh quota/balance with: 02-session.sh [--send] (GET poll), then DELETE.
EOF
  exit 0
fi

# ---------------- LIVE (explicit --send only) ----------------
if [ -z "$TOKEN" ]; then echo "ERROR: FREEBUFF_TOKEN required for --send (env only)." >&2; exit 2; fi
if [ -z "$MODEL" ]; then echo "ERROR: FREEBUFF_MODEL required for --send." >&2; exit 2; fi
echo "LIVE: POST one turn (model=$MODEL, prompt_len=${#PROMPT}); SSE relay, no retry."
build_body | curl -sS -N -X POST "$API_URL/api/v1/chat/completions" \
  -H "Authorization: Bearer $TOKEN" -H "User-Agent: $AI_UA" \
  -H 'Content-Type: application/json' --data-raw @-
echo
echo "Turn complete. Quota/balance: run the session GET poll next (02), not parsed here."
