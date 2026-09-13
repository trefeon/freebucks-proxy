#!/usr/bin/env bash
# Classify upstream wire-file drift so a needs-port issue says what actually
# changed: COMMENT-ONLY drift needs a baseline refresh, NOTICE drift needs a
# snapshots re-pin plus wiregen regen, FUNCTIONAL drift needs a Go-side port.
# Pairs with check-upstream.sh, which reports DRIFT but not what kind.
#
# Usage:
#   scripts/review-wire-drift.sh [baseline.tsv] [end_ref]
#
# Environment:
#   FREEBUFF_REFERENCE_DIR  upstream clone (default upstream/freebuff);
#                           must have the end_ref fetched
#   FREEBUFF_REVIEW_END_REF explicit ref/SHA to classify against (default
#                           origin/main); a positional end_ref wins
#
# Per wire file in the baseline TSV (hash<TAB>path):
#   SAME              current origin/main content hashes to the baseline
#   COMMENT-ONLY      diff from the baseline-state commit strips to nothing
#                     (comments/docs only) — refresh the baseline, no port
#   NOTICE            the anchor..end drift is confined to the five
#                     FREEBUFF_*_NOTICE string values consumed by the
#                     wireNotices table in backend/internal/wirefacts/
#                     emit_wire.go — refresh snapshots plus wiregen regen,
#                     no port (needs python3; without it the row stays
#                     FUNCTIONAL, fail-closed)
#   FUNCTIONAL        the stripped diff is non-empty and touches more than
#                     notice copy — needs a port; the stripped diff is
#                     printed below the line
#   UNKNOWN-BASELINE  no commit in the recent history of the file hashes to
#                     the baseline (baseline predates the fetched history —
#                     deepen the clone before classifying)
#
# Exit codes: 0 = nothing FUNCTIONAL/UNKNOWN, 1 = at least one, 2 = setup error.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BASELINE_FILE="${1:-$REPO_ROOT/scripts/wire-baseline.tsv}"
CLONE_DIR="${FREEBUFF_REFERENCE_DIR:-$REPO_ROOT/upstream/freebuff}"

[[ -f "$BASELINE_FILE" ]] || { echo "baseline file not found: $BASELINE_FILE" >&2; exit 2; }
if [[ -n "${2:-}" ]]; then
	END_REF="$2"
elif [[ -n "${FREEBUFF_REVIEW_END_REF:-}" ]]; then
	END_REF="$FREEBUFF_REVIEW_END_REF"
else
	END_REF="origin/main"
fi
if ! git -C "$CLONE_DIR" rev-parse --verify "$END_REF" >/dev/null 2>&1; then
	echo "$CLONE_DIR has no $END_REF — fetch the upstream clone first" >&2; exit 2
fi
END_REF="$(git -C "$CLONE_DIR" rev-parse "$END_REF")"
# The five notice exports consumed by the wireNotices table in
# backend/internal/wirefacts/emit_wire.go. A reword touches only these
# string values; wiregen fails explicitly on it, and the re-pin chain
# (scripts/repin-all.sh, notice bot job) refreshes the snapshots plus
# regens notices_gen.go with no Go-side port.
NOTICE_EXPORTS="FREEBUFF_TIER_CHANGE_NOTICE FREEBUFF_CAPACITY_NOTICE FREEBUFF_RESTRICTED_NOTICE FREEBUFF_BUDGET_NOTICE FREEBUFF_FREEBUCKS_CEILING_NOTICE"

TMPD="$(TMPDIR=/tmp mktemp -d)"
trap 'rm -rf "$TMPD"' EXIT

# notice_only <path> <anchor> <end>: exit 0 when the anchor..end drift is
# confined to the notice string values above. Both blobs are normalized by
# replacing each notice literal (parsed with the same `export const NAME`
# plus single-quoted string rule as wireExtractConst in emit_wire.go) with
# a placeholder; identical normalized blobs mean only copy changed. An
# export present on exactly one side, or any other byte difference, is not
# notice-only. At least one notice value must differ (callers only reach
# here on drifted files). No python3: exit 1, fail-closed to FUNCTIONAL.
notice_only() {
	command -v python3 >/dev/null 2>&1 || return 1
	old_blob="$(git -C "$CLONE_DIR" show "$2:$1" | tr -d '\r')"
	new_blob="$(git -C "$CLONE_DIR" show "$3:$1" | tr -d '\r')"
	NOTICE_OLD="$old_blob" NOTICE_NEW="$new_blob" \
		NOTICE_EXPORTS="$NOTICE_EXPORTS" python3 - <<'PY'
import os
exports = os.environ["NOTICE_EXPORTS"].split()
old, new = os.environ["NOTICE_OLD"], os.environ["NOTICE_NEW"]
def spans(src):
    out = {}
    for name in exports:
        key = "export const " + name
        i = src.find(key)
        if i < 0:
            continue
        q = src.find("'", i + len(key))
        if q < 0:
            return None
        j, esc, val = q + 1, False, []
        while j < len(src):
            c = src[j]
            if esc:
                val.append(c)
                esc = False
            elif c == "\\":
                esc = True
            elif c == "'":
                break
            elif c == "\n":
                return None
            else:
                val.append(c)
            j += 1
        else:
            return None
        if j >= len(src) or src[j] != "'":
            return None
        out[name] = (q, j + 1, "".join(val))
    return out
so, sn = spans(old), spans(new)
if so is None or sn is None:
    raise SystemExit(1)
if set(so) != set(sn):
    raise SystemExit(1)
if not any(so[n][2] != sn[n][2] for n in so):
    raise SystemExit(1)
def norm(src, s):
    parts, prev = [], 0
    for name in exports:
        if name not in s:
            continue
        q, e, _ = s[name]
        parts.append(src[prev:q])
        parts.append("'\x00NOTICE\x00'")
        prev = e
    parts.append(src[prev:])
    return "".join(parts)
raise SystemExit(0 if norm(old, so) == norm(new, sn) else 1)
PY
}

rc=0
while read -r baseline path; do
	[[ -z "$baseline" || "$path" == "" ]] && continue
	case "$baseline" in '#'*) continue ;; esac

	current="$(git -C "$CLONE_DIR" show "$END_REF:$path" | tr -d '\r' | sha256sum | cut -c1-12)"
	if [[ "$current" == "$baseline" ]]; then
		echo "SAME $path"
		continue
	fi

	anchor=""
	for c in $(git -C "$CLONE_DIR" log --format=%h -30 "$END_REF" -- "$path"); do
		h="$(git -C "$CLONE_DIR" show "$c:$path" | tr -d '\r' | sha256sum | cut -c1-12)"
		if [[ "$h" == "$baseline" ]]; then anchor="$c"; break; fi
	done
	if [[ -z "$anchor" ]]; then
		echo "UNKNOWN-BASELINE $path (baseline $baseline matches no commit in the last 30 touching the file)"
		rc=1
		continue
	fi

	stripped="$(git -C "$CLONE_DIR" diff "$anchor..$END_REF" -- "$path" \
		| grep -E '^[+-]' | grep -vE '^(\+\+\+|---)' \
		| grep -vE '^[+-][[:space:]]*(/\*|\*|\*/|//|$)' || true)"
	if [[ -z "$stripped" ]]; then
		echo "COMMENT-ONLY $path (baseline anchor $anchor) — refresh baseline, no port"
	elif notice_only "$path" "$anchor" "$END_REF"; then
		echo "NOTICE $path (baseline anchor $anchor) — notice copy only, re-pin snapshots plus wiregen regen, no port"
	else
		echo "FUNCTIONAL $path (baseline anchor $anchor):"
		echo "$stripped"
		rc=1
	fi
done <"$BASELINE_FILE"
exit $rc
