#!/usr/bin/env bash
# watch-freebuff-release.sh -- reference poller that wakes the upstream-drift
# workflow exactly when the freebuff CLI wrapper releases.
#
# Polls `npm view freebuff version`, compares against the pinned version
# (scripts/vendor-version.txt by default, or --pinned <version>), and on a
# mismatch fires `repository_dispatch` type `freebuff-cli-release` with the
# new version as client_payload.version via `gh api`. The workflow's
# version_gate validates the payload (malformed -> unknown, fail-open) and
# still requires it to differ from pinned: the payload is never trusted
# blindly, so a spurious fire is harmless (the run stops at the gate).
#
# INTENDED HOME: a cron entry on the infra box (NOT GitHub CI -- CI already
# polls on its own 12h schedule; this exists to cut the up-to-12h lag):
#   */15 * * * * /path/to/freebuff-proxy/scripts/watch-freebuff-release.sh >>/var/log/watch-freebuff-release.log 2>&1
# Requires: npm, gh (authenticated with `repo` scope; GH_TOKEN works), jq.
# Never fires when live is unknown or equals pinned -- the scheduled
# workflow already covers those cases fail-open.
#
# Usage: watch-freebuff-release.sh [--check] [--repo OWNER/REPO]
#          [--pinned VERSION] [--event TYPE]
#   --check   poll + compare only, never fire (safe for testing).
#   --pinned  compare against VERSION instead of scripts/vendor-version.txt.
#   --repo    target repository (default: derived via `gh repo view`).
#   --event   dispatch type (default: freebuff-cli-release).
set -euo pipefail

CHECK=0
REPO=""
PINNED=""
EVENT="freebuff-cli-release"

while [ $# -gt 0 ]; do
  case "$1" in
    --check) CHECK=1; shift ;;
    --repo) REPO="${2:?--repo needs a value}"; shift 2 ;;
    --pinned) PINNED="${2:?--pinned needs a value}"; shift 2 ;;
    --event) EVENT="${2:?--event needs a value}"; shift 2 ;;
    -h|--help) sed -n '1,32p' "$0"; exit 0 ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

need() {
  command -v "$1" >/dev/null 2>&1 || { echo "missing required command: $1" >&2; exit 2; }
}
need npm
need jq

if [ -z "$PINNED" ]; then
  PINNED="$(tr -d '\r\n \t' <scripts/vendor-version.txt || true)"
fi
if [ -z "$PINNED" ]; then
  echo "pinned version unknown and no --pinned given" >&2
  exit 2
fi

LIVE="$(npm view freebuff version 2>/dev/null || true)"
LIVE="$(printf '%s' "$LIVE" | tr -d '\r\n \t' || true)"

if [ -z "$LIVE" ]; then
  echo "live version unknown (npm lookup failed); not firing -- the scheduled workflow covers this fail-open."
  exit 0
fi
if [ "$LIVE" = "$PINNED" ]; then
  echo "live $LIVE == pinned $PINNED; not firing."
  exit 0
fi
echo "release detected: pinned $PINNED -> live $LIVE."
if [ "$CHECK" = "1" ]; then
  echo "--check: not firing."
  exit 0
fi
need gh
if [ -z "$REPO" ]; then
  REPO="$(gh repo view --json nameWithOwner -q .nameWithOwner)"
fi
echo "firing $EVENT on $REPO."
jq -n --arg et "$EVENT" --arg v "$LIVE" '{event_type: $et, client_payload: {version: $v}}' \
  | gh api "repos/$REPO/dispatches" --method POST --input - >/dev/null
echo "dispatched."
