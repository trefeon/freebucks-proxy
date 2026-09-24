#!/usr/bin/env bash
# 06-web-shapes.sh -- freebuff.com web shape probes (read-only).
#
# Reads the user-provided cookie jar (Netscape-style 12-col dump OR standard
# 7-col Netscape TSV) via COOKIE_JAR, builds the Cookie header AT RUNTIME,
# and performs authenticated GETs only. Shapes are printed as key->type
# trees (jq walk redaction); VALUES never leave the process.
#
#   COOKIE_JAR='/path/to/jar' ./capture/06-web-shapes.sh pages
#   COOKIE_JAR='/path/to/jar' ./capture/06-web-shapes.sh shapes
#
# Subcommands (all GET, zero state change):
#   pages   -- GET /chat + /account: status/time/bytes/shell markers only
#   session -- GET /api/auth/session + /api/auth/providers (key->type shapes)
#   threads -- GET /api/chat/threads + first thread item (key->type shapes)
#   usage   -- GET /api/web/usage-summary (key->type shape)
#   account -- GET subscriptions/freebuff-session/convex-token/providers/
#              country/paid-api/access/auto-topup/changelog/stars (shapes)
#   shapes  -- session + threads + usage + account (full read-only sweep)
#
# NEVER invoked here (documented in WEB.md from static code, not probed):
#   POST /api/chat/stream|upload|ads|feedback|logs|gravity/conversion,
#   PATCH/DELETE /api/chat/threads/{id}, account delete/buy/topup/email.
set -u
ORIGIN="${WEB_ORIGIN:-https://freebuff.com}"
JAR="${COOKIE_JAR:-}"
UA="${WEB_UA:-Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/151.0.0.0 Safari/537.36}"

need() { command -v "$1" >/dev/null 2>&1 || { echo "need $1" >&2; exit 2; }; }
need curl; need jq; need python3
[ -n "$JAR" ] || { echo "COOKIE_JAR env must point at the cookie jar file (values never embedded here)" >&2; exit 2; }
[ -f "$JAR" ] || { echo "jar not found: $JAR" >&2; exit 2; }

# Build Cookie header at runtime from the jar (name=value pairs for the
# freebuff.com domains only). Values stay in memory; use --dry-run to verify
# without sending (header shown REDACTED).
COOKIE_HEADER="$(python3 - "$JAR" <<'EOF'
import sys
pairs = []
with open(sys.argv[1], encoding="utf-8", errors="replace") as f:
    for ln in f:
        ln = ln.rstrip("\n")
        if not ln.strip() or ln.startswith("#"):
            continue
        p = ln.split("\t")
        if len(p) >= 7 and p[0] in ("freebuff.com", ".freebuff.com"):
            pairs.append((p[5], p[6]))       # standard Netscape TSV
        elif len(p) >= 3 and p[2] in ("freebuff.com", ".freebuff.com"):
            pairs.append((p[0], p[1]))       # 12-col dump layout
print("; ".join("%s=%s" % (n, v) for n, v in pairs))
EOF
)"
export COOKIE_HEADER  # env-only transport to curl below

# shape-redactor: keys preserved, every scalar -> its type name.
SHAPE='walk(if type == "string" then "string" elif type == "number" then "number" elif type == "boolean" then "bool" elif type == "null" then "null" else . end)'

jget() { # jget <path> : JSON GET -> key->type shape (values redacted)
  curl -sS -m 25 -w '\nHTTP %{http_code} %{time_total}s %{size_download}B\n' \
    -H "User-Agent: $UA" -H 'Accept: application/json' \
    -H "Cookie: $COOKIE_HEADER" "$ORIGIN$1" \
  | { IFS= read -r body; IFS= read -r meta; printf '%s %s\n' "GET $1 ->$meta" ""; printf '%s\n' "$body" | jq "$SHAPE" 2>/dev/null || echo "(non-JSON body)"; }
}

pget() { # pget <path> : page GET -> status/time/bytes + shell markers (no body)
  out="$(curl -sS -m 25 -D - -o /tmp/web-shapes-page.html -w 'HTTP %{http_code} %{time_total}s %{size_download}B' \
    -H "User-Agent: $UA" -H 'Accept: text/html' \
    -H "Cookie: $COOKIE_HEADER" "$ORIGIN$1")"
  printf 'GET %s -> %s\n' "$1" "$(printf '%s' "$out" | tail -1)"
  grep -c -o 'self.__next_f' /tmp/web-shapes-page.html | xargs printf '  flight-markers: %s\n'
  grep -c -oE 'src="(/_next/[^"]+\.js)"' /tmp/web-shapes-page.html | xargs printf '  chunk-refs: %s\n'
  rm -f /tmp/web-shapes-page.html
}

cmd="${1:-shapes}"
case "$cmd" in
  pages)   pget /chat; pget /account ;;
  session) jget /api/auth/session; jget /api/auth/providers ;;
  threads)
    jget /api/chat/threads
    tid="$(curl -sS -m 25 -H "User-Agent: $UA" -H 'Accept: application/json' \
      -H "Cookie: $COOKIE_HEADER" "$ORIGIN/api/chat/threads" | jq -r '.threads[0].id // empty')"
    [ -n "$tid" ] && jget "/api/chat/threads/$tid"
    ;;
  usage)   jget /api/web/usage-summary ;;
  account)
    for p in /api/web/subscriptions /api/web/freebuff-session /api/web/convex-token \
             /api/account/providers /api/account/country /api/account/paid-api/access \
             /api/web/freebucks/auto-topup /api/changelog /api/github/stars; do
      jget "$p"
    done ;;
  shapes)
    "$0" session; "$0" threads; "$0" usage; "$0" account ;;
  *) echo "usage: $0 {pages|session|threads|usage|account|shapes}" >&2; exit 2 ;;
esac
