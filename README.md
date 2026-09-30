# freebuff-proxy

Go wire gateway in front of the upstream Freebuff service: pooled
multi-account OpenAI-compatible (`/v1/chat/completions`, `/v1/responses`,
`GET /v1/models`) and Anthropic-compatible (`POST /v1/messages`) endpoints,
an embedded Svelte dashboard at `/admin`, and automatic session lifecycle.
(`freebucks` — with an *s* — is the upstream credit unit, wire fields
`freebucks*`; runnable artifacts — module, binary, compose service, image —
are all `freebuff-proxy`.)

## What it is

- **Translation gateway, not a model host.** Client surfaces are translated
  to the upstream wire (`https://www.codebuff.com`, see
  `backend/internal/upstream/`) and relayed back. Request layers top-down:
  `server` → `pool` → `session`+`runs` → `upstream` → `convert`
  (dependency layering enforced by
  `backend/internal/archtest/arch_test.go`).
- **Pool-only.** `Config.EffectiveMode()` always returns `"pooled"`
  (`backend/internal/config/config.go`); every request draws from the
  `AUTH_TOKENS` pool. When `API_KEYS` is set, only requests bearing one of
  those keys are served (`401 invalid_api_key` otherwise,
  `backend/internal/server/engine.go`); when empty, client auth is skipped
  (`backend/internal/server/middleware_auth.go`). Bridge/hybrid/relay paths
  were excised — see `docs/decisions/pool-only-removal.md`. (Older notes
  naming bridge/hybrid modes predate that removal.)
- **Dashboard** at `/admin` (Svelte 5 SPA, `frontend/`, committed bundle
  `backend/internal/dashboard/dist/` is what the binary serves).
  Health probe: `GET /healthz` → 200 (no auth, never rate-limited).
- **Credit metering** follows the wire `prices` map (the sole cost source):
  charged once per session-hour at session start, refunded on early session
  `DELETE`, refilled at the daily reset the server advertises
  (`resetTimeZone`/`resetAt` — the account's own local midnight; Pacific
  midnight only on servers that omit the zone). The proxy's own counters
  keep bucketing on the Pacific day (`pool/spend.go:bucketStart`).

## Quickstart

```sh
cp .env.example .env   # then edit: AUTH_TOKENS, ADMIN_TOKEN, ...
task build             # frontend bundle + gateway binary (bin/freebuff-proxy)
task dev               # go run ./backend/cmd/freebuff-proxy
```

`task` targets are defined in `Taskfile.yml` (a `Makefile` mirror exists).
With plain Go: `go build ./backend/...` — the gateway main package is
`backend/cmd/freebuff-proxy`.

Run from GHCR (release image, no local build):

```sh
cp .env.example .env   # then edit: AUTH_TOKENS, ADMIN_TOKEN, ...
docker compose pull
VERSION=<release-tag> docker compose up -d
```

Pin `VERSION` to a release tag for a reproducible deploy, or leave it unset
to follow `latest` (`image: ghcr.io/trefeon/freebuff-proxy:${VERSION:-latest}`
in `docker-compose.yml`). Or build locally: `docker compose up -d --build`.

Then:

- `GET http://localhost:3457/healthz` → 200
- `GET http://localhost:3457/v1/models` → live model list
  (needs your `API_KEYS` key only when you set `API_KEYS`)
- `http://localhost:3457/admin` → dashboard (login gate, factory password
  `123456` — rotate it before exposing the port)

Defaults that matter (`.env.example`): `SAFE_MODE=true` (anti-ban preset),
`COST_MODE=free` (anything else bills paid — fresh free accounts get
`402 "Out of credits"`). Per-IP rate limiting is off by default
(`RATE_LIMIT_PER_IP=0`; set it and `RATE_LIMIT_BURST` to enable).
`LISTEN_ADDR=127.0.0.1:3457` is loopback-only; compose overrides it to
`:3457` inside the container and maps `3457:3457` — note `/healthz` and
`/metrics` are deliberately unauthenticated, so bind `127.0.0.1:3457:3457`
on the host if you don't want them public. Optional TLS front:
`docker compose --profile https up -d` (Caddy, `DOMAIN=` for Let's Encrypt).

Get pool tokens with `scripts/gen-token.sh` (or `gen-token.ps1`/`.cmd`);
the device-code flow is `scripts/device-login.py start|poll`.

## Modes

There is one mode: **pooled**. `AUTH_TOKENS` holds the upstream pool
token(s); every client is served from this pool. `API_KEYS` is an optional
client-credential gate, not a routing mode:

| Setup | Behavior |
|---|---|
| `AUTH_TOKENS` set, `API_KEYS` set | Pool serving; clients must send a listed key (`Authorization: Bearer` or `x-api-key`), else `401` |
| `AUTH_TOKENS` set, `API_KEYS` empty | Pool serving; no client auth |
| `AUTH_TOKENS` empty | Gateway serves errors — configure pool tokens (or CLI-credential auto-discovery via env-only `AUTO_DISCOVER_TOKEN`) |

Decision record: `docs/decisions/pool-only-removal.md`.

## Client compatibility

Point any OpenAI- or Anthropic-shaped agentic client at the proxy as a
custom provider. Full per-client recipes live in
`docs/UNIVERSAL-CLIENTS.md` — the table below is the short form:

| Client | Base URL | Auth | Notes |
|---|---|---|---|
| OMP / pi (OpenAI) | `http://HOST:3457/v1` | `Authorization: Bearer <proxy-key>` | compat baked at build; rerouting needs `registerProvider` |
| OMP / pi (Anthropic) | `http://HOST:3457` (no `/v1`) | `x-api-key: <proxy-key>` | SDK appends `/v1/messages` |
| claude-code | `ANTHROPIC_BASE_URL=http://HOST:3457` | `ANTHROPIC_API_KEY=<proxy-key>` | `cc_*` markers stripped (also server-side) |
| codex | `base_url=http://HOST:3457/v1` in `config.toml` | `env_key` → proxy key | `wire_api="responses"`; `"chat"` is a hard config error |
| opencode | `provider.<id>.options{baseURL:http://HOST:3457/v1,apiKey}` | `OPENCODE_API_KEY` or options | keep Responses item order + `reasoning_text` |
| crush / kimi / jcode / goose / hermes | `*_BASE_URL=http://HOST:3457/v1` | corresponding key env → proxy key | never forward vendor env names upstream |
| openclaw / openhands / swe-agent | `baseUrl/base_url/api_base=http://HOST:3457/v1` | `apiKey/api_key` → proxy key | keep tool history when trimming tools |

Rules that hold for every client: OpenAI `baseUrl` includes `/v1`,
Anthropic does not; `model` is any served model id (allowlist via
`MODELS_ALLOW`, per-slot pins via `PIN_MODEL`).

**Translation notes** (details in `docs/OMP-TRANSLATION.md`, rules in
`docs/decisions/tool-name-translation.md`):

- Non-OMP clients: **rename-only** — foreign tool names become the official
  signature equivalents on the wire (`bash`→`run_terminal_command`,
  `read`→`read_files`, `edit`→`str_replace`, `write`→`write_file`,
  `grep`→`code_search`, `todo`→`write_todos`, `find`→`glob`,
  `edit-diff`→`apply_patch`; `glob`/`web_search` are identity), client names
  restored on every response path. Parameters pass through after structural
  normalization — only names are rewritten.
- OMP / pi-family: **floor-only replacement** — tool defs become exactly the
  16 canonical CLI definitions plus the `end_turn` pin (`floorOnlyOMP`,
  `backend/internal/convert/tools_floor.go`); response args are reshaped to
  OMP shape (`tools_reshape.go`). Zero foreign riders reach the wire — the
  upstream gate keys on tool *definitions* (live bisect 2026-09-30).
- Missing `end_turn` sentinel is appended, never duplicated; `decide` is
  stripped before egress (`backend/internal/upstream/clitools.go`).
- `POST /v1/embeddings` answers `400 unsupported_endpoint` by design — this
  proxy serves chat completions only. **gemini-cli is not usable** via its
  gateway env (needs a `/v1beta` surface the proxy doesn't expose).

## Dashboard

Pages: overview, tokens (Accounts), plans (Models), activity (Logs), ads,
settings, plus setup and a playground. Live updates ride SSE
(`GET /admin/api/events`, polling fallback); per-page UI snapshots persist
server-side (`GET`/`PUT /admin/api/pages/{id}`).

Admin API groups (`backend/internal/dashboard/admin_wire.go`,
`AdminAPIPaths`; machine schema at `backend/internal/dashboard/data/openapi.json`,
emitted by `backend/cmd/openapi-emit`): `overview`, `tokens`, `models`,
`traces`, `setup`, `config` (+ `config/meta`, the settings-form schema),
`settings` (overlay save/reset), `pages`, `logs` (+ `history`, `rollup`,
`export`/`import`), `quota/history`, `maturity/history`, `metrics`, `usage`,
`version`, `events`, `auth/status`, `notices`, `ads/summary`, `ads/legs`,
plus `POST /admin/diag` (config + upstream reachability checks),
`change-password`, `require-login`, `smoke` (zero-cost probe), and
`POST /admin/restart`.

Auth: `ADMIN_TOKEN` (factory default `123456` — a startup warning prompts
rotation). Leave empty only on loopback-only single-user installs. Set
`DASHBOARD_ENABLED=false` to disable all `/admin` routes (404, headless).

## Egress and country verdict

Upstream resolves country from the **egress IP alone** — no client-settable
field changes it, and VPN/proxy/Tor/hosting/relay egress reads as
`anonymous_network`, which caps trust and gates full-access seats
(`docs/TIERS.md` for the full/limited matrix).

- Knobs that only align *consistency signals* (timezone header, locale):
  `SESSION_TIMEZONE` (auto by default), `US_CONSISTENCY`. They never move an
  ASN verdict.
- `UPSTREAM_EGRESS_PROXY=http(s)|socks5://…` exits *every* upstream call
  (admission, poll, chat, agent runs) through a clean directly-attributed
  route. **Restart-only** (transport built once at startup); a relay that
  looks like a relay reproduces the same refusal — the exit itself must read
  clean. H2 rides the override.
- Live workaround runbook (VPS egress flagged, WARP proxy-mode exit
  verified `accessTier: full` 2026-09-30): **`docs/EGRESS.md`** —
  `scripts/setup-warp-egress.sh` / `scripts/revert-warp-egress.sh`.
  Re-check standing after any exit-IP change.

## Configuration

`.env.example` documents the safe defaults; `.env.full-example` is the
complete reference (every key); `.env.minimal` is the 7-key starter. The
dashboard settings form schema is `GET /admin/api/config/meta`
(`backend/internal/config/keycatalog.go`). Precedence, low to high:
built-in defaults < JSON `-config` < `.env` < DB overlay < process env.

| Area | Key knobs (defaults) |
|---|---|
| Pool | `AUTH_TOKENS` (empty serves errors), `API_KEYS` (empty = no client auth), `PIN_MODEL`, `MODELS_ALLOW` |
| Safety | `SAFE_MODE=true` (anti-ban preset: 30m idle rotation, 200ms `REQUEST_JITTER`), `COST_MODE=free`, `TRANSIENT_RETRIES` (transport failures only — never 429/403/401), `ACTING_USER_ID` (leave empty; any foreign id is impersonation) |
| Sessions | `SESSION_PERSIST=true` (restart resumes, `SESSION_STATE_FILE`, 0600, token never written), `SESSION_TIMEZONE` (auto), `US_CONSISTENCY=false`, `UPSTREAM_EGRESS_PROXY` (restart-only), `SESSION_PARK_ENABLED=true` (≤15m cooldowns park, fatal refusals stay terminal) |
| Cost/queue | `SLOTS_PER_ACCOUNT=3`, `QUEUE_WAIT=30s`, `QUEUE_DEPTH=16`, `MAX_SPILL_ACCOUNTS=0`, `MATURITY_ENABLED=true` (nightly streak touch), `SMART_PROBE_ENABLED=true` (+ `SMART_PROBE_BACKOFF_MAX=30m`) |
| Cooldowns | `COOLDOWN_*_MS` family (auth 30m, country-block 15m, ceiling 7d, `COOLDOWN_IP_MAX_READMITS=3` re-admits/Pacific day ±20% jitter) |
| Access | `LISTEN_ADDR=127.0.0.1:3457`, `ADMIN_TOKEN=123456`, `DASHBOARD_ENABLED=true`, `RATE_LIMIT_PER_IP=0`, `HTTP_READ_TIMEOUT=60s` (restart-only) |
| Observability | `LOG_LEVEL=info`, `LOG_FORMAT=text`, `LOG_ACCESS=true`, `DEBUG_DUMP=false` (redact-and-dump to `./dump/`, debugging only) |
| Upstream | `UPSTREAM_BASE_URL=https://www.codebuff.com`, `REGISTRY_REFRESH`, `HTTP2_UPSTREAM` (false = force HTTP/1.1 escape hatch) |
| Translation | `COMPRESS_PROMPT=false`, `CACHE_CONTROL_INJECTION=true`, `REASONING_IN_CONTENT=false` (all **env-only**: inert as `.env` lines in native installs — export them; compose `env_file` exports them) |

First boot imports the effective config into the dashboard DB (`DB_PATH`,
SQLite, mode `0600`) as `config:` overlay rows plus a
`config:migrated_env_v1` marker — later boots are no-ops via the marker, and
explicit process env still wins at runtime. Any `.env` knob must propagate
dotenv → static → live → SSE hash → store refresh.

## Sessions and metering

- Sessions are 1-hour, charged once at admission from the wire `prices` map;
  early `DELETE` refunds; within the hour, re-admission on the same holder
  costs nothing. The purchase claim (`cli:<uuid>`) is stable across rejoins;
  on the dead-start 409 (`admission_attempt_closed`) refresh rotates the
  persisted claim and retries admission **exactly once**
  (`docs/decisions/claim-rotate-closed.md`).
- Concurrency is slot-bound (`purchase_capacity` names the holder; the pool
  sees it, never re-fights it). `model_unavailable` falls back to the
  cheapest served unmetered row for the token's live meter; withdrawn ids
  stay recognized and coerced, never deleted.
- Streak/maturity automation (`MATURITY_*`) and the smart quota prober
  (`SMART_PROBE_*`, session-less, Pacific-day scheduled) default ON; set
  false to restore manual-only behavior. Retry/churn storms get accounts
  banned — keep `CHAT_AUTO_RETRY=false` while probing new envelopes and
  probe spaced, never in loops.

## Development

Operating guide: **`AGENTS.md`** (commands, lanes, CI, drift flow — not
repeated here). The short form:

- Protected `main`: branch → PR → required checks green (`analyze`,
  `dependency-review`, `frontend`, `golangci`, `test`) → squash merge.
  Conventional Commits. Never commit secrets, `reference/`, or devdocs.
- `task verify` is the fast gate (gofmt + vet + build + fast-tier tests +
  frontend check/lint/format); `task test:fast|pool|server|e2e`, `task verify:full`
  (race + e2e + dist freshness) above it. Hermetic: tests run with
  `AUTH_TOKENS`/`ADMIN_TOKEN` unset. CI path-filters by domain (docs-only
  PRs skip heavy suites); Windows-only flakes defer to Linux CI.
- Frontend `dist/` is rebuilt (`npm --prefix frontend run build`) and
  committed whenever `frontend/src` changes — CI diffs bundle freshness.
- Upstream sync: pins are `scripts/vendor-version.txt` + wire snapshots
  (verified by `scripts/check-upstream.sh`); classify drift with
  `scripts/drift-exact.sh` / `review-wire-drift.sh` **before** refreshing
  baselines; merge drift PRs strictly serially wire → registry → dashboard
  → re-pin. Status: `docs/UPSTREAM-PORT-QUEUE.md`.

Key scripts: `sync-upstream.sh`, `check-upstream.sh`, `drift-exact.sh`,
`review-wire-drift.sh`, `drift-tui.sh`, `repin-all.sh`,
`extract-tool-calls.sh` (convert corpus fixture), `install.sh`/`install.ps1`,
`backup-state.sh` + `verify-state.sh` (update gate), `collect-debug.sh`,
`device-login.py`, `gen-token.*`, `live-pool-smoke.py`,
`free-tier-gate-probe.py`.

## Troubleshooting

`bin/freebuff-proxy -doctor` (or `POST /admin/diag` on a running server)
covers environment + config + upstream reachability. For wire forensics:
`DEBUG_DUMP=true` redacts and dumps to `./dump/`
(`dump/session-*-admission.dump` carries the admission verdict).

| Symptom | Cause → fix |
|---|---|
| `401 invalid_api_key` on `/v1/*` | `API_KEYS` set but client key missing/wrong → send a listed key (`Bearer` or `x-api-key`); empty `API_KEYS` skips client auth |
| `402 Out of credits` on fresh free accounts | `COST_MODE` ≠ free → must stay `free` |
| `403 country_blocked` / `anonymous_network` | Egress IP verdict (terminal for this account+egress) → move egress (`UPSTREAM_EGRESS_PROXY`, `docs/EGRESS.md`); `US_CONSISTENCY` aligns signals only, never the verdict |
| `403 banned` | Dead account → drop the token (dashboard), confirm with per-token Test / `-test-token` |
| `409 purchase_capacity` / `purchase_in_use` | Seat held by another live session → wait for expiry/release; never re-fight a held seat |
| `409 admission_attempt_closed` | Dead claim — auto-rotated once with one fresh-claim retry; if it persists, read the dump instead of retrying |
| `429 ip_capped` | Too many distinct users on the egress IP: 3 re-admits per Pacific day, then midnight lock → spread accounts across clean exits |
| `429 spend_limited` / `rate_limited` | Soft cooldowns, pool-owned → back off; smart-probe 429 doubling caps at `SMART_PROBE_BACKOFF_MAX` |
| `503 The model is temporarily unavailable` | Upstream refusal, not capacity: rejected envelope (check tool defs — `docs/OMP-TRANSLATION.md` §1), waiting room, or outside-hours (`deployment_outside_hours` is retryable) |
| `model_unavailable` with `availableHours` | Paused window / plan gate → proxy falls back; withdrawn ids coerce, unknown ids refuse |
| `GET /api/v1/me` → 401 on a healthy free token | Normal — `/me` is paid-only; the session GET is the authoritative free-tier probe (`docs/FREE-TIER-GATE.md` §1) |
| Dashboard login loops / cookie loss | Scheme mismatch — cookies adapt to the connection protocol (`X-Forwarded-Proto: https` behind a TLS front, or `ADMIN_FORCE_SECURE_COOKIES`) |

Updates (read before every recreate): the live store is `DB_PATH` on the
`db_data` volume (`/app/data/freebuff.db` under compose), the bind checkout
is env/logs/legacy-migration source. Every update runs
`docker compose stop` → `scripts/backup-state.sh` → `up -d --build` →
`ADMIN_TOKEN=… scripts/verify-state.sh` (healthz 200 + wrong-token 401 +
strict no-op boot + manifest counts). Never plain-`cp` a live DB trio;
stop first or use the backup script.

## Repo map

Full structure guide: `docs/REPO-MAP.md`. Condensed:

| Path | Job |
|---|---|
| `backend/cmd/freebuff-proxy` | gateway entry (`-doctor`, `-test-token`, `-validate-tokens`, `-refresh-token`, `-setup`, `-update`, `-version`) |
| `backend/cmd/openapi-emit`, `cmd/wiregen` | codegen (admin `openapi.json`; catalog/wirecodes/notices/toolmap) |
| `backend/internal/server` | HTTP surface (`server_routes.go`, `openai.go`, `anthropic*.go`, `engine*.go`) |
| `backend/internal/pool` | multi-token front door (lease/acquire, routing, lifecycle, quarantine) |
| `backend/internal/session`, `runs` | per-token admission/poll/persist; per-agent run lifecycle |
| `backend/internal/upstream` | single-token wire client (transport/TLS/egress, chat envelope, session, ads, auth, `dump.go`, typed errors) |
| `backend/internal/convert` | pure OpenAI normalization + tool-name translation + OMP floor/reshape |
| `backend/internal/registry`, `modelcat`, `wirefacts` | model→agent mapping; generated picker mirror; pinned upstream snapshots + codegen contract |
| `backend/internal/store`, `config`, `dashboard` | SQLite (schema v5) + settings overlay; load/validate/catalog; view models + committed `dist/` |
| `frontend/` | Svelte 5 + Tailwind 4 SPA (`src/`, `e2e/`) |
| `scripts/` | upstream sync/drift, token helpers, backup/verify, WARP egress, probes |
| `.env.example`, `.env.full-example`, `.env.minimal` | knob docs (placeholders only — real keys never tracked) |
| `Dockerfile`, `docker-compose.yml`, `Caddyfile` | multi-stage build (`VERSION` ldflag); `db_data` volume + bind; optional TLS front |

Docs index:

| Doc | Subject |
|---|---|
| `docs/EGRESS.md` | Upstream country verdict + WARP workaround runbook (live) |
| `docs/OMP-TRANSLATION.md` | OMP/pi-family request/response translation, code paths + live proof |
| `docs/UNIVERSAL-CLIENTS.md` | Custom-provider recipes per client |
| `docs/FREE-TIER-GATE.md` | Proxy bug vs upstream refusal on the free tier (verified live) |
| `docs/TIERS.md` | Full vs limited accounts + per-tier proxy handling |
| `docs/UPSTREAM-CLI.md` | Upstream CLI reference notes |
| `docs/UPSTREAM-PORT-QUEUE.md` | Vendor-have-we-haven't port status + pin state |
| `docs/ANTI-BAN-DESIGN.md` | Anti-ban posture |
| `docs/REPO-MAP.md` | Full structure + request-flow guide |
| `docs/LIVE-CAPTURE.md`, `MITM-CAPTURE.md`, `CLI-WIRE-TRACE.md`, `ADS-TRACE.md`, `CHAT-TOOLS-PROBE.md` | Capture/trace/probe evidence |
| `docs/FREE-TIER-REMAKE-PLAN.md` | Free-tier remake plan |
| `docs/reverse-guide/` | Static-first reverse-engineering method (5 parts) |
| `docs/decisions/` | Publishable ADRs (index + house style in `docs/decisions/README.md`) |
| `DESIGN.md` | Visual grammar (dashboard) |
| `AGENTS.md` | Operator guide: commands, workflow, budgets, hygiene |

## Contributing

Protected `main`: branch → PR → green CI → squash merge, Conventional
Commits. Read `AGENTS.md` first — it is the operating contract (lanes,
dist freshness, drift serialization, public-repo hygiene). Never commit
secrets: real keys live only in untracked `.env`/shell env; pre-push run
`gitleaks git -v --redact .` + `gitleaks dir -v --redact .`.
