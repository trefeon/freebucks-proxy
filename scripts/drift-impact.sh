#!/usr/bin/env bash
# drift-impact.sh — export->consumer impact map for upstream drift.
#
# Usage:
#   scripts/drift-impact.sh [exact-report] [repo-root]
#
#   exact-report  JSON written by scripts/drift-exact.sh
#                 (default: $REPO_ROOT/.exact-drift.json)
#   repo-root     repo tree to search for consumers
#                 (default: the repo containing this script)
#
# What it does: for every changed/added/removed export in the exact report's
# FUNCTIONAL files (plus every `untracked[]` entry), it resolves which
# repo-side files reference the export and emits `impact[]` entries
# {file, export, change, kind, confidence, consumers[]}:
#   kind        MODEL | PRICE | WIRE | OTHER (same name heuristic as
#               drift-exact.sh label_of) | NOTICE (notice-copy exports) |
#               UNTRACKED (new-file discovery rows)
#   change      added | removed | changed | modified (raw drift status word)
#   consumers[] repo-relative files referencing the export (capped, see
#               CONSUMER_CAP), or ["unresolved"] when nothing references it —
#               `unresolved` is a valid answer, never a failure.
#   confidence  direct     — a whole-word hit in a non-mirror, non-test
#                            source file (.go/.ts/.svelte/.js/.tsx)
#               name-match — hits only under testdata mirrors, tests, or docs
#               unresolved — no hits at all (e.g. a brand-new upstream export
#                            nothing consumes yet)
#
# Heuristic, grep-based mapping — it marks what it can prove and says
# `unresolved` otherwise. Consumer categories it looks through (by path):
# registry pins (backend/internal/registry/**), modelcat consts
# (backend/internal/modelcat/**), convert/wirecodes consumers
# (backend/internal/convert/**, backend/internal/upstream/**), wirefacts
# parsers (backend/internal/wirefacts/**), dashboard static copies
# (frontend/src/**, backend/internal/dashboard/**), plus anything else
# tracked that mentions the export. Search uses `git grep` over tracked
# files only, so node_modules/dist/untracked noise never shows up.
#
# Special exports: `__header__` (import-block changes) and `(whole file)`
# (added/removed files) carry no export name, so the file's basename stem
# (dash/underscore/snake forms plus the CamelCase form) is searched instead
# at name-match confidence at best.
#
# Report: $IMPACT_REPORT (default $REPO_ROOT/.impact-drift.json) shaped
# {old_sha, new_sha, checked_at, announce[], impact[]}; announce[] holds
# one compact line per entry for the wire/port PR body.
# Exit: 0 no functional impact entries, 1 at least one, 2 setup error.
# Version never affects the exit code (gate lives in workflow `if:`
# conditions only).
#
# Windows: run under Git Bash like the other drift scripts.

set -euo pipefail

REPO_ROOT_DEFAULT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd | sed 's|\\|/|g')"
REPO_ROOT="${2:-$REPO_ROOT_DEFAULT}"
EXACT_IN="${1:-$REPO_ROOT/.exact-drift.json}"
IMPACT_REPORT="${IMPACT_REPORT:-$REPO_ROOT/.impact-drift.json}"
CONSUMER_CAP=12

die() { printf 'drift-impact: error: %s\n' "$1" >&2; exit 2; }
command -v git >/dev/null 2>&1 || die "git not found on PATH"
command -v jq >/dev/null 2>&1 || die "jq not found on PATH"
[[ -f "$EXACT_IN" ]] || die "exact report not found: $EXACT_IN (run scripts/drift-exact.sh first)"
git -C "$REPO_ROOT" rev-parse --git-dir >/dev/null 2>&1 || die "not a git repo: $REPO_ROOT"

# kind_of_name <group> <export>: MODEL/PRICE/WIRE/OTHER, same heuristic as
# drift-exact.sh label_of (duplicated, never imported, so the two scripts
# stay runnable standalone).
kind_of_name() {
	case "$2" in
	*MODEL_ID* | *MODELS* | *MODEL_IDS* | *AGENT* | *ENTITLEMENT* | *REWARD* | *PAUSED* | *SUPPORTED* | *LIMITED*) printf 'MODEL' ;;
	*PRICE* | *CAP* | *SPEND* | *CEILING* | *POOL* | *STIPEND* | *COST*) printf 'PRICE' ;;
	*) [[ "$1" == "wire" ]] && printf 'WIRE' || printf 'OTHER' ;;
	esac
}

# stem_terms <upstream-path>: search terms for file-level rows (__header__,
# whole-file, untracked): basename stem in its raw, lower-smashed, and
# CamelCase forms, one per line.
stem_terms() {
	local base stem smashed camel
	base="$(basename "$1")"
	stem="${base%.*}"
	printf '%s\n' "$stem"
	smashed="$(printf '%s' "$stem" | tr -d -- '-_')"
	[[ "$smashed" != "$stem" ]] && printf '%s\n' "$smashed"
	camel="$(printf '%s' "$stem" | awk -F'[-_]' '{ s=""; for (i=1;i<=NF;i++) s=s toupper(substr($i,1,1)) substr($i,2); print s }')"
	[[ "$camel" != "$stem" && "$camel" != "$smashed" ]] && printf '%s\n' "$camel"
}

# consumers_of <term> [extra-term...]: repo-relative tracked files with a
# whole-word hit for any term, excluding the drift scripts/reports
# themselves (they echo every export name by construction).
consumers_of() {
	local t
	for t in "$@"; do
		[[ -n "$t" ]] || continue
		git -C "$REPO_ROOT" grep -lw -F -- "$t" -- \
			':!scripts/drift-exact.sh' ':!scripts/drift-impact.sh' ':!scripts/review-wire-drift.sh' ':!scripts/drift-tui.sh' \
			':!*.json' 2>/dev/null || true
	done | sort -u
}

# is_source <path>: a non-mirror, non-test source file — hits here mean
# direct confidence; hits only elsewhere mean name-match.
is_source() {
	local p="$1"
	[[ "$p" == *testdata* ]] && return 1
	[[ "$p" == *test* || "$p" == *__tests__* ]] && return 1
	[[ "$p" == docs/* || "$p" == *.md ]] && return 1
	[[ "$p" == *.go || "$p" == *.ts || "$p" == *.tsx || "$p" == *.js || "$p" == *.svelte ]] && return 0
	return 1
}

OLD_SHA="$(jq -r '.old_sha // empty' "$EXACT_IN")"
NEW_SHA="$(jq -r '.new_sha // empty' "$EXACT_IN")"
[[ -n "$OLD_SHA" && -n "$NEW_SHA" ]] || die "exact report $EXACT_IN has no old_sha/new_sha"

IMPACT_JSON=()
ANNOUNCE=()

# map_row <file> <group> <export> <change> <kind> [search-term...]:
# resolve consumers, assign confidence, append the impact entry + announce.
map_row() {
	local file="$1" group="$2" export="$3" change="$4" kind="$5"
	shift 5
	local hits conf consumers_json extra="" capped
	hits="$(consumers_of "$@")"
	if [[ -z "$hits" ]]; then
		conf="unresolved"
		consumers_json='["unresolved"]'
	else
		conf="name-match"
		while IFS= read -r h; do
			[[ -z "$h" ]] && continue
			if is_source "$h"; then conf="direct"; break; fi
		done <<<"$hits"
		capped="$(printf '%s\n' "$hits" | head -$CONSUMER_CAP)"
		local n total
		n="$(printf '%s\n' "$hits" | grep -c . || true)"
		total="$n"
		consumers_json="$(printf '%s\n' "$capped" | jq -R . | jq -s 'map(select(length > 0))')"
		if ((total > CONSUMER_CAP)); then extra=" (+$((total - CONSUMER_CAP)) more)"; fi
	fi
	IMPACT_JSON+=("$(jq -n --arg file "$file" --arg export "$export" --arg change "$change" \
		--arg kind "$kind" --arg conf "$conf" --argjson consumers "$consumers_json" \
		'{file:$file,export:$export,change:$change,kind:$kind,confidence:$conf,consumers:$consumers}')" )
	if [[ "$conf" == "unresolved" ]]; then
		ANNOUNCE+=("$kind $file ~$export ($change) -> unresolved")
	else
		ANNOUNCE+=("$kind $file ~$export ($change) -> $(printf '%s' "$hits" | head -$CONSUMER_CAP | paste -sd', ' -)$extra ($conf)")
	fi
}

# Row lists are materialized to a temp file and read back: `< <(…)` process
# substitution is avoided (unreliable on some Windows shells, and
# unsupported by plain POSIX sh).
IJ_TMP="$(TMPDIR=/tmp mktemp -d)"
trap 'rm -rf "$IJ_TMP"' EXIT

# 1. FUNCTIONAL watched-file exports: changed_functional + added + removed.
jq -r '.files[] | select(.status == "FUNCTIONAL") |
	(.path) as $p | (.group) as $g |
	(.changed_functional[]? | "\($p)\t\($g)\t\(.)\tchanged"),
	(.added[]? | "\($p)\t\($g)\t\(.)\tadded"),
	(.removed[]? | "\($p)\t\($g)\t\(.)\tremoved")' "$EXACT_IN" >"$IJ_TMP/rows1.txt" || true
while IFS=$'\t' read -r file group export change; do
	file="${file%$'\r'}"; group="${group%$'\r'}"; export="${export%$'\r'}"; change="${change%$'\r'}"
	[[ -z "$file" ]] && continue
	kind="$(kind_of_name "$group" "$export")"
	if [[ "$export" == "__header__" || "$export" == "(whole file)" ]]; then
		map_row "$file" "$group" "$export" "$change" "$kind" $(stem_terms "$file")
	else
		map_row "$file" "$group" "$export" "$change" "$kind" "$export"
	fi
done <"$IJ_TMP/rows1.txt"

# 2. NOTICE_ONLY watched-file exports: no port, but the notices table
# (emit_wire.go + notices_gen.go) still consumes them.
jq -r '.files[] | select(.status == "NOTICE_ONLY") |
	(.path) as $p | (.group) as $g |
	(.changed_notice[]? | "\($p)\t\($g)\t\(.)")' "$EXACT_IN" >"$IJ_TMP/rows2.txt" || true
while IFS=$'\t' read -r file group export; do
	file="${file%$'\r'}"; group="${group%$'\r'}"; export="${export%$'\r'}"
	[[ -z "$file" ]] && continue
	map_row "$file" "$group" "$export" "changed" "NOTICE" "$export"
done <"$IJ_TMP/rows2.txt"

# 3. Untracked discovery rows: file-level, stem search.
jq -r '.untracked[]? | "\(.status)\t\(.path)"' "$EXACT_IN" >"$IJ_TMP/rows3.txt" || true
while IFS=$'\t' read -r status path; do
	status="${status%$'\r'}"; path="${path%$'\r'}"
	[[ -z "$path" ]] && continue
	case "$status" in
	A) change="added" ;; D) change="removed" ;; M) change="modified" ;; *) change="$status" ;;
	esac
	map_row "$path" "untracked" "(whole file)" "$change" "UNTRACKED" $(stem_terms "$path")
done <"$IJ_TMP/rows3.txt"

if ((${#IMPACT_JSON[@]} == 0)); then
	ANNOUNCE=("no functional exports or untracked files; nothing to map")
fi

jq -n --arg old "$OLD_SHA" --arg new "$NEW_SHA" \
	--argjson impact "$(printf '%s\n' ${IMPACT_JSON[@]+"${IMPACT_JSON[@]}"} | jq -s 'map(select(.export | length > 0))')" \
	--argjson announce "$(printf '%s\n' ${ANNOUNCE[@]+"${ANNOUNCE[@]}"} | jq -R . | jq -s 'map(select(length > 0))')" \
	'{old_sha:$old,new_sha:$new,checked_at:(now|todate),announce:$announce,impact:$impact}' >"$IMPACT_REPORT"

echo "report: $IMPACT_REPORT"
echo "impact_entries=${#IMPACT_JSON[@]}"
if ((${#ANNOUNCE[@]})); then printf '  - %s\n' "${ANNOUNCE[@]}"; fi
if ((${#IMPACT_JSON[@]} > 0)); then exit 1; else exit 0; fi
