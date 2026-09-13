#!/usr/bin/env bash
# repin-all.sh — chain the upstream re-pin drift-update flow into one entry point.
#
# Usage:
#   scripts/repin-all.sh [--dry-run] <vendor-sha> [clone-dir]
#
#   vendor-sha  full 40-char upstream commit SHA in upstream/freebuff
#   clone-dir   local reference clone (default: $FREEBUFF_REFERENCE_DIR,
#               else <repo>/upstream/freebuff)
#   --dry-run   classify drift and print the planned refresh plus the exact
#               gh commands for the three PRs; write nothing
#
# Steps (in order, per the proven playbook):
#   1. Classify wire drift with review-wire-drift.sh BEFORE touching baselines.
#      FUNCTIONAL or UNKNOWN-BASELINE rows abort the refresh: port the Go
#      side first, then re-run. NOTICE rows (notice-string-only drift) flow
#      through the same refresh: the snapshots re-pin plus wiregen regen
#      carries the new copy with no port.
#   2. Refresh the 13 wire snapshots via git show plus LF normalize and stamp
#      snapshots.json upstream_sha and vendor_version.
#   3. Update wirefacts_test testUpstream and the wirefacts.go go:generate line.
#   4. Run go run ./backend/cmd/wiregen -upstream <sha>.
#   5. Verify with check-upstream.sh <sha> plus hermetic wirefacts and
#      TestFallbackParityWithPinnedUpstream.
#   6. Print the exact gh commands for the wire, registry, dashboard PRs in
#      serial order (update-branch with no merge flag, squash merge).
#
# Only the refresh plus stamp plus wiregen chain is scripted here. Registry
# sync stays in sync-upstream.sh and the dashboard embed stays in
# check-upstream.sh with DRIFT_REPORT set. This script calls those; it does
# not reimplement them.
#
# Non-interactive bot path: .github/workflows/upstream-drift.yml runs steps
# 2-5 of this chain for the notice-copy re-pin job (classification already
# done in the drift job: exact_functional == 0 with notice_only_files > 0).
# The steps are invoked here, not duplicated in the workflow, so the manual
# flow and the bot flow share one implementation.
#
# Windows: run under Git Bash, e.g.
#   "C:\Program Files\Git\bin\bash.exe" scripts/repin-all.sh --dry-run <sha>

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DRY_RUN=0
if [[ "${1:-}" == "--dry-run" ]]; then
  DRY_RUN=1
  shift
fi
VENDOR_SHA="${1:-}"
CLONE_DIR="${2:-${FREEBUFF_REFERENCE_DIR:-$REPO_ROOT/upstream/freebuff}}"
WIRE_DIR="$REPO_ROOT/backend/internal/wirefacts/testdata/wire"
SNAPSHOTS="$WIRE_DIR/snapshots.json"

if ! [[ "$VENDOR_SHA" =~ ^[0-9a-fA-F]{40}$ ]]; then
  echo "repin-all: vendor-sha must be a full 40-char SHA, got: $VENDOR_SHA" >&2
  exit 2
fi
if ! git -C "$CLONE_DIR" rev-parse --verify "$VENDOR_SHA" >/dev/null 2>&1; then
  echo "repin-all: $CLONE_DIR has no $VENDOR_SHA (fetch the reference clone first)" >&2
  exit 2
fi
VENDOR_SHA="$(git -C "$CLONE_DIR" rev-parse "$VENDOR_SHA")"
SHORT="${VENDOR_SHA:0:7}"
[[ -f "$SNAPSHOTS" ]] || { echo "repin-all: missing $SNAPSHOTS" >&2; exit 2; }

echo "==> 1. Classifying wire drift at $VENDOR_SHA (before touching baselines)"
CLASSIFY_OUT="$(FREEBUFF_REFERENCE_DIR="$CLONE_DIR" FREEBUFF_REVIEW_END_REF="$VENDOR_SHA" bash "$REPO_ROOT/scripts/review-wire-drift.sh" 2>&1)" || CLASSIFY_RC=$?
CLASSIFY_RC="${CLASSIFY_RC:-0}"
echo "$CLASSIFY_OUT"
SAME_COUNT="$(echo "$CLASSIFY_OUT" | grep -c '^SAME ' || true)"
FUNC_COUNT="$(echo "$CLASSIFY_OUT" | grep -c '^FUNCTIONAL ' || true)"
COMMENT_COUNT="$(echo "$CLASSIFY_OUT" | grep -c '^COMMENT-ONLY ' || true)"
NOTICE_COUNT="$(echo "$CLASSIFY_OUT" | grep -c '^NOTICE ' || true)"
UNKNOWN_COUNT="$(echo "$CLASSIFY_OUT" | grep -c '^UNKNOWN-BASELINE ' || true)"
echo "classify: $SAME_COUNT SAME, $FUNC_COUNT FUNCTIONAL, $COMMENT_COUNT COMMENT-ONLY, $NOTICE_COUNT NOTICE, $UNKNOWN_COUNT UNKNOWN"
if [[ "$UNKNOWN_COUNT" != "0" ]]; then
  echo "repin-all: UNKNOWN-BASELINE rows need a deeper reference fetch; aborting refresh" >&2
  exit 1
fi
if [[ "$FUNC_COUNT" != "0" ]]; then
  echo "repin-all: FUNCTIONAL drift needs a Go-side port first; refresh aborted so the signal stays visible" >&2
  exit 1
fi

# File list comes from the manifest so the 13 paths stay in one place.
mapfile -t WIRE_FILES < <(python3 -c "import json; print('\n'.join(f['path'] for f in json.load(open(r'$SNAPSHOTS'))['files']))" | tr -d '\r')

if ((DRY_RUN)); then
  echo "==> dry-run: would refresh ${#WIRE_FILES[@]} wire snapshots via git show plus LF normalize"
  for p in "${WIRE_FILES[@]}"; do echo "  refresh $p"; done
  echo "dry-run: would stamp snapshots.json upstream_sha to $VENDOR_SHA and vendor_version to npm freebuff (or keep pinned when npm is absent)"
  echo "dry-run: would update backend/internal/wirefacts/wirefacts_test.go testUpstream and backend/internal/wirefacts/wirefacts.go go:generate"
  echo "dry-run: would run go run ./backend/cmd/wiregen -upstream $VENDOR_SHA"
  echo "dry-run: would verify with check-upstream.sh $VENDOR_SHA plus hermetic wirefacts and TestFallbackParityWithPinnedUpstream"
  echo ""
  echo "Manual remainder after this script: review the wiregen diff, run the three PR commands below serially."
else
  echo "==> 2. Refreshing ${#WIRE_FILES[@]} wire snapshots from $SHORT"
  for p in "${WIRE_FILES[@]}"; do
    dest="$WIRE_DIR/$p"
    mkdir -p "$(dirname "$dest")"
    git -C "$CLONE_DIR" show "$VENDOR_SHA:$p" | tr -d '\r' >"$dest"
  done
  echo "==> 3. Stamping snapshots.json plus wirefacts pins"
  PINNED_VERSION="$(tr -d '\r\n' <"$REPO_ROOT/scripts/vendor-version.txt")"
  NPM_VERSION=""
  if command -v npm >/dev/null 2>&1; then
    NPM_VERSION="$(npm view freebuff version 2>/dev/null || true)"
  fi
  VENDOR_VERSION="${NPM_VERSION:-$PINNED_VERSION}"
  python3 - "$SNAPSHOTS" "$VENDOR_SHA" "$VENDOR_VERSION" "$WIRE_DIR" <<'PY'
import hashlib, json, pathlib, sys
snap_path, sha, version, wiredir = sys.argv[1], sys.argv[2], sys.argv[3], pathlib.Path(sys.argv[4])
d = json.loads(pathlib.Path(snap_path).read_text())
d["upstream_sha"] = sha
d["vendor_version"] = version
for f in d["files"]:
    raw = (wiredir / f["path"]).read_bytes()
    f["sha256"] = hashlib.sha256(raw).hexdigest()
pathlib.Path(snap_path).write_text(json.dumps(d, indent=2) + "\n", newline="\n")
print(f"stamped {snap_path} upstream_sha={sha[:12]} vendor_version={version}")
PY
  python3 - "$VENDOR_SHA" <<'PY'
import pathlib, sys
sha = sys.argv[1]
for rel, old in [
    ("backend/internal/wirefacts/wirefacts_test.go", None),
    ("backend/internal/wirefacts/wirefacts.go", None),
]:
    p = pathlib.Path(rel)
    text = p.read_text()
    import re
    text, n = re.subn(r"-upstream [0-9a-f]{40}", f"-upstream {sha}", text)
    text, m = re.subn(r'testUpstream = "[0-9a-f]{40}"', f'testUpstream = "{sha}"', text)
    p.write_text(text, newline="\n")
    print(f"stamped {rel} ({n + m} pins)")
PY
  echo "==> 3b. Refreshing pinned notice copy from the new snapshots"
  python3 - "$WIRE_DIR" "$REPO_ROOT" <<'PY'
import pathlib, re, sys
wiredir, root = pathlib.Path(sys.argv[1]), pathlib.Path(sys.argv[2])
# Same `export const NAME` plus single-quoted string rule as wireExtractConst
# in backend/internal/wirefacts/emit_wire.go: first quote after the export
# key opens the literal, escapes collapse, newline before close fails.
entries = [
    ("FREEBUFF_TIER_CHANGE_NOTICE", "common/src/util/freebuff-model-availability.ts"),
    ("FREEBUFF_CAPACITY_NOTICE", "common/src/constants/freebuff-spend-ceilings.ts"),
    ("FREEBUFF_RESTRICTED_NOTICE", "common/src/constants/freebuff-spend-ceilings.ts"),
    ("FREEBUFF_BUDGET_NOTICE", "common/src/constants/freebuff-spend-ceilings.ts"),
    ("FREEBUFF_FREEBUCKS_CEILING_NOTICE", "common/src/constants/freebuff-spend-ceilings.ts"),
]
def value(src, name):
    i = src.find("export const " + name)
    assert i >= 0, name
    q = src.find("'", i)
    assert q >= 0, name
    j, esc, val = q + 1, False, []
    while True:
        c = src[j]
        if esc:
            val.append(c)
            esc = False
        elif c == "\\":
            esc = True
        elif c == "'":
            break
        else:
            assert c != "\n", name
            val.append(c)
        j += 1
    return "".join(val)
def goq(s):
    # Go %q rendering for notice copy: printable Unicode stays literal,
    # only backslash, double-quote, and control chars escape.
    out = ['"']
    for c in s:
        if c == "\\":
            out.append("\\\\")
        elif c == '"':
            out.append('\\"')
        elif c == "\n":
            out.append("\\n")
        elif ord(c) < 0x20 or ord(c) == 0x7F:
            out.append("\\u%04x" % ord(c))
        else:
            out.append(c)
    out.append('"')
    return "".join(out)
texts = {}
for name, rel in entries:
    texts[name] = value((wiredir / rel).read_text(encoding="utf-8"), name)
# wireNotices table in emit_wire.go: replace the verbatim value literal of
# each export (the Go string on the same entry, matched by export name).
emit = root / "backend/internal/wirefacts/emit_wire.go"
text = emit.read_text(encoding="utf-8")
n = 0
for name, val in texts.items():
    pat = re.compile(r'(\{"\w+", "%s", wire\w+,\n\t\t)' % re.escape(name) + r'"(?:[^"\\]|\\.)*"', re.M)
    text, k = pat.subn(lambda m: m.group(1) + goq(val), text, count=1)
    assert k == 1, name
    n += k
emit.write_text(text, newline="\n")
print(f"refreshed {emit} ({n} notice values)")
# TestNoticeConstants want map in notices_test.go carries the four
# spend-ceilings notices verbatim (TierChangeNotice is only checked
# non-empty there; its value is pinned by wiregen regen plus
# TestEmitWireUpToDate). Refresh only entries present in that map.
by_go = {
    "CapacityNotice": texts["FREEBUFF_CAPACITY_NOTICE"],
    "RestrictedNotice": texts["FREEBUFF_RESTRICTED_NOTICE"],
    "BudgetNotice": texts["FREEBUFF_BUDGET_NOTICE"],
    "FreebucksCeilingNotice": texts["FREEBUFF_FREEBUCKS_CEILING_NOTICE"],
}
t = root / "backend/internal/upstream/notices_test.go"
text = t.read_text(encoding="utf-8")
m = 0
for goname, val in by_go.items():
    pat = re.compile(r'("%s":\s+)' % re.escape(goname) + r'"(?:[^"\\]|\\.)*"')
    text, k = pat.subn(lambda mm: mm.group(1) + goq(val), text, count=1)
    assert k == 1, goname
    m += k
t.write_text(text, newline="\n")
print(f"refreshed {t} ({m} notice values)")
PY
  echo "==> 4. Running wiregen"
  (cd "$REPO_ROOT" && go run ./backend/cmd/wiregen -upstream "$VENDOR_SHA")
  echo "==> 5. Verifying pins plus hermetic tests"
  # The re-pinned content is reviewed by definition (classification ran BEFORE
  # the refresh and FUNCTIONAL rows abort above), so record it as the new
  # wire baseline before the pinned verification; otherwise check-upstream.sh
  # <sha> reports DRIFT against the pre-re-pin anchors.
  FREEBUFF_REFERENCE_DIR="$CLONE_DIR" bash "$REPO_ROOT/scripts/check-upstream.sh" --update-wire-baseline "$VENDOR_SHA" "$CLONE_DIR" >/dev/null
  FREEBUFF_REFERENCE_DIR="$CLONE_DIR" bash "$REPO_ROOT/scripts/check-upstream.sh" "$VENDOR_SHA" "$CLONE_DIR"
  (cd "$REPO_ROOT" && env -u AUTH_TOKENS -u ADMIN_TOKEN go test ./backend/internal/wirefacts/...)
  (cd "$REPO_ROOT" && env -u AUTH_TOKENS -u ADMIN_TOKEN go test ./backend/internal/registry/ -run TestFallbackParityWithPinnedUpstream -v)
fi

echo ""
echo "==> 6. Serial PR commands (wire, registry, dashboard order; one green merge before the next opens)"
cat <<EOF
# Wire layer (this refresh):
git checkout -b chore/wire-repin-$SHORT origin/main
# commit the wiregen output, push, then:
gh pr create --title "chore(wire): re-pin snapshots to vendor $SHORT" --body "Wire files classified BEFORE baseline refresh: <SAME/FUNCTIONAL counts>. Snapshots re-pinned to $VENDOR_SHA, wiregen regen, check-upstream OK, TestFallbackParityWithPinnedUpstream PASS."
gh pr checks --watch
gh pr merge --squash --delete-branch

# Registry layer (run after the wire merge lands):
git fetch origin
git checkout -b chore/registry-repin-$SHORT origin/main
bash scripts/sync-upstream.sh $VENDOR_SHA
# if the sync changed backend/internal/registry/testdata/upstream, commit and:
gh pr create --title "chore(registry): sync pinned upstream models to vendor $SHORT" --body "Registry sync to $VENDOR_SHA."
gh pr checks --watch
gh pr merge --squash --delete-branch
# if the sync reports no drift, skip the registry PR and note it in the final report.

# Dashboard layer (rebase onto latest origin/main so the embed lands on top, e.g. after PR 436):
git fetch origin
git checkout -b chore/dashboard-embed-$SHORT origin/main
DRIFT_REPORT=backend/internal/dashboard/data/upstream_drift.json bash scripts/check-upstream.sh main
gh pr create --title "chore(dashboard): refresh upstream-drift embed to vendor $SHORT" --body "Dashboard embed refresh to $VENDOR_SHA."
gh pr checks --watch
gh pr merge --squash --delete-branch
EOF
