# AGENTS.md — freebucks-proxy operating guide

Machine-readable rules for agents working in this repo. Human overview lives in
`README.md`; visual grammar in `DESIGN.md`; multi-agent workflow in
`devdocs/AGENTIC-WORKFLOW.md`.

## 1. Identity

- Go 1.26 (`go.mod`) gateway for the upstream wire protocol. OpenAI-compatible surfaces
  (`/v1/chat/completions`, `/v1/models` — see `backend/cmd/freebucks-proxy/e2e_test.go`,
  `backend/internal/cli/cli_serve.go`) plus an Anthropic translation layer
  (`backend/internal/server/anthropic*.go`).
- Svelte 5 dashboard (`frontend/`, `freebucks-proxy-dashboard`) embedded via
  `go:embed` (`backend/internal/dashboard/assets_embed.go`) and served at `/admin`.
  Health probe: `GET /healthz` → 200.
- Modes (`backend/internal/config/config.go:HybridBridgeMode/EffectiveMode`):
  pooled (`AUTH_TOKENS` set + `BRIDGE_ENABLED=0`), bridge (`AUTH_TOKENS`
  empty, per-request client token), hybrid (default when `AUTH_TOKENS` set:
  `API_KEYS` credential uses the pool, any other credential relays as bridge).
- Upstream credits meter (wire fields: `freebucks*`): the wire `prices` map is
  the sole cost source; charge-once at session start; 1h sessions; `DELETE`
  refund; Pacific-midnight refill. `deepseek/deepseek-v4-flash` is an unpriced
  row (verified cost-0 live 2026-09-08).

## 2. Topology

- `backend/` — Go gateway (`cmd/`, `internal/`). `internal/` packages include
  `server`, `pool`, `upstream`, `session`, `store`, `config`, `dashboard`,
  `modelcat`, `registry`, `wirefacts`.
- `frontend/` — Svelte 5 SPA. Committed bundle
  `backend/internal/dashboard/dist` is what the binary serves.
- The gitignored upstream vendor clone (live checkout) — never commit; the exact path lives in `scripts/check-upstream.sh`. Source of truth for all wire/registry/model work. Keep freshly fetched to `origin/main` before starting; pins live in `backend/internal/wirefacts/testdata/wire/snapshots.json` (`upstream_sha`) + `scripts/vendor-version.txt`, verified by `scripts/check-upstream.sh`.
- `scripts/` — `sync-upstream.sh`, `check-upstream.sh` (canonical parity check),
  `review-wire-drift.sh`, `drift-exact.sh` (exact export-level MODEL/PRICE/WIRE report),
  `drift-tui.sh` (picker wireframe per tier: rows/order/fields + refresh checklist).
- `.github/workflows/` — `ci.yml` (jobs `test`, `frontend`), `lint.yml` (job
  `golangci`), `codeql.yml` (job `analyze`), `dependency-review.yml`,
  `upstream-drift.yml`, `release.yml`.

## 3. Commands

### Fast Audit by Domain (< 5 seconds)

```sh
# Frontend Dashboard: typecheck in ~3s (zero screenshot / e2e overhead)
npm --prefix frontend run check
# Targeted single-spec Playwright test (when validating a specific UI flow)
npx --prefix frontend playwright test e2e/<target>.spec.ts

# Backend Gateway: test specific package without full-suite 10m race runner
go test -v ./backend/internal/<pkg>/...    # e.g. ./backend/internal/pool/...
go vet ./backend/internal/<pkg>/...

# Upstream Parity / Wire Drift: instant export-level check
bash scripts/drift-exact.sh

# Config validation
go test ./backend/internal/config/...

# Test tiers (seconds → minutes; CI stays authority for -race/golangci/e2e)
task verify:quick                            # gofmt + vet + svelte-check, seconds
task test:fast                               # all backend pkgs except pool+server+cmd
go test -short ./backend/internal/<pkg>/...  # skip heavy keepers (pool ladder/pin, conformance, mock-e2e)
task test:pool | task test:server | task test:e2e   # heavy lanes, run alone serially
npm --prefix frontend run test:unit          # node unit suites, no browser
```

### Full Verification (Pre-Merge / Nightly)

```sh
# Hermetic backend tests (CI equivalent: go test -race -timeout 10m ./backend/...)
env -u AUTH_TOKENS -u ADMIN_TOKEN go test ./backend/...

# Build / vet / lint
go build ./backend/...
go vet ./backend/...
golangci-lint run ./backend/...

# Frontend bundle & full e2e suite
npm --prefix frontend run check && npm --prefix frontend run lint && npm --prefix frontend run format:check
npm --prefix frontend run build             # vite build → refresh backend/internal/dashboard/dist
npm --prefix frontend run test:e2e          # Playwright 18 specs (needs built dist)
```

# Windows host: AV blocks test-exe link in %TEMP% (e2e builds outside it),
# -race never links (cgo exit 2 — CI owns race), 0600 asserts skip on Windows.
# Rotating server-suite failures that pass solo are the known pre-existing flake
# (proven on pristine HEAD) — note-and-move-on after 2 reruns. `task verify:full`
# is the CI mirror (race + e2e + dist-diff); `task verify` is the fast gate.

Knob chain: any `.env` knob must propagate
dotenv → static → live → SSE hash → store refresh.

## 4. Workflow (protected main)

1. Feature branch off `origin/main` in a `/tmp` worktree (never the shared
   checkout — it carries uncommitted user work) → PR → exact required-check
   contexts green (`analyze`, `dependency-review`, `frontend`, `golangci`,
   `test` — audit via `gh api repos/trefeon/freebucks-proxy/branches/main/protection
   --jq .required_status_checks.contexts`; CI jobs `test`+`frontend`, lint job
   `golangci`, CodeQL job `analyze`, `dependency-review` job) → squash merge,
   then **always return to `main` and delete merged branches**. Never claim
   green without a fresh `gh pr checks` run (`gh pr checks --watch` live-waits,
   never sleep-poll). `gh pr update-branch` takes NO `--merge` flag on this
   host — retrigger zero-signal bot-push PRs (a `GITHUB_TOKEN` push triggers no
   workflows) positionally via `gh pr update-branch <N>`, re-green, then merge.
   NEVER squash-merge a branch still checked out in a lane worktree (the CLI
   exits 1 and the delete fails): `git worktree remove <path>` first, or merge
   API-only then delete refs manually (`git branch -D` locally, verify the
   remote delete via `git ls-remote --heads`). Post-merge: `git checkout main`,
   `git fetch origin --prune`, `git pull origin main`, `git branch -d <branch>`
   (`-D` when squash-merged, the tip is never an ancestor).
2. Conventional Commits (`feat|fix|chore|docs|…(scope): subject`).
3. Never stage/commit unless asked. Never commit secrets, `reference/`, or devdocs.
4. No local docker. Preview on a review host from a `/tmp` worktree (never the shared
   checkout — it carries uncommitted user work):
   `docker build --network=host` + compose up, then `GET /healthz` → 200.
   GHCR preview: `docker compose pull && VERSION=x docker compose up -d` runs the
   release image; pin `VERSION` to the release tag. Prod is the production VPS.
5. Frontend `dist` is rebuilt and committed before merge when `frontend/src`
   changes (dist-freshness CI diffs the bundle). For fast audits or static
   reviews, skip dist rebuild/e2e and run `npm --prefix frontend run check` (typecheck ~3s).
6. Upstream syncs: classify wire drift BEFORE refreshing the baseline, else
   `review-wire-drift.sh` reports all-SAME against the new anchors and hides
   FUNCTIONAL rows. LF-normalize `snapshots.json` comparisons (CRLF checkouts
   fake drift). Merge drift PRs strictly serially wire → registry → dashboard,
   with green CI between each merge — never batch or overlap drift merges.
   Version-gated bot: the `version_gate` job runs FIRST and only signals —
   `pinned_version` (scripts/vendor-version.txt), `live_version` (npm,
   empty when unknown), `live_known`, `version_changed` (true only when
   live is known AND differs), `skip` (true only on positively-confirmed
   SAME: live known AND equal). Unknown/empty live fails OPEN to a full run
   (skip=false). Full classification + PRs run only on a confirmed wrapper
   bump; per-version PRs are reused by title match on the new version, never
   duplicated. The version signal NEVER changes script exit codes — the gate
   lives in workflow `if:` conditions only. Dual pins
   (vendor-version.txt + snapshots.json vendor_version) land atomically in
   the same bump commit before the wiregen SHA gate.
7. Upstream-first: start any wire/registry/model work by updating the gitignored upstream vendor clone to latest `origin/main` (`git -C upstream/freebuff fetch origin main`, checkout `origin/main`). Nothing gates or pre-approves this update. If it moved past the recorded pins, classify with `check-upstream.sh` + `review-wire-drift.sh` and carry any port/re-pin through the drift PR flow.
8. Subagent worktrees & fast lanes: many subagents share ONE tree (one checkout + branch) when editing the same domain — same feature area, disjoint files or tightly-coupled edits, with hub coordination before touching shared files. Split to one-worktree-per-agent only when domains differ or clobber risk is real. In multi-agent parallel lanes touching frontend/, the integrating lane rebuilds + commits dist LAST; parallel lanes NEVER rebuild dist concurrently (stale-bundle races).
9. Domain-gated CI: CI uses path filtering (`dorny/paths-filter`). PRs modifying only frontend bypass backend race tests, CodeQL, and Go lint in ~3 seconds. PRs modifying only backend bypass Playwright e2e in ~3 seconds. Docs PRs bypass all heavy suites. Always keep PR changes tightly scoped to the domain.
10. Rotating server-suite flake triage: a FAIL set that passes solo is the known pre-existing Windows-host flake — solo-rerun the failing tests, then run the full suite on a pristine `/tmp` worktree at HEAD; rotating-set + pristine-FAIL = note-and-move-on, CI Linux is authority (see §5 flake policy).

## 5. Budgets and freezes (as observed)

- Autonomy under ~10-step rails; fan out via isolated lanes (see
  `devdocs/AGENTIC-WORKFLOW.md` §1).
- Request limits default 30/min, 1500/day Pacific; `SAFE_MODE=true` is the
  anti-ban preset (`.env.example`); `COST_MODE=free`.
- Test flake policy: single FAIL with greens before/after (e.g. wall-clock
  quota-boot probe before ~09:05 PDT) is note-and-move-on after 2 reruns;
  reproduce on pristine `main` before blaming the branch.
- `archtest.test.exe` "Access is denied" on Windows is the AV block; hand-verify
  via import grep, CI Linux is the real proof.
- Public repo hygiene (ZERO private leaks — this repo is public):
  - NEVER commit hostnames (review/prod hosts), public or private IPs,
    key-file names containing hostnames, local usernames/paths
    (`C:/Users/…`, `/home/…`, `/tmp/fb-…`), or infra names
    (Tailscale/cloudflared/DNS). Write "a review host" / "production" /
    `http://127.0.0.1:3457` / `api-keys.local` instead. Past comments that
    cited real hosts were scrubbed 2026-09-20 — do not reintroduce them.
  - NEVER commit secrets: real keys live only in untracked `.env`/shell env
    (gitignored); tracked tree holds placeholders only
    (`ADMIN_TOKEN=123456` factory default). Test fixtures use synthetic
    values (`SENTINEL-*`, sequential hex) allowlisted in `.gitleaks.toml`.
  - Pre-push on a public repo: `gitleaks git -v --redact .` (history) +
    `gitleaks dir -v --redact .` (tree); baseline 2026-09-20 = 7 findings,
    all verified false positives, trufflehog 0 verified. New findings must
    be explained before push. History mentions of old hostnames are
    grandfathered — a filter-repo rewrite is NOT approved for those alone.
  - Test live with user-provided keys only; rotate on suspicion.
