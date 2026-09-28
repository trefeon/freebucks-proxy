# Free-Tier Gate (verified live 2026-09-28/29)

How to tell a **proxy bug** apart from an **upstream refusal** on the free
tier. All findings below were verified live against the real freebuff.com /
codebuff.com legs; shapes only, no live tokens or ids.

> Companion probe: `scripts/free-tier-gate-probe.py` (stdlib only). Dry-run by
> default (stops before admission); `--spend` runs the billing leg. Token via
> `FB_AUTH_TOKEN` env or `--token-file` (a path **outside** the repo).
> Mint tokens with `scripts/device-login.py start|poll`; smoke the proxy pool
> with `scripts/live-pool-smoke.py`.

## 1. `/api/v1/me` is paid-only — never a free-tier health gate

- `GET /api/v1/me?fields=id,email` with a healthy free CLI `authToken`
  returns **401 `Invalid API key or user not found`**.
- The **same token** gets **200** on `GET /api/v1/freebuff/streak` and on
  `GET /api/v1/freebuff/session` (body `{status: none|active..., ...}` plus
  Freebucks balance/prices).
- Consequence: a `CheckTokenHealth`-style `401-on-/me => INVALID`
  classification **mislabels healthy free accounts**. `/me` is the paid-tier
  account check, not a free-tier reachability signal.
- The authoritative free-tier reachability probe is
  `GET /api/v1/freebuff/session` (200 with session status + balance).
- Code: `backend/internal/upstream/tokenhealth.go` (`CheckTokenHealth`,
  `probeMe`).

## 2. Device-code login chain (freebuff.com leg)

Base: `https://freebuff.com`. Unauthenticated UA:
`ai-sdk/openai-compatible/1.0.0/codebuff`.

1. `POST /api/auth/cli/code` with `{fingerprintId: "enhanced-<b64url>"}` =>
   `{loginUrl, fingerprintHash, expiresAt, expiresInMs}`. `expiresInMs` is
   `3600000` (1h); `expiresAt` is epoch-ms; `fingerprintHash` is hex64.
2. The CLI opens `loginUrl` **verbatim**
   (`https://freebuff.com/login?auth_code=...`). Never rewrite it to
   `/onboard` — the onboard rewrite breaks device binding (fixed in
   `scripts/gen-freebuff-token.sh`).
3. Poll `GET /api/auth/cli/status?fingerprintId&fingerprintHash&expiresAt`
   (same UA): **401 while pending**, **200** on approval with
   `{user: {id, name, email, authToken, fingerprintId, fingerprintHash}}`.
4. `user.id` equals the `x-freebuff-acting-user-id` value used on the
   codebuff.com leg.
5. Credential file: `<ConfigDir>/credentials.json`, profile `default`, field
   `authToken`. Code: `backend/internal/clicreds/clicreds.go`;
   scripts: `scripts/device-login.py start|poll`.

## 3. Per-turn free chain (codebuff.com leg)

Base: `https://codebuff.com` (307 on POST re-POSTs to `www` preserving
`Authorization`; 301 on GET goes to `www`).

1. Streak `GET /api/v1/freebuff/streak` (optional, 200 for free tokens).
2. Ads `POST /api/v1/ads` body `waiting_room`, UA `Freebuff-CLI/<ver>`.
3. Session admission `POST /api/v1/freebuff/session/admission` with **no
   body** and headers: `Authorization: Bearer <authToken>`,
   `x-freebuff-model`, `x-freebuff-wallet-spend-limit: 0`,
   `x-freebuff-instance-id: cli:<uuid>`, `x-freebuff-multi-session: 1`,
   `x-freebuff-purchase-continuity: 1`,
   `x-freebuff-desktop-attempt-id: <uuid-suffix>`, `x-fb-timezone`,
   `x-freebuff-first-tab-discount: 0`. Admission **bills the hour** (prices
   e.g. `z-ai/glm-5.3-flash` 5, `deepseek/deepseek-v4-flash` 15; balance
   `25->20` observed). Code: `backend/internal/upstream/session.go`.
4. Agent-runs `START`: `POST` `{action: START, agentId: base3-free-<slug>,
   ancestorRunIds: []}` + `x-freebuff-acting-user-id` => `{runId}`.
5. Chat `POST /api/v1/chat/completions` (ai-sdk UA
   `ai-sdk/openai-compatible/0.0.0-test/codebuff
   ai-sdk/provider-utils/3.0.25 runtime/browser`, envelope per §4).
   Code: `backend/internal/upstream/chat.go` (`injectEnvelope`).
6. Agent-runs `FINISH`, then session `DELETE`: `/session/attempt` for
   attempt ids else `/session` (same instance headers) =>
   `{status: ended, freebucksRefund}`.

## 4. Chat envelope gate (the 503 root cause)

Upstream returns **503 `The model is temporarily unavailable`** when the
chat body lacks the official CLI shape. Passing shape:

- **Base3 system prompt**: `agents/base3.ts` `createBase3CliRoot`
  (`You are Buffy, the coding agent behind Codebuff...` + 6 convention
  bullets). A short 46-char marker alone **fails**.
- **16 official tools**: `read_files`, `str_replace`, `write_file`,
  `run_terminal_command`, `code_search`, `glob`, `list_directory`,
  `write_todos`, `web_search`, `read_url`, `ask_user`,
  `suggest_followups`, `gravity_index`, `render_ui`, `skill` plus
  `report_project_profile` (per `flows.log` line 176).
- `tool_choice: auto`, `codebuff_metadata` with `{run_id,
  trace_session_id, client_id` (13-char base36)`, freebuff_instance_id
  (cli:<uuid>), freebuff_multi_session: 1, surface: cli, cost_mode: free,
  llm_step_number: String(n)`}` (+ optional `repo_snapshot` — proven NOT
  required 2026-09-29, see matrix),
  `provider: {data_collection: deny}`, `stream: true`, and the Bun UA on
  non-chat legs.
- Verified matrix: short-prompt + tools + snapshot = **503**;
  full-captured-176-body + fresh ids = **200** (math `25*4=100`,
  `15+15=30`, python `reverse_string`); standard-base3-prompt + tools +
  snapshot = **200**. Isolation 2026-09-29 (one variable at a time):
  full base3 head + 16 full-schema tools with NO snapshot = **200**
  (twice) — `repo_snapshot` is NOT load-bearing; prompt completeness +
  schema richness are. Correction of the earlier confounded read.

## 5. Concurrency: slotLimit 1 (slot-bound)

- A second admission while one session is active returns **409
  `purchase_capacity`** with `{currentInstanceId,
  desktopPurchases: [{model, expiresAt, holderInstanceId}],
  desktopRefunds, desktopSessionCounts: {premium: 1, nextExpiryAt}}`.
- You **must DELETE the holder** before admitting another model.
- Observed: GLM session `cli:<uuid>` held to `17:11:59Z`; a DeepSeek admit
  409'd against it; `DELETE` returned `{status: ended, freebucksRefund: 0}`.

## 6. Proxy troubleshooting map
| Proxy symptom | Meaning | Action |
|---|---|---|
| 502 `upstream_unavailable` + `upstream auth rejected 401 Invalid API key` on admission | Proxy sent a stale/expired token | Check `AUTH_TOKENS` / discovered `credentials.json`, **not** a gate bug |
| 503 `waiting_room_queued` wrapping upstream 503 `model temporarily unavailable` | Envelope gate tripped (§4) | Compare proxy chat body vs §4 shape: full base3 head (not stub), 16 fully-schematized tools (skeletons fail), `codebuff_metadata` keys (`repo_snapshot` NOT required) |
| 409 `purchase_capacity` | Slot held (§5) | Release the holder (`DELETE`) first |
| Upstream 428 `waiting_room_required` | Seat gone mid-chat | Re-admit |

For exact request shapes use `scripts/free-tier-gate-probe.py` modes (dry-run default; `--spend` for the admission leg) — no inline curl here.

## Probe modes (`scripts/free-tier-gate-probe.py`)

- `health` (default, free): session GET + streak GET. Use this to prove the
  account and route are reachable before blaming the proxy.
- `login` (free): device-code `start|poll` mirror. Use it when `health`
  401s to distinguish an expired token from a gate regression.
- `admit` (billed, requires `--spend`): admission + agent-runs START +
  minimal chat + FINISH + DELETE. Spends one hour of Freebucks credit;
  run once per model under test, then release the slot.
- Golden signals: `health` 200 + `admit` 200 + chat 200 with the §4 shape
  means the free tier is fine and the fault is proxy-side; `health` 401
  means the token is dead; `admit` 409 means the slot is held (§5).

## 7. Opsec + cost notes

- Every admission **bills the hour** from the daily Freebucks balance, so
  the probe **defaults to dry-run** (stops before admission); pass
  `--spend` explicitly for the billing leg.
- Never paste `Bearer`/`authToken` values, user uuids, or emails into logs,
  issues, or chat. Mask tokens (`****1234`) in debug output only.
- Capture bodies (e.g. `flows.log`) stay **redacted**.

## Links

- Capture method: [LIVE-CAPTURE](LIVE-CAPTURE.md),
  [MITM-CAPTURE](MITM-CAPTURE.md), [CLI-WIRE-TRACE](CLI-WIRE-TRACE.md)
- Upstream surface: [UPSTREAM-CLI](UPSTREAM-CLI.md),
  [UPSTREAM-PORT-QUEUE](UPSTREAM-PORT-QUEUE.md)
- Code: `backend/internal/clicreds/clicreds.go`,
  `backend/internal/upstream/chat.go` (`injectEnvelope`),
  `backend/internal/upstream/tokenhealth.go` (`probeMe`,
  `CheckTokenHealth`), `backend/internal/upstream/session.go`
- Scripts: `scripts/device-login.py`, `scripts/free-tier-gate-probe.py`
