#!/usr/bin/env bash
# 04-live-debug.sh -- live freebuff CLI debug/trace harness (PROBE default, unauthenticated).
#
# Two phases:
#   PROBE (default, no flags): fully unauthenticated. Preflight (version, help
#     capture, flag discovery), env KEY-NAMES only (never values), config dir
#     NAMES only (never contents), DEBUG=* / --verbose trial runs that exit
#     without auth (15s timeout each), strace -f -e trace=%network of a
#     no-login startup (killed after 10s if it blocks), SNI-only packet
#     capture when available (no -w pcap with payloads), /proc/<pid>/fd +
#     cmdline snapshot, timing log. Creates no session, sends no auth.
#   LIVE (--live + FREEBUFF_TOKEN): single admission -> one GET poll ->
#     DELETE cleanup -> exit. Timing + SNI + strace network slice. Bodies
#     are NEVER logged (shapes + status codes only).
#
# Static sources: capture/02-session.sh (admission/poll/DELETE routes and
#   headers), capture/01-auth.sh (dry-run default + redaction rules),
#   kit docs ENDPOINTS.md / HEADERS.md / SESSION.md.
# Pin note: repo pins 0.0.193; live CLI is 0.0.194 (UNVERIFIED delta until
#   the vendor clone is re-pinned). Preflight records the actual version.
# Remote-write rule: every log lands under $OUTDIR (default
#   /tmp/fb-live-debug on the target host). Remove it after review
#   (rm -rf "$OUTDIR"). Nothing here writes outside $OUTDIR.
set -euo pipefail

OUTDIR="${FB_DEBUG_OUTDIR:-/tmp/fb-live-debug}"
API_URL="${FREEBUFF_API_URL:-https://www.codebuff.com}"  # UNVERIFIED default; override per run (cf. 02-session.sh).
LIVE=0

usage() {
  cat <<'EOF'
Usage: 04-live-debug.sh [--outdir DIR]
       04-live-debug.sh --live [--outdir DIR]
  Default (no flags): PROBE -- unauthenticated only, no session, no token use.
  --live              Authenticated single-seat trace. REFUSED unless
                      FREEBUFF_TOKEN is exported in the environment AND this
                      flag is passed AND a dedicated off-prod test token has
                      explicit per-use approval. Never run against a pool that
                      serves live gateway traffic (supersede/takeover risk).
  --outdir DIR        Log directory (default /tmp/fb-live-debug). All raw
                      logs stay here; rm -rf it after review.
Env: FB_DEBUG_OUTDIR, FREEBUFF_API_URL, FREEBUFF_TOKEN (LIVE only, env only).
Redaction: token as first2...last2; IPs as <IP>/<IP6>; SNI/hostnames as
  registrable-ish domains (last two DNS labels); config/credential VALUES
  never read, never logged (names/presence only).
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --live) LIVE=1; shift ;;
    --outdir) OUTDIR="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown arg: $1" >&2; usage; exit 2 ;;
  esac
done

mkdir -p "$OUTDIR"
TLOG="$OUTDIR/timing.log"
: > "$TLOG"

T0=0
t_start() { T0=$(date +%s); echo "[$(date '+%H:%M:%S')] START $1" | tee -a "$TLOG"; }
t_end() { local now_s; now_s=$(date +%s); echo "[$(date '+%H:%M:%S')] END $1 ($((now_s - T0))s)" | tee -a "$TLOG"; }

redact_token() { # redact_token <secret>: first2...last2, never the middle
  local s="$1"
  if [ "${#s}" -le 6 ]; then echo "<REDACTED len=${#s}>";
  else echo "${s:0:2}...${s: -2} (len ${#s})"; fi
}
redact_ips() { # stdin->stdout: mask IPv4/IPv6 literals (counts/ports preserved)
  sed -E -e 's/[0-9]{1,3}(\.[0-9]{1,3}){3}/<IP>/g' \
         -e 's/([0-9a-fA-F]{0,4}:){2,}[0-9a-fA-F:.]+/<IP6>/g'
}
redact_domain() { # stdin hostnames -> registrable-ish domains (last two labels)
  awk -F. 'NF>=2{print $(NF-1)"."$NF} NF<2{print "<NON-DNS>"}'
}
have() { command -v "$1" >/dev/null 2>&1; }

# ---------------------------------------------------------------- PROBE --
probe() {
  # The probe never authenticates: drop any inherited token from this phase
  # so no step can attach it by accident (LIVE re-reads it from the env).
  if [ -n "${FREEBUFF_TOKEN:-}" ]; then
    echo "PROBE: FREEBUFF_TOKEN is set in env; ignoring it for this phase (token $(redact_token "$FREEBUFF_TOKEN"))."
    unset FREEBUFF_TOKEN
  fi
  echo "== 04-live-debug PROBE (unauthenticated; outdir=$OUTDIR) =="

  t_start "preflight"
  echo "--- preflight: binary, version, help ---"
  have freebuff || { echo "ERROR: freebuff not on PATH; nothing further to probe." >&2; exit 1; }
  echo "which: $(command -v freebuff)"
  if have readlink; then echo "realpath: $(readlink -f "$(command -v freebuff)" 2>/dev/null || echo '<unresolvable>')"; fi
  echo "version exit/code+output:"
  ( freebuff --version 2>&1; echo "exit=$?" ) | tee "$OUTDIR/version.log"
  ( freebuff --help >"$OUTDIR/help.log" 2>"$OUTDIR/help-err.log"; echo "exit=$?" ) | tee -a "$TLOG"
  echo "help bytes: $(wc -c <"$OUTDIR/help.log" | tr -d ' ') ; stderr bytes: $(wc -c <"$OUTDIR/help-err.log" | tr -d ' ')"
  echo "--- flag discovery (from captured --help text; presence only) ---"
  grep -aiEo -- '--[a-zA-Z0-9][a-zA-Z0-9-]+' "$OUTDIR/help.log" 2>/dev/null | sort -u | head -60 || echo "<no flags parsed>"
  echo "--- smoke/api-url equivalents ---"
  if grep -aiEo -- '--[a-z0-9-]*smoke[a-z0-9-]*|--[a-z0-9-]*api-?url[a-z0-9-]*' "$OUTDIR/help.log" 2>/dev/null | sort -u | tee "$OUTDIR/smoke-flags.log"; then
    [ -s "$OUTDIR/smoke-flags.log" ] || echo "<no --smoke* / --*api-url flag in --help>"
  fi
  for sub in login chat session models; do
    if timeout 15s freebuff "$sub" --help >"$OUTDIR/help-$sub.log" 2>&1; then
      echo "subcommand '$sub': has --help ($(wc -c <"$OUTDIR/help-$sub.log" | tr -d ' ') bytes)"
    else
      echo "subcommand '$sub': no --help (exit $?)"
    fi
  done
  if have node; then echo "node: $(node --version 2>&1)"; else echo "node: <absent>"; fi
  t_end "preflight"

  t_start "env-names"
  echo "--- env: KEY NAMES ONLY (values never printed, never logged) ---"
  # cut BEFORE grep so values can never reach output even on match.
  env | cut -d= -f1 | grep -E '^(FREEBUFF_|CODEBUFF_|NEXT_PUBLIC_)' | sort | tee "$OUTDIR/env-names.log" || echo "<none of FREEBUFF_*/CODEBUFF_*/NEXT_PUBLIC_* set>"
  echo "count: $(wc -l <"$OUTDIR/env-names.log" | tr -d ' ')"
  env | cut -d= -f1 | grep -aiE '^(http_proxy|https_proxy|all_proxy|no_proxy)$' | sort || echo "<no proxy overrides set>"
  t_end "env-names"

  t_start "config-names"
  echo "--- config: NAMES ONLY (contents never read) ---"
  CFG_BASE="${XDG_CONFIG_HOME:-$HOME/.config}/manicode"
  CFG="$CFG_BASE/freebuff"
  echo "config base: $CFG_BASE ; legacy path: $CFG"
  # NOTE: on some installs $CFG is a bundled executable FILE, not a dir.
  if [ -d "$CFG" ]; then echo "$CFG is a DIRECTORY:"
  elif [ -f "$CFG" ]; then echo "$CFG is a FILE ($(wc -c <"$CFG" | tr -d ' ') bytes; content never read):"
  else echo "$CFG: absent"; fi
  if [ -d "$CFG_BASE" ]; then
    ls -la "$CFG_BASE" | tee "$OUTDIR/config-names.log"
    if [ -e "$CFG_BASE/credentials.json" ]; then echo "credentials.json: PRESENT (content never read)"; else echo "credentials.json: absent"; fi
  else
    echo "config base: absent"
  fi
  ls -l "$(command -v freebuff)" 2>/dev/null || true
  t_end "config-names"

  t_start "debug-trials"
  echo "--- DEBUG=* / --verbose trials (each must exit without auth, 15s cap) ---"
  ( DEBUG='*' timeout 15s freebuff --version >"$OUTDIR/debug-version.log" 2>&1; echo "DEBUG=* --version exit=$?" ) | tee -a "$TLOG"
  echo "DEBUG=* --version bytes: $(wc -c <"$OUTDIR/debug-version.log" | tr -d ' ')"
  ( DEBUG='*' timeout 15s freebuff --help >"$OUTDIR/debug-help.log" 2>&1; echo "DEBUG=* --help exit=$?" ) | tee -a "$TLOG"
  ( timeout 15s freebuff --verbose --help >"$OUTDIR/verbose-help.log" 2>&1; echo "--verbose --help exit=$? (caveat: --help may take precedence; compare bytes with help.log)" ) | tee -a "$TLOG"
  echo "verbose-help vs help identical: $(cmp -s "$OUTDIR/verbose-help.log" "$OUTDIR/help.log" && echo yes || echo no)"
  echo "--- debug-signal scan (key names / shapes only, values redacted) ---"
  grep -aiEo -- 'DEBUG|verbose|trace|trace_session_id|freebuff_instance_id|llm_step_number' "$OUTDIR/debug-version.log" "$OUTDIR/debug-help.log" "$OUTDIR/verbose-help.log" 2>/dev/null | sort | uniq -c | sort -rn | head -20 || echo "<no debug tokens>"
  t_end "debug-trials"

  t_start "packet-capture"
  echo "--- packet capture: SNI/hostnames ONLY (no -w pcap, no payloads) ---"
  SNI_LOG="$OUTDIR/sni.log"
  : > "$SNI_LOG"
  CAP_PID=""
  if have tshark; then
    echo "tool: tshark"
    tshark -a duration:14 -f 'tcp port 443' -Y 'tls.handshake.extensions_server_name' \
      -T fields -e tls.handshake.extensions_server_name >"$SNI_LOG" 2>"$OUTDIR/tshark-err.log" &
    CAP_PID=$!
  elif have tcpdump; then
    echo "tool: tcpdump (header-only; SNI not parseable from text output)"
    timeout 14s tcpdump -n -nn -s 120 -l 'tcp port 443' >"$OUTDIR/tcpdump.log" 2>"$OUTDIR/tcpdump-err.log" &
    CAP_PID=$!
  else
    echo "tool: <neither tshark nor tcpdump; SNI capture skipped (expected without CAP_NET_RAW)>"
  fi
  sleep 1  # let the capture settle before generating traffic
  t_end "packet-capture"

  t_start "strace-net"
  echo "--- strace: %network of a no-login startup (kill after 10s on block) ---"
  if have strace; then
    ( timeout -s KILL 20s strace -f -e trace=%network -o "$OUTDIR/fb-strace.log" \
        freebuff --help </dev/null >"$OUTDIR/help-straced.log" 2>&1; echo "strace --help exit=$?" ) | tee -a "$TLOG"
    echo "strace bytes: $(wc -c <"$OUTDIR/fb-strace.log" 2>/dev/null | tr -d ' ')"
    echo "--- connect() summary (hosts as domains/ports; IPs redacted) ---"
    if [ -s "$OUTDIR/fb-strace.log" ]; then
      grep -a 'connect(' "$OUTDIR/fb-strace.log" | redact_ips | sort | uniq -c | sort -rn | head -30 | tee "$OUTDIR/connect-summary.log"
      echo "AF_UNIX (local socket paths, no redaction needed):"
      grep -a 'connect(' "$OUTDIR/fb-strace.log" | grep -c 'AF_UNIX' || true
      echo "AF_INET/INET6 (remote; endpoints redacted above):"
      grep -acE 'connect\(.*AF_INET' "$OUTDIR/fb-strace.log" || true
    else
      echo "<empty strace log>"
    fi
  else
    echo "tool: <strace absent; network-slice skipped>"
  fi
  t_end "strace-net"

  t_start "bare-startup"
  echo "--- bare startup (stdin /dev/null, 10s cap; note TUI block, no login) ---"
  BARE_CODE=0
  timeout -s KILL 10s freebuff </dev/null >"$OUTDIR/bare-stdout.log" 2>"$OUTDIR/bare-stderr.log" || BARE_CODE=$?
  echo "bare startup exit=$BARE_CODE" | tee -a "$TLOG"
  echo "stdout bytes: $(wc -c <"$OUTDIR/bare-stdout.log" | tr -d ' '); stderr bytes: $(wc -c <"$OUTDIR/bare-stderr.log" | tr -d ' ')"
  echo "first printable line of stdout (control chars stripped, 200ch max):"
  tr -dc '[:print:]\n\t' <"$OUTDIR/bare-stdout.log" | head -c 200; echo
  echo "first printable line of stderr (control chars stripped, 200ch max):"
  tr -dc '[:print:]\n\t' <"$OUTDIR/bare-stderr.log" | head -c 200; echo
  if [ "$BARE_CODE" -eq 124 ] || [ "$BARE_CODE" -eq 137 ]; then
    echo "note: CLI blocked on input (killed by timeout) -- interactive TUI confirmed, no auth attempted."
  else
    echo "note: CLI exited on its own (exit $BARE_CODE) with stdin /dev/null."
  fi
  t_end "bare-startup"

  t_start "proc-snapshot"
  echo "--- /proc/<pid>/fd + cmdline snapshot (names only) ---"
  freebuff </dev/null >"$OUTDIR/proc-stdout.log" 2>&1 &
  BGPID=$!
  sleep 2
  if kill -0 "$BGPID" 2>/dev/null; then
    echo "pid $BGPID alive after 2s (blocking on input)"
    echo "cmdline: $(tr '\0' ' ' <"/proc/$BGPID/cmdline" 2>/dev/null || echo '<unreadable>')"
    ls -l "/proc/$BGPID/fd" 2>/dev/null | tee "$OUTDIR/fd.log" || echo "<fd unreadable>"
    kill -KILL "$BGPID" 2>/dev/null || true
    wait "$BGPID" 2>/dev/null || true
  else
    wait "$BGPID" 2>/dev/null && PC_CODE=0 || PC_CODE=$?
    echo "pid $BGPID already exited (exit $PC_CODE); no live /proc snapshot -- see bare-startup logs."
  fi
  t_end "proc-snapshot"

  if [ -n "$CAP_PID" ]; then
    wait "$CAP_PID" 2>/dev/null || true
    echo "--- SNI summary (domains only: last two DNS labels) ---"
    if have tshark; then
      if [ -s "$SNI_LOG" ]; then
        tr -s '[:space:]' '\n' <"$SNI_LOG" | grep -a . | redact_domain | sort | uniq -c | sort -rn | tee "$OUTDIR/sni-domains.log"
      else
        echo "<no ClientHello SNI observed during probe window (startup opened no new TLS)>"
      fi
    else
      echo "<tcpdump text mode: $(grep -ac . "$OUTDIR/tcpdump.log" 2>/dev/null || echo 0) header lines; SNI unparsed>"
      grep -a . "$OUTDIR/tcpdump.log" 2>/dev/null | redact_ips | head -10 || true
    fi
  fi

  echo "== PROBE COMPLETE =="
  echo "timing:"; cat "$TLOG"
  echo "outdir: $OUTDIR (rm -rf after review; raw logs never leave the host except redacted summaries)"
}

# ----------------------------------------------------------------- LIVE --
live() {
  if [ -z "${FREEBUFF_TOKEN:-}" ]; then
    cat >&2 <<'EOF'
REFUSAL: LIVE phase requires FREEBUFF_TOKEN exported in the environment.
Missing piece: a dedicated off-prod test token (no production seats) PLUS
explicit user approval for this run. Re-run probe (no flags) until then.
Nothing was sent; no seat held.
EOF
    exit 2
  fi
  echo "== 04-live-debug LIVE (single seat; token $(redact_token "$FREEBUFF_TOKEN")) =="
  TZ="${FREEBUFF_TZ:-UTC}"
  INSTANCE_ID=""
  cleanup() {
    if [ -n "$INSTANCE_ID" ]; then
      echo "cleanup: DELETE instance $(echo "$INSTANCE_ID" | cut -c1-8)... (404 tolerated)" >&2
      curl -sS -o /dev/null -w 'DELETE http=%{http_code} total=%{time_total}s\n' -X DELETE \
        "$API_URL/api/v1/freebuff/session" \
        -H "Authorization: Bearer $FREEBUFF_TOKEN" \
        -H "x-fb-timezone: $TZ" \
        -H "x-freebuff-instance-id: $INSTANCE_ID" || true
    fi
  }
  trap cleanup EXIT

  STRACE=""
  if have strace; then STRACE="strace -f -e trace=%network -o $OUTDIR/fb-live-strace.log"; fi

  t_start "admission"
  # shellcheck disable=SC2086
  $STRACE curl -sS -D "$OUTDIR/live-adm-hdrs.log" -o "$OUTDIR/live-adm-body.json" \
    -w 'admission http=%{http_code} total=%{time_total}s connect=%{time_connect}s tls=%{time_appconnect}s\n' \
    -X POST "$API_URL/api/v1/freebuff/session/admission" \
    -H "Authorization: Bearer $FREEBUFF_TOKEN" -H 'User-Agent: Bun' \
    -H "x-fb-timezone: $TZ" \
    -H "x-freebuff-wallet-spend-limit: ${FREEBUFF_WALLET_LIMIT:-0}" \
    -H "x-freebuff-first-tab-discount: ${FREEBUFF_FIRST_TAB:-0}" | tee -a "$TLOG"
  echo "admission shape (keys only, values never logged):"
  jq '{status, instanceId_set: ((.instanceId // "") != ""), model, keys: keys_unsorted}' "$OUTDIR/live-adm-body.json" \
    || { echo "non-JSON admission body (first 200 bytes, control-stripped):"; tr -dc '[:print:]\n\t' <"$OUTDIR/live-adm-body.json" | head -c 200; echo; }
  INSTANCE_ID=$(jq -r '.instanceId // empty' "$OUTDIR/live-adm-body.json" 2>/dev/null || true)
  if [ -z "$INSTANCE_ID" ]; then echo "No instanceId granted; exiting without seat." >&2; exit 1; fi
  echo "Seat held: instance $(echo "$INSTANCE_ID" | cut -c1-8)... -- exactly ONE session this run."
  t_end "admission"

  t_start "poll"
  curl -sS -o "$OUTDIR/live-poll-body.json" \
    -w 'poll http=%{http_code} total=%{time_total}s\n' \
    "$API_URL/api/v1/freebuff/session" \
    -H "Authorization: Bearer $FREEBUFF_TOKEN" \
    -H "x-fb-timezone: $TZ" \
    -H "x-freebuff-instance-id: $INSTANCE_ID" \
    -H "x-freebuff-compact-session: 1" | tee -a "$TLOG"
  echo "poll shape (keys only):"
  jq '{status, model, keys: keys_unsorted}' "$OUTDIR/live-poll-body.json" \
    || { echo "non-JSON poll body (first 200 bytes, control-stripped):"; tr -dc '[:print:]\n\t' <"$OUTDIR/live-poll-body.json" | head -c 200; echo; }
  t_end "poll"

  if have strace && [ -s "$OUTDIR/fb-live-strace.log" ]; then
    echo "--- strace network slice (IPs redacted) ---"
    grep -a 'connect(' "$OUTDIR/fb-live-strace.log" | redact_ips | sort | uniq -c | sort -rn | head -20
  fi
  echo "== LIVE COMPLETE (trap DELETEs the seat on exit) =="
  echo "timing:"; cat "$TLOG"
}

if [ "$LIVE" -eq 1 ]; then live; else probe; fi
