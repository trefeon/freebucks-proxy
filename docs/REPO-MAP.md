# REPO-MAP — full structure guide

Where everything lives and how a request flows. Commands live in `AGENTS.md` §3
(not repeated here); decisions in `docs/decisions/`; upstream port status in
`docs/UPSTREAM-PORT-QUEUE.md`. All paths repo-relative, verified against main.

## What this is

Go 1.26 gateway (`backend/`) translating OpenAI-compatible (`/v1/chat/completions`,
`/v1/responses`, `/v1/models`, `/v1/embeddings`) and Anthropic (`/v1/messages`)
surfaces to the upstream wire (`https://www.codebuff.com`), plus a Svelte 5
dashboard (`frontend/`) embedded via `go:embed` and served at `/admin`.
Health probe: `GET /healthz` → 200.

**Pool-only.** `Config.EffectiveMode()` always returns `"pooled"`
(`backend/internal/config/config.go:335-336`); `API_KEYS` gates pool access
(401 otherwise). There are no bridge/hybrid/relay paths
(`backend/internal/config/config_pooled_test.go:8-9`).
> Stale-pointer warning: `AGENTS.md` §1 still describes pooled/bridge/hybrid
> modes — that section predates the pool-only removal
> (`docs/decisions/pool-only-removal.md`). Test names containing "hybrid"
> (e.g. `server_pooled_test.go`) are historic labels for pooled behavior.

## Backend (`backend/`)

Boot: `cmd/freebuff-proxy/main.go` → `internal/cli/cli_serve.go:44-70`
(config load + DB overlay → store open → registry + pool wiring → HTTP serve).
Codegen tools: `cmd/openapi-emit` (dashboard `openapi.json`),
`cmd/wiregen` (catalog/wirecodes/notices/toolmap emission).

Request layers, top-down: `server` → `pool` → `session`+`runs` → `upstream`
→ `convert`. Dependency layering is enforced by
`backend/internal/archtest/arch_test.go:1-90`.

| Package | Purpose — key files, owned state |
|---|---|
| `config` | Load/validate; `config.go` struct, `config_keys.go` defaults (`SESSION_CALL_TIMEOUT "30s"`), `config_load.go` dotenv→static→live→SSE→store chain, `keycatalog.go` dashboard catalog |
| `server` | HTTP surface: `server_init.go`, `server_routes.go:190-305`; chat entry `openai.go:17-98`; acquire→relay core `engine.go:68-151,152-248`; single-attempt path `engine_attempt.go:65-103`; Anthropic layer `anthropic*.go`; dashboard JSON `admin*.go`, `dashboard_pages_test.go` |
| `pool` | Multi-token front door: `pool.go:48-80` Lease, `:652` Acquire, `:976-1002` AddToken; routing `acquire_route.go` (smart scan/walk/park vs legacy loop); liveness cadence `pool_lifecycle.go:26-47` (30s±20%); quarantine/cooldown on terminal states |
| `session` | Per-token lifecycle: `session_manager.go:142-149`, `session_admission.go` (incl. `model_unavailable` fallback `:659`, window cache `model_unavailable.go`), `session_poll.go:42-155` (purchase arms `:118`), `sessions_persist` DB rows |
| `runs` | Per-agent run lifecycle: `runs.go:1-43` (lazy START, 6h rotation, FINISH drain) |
| `upstream` | Single-token wire client. `client.go` (transport/TLS/redirect/egress), `client_chat.go` (request build, retry), `chat.go` (envelope `injectEnvelope :444-563`), `session.go` (admission/poll/DELETE, header consts `:18-61`, streak logic; `session_streak_test.go` is test-only — no standalone `streak` file), `session_parse.go`, `classify.go` + `errors.go` (typed errors), `wirecodes_gen.go` (marker vocab), `notices_gen.go`, `ads.go` + `ads_chat.go`, `auth_login.go` + `auth_github.go`, `login/fingerprint.go`, `tokenhealth.go`, `ratelimit.go`, `availability*.go`, `consistency.go`, `dump.go` |
| `convert` | Pure OpenAI normalization (`convert.go`, `convert_request.go:159-168`), tool-name translation (`toolmap_request.go`, `foreign_signals.go`, rules in `docs/decisions/tool-name-translation.md`), OMP floor-only wire (`tools_floor.go`, hooked at `convert_request.go:182-187`) + response arg reshape (`tools_reshape.go`) |
| `registry` | Model→agent mapping (`registry.go:106-127`), upstream TS refresh |
| `modelcat` | Generated picker mirror: `catalog_gen.go:12-61` (`ModelInfo{Served,Tiers,Premium,PlanRequired}`) |
| `wirefacts` | Pinned upstream snapshots (`testdata/wire/`), codegen contract (`wirefacts_gen.go`: UpstreamSHA, VendorVersion, LlmProvidersVersion, BunVersion) |
| `store` | Single SQLite file (modernc, goose `migrations/00001..00005`, schema v5): history + settings overlay + persist; `store.go:1-46` |
| `dashboard` | View models over pool/registry/upstream (`dashboard_cards.go`), `data/openapi.json`, committed bundle `dist/` (CI freshness-diffed) |
| `cli` | Serve/boot wiring (`cli_serve.go`) |

### Chat path, end to end

`POST /v1/chat/completions` (`server/openai.go`) → sanitize → `chatCore` →
`engine.go` acquire→relay (API_KEYS 401 gate; `ErrRunInvalid`
rotate-and-retry-once; release/abandon) → `pool` Lease/Acquire/Chat →
`session` EnsureSessionForModel (admission, single-flight, persist) →
`runs` lazy START → `upstream` ChatCompletions + envelope →
`convert` relay back. Typed sentinels (`upstream/errors.go`,
`classify.go:225-228`): `ErrSessionInvalid`, `ErrRunInvalid`,
`ErrAuthRejected`, `ErrWaitingRoom`.

## Frontend (`frontend/`)

Svelte 5 + Tailwind 4 SPA (`src/App.svelte`, hash+path routing via
`src/lib/nav.js`: overview/tokens(Accounts)/plans(Models)/activity(Logs)/
ads/settings + hidden devtools/review). API: `src/lib/api/paths.js`
(endpoint map), `client.js` (fetch helpers, `fb_csrf` double-submit,
401→session-expired latch in `stores/session.js`). Polling: refcounted
`stores/query.js` (FULL_EVERY_POLLS=30/5min, visibility-aware) +
`stores/tokens.js` (10s hot `?view=live` merge) + SSE
`/admin/api/events` (`utils/events.js`, polling fallback). ~40 components
in `src/lib/components/` (TokenCard/Table/Drawer, Maturity/Allowances,
LiveConsole, SessionSpawn, BatchTest, …); utils in `src/lib/utils/`.
Types: `src/lib/api/openapi.d.ts` (generated — see `cmd/openapi-emit`).

## Scripts, workflows, root

| Path | Job |
|---|---|
| `scripts/check-upstream.sh` | Canonical parity check (pins vs live clone) |
| `scripts/drift-exact.sh` | Export-level MODEL/PRICE/WIRE report |
| `scripts/review-wire-drift.sh`, `drift-tui.sh` | Classify drift (classify BEFORE refreshing baseline) |
| `scripts/sync-upstream.sh` | Registry sync (6 files + parity + tests) |
| `scripts/repin-all.sh` | Atomic dual-pin bump (pins move together, never alone) |
| `scripts/extract-tool-calls.sh` | Regenerate convert corpus fixture |
| `scripts/vendor-version.txt` + `wirefacts` snapshots | Dual pins (see port queue doc) |
| `scripts/install.sh` / `install.ps1`, `gen-token.*` | Install + token helpers |
| `.github/workflows/ci.yml` | `test` + `frontend` jobs (path-filtered) |
| `lint.yml` / `codeql.yml` / `dependency-review.yml` | `golangci`, `analyze`, dependency review |
| `upstream-drift.yml` | 12h cron: version_gate → drift → registry/port/notices/dashboard PRs, serial, never auto-merge |
| `release.yml` | `v*` tags → GoReleaser + SLSA → GHCR |
| `Taskfile.yml` (+ `Makefile` mirror) | Canonical runner (`verify`, `test:fast`, `test:pool/server/e2e`) |
| `Dockerfile`, `docker-compose.yml` | Multi-stage build (`VERSION` ldflag), `db_data` volume + bind |
| `.env.example` | Knob documentation (placeholders only; real keys never tracked) |

## State: where each datum lives

Env defaults < JSON < `.env` < DB overlay < process env (precedence), moving
toward unified-store (mem-authoritative, DB-persisted, `.env`-seed-only) —
see `docs/decisions/data-architecture.md` + `unified-store.md`. Runtime DB:
`DB_PATH` else `./data/freebuff.db`; prod volume `db_data`
(`/var/lib/docker/volumes/freebuff-proxy_db_data/_data`, root-owned —
online snapshots via `sqlite3 .backup`, host reads only after clean stop
checkpoints WAL). Timeouts/knobs propagate dotenv → static → live → SSE
hash → store refresh (any `.env` knob must walk the whole chain).
