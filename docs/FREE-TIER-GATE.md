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

Base: `https://codebuff.com` (POSTs 307-redirect to `www` preserving `Authorization`; GETs 301-redirect to `www`).

1. **Reachability / Pre-flight**:
   - `GET /api/v1/freebuff/session` (Authoritative free-tier endpoint, returns status `none` or `active`, Freebucks balance, daily allowance, model prices, and `planRequiredModelIds`).
   - `GET /api/v1/freebuff/streak` (Returns daily streak, todayUsed flag, bonus credits).
   - `GET /api/v1/me?fields=id,email,banned,created_at` (User profile info; `id` is the acting user ID).
2. **Session Admission (Billed per hour)**:
   - `POST /api/v1/freebuff/session/admission` with **NO body** and headers:
     - `Authorization: Bearer <authToken>`
     - `User-Agent: Bun/1.3.14`
     - `x-freebuff-model: <model-id>`
     - `x-freebuff-wallet-spend-limit: 0`
     - `x-freebuff-instance-id: cli:<uuid>`
     - `x-freebuff-multi-session: 1`
     - `x-freebuff-purchase-continuity: 1`
     - `x-freebuff-desktop-attempt-id: <uuid-suffix>`
     - `x-fb-timezone: <IANA-timezone>` (e.g. `Asia/Jakarta`)
     - `x-freebuff-first-tab-discount: 0`
   - **Purchase Seat**: Charged once at admission (e.g. 5 Freebucks for GLM/Solar Mini, 15 for DeepSeek, 0 for Space Bunny).
   - **1-Hour Window**: Session remains active for 3600s (`remainingMs: 3600000`). Within this hour, re-admission using the same `holderInstanceId` costs **0 additional Freebucks**.
3. **Model Availability & Tiers**:
   - **Free models (Limited Tier)**: `z-ai/glm-5.3-flash` (5), `upstage/solar-mini4` (5), `crof/kimi-k3-eco` (5), `mimo/mimo-v2.5` (10), `upstage/solar-pro4` (10), `deepseek/deepseek-v4-flash` (15 peak / 10 off-peak), `stealth/space-bunny-alpha` (0).
   - **Paid-only models (`planRequiredModelIds`)**: `openai/gpt-6-luna`, `mimo/mimo-v2.6-pro`, `google/gemini-3.8-flash`, `meta/muse-spark-1.3-contributor`.
4. **Agent-runs START**:
   - `POST /api/v1/agent-runs` with body `{"action": "START", "agentId": "base3-free-<model-slug>", "ancestorRunIds": []}`
   - Header `x-freebuff-acting-user-id: <user.id>` + `Authorization: Bearer <authToken>`.
   - Returns `{"runId": "<uuid>"}`.
5. **Chat Completion**:
   - `POST /api/v1/chat/completions` with ai-sdk UA (`ai-sdk/openai-compatible/0.0.0-test/codebuff ai-sdk/provider-utils/3.0.25 runtime/browser`).
   - Must carry full CLI envelope per §4 below.
   - Streams delta reasoning (`reasoning_content`) and text content (`content`), or emits tool calls.
6. **Agent-runs FINISH**:
   - `POST /api/v1/agent-runs` with body `{"action": "FINISH", "runId": "<runId>", "status": "completed", "totalSteps": 1, "directCredits": 0, "totalCredits": 0, "steps": []}`.
7. **Session DELETE**:
   - `DELETE /api/v1/freebuff/session` with `x-freebuff-instance-id: cli:<uuid>`. Returns `{"status": "ended", "freebucksRefund": 0}`.
   - Note: Concurrency slot remains bound to the purchased model until the 1-hour expiry time (`expiresAt`).

## 4. Chat envelope gate (the 503 root cause)

Upstream returns **503 `The model is temporarily unavailable. Please try again later.`** when the chat body lacks the official CLI shape.

Passing shape requirements:

1. **Base3 system prompt**: Must include the canonical base3 coding agent instructions head from `agents/base3.ts` (the 6 standard convention bullets + dynamic date line). A stub 46-char marker alone **fails with 503**.
2. **Official tools array**: Must include canonical tool definitions with full schemas (`read_files`, `str_replace`, `write_file`, `run_terminal_command`, `code_search`, `glob`, `list_directory`, `write_todos`, `web_search`, `read_url`, `ask_user`, `suggest_followups`, `gravity_index`, `render_ui`, `skill`, `report_project_profile`). Minimal skeletons without schemas fail with 503.
3. **Codebuff metadata**: Must include:
   - `run_id`: matching `agent-runs` START `runId`
   - `trace_session_id`: UUID
   - `client_id`: 13-character base36 string
   - `freebuff_instance_id`: matching admitted `instanceId` (`cli:<uuid>`)
   - `freebuff_multi_session`: `"1"`
   - `surface`: `"cli"`
   - `cost_mode`: `"free"`
   - `llm_step_number`: String (e.g. `"1"`)
4. **Provider configuration**: `{"data_collection": "deny"}`.
5. **Streaming**: `stream: true`.

## 5. Concurrency: slotLimit 1 (slot-bound)

- In limited access tier, concurrency is strictly **`slotLimit: 1`**.
- When a model session is admitted, a 1-hour purchase seat (`desktopPurchases`) is locked to that model until `expiresAt`.
- Requesting admission for a *different* model during that hour returns **409 `purchase_capacity`** with the holder instance ID and expiry timestamp.
- Re-admitting the *same* purchased model with its `holderInstanceId` costs **0 Freebucks** and cleanly resumes the seat.

## 6. Proxy troubleshooting map

| Proxy symptom | Meaning | Action |
|---|---|---|
| 502 `upstream_unavailable` + `upstream auth rejected 401 Invalid API key` on admission | Proxy sent a stale/expired token | Run `scripts/device-login.py start|poll` or `scripts/gen-freebuff-token.sh` to get a fresh token. |
| 503 `waiting_room_queued` wrapping upstream 503 `model temporarily unavailable` | Envelope gate tripped (§4) | Check: system-prompt full base3 head, canonical tool schemas, metadata keys present. |
| 409 `purchase_capacity` | Slot held by another model purchase (§5) | Wait until `nextExpiryAt` or continue using the currently purchased model seat. |
| Upstream 428 `waiting_room_required` | Session expired or waiting room required | Call session admission to admit or re-admit the session. |


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
