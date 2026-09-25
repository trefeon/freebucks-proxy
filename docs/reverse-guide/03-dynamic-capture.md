# 03 — Dynamic capture (live traffic, last resort)

> **Static first.** Live capture only when the vendor clone
> (`upstream/freebuff`, gitignored) + pins cannot answer the question.
> Every live run: **dry-run first, single session, always-DELETE.**
> See [scope + evidence rules](01-scope-evidence.md).

## Hard rules (binding)

- **Dry-run default.** Every script prints curls and sends nothing
  unless `--send`/`--live` is passed with explicit per-use approval.
- **One seat per run.** Admission → one poll → one chat turn →
  trap-DELETE the SAME instance id. 404 on DELETE tolerated, never retried.
- **Never against a pool serving live traffic** (supersede/takeover risk).
  Dedicated off-prod test token only.
- **Token in env only** (`FREEBUFF_TOKEN`); logs show first2…last2.
  No captures, tokens, `.env`, hostnames, IPs committed — capture
  scripts live untracked under `devdocs/re-kit/capture/`, never in repo.
- Values never logged: `loginUrl` shape only, `fingerprintHash`,
  `expiresAt`, `instanceId` presence only (`jq has(...)`).

## Sequence (partitioned order)

| Step | Script | What |
|---|---|---|
| 1 | `capture/01-auth.sh` | Device-code login, no token. Dry-run prints curl; `--send` POSTs `{fingerprintId}`, polls `GET status` (401=pending, 5s/5min). Open `loginUrl` verbatim |
| 2 | `capture/02-session.sh` | Admission POST (no body) → one GET poll (instance + compact) → trap DELETE same instance |
| 3 | `capture/03-chat.sh` | One turn `POST /api/v1/chat/completions {model, stream:true}` + `codebuff_metadata{run_id, client_id, trace_session_id, freebuff_instance_id, llm_step_number, cost_mode}`. SSE verbatim; quota from NEXT session poll, never the stream |
| 4 | `capture/04-live-debug.sh` | PROBE default (unauthenticated: version/help/flag discovery, env KEY-NAMES only, `DEBUG=*` trials that exit w/o auth, SNI-only). `--live` = single admit→poll→DELETE. Bodies NEVER logged |
| 5 | `capture/05-live-session.sh` | Full run: passive monitoring (SNI-only or `ss`/`/proc` snapshots) → browser-login handover (token in shell var only) → one free-model admit→ping→DELETE |
| 6 | `capture/06-web-shapes.sh` | Read-only web: `COOKIE_JAR` env pointer, GETs only (pages/session/threads/usage/account). Scalars redacted to types via `jq walk` |

Static sources per script header: `devdocs/re-kit/ENDPOINTS.md`,
`HEADERS.md`, `SESSION.md`, `LOGIN-TUI.md`, `MODEL-SELECT.md`.

## Replay without live: local-emu.sh

`devdocs/re-kit/local-emu.sh` replays login→token→model→ping→DELETE;
see `devdocs/re-kit/LOCAL-EMU.md`. `--dry-run` sends nothing;
default LIVE still single-session + EXIT-trap DELETE. Token file
`C:/tmp/fb-local-emu/credentials.json` (0700/0600, atomic write).

## protocol-reverse lens on SSE/chat envelopes

Port of the zhaoxuya `protocol-reverse` pattern
(capture→frame-layout→serde→replay) to our SSE path:

1. **Capture**: 03 script relays SSE bytes verbatim; save nothing.
2. **Frame layout**: `data: {json}` lines; terminal `[DONE]`-style
   marker; per-line JSON shapes recorded as key→type, never values.
3. **Serde**: chat envelope keys fixed by static source
   (`sdk/src/impl/model-provider.ts`, `llm.ts`, `run-agent-step.ts`).
   Dynamic work only confirms field presence/order on wire.
4. **Replay**: local-emu ping replays the single turn; quota/balance
   assertions read the next session GET poll, never stream content.

## UA / fingerprint personas (wirefacts-sourced)

From `devdocs/re-kit/HEADERS.md` + `backend/internal/upstream/client.go:126-151`:

1. **Non-chat** (auth/session/agent-runs/streak/usage): bare-Bun default
   `Bun/<wirefacts.BunVersion>` (fallback `1.3.14`), no override.
2. **Chat only**: `ai-sdk/openai-compatible/<wirefacts.LlmProvidersVersion>/codebuff`
   (fallback `1.0.0`); no `Accept` header.
3. **Ads**: `Freebuff-CLI/<cli-version>`.

`VendorVersion` (npm wrapper, `scripts/vendor-version.txt`) is
informational — it MUST NEVER feed CLI UAs; wrapper ≠ SDK ≠ runtime.

## Sources

- `devdocs/re-kit/capture/` (01–06 + README), `devdocs/re-kit/LOCAL-EMU.md`
- `devdocs/re-kit/HEADERS.md`, `backend/internal/wirefacts/wirefacts.go`
- zhaoxuya `protocol-reverse/SKILL.md` (frame-layout/serde/replay pattern)
