#!/usr/bin/env bash
# drift-exact.sh — exact export-level drift between two upstream refs.
#
# Usage:
#   scripts/drift-exact.sh [old_ref] [new_ref] [clone-dir]
#
#   old_ref   upstream commit the pins currently record
#             (default: upstream_sha from backend/internal/wirefacts/testdata/wire/snapshots.json)
#   new_ref   upstream ref to compare against (default: origin/main)
#   clone-dir local clone of https://github.com/CodebuffAI/freebuff
#             (default: $FREEBUFF_REFERENCE_DIR, else <repo>/upstream/freebuff)
#
# What "exact" means: each watched file is split into export blocks (one
# `export ...` statement plus its attached doc comment) at both refs.
# Added/removed/changed export names are reported; changed blocks are
# strip-tested (comments and blanks removed) into COMMENT_ONLY vs
# FUNCTIONAL, except a block named for one of the five FREEBUFF_*_NOTICE
# exports whose diff is confined to its string value: that is NOTICE_ONLY
# (re-pin plus regen, never a port). FUNC hunks printed stay functional
# only. Registry model files get MODEL vs PRICE labels from the export
# name, so the bot announces "price change only" instead of "models drifted".
# Classification compares the unified diff of the stripped blocks: any
# remaining +/- line is functional. (The old bare-diff + '^[+-]' grep could
# never match, so it mislabeled FUNCTIONAL rows as COMMENT_ONLY.)
# Row lists are materialized to temp files (no `< <(…)` procsub) and every
# line read is CR-stripped: byte-clean under text-mode Windows shells and
# CRLF checkouts alike.
#
# File status is one of SAME, COMMENT_ONLY, NOTICE_ONLY, FUNCTIONAL.
# NOTICE_ONLY files carry changed_notice (the reworded exports) and need
# the notice re-pin chain, not a Go-side port. True functional shape drift
# anywhere keeps FUNCTIONAL: the notice path never fires.
#
# New-file discovery: upstream files that are added, renamed, deleted, or
# modified under the watched source trees (cli/src, common/src, packages,
# sdk) but outside the fixed REGISTRY_FILES/WIRE_FILES lists are NOT folded
# into the per-file report — they are emitted as a separate top-level
# `untracked[]` array ({status, path}) plus `summary.untracked_files` and
# `UNTRACKED ...` announce lines, so a rename (e.g. freebuff-trust.ts ->
# freebuff-standing.ts) or a brand-new constants file is never silently
# ignored. A git rename (R status) surfaces as a D row for the old path plus
# an A row for the new path. All pre-existing fields keep their names;
# `untracked`/`untracked_files` are purely additive. Version never affects
# the exit code (gate lives in workflow `if:` conditions only).

set -euo pipefail
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd | sed 's|\\|/|g')"
VENDOR_URL="https://github.com/CodebuffAI/freebuff.git"
SNAPSHOTS="$REPO_ROOT/backend/internal/wirefacts/testdata/wire/snapshots.json"
EXACT_REPORT="${EXACT_REPORT:-$REPO_ROOT/.exact-drift.json}"
HUNK_CAP=150

die() { printf 'drift-exact: error: %s\n' "$1" >&2; exit 2; }
command -v git >/dev/null 2>&1 || die "git not found on PATH"
command -v jq >/dev/null 2>&1 || die "jq not found on PATH"
command -v awk >/dev/null 2>&1 || die "awk not found on PATH"

# ---- clone ----
if [[ -n "${3:-}" ]]; then CLONE_DIR="$3"
elif [[ -n "${FREEBUFF_REFERENCE_DIR:-}" ]]; then CLONE_DIR="$FREEBUFF_REFERENCE_DIR"
elif [[ -d "$REPO_ROOT/upstream/freebuff/.git" ]]; then CLONE_DIR="$REPO_ROOT/upstream/freebuff"
else CLONE_DIR="$REPO_ROOT/../freebuff-reference"; fi

# Resolve relative to the repo so callers work from any cwd.
if [[ ! "$CLONE_DIR" =~ ^(/|[A-Za-z]:/) ]]; then CLONE_DIR="$REPO_ROOT/$CLONE_DIR"; fi
if ! git -C "$CLONE_DIR" rev-parse --git-dir >/dev/null 2>&1; then
	echo "drift-exact: cloning $VENDOR_URL (--depth 500)..." >&2
	git clone --depth 500 -- "$VENDOR_URL" "$CLONE_DIR" || die "clone failed"
fi

# ---- refs ----
OLD_REF="${1:-}"
if [[ -z "$OLD_REF" ]]; then
	[[ -f "$SNAPSHOTS" ]] || die "missing $SNAPSHOTS and no old_ref given"
	OLD_REF="$(grep -o '"upstream_sha"[[:space:]]*:[[:space:]]*"[^"]*"' "$SNAPSHOTS" | head -1 | sed 's/.*"\(.*\)"$/\1/')"
	[[ -n "$OLD_REF" ]] || die "cannot read upstream_sha from $SNAPSHOTS"
fi
NEW_REF="${2:-origin/main}"

resolve() {
	local r="$1" sha=""
	if [[ "$r" =~ ^[0-9a-fA-F]{40}$ ]]; then
		if ! git -C "$CLONE_DIR" cat-file -e "${r}^{commit}" 2>/dev/null; then
			# Shallow clones may predate the pin; deepen before fetching it.
			git -C "$CLONE_DIR" fetch --unshallow 2>/dev/null || true
			git -C "$CLONE_DIR" fetch origin "$r" 2>/dev/null || true
		fi
		sha="$(git -C "$CLONE_DIR" rev-parse --verify "${r}^{commit}" 2>/dev/null || true)"
	else
		git -C "$CLONE_DIR" fetch origin -- "$r" 2>/dev/null || true
		sha="$(git -C "$CLONE_DIR" rev-parse --verify "origin/${r}^{commit}" 2>/dev/null || git -C "$CLONE_DIR" rev-parse --verify "${r}^{commit}" 2>/dev/null || true)"
	fi
	[[ -n "$sha" ]] || die "cannot resolve ref '$r' in $CLONE_DIR (fetch it first)"
	printf '%s' "$sha"
}
OLD_SHA="$(resolve "$OLD_REF")"
NEW_SHA="$(resolve "$NEW_REF")"

# ---- watch sets (mirror check-upstream.sh groups) ----
REGISTRY_FILES=(
	free-agents.ts
	freebuff-model-ids.ts
	freebuff-models.ts
	gemini.ts
	model-config.ts
	freebuff-model-entitlements.ts
)
WIRE_FILES=(
	cli/src/components/freebuff-model-selector.tsx
	common/src/constants/foreign-client-signals.ts
	common/src/constants/freebuff-peak-hours.ts
	common/src/constants/freebuff-signup-block.ts
	common/src/constants/freebuff-spend-ceilings.ts
	common/src/constants/freebuff-standing.ts
	common/src/tools/constants.ts
	common/src/types/freebuff-session.ts
	common/src/util/freebuff-model-availability.ts
	packages/agent-runtime/src/constants.ts
	packages/agent-runtime/src/prompt-agent-stream.ts
	packages/agent-runtime/src/run-agent-step.ts
	packages/agent-runtime/src/run-programmatic-step.ts
)

group_of() {
	local p="$1" f
	for f in "${REGISTRY_FILES[@]}"; do
		[[ "$p" == "common/src/constants/$f" ]] && { printf 'registry'; return; }
	done
	for f in "${WIRE_FILES[@]}"; do
		[[ "$p" == "$f" ]] && { printf 'wire'; return; }
	done
	printf 'unwatched'
}

# Source trees whose non-noise churn must surface even when no fixed watch
# list names the file. Covers every WIRE_FILES/REGISTRY_FILES directory plus
# sdk (new surface, no pinned files yet). Anything outside these trees
# (repo docs, evals, infra) stays out of scope and is skipped as before.
WATCH_TREES=(
	cli/src
	common/src
	packages
	sdk
)

is_noise() {
	local p="$1"
	[[ "$p" == "package.json" || "$p" == "bun.lock" ]] && return 0
	[[ "$p" == *.md ]] && return 0
	[[ "$p" == *.test.ts || "$p" == *.test.tsx ]] && return 0
	[[ "$p" == *__tests__* || "$p" == */test/* || "$p" == e2e/* || "$p" == */e2e/* || "$p" == docs/* || "$p" == assets/* ]] && return 0
	return 1
}

# Exit 0 when the upstream path lives under one of WATCH_TREES.
under_watch() {
	local p="$1" t
	for t in "${WATCH_TREES[@]}"; do
		[[ "$p" == "$t"/* ]] && return 0
	done
	return 1
}
# Reads file content on stdin, writes one file per export block into $1 and
# the ordered block names to $1/MANIFEST. A block is the `export ...`
# statement plus its attached leading comments; lines before the first export
# form __header__.
split_blocks() {
	awk -v out="$1" '
	function fname(k) { gsub(/[^A-Za-z0-9_#+.,=-]/, "_", k); return k }
	function flush() {
		if (buf == "" && name == "") return
		if (name == "") name = "__header__"
		key = name
		if (key in seen) { seen[key]++; key = key "#" seen[key] } else seen[key] = 1
		print buf > (out "/" fname(key))
		order[++n] = key
	}
	BEGIN { buf = ""; name = ""; n = 0 }
	/^export / {
		flush()
		buf = $0 "\n"
		line = $0
		sub(/^export[ \t]+(default[ \t]+)?(async[ \t]+)?/, "", line)
		if (line ~ /^\{/) name = "reexport:" line
		else {
			nw = split(line, w, /[^A-Za-z0-9_]+/)
			if (w[1] ~ /^(const|let|var|function|class|interface|type|enum|abstract|declare|async)$/ && nw >= 2) name = w[2]
			else if (nw >= 1 && w[1] != "") name = w[1]
			else name = ("exportline:" NR)
		}
		next
	}
	{ buf = buf $0 "\n" }
	END { flush(); for (i = 1; i <= n; i++) print order[i] }
	' >"$1/MANIFEST"
}

# COMMENT_ONLY when the block diff strips to nothing (same rule as
# review-wire-drift.sh: drop +/- markers, file headers, comment/blank lines).
# The diff MUST be unified (-U3): bare `diff` emits normal format (`<`/`>`
# prefixes) which the `^[+-]` grep below never matches, mislabeling every
# genuine FUNCTIONAL hunk as comment-only. The strip result is captured
# into a variable (never a trailing `grep -q`): under `set -o pipefail` a
# `grep -q` closes the pipe early, the upstream greps die with SIGPIPE
# (141), and pipefail turns that into pipeline failure — again mislabeling
# FUNCTIONAL as comment-only. This mirrors review-wire-drift.sh exactly.
kind_of() {
	local stripped
	stripped="$(diff -U3 --label a --label b "$1" "$2" 2>/dev/null | grep -E '^[+-]' | grep -vE '^(\+\+\+|---)' | grep -vE '^[+-][[:space:]]*(/\*|\*|\*/|//|$)' || true)"
	if [[ -n "$stripped" ]]; then
		printf 'functional'
	else
		printf 'comment'
	fi
}
# strip_notice <old_block> <new_block> <export>: exit 0 when the two block
# files differ only in the single-quoted string value of the named notice
# export (same `export const NAME` plus same-line/next-line string rule as
# wireExtractConst in emit_wire.go). A parse failure on either side exits 1
# (fail-closed: the block stays FUNCTIONAL). Needs python3.
strip_notice() {
	command -v python3 >/dev/null 2>&1 || return 1
	old_blob="$(cat "$1")"
	new_blob="$(cat "$2")"
	NOTICE_OLD="$old_blob" NOTICE_NEW="$new_blob" NOTICE_EXPORT="$3" python3 - <<'PY'
import os, sys
name = os.environ["NOTICE_EXPORT"]
def value(src):
    key = "export const " + name
    i = src.find(key)
    if i < 0:
        return None
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
    norm = src[:q] + "'\x00NOTICE\x00'" + src[j + 1:]
    return ("".join(val), norm)
v = value(os.environ["NOTICE_OLD"])
w = value(os.environ["NOTICE_NEW"])
if v is None or w is None:
    sys.exit(1)
# Same export, different copy, identical surroundings: notice-only.
sys.exit(0 if v[0] != w[0] and v[1] == w[1] else 1)
PY
}

# MODEL vs PRICE vs WIRE label from the export name (heuristic, documented).
label_of() {
	case "$2" in
	*MODEL_ID* | *MODELS* | *MODEL_IDS* | *AGENT* | *ENTITLEMENT* | *REWARD* | *PAUSED* | *SUPPORTED* | *LIMITED*) printf 'MODEL' ;;
	*PRICE* | *CAP* | *SPEND* | *CEILING* | *POOL* | *STIPEND* | *COST*) printf 'PRICE' ;;
	*) [[ "$1" == "wire" ]] && printf 'WIRE' || printf 'OTHER' ;;
	esac
}

TMP="$(TMPDIR=/tmp mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

echo "drift-exact: $OLD_SHA -> $NEW_SHA"
FILES_JSON=()
ANNOUNCE=()
IGNORED=()
UNTRACKED=()
functional_files=0
comment_files=0
notice_files=0
# The notice exports consumed by the wireNotices table in
# backend/internal/wirefacts/emit_wire.go. A block change confined to one
# of these string values is notice copy (re-pin plus regen), never a port.
NOTICE_EXPORTS="FREEBUFF_TIER_CHANGE_NOTICE FREEBUFF_CAPACITY_NOTICE FREEBUFF_RESTRICTED_NOTICE FREEBUFF_BUDGET_NOTICE FREEBUFF_FREEBUCKS_CEILING_NOTICE"

# Describe a raw --name-status letter for the UNTRACKED announce lines.
status_word() {
	case "$1" in
	A) printf 'added' ;;
	D) printf 'removed' ;;
	M) printf 'modified' ;;
	*) printf '%s' "$1" ;;
	esac
}

if [[ "$OLD_SHA" == "$NEW_SHA" ]]; then
	echo "drift-exact: refs identical, nothing to compare"
else
	# Materialize the rename-split name list to a temp file and read that:
	# `< <(…)` process substitution is avoided (unreliable on some Windows
	# shells, and unsupported by plain POSIX sh).
	git -C "$CLONE_DIR" diff --name-status "$OLD_SHA" "$NEW_SHA" -- | awk -F'\t' '{ if ($1 ~ /^R/) { print "D\t" $2; print "A\t" $3 } else { print $1 "\t" $2 } }' >"$TMP/names.txt" || die "diff failed"
	while IFS=$'\t' read -r status path; do
		# Trailing-CR strip: native-Windows stdout (e.g. jq) may emit CRLF
		# when this script runs under a text-mode console; also makes the
		# loop robust to CRLF checkouts. No-op on clean input.
		status="${status%$'\r'}"; path="${path%$'\r'}"
		[[ -z "$path" ]] && continue
		group="$(group_of "$path")"
		if [[ "$group" == "unwatched" ]]; then
			if is_noise "$path"; then
				IGNORED+=("$path")
			elif under_watch "$path"; then
				UNTRACKED+=("$status	$path")
				ANNOUNCE+=("UNTRACKED $path ($(status_word "$status"))")
			fi
			continue
		fi
		added=() removed=() changed_c=() changed_f=() changed_n=() hunks=""
		if [[ "$status" == "A" ]]; then
			added+=("(whole file)")
			fstatus="FUNCTIONAL"
		elif [[ "$status" == "D" ]]; then
			removed+=("(whole file)")
			fstatus="FUNCTIONAL"
		else
			mkdir -p "$TMP/old" "$TMP/new"
			rm -f "$TMP"/old/* "$TMP"/new/*
			git -C "$CLONE_DIR" show "$OLD_SHA:$path" >"$TMP/full_old" 2>/dev/null || die "no $path at $OLD_SHA"
			git -C "$CLONE_DIR" show "$NEW_SHA:$path" >"$TMP/full_new" 2>/dev/null || die "no $path at $NEW_SHA"
			split_blocks "$TMP/old" <"$TMP/full_old"
			split_blocks "$TMP/new" <"$TMP/full_new"
			while IFS= read -r b; do
				b="${b%$'\r'}"
				[[ -z "$b" ]] && continue
				fb="$(printf '%s' "$b" | sed 's/[^A-Za-z0-9_#+.,=-]/_/g')"
				if [[ ! -f "$TMP/new/$fb" ]]; then
					removed+=("$b")
				elif cmp -s "$TMP/old/$fb" "$TMP/new/$fb"; then
					:
				elif [[ " $NOTICE_EXPORTS " == *" $b "* ]] && strip_notice "$TMP/old/$fb" "$TMP/new/$fb" "$b"; then
					# A block named for a notice export whose diff is confined
					# to its string value is copy, not shape: keep it out of
					# changed_f so a pure reword never reads as FUNCTIONAL.
					# Checked BEFORE kind_of (a reworded string line starts
					# with a quote, which kind_of strips as comment-like).
					changed_n+=("$b")
				elif [[ "$(kind_of "$TMP/old/$fb" "$TMP/new/$fb")" == "comment" ]]; then
					changed_c+=("$b")
				else
					changed_f+=("$b")
					h="$(diff -U3 --label "a/$b" --label "b/$b" "$TMP/old/$fb" "$TMP/new/$fb" || true)"
					hunks+="$h"$'\n'
				fi
			done <"$TMP/old/MANIFEST"
			while IFS= read -r b; do
				b="${b%$'\r'}"
				[[ -z "$b" ]] && continue
				fb="$(printf '%s' "$b" | sed 's/[^A-Za-z0-9_#+.,=-]/_/g')"
				[[ -f "$TMP/old/$fb" ]] || added+=("$b")
			done <"$TMP/new/MANIFEST"
			# Reads of the same sanitized filename from both dirs: names are
			# unique per manifest (split_blocks dedupes with #2), so a shared
			# filename means the same block.
			if ((${#added[@]} + ${#removed[@]} + ${#changed_f[@]} == 0)); then
				if ((${#changed_n[@]} > 0)); then fstatus="NOTICE_ONLY"
				elif ((${#changed_c[@]} == 0)); then fstatus="SAME"; else fstatus="COMMENT_ONLY"; fi
			else
				fstatus="FUNCTIONAL"
			fi
		fi
		case "$fstatus" in
		FUNCTIONAL) functional_files=$((functional_files + 1)) ;;
		COMMENT_ONLY) comment_files=$((comment_files + 1)) ;;
		NOTICE_ONLY) notice_files=$((notice_files + 1)) ;;
		esac
		for b in ${added[@]+"${added[@]}"}; do ANNOUNCE+=("$(label_of "$group" "$b") $path: +$b (added)"); done
		for b in ${removed[@]+"${removed[@]}"}; do ANNOUNCE+=("$(label_of "$group" "$b") $path: -$b (removed)"); done
		for b in ${changed_f[@]+"${changed_f[@]}"}; do ANNOUNCE+=("$(label_of "$group" "$b") $path: ~$b"); done
		for b in ${changed_n[@]+"${changed_n[@]}"}; do ANNOUNCE+=("NOTICE $path: ~$b (notice copy)"); done
		if ((${#changed_c[@]} > 8)); then ANNOUNCE+=("DOC $path: ${#changed_c[@]} comment-only blocks"); else for b in ${changed_c[@]+"${changed_c[@]}"}; do ANNOUNCE+=("DOC $path: ~$b (comment-only)"); done; fi
		hunk_lines="$(printf '%s' "$hunks" | wc -l)"
		if ((hunk_lines > HUNK_CAP)); then
			hunks="$(printf '%s' "$hunks" | head -$HUNK_CAP)"$'\n…(hunks truncated at '"$HUNK_CAP"' lines)'
		fi
		FILES_JSON+=("$(jq -n --arg path "$path" --arg group "$group" --arg status "$fstatus" \
			--argjson added "$(printf '%s\n' ${added[@]+"${added[@]}"} | jq -R . | jq -s 'map(select(length > 0))')" \
			--argjson removed "$(printf '%s\n' ${removed[@]+"${removed[@]}"} | jq -R . | jq -s 'map(select(length > 0))')" \
			--argjson changed_functional "$(printf '%s\n' ${changed_f[@]+"${changed_f[@]}"} | jq -R . | jq -s 'map(select(length > 0))')" \
			--argjson changed_comment "$(printf '%s\n' ${changed_c[@]+"${changed_c[@]}"} | jq -R . | jq -s 'map(select(length > 0))')" \
			--argjson changed_notice "$(printf '%s\n' ${changed_n[@]+"${changed_n[@]}"} | jq -R . | jq -s 'map(select(length > 0))')" \
			--arg hunks "$hunks" \
			'{path:$path,group:$group,status:$status,added:$added,removed:$removed,changed_functional:$changed_functional,changed_comment:$changed_comment,changed_notice:$changed_notice,hunks:$hunks}')")
	done <"$TMP/names.txt"
fi

action="false"
((functional_files > 0)) && action="true"
jq -n --arg old "$OLD_SHA" --arg new "$NEW_SHA" \
	--argjson files "$(printf '%s\n' ${FILES_JSON[@]+"${FILES_JSON[@]}"} | jq -s .)" \
	--argjson ignored "$(printf '%s\n' ${IGNORED[@]+"${IGNORED[@]}"} | jq -R . | jq -s 'map(select(length > 0))')" \
	--argjson announce "$(printf '%s\n' ${ANNOUNCE[@]+"${ANNOUNCE[@]}"} | jq -R . | jq -s 'map(select(length > 0))')" \
	--argjson untracked "$(printf '%s\n' ${UNTRACKED[@]+"${UNTRACKED[@]}"} | jq -R 'split("\t") | select(length == 2) | {status: .[0], path: .[1]} | select(.path | length > 0)' | jq -s .)" \
	--argjson functional_files "$functional_files" --argjson comment_files "$comment_files" \
	--argjson notice_files "$notice_files" --argjson action_needed "$action" \
	'{old_sha:$old,new_sha:$new,checked_at:(now|todate),files:$files,ignored_paths:$ignored,untracked:$untracked,announce:$announce,
    summary:{functional_files:$functional_files,comment_only_files:$comment_files,notice_only_files:$notice_files,untracked_files:($untracked|length),action_needed:$action_needed}}' >"$EXACT_REPORT"

echo "report: $EXACT_REPORT"
echo "functional_files=$functional_files comment_only_files=$comment_files notice_only_files=$notice_files untracked_files=$(jq -r '.summary.untracked_files' "$EXACT_REPORT") action_needed=$action"
if ((${#ANNOUNCE[@]})); then printf '  - %s\n' "${ANNOUNCE[@]}"; fi
if [[ "$action" == "true" ]]; then exit 1; else exit 0; fi
