# Live capture: official CLI chat turn (0.1.0-era, 2026-09-27)

Captured via `devtools/mitm` (reverse-proxy MITM, gitignored rig) against a
healthy account. IDs redacted; shapes verbatim.

## Per-turn chain on `codebuff.com` (all under `NEXT_PUBLIC_CODEBUFF_APP_URL`)

1. `POST /api/agents/validate` (~710KB `{"agentDefinitions":[...]}`; first
   def `id=base-chat, publisher=codebuff, model=deepseek/deepseek-v4-flash`)
   → `307` → `https://www.codebuff.com/api/agents/validate`
   → `200 {"success":true,"configs":[108],"validationErrors":[],"errorCount":0}`
2. `POST /api/v1/agent-runs`
   `{"action":"START","agentId":"base3-free-deepseek-flash","ancestorRunIds":[]}`
   + header `x-freebuff-acting-user-id: <user-uuid>`
   → `307` → `www` → `200 {"runId":"<uuid>"}`
3. `POST /api/v1/chat/completions` (~197KB) with the **ai-sdk UA**
   `ai-sdk/openai-compatible/0.0.0-test/codebuff ai-sdk/provider-utils/3.0.25 runtime/browser`
   (+ same `x-freebuff-acting-user-id`) → `307` → `www`
   → `200 text/event-stream` (standard `chatcmpl-*` chunks echoing
   `"model":"deepseek/deepseek-v4-flash"`)
4. `GET /api/v1/project-profile?project_key=local%3A<sha>`
   → `200 {"due":false,"profile":{"appKinds":[3],"technologies":[10]}}`
5. `POST /api/v1/agent-runs`
   `{"action":"FINISH","runId":"<same>","status":"completed","totalSteps":2,`
   `"directCredits":0,"totalCredits":0,"steps":[{id,stepNumber,credits,childRunIds,messageId,status,startTime}]}`
   → `307` → `www` → `200 {"success":true}`

## Request open (truncated prefix, full body pending one more turn)

```json
{"model":"deepseek/deepseek-v4-flash","codebuff_metadata":{
  "freebuff_instance_id":"cli:<uuid>",
  "freebuff_multi_session":"1","surface":"cli",
  "freebuff_reasoning_effort":"max",
  "trace_session_id":"<uuid>",
  "repo_snapshot":{"gitAvailable":true,"fileCount":914,"testFileCount":383,
    "commitCount":1235,"mergedPullRequestCount":427,
    "humanContributorCount":5,"botContributorCount":2, ...}}}
```

## Findings vs proxy model

- **No `freebuff/session` admission on this leg.** Session linkage rides

  inside `codebuff_metadata.freebuff_instance_id` (`cli:<uuid>`); run
  lifecycle is `agent-runs` START → chat → FINISH. (Freebucks metering —
  the 1h timer and balance — is enforced elsewhere; no `:8444` traffic was
  observed because the login route ignores the env override and the meter
  leg stays direct.)
- **Resume is free**: close + reopen replays `Session active, <N>m left`
  from local state — zero wire calls (only `/me` + `healthz` polls).
- **POSTs redirect with 307** (method-preserving; the client re-POSTs to
  `www`), GETs with 301. Any replay/proxy must follow both.
- **Chat UA is ai-sdk, not Bun**: everything else (`/me`, `/healthz`,
  `agent-runs`, `project-profile`) sends `Bun/1.3.14`; only
  `/api/v1/chat/completions` sends the ai-sdk UA. The proxy must match
  per-route, not globally. **Exact captured string** (note TWO segments +
  a `0.0.0-test` version placeholder in this build):
  `ai-sdk/openai-compatible/0.0.0-test/codebuff ai-sdk/provider-utils/3.0.25 runtime/browser`
- **Auth survives the 307**: every POST redirects bare → `www` and the
  follow-up re-POST is authenticated (upstream answers 200 + `runId`).
  A client that strips `Authorization` on cross-host redirect (Go's
  default, incl. bare→`www`) 401s against the real upstream — preserve
  credentials within the upstream registrable domain.
- `FINISH` reports `totalCredits: 0` on a free-tier turn; `totalSteps: 2`.
- `project-profile` `due: false` is per-project, fetched every turn.

## Second turn: new session, GLM model (same rig, 2026-09-27)

New session (`/model` → GLM, first message `hello`) repeats the chain with
model-specific values:

- START `agentId` is a per-model slug: `base3-free-glm-5-3-flash`
  (was `base3-free-deepseek-flash`). Pattern: `base3-free-<model-slug>`.
- Chat `model` carries the provider prefix: `z-ai/glm-5.3-flash`.
- `freebuff_instance_id` is **fresh per session** (`cli:<new-uuid>`):
  the instance id IS the session. Resume reuses it (zero wire calls);
  new session mints a new one.
- `codebuff_metadata` + `run_id` (= START `runId`), `llm_step_number: 1`,
  `client_id`, `cost_mode: free`. GLM turn **omits**
  `freebuff_reasoning_effort` (DeepSeek turn sent `"max"` — Tab-selected
  reasoning, per-model).
- `provider: {"data_collection":"deny"}` — the "May use data for AI
  training" opt-out, sent per chat request.
- Chat body top keys exactly:
  `model, codebuff_metadata, provider, messages, tools, tool_choice, stream`.
  `messages`: system (Buffy prompt, ~21KB) + user as parts array
  `[{"type":"text","text":"hello"}]`. `tools`: 16 `type:function` defs
  (`read_files`, …), `tool_choice: auto`, `stream: true`.
- System message carries `cache_control: {"type":"ephemeral"}` (0.1.2;
  0.1.0 turns showed bare `{role,content}`) — prompt-cache marker on the
  message object, string content, all models; ported 2026-09-28 on the
  OpenAI-family ingress only (Anthropic path stays marker-free).
- SSE stream is pure OpenAI shape: `chatcmpl-*` chunks each with
  `tool_calls` + `usage`, final `finish_reason: stop`, `data: [DONE]`.
  **Zero `ads`/`gravity`/`freebuck`/`auction` markers in-stream** — chat
  carries no ad payload; ads ride the waiting-room chain only.

## Third turn: tool-use loop (same GLM session)

Prompt `list the files in the frontend/src directory...` produced one
START + **3 chat POSTs** (197451 → 197917 → 198526B) + FINISH:

- `llm_step_number` 1 → 2 → 3 across the POSTs; `messages` 4 → 6 → 8.
  Each step appends the pair
  `assistant{content:"", reasoning_content, tool_calls:[{id, function:{name,arguments}}]}`
  + `tool{tool_call_id:<same-id>, content:"<JSON-string result>"}`.
  Observed calls: `list_directory {"path":"frontend/src"}` →
  `read_files {"paths":["frontend/src/main.js"]}`; result content is a
  JSON-encoded string (`{"files":["app.css","App.svelte","main.js"],...}`).

- Full transcript replayed every step (no compaction in this window);
  `run_id`, `freebuff_instance_id`, `trace_session_id` constant.
- FINISH: `totalSteps: 4`, `steps[3]`, `directCredits: 0`,
  `totalCredits: 0` — free-tier tool turns also report zero credits.

## Fourth turn: full login + session-start ads (hosts-override rig)

Capturing the `freebuff.com` leg needs more than env overrides (the login
route ignores `NEXT_PUBLIC_FREEBUFF_APP_URL` at runtime): lab recipe is
`hosts: 127.0.0.1 freebuff.com` + `netsh portproxy 443->8444` + leaf cert
with DNS SANs + DoH-based upstream resolution (the poisoned hosts file
would otherwise loop the forwarder into itself) + correct `Host` header
(`http.client` defaults to the dial IP; Cloudflare 403s that). Rig notes
in `docs/MITM-CAPTURE.md`; scripts in `devtools/mitm/` (gitignored).
Overrides removed after capture; values below redacted to shapes.

- `POST /api/auth/cli/code` `{"fingerprintId":"enhanced-<64B64url>"}`
  (Bun UA, no auth) → `200 {fingerprintId, fingerprintHash (hex64),
  loginUrl: "https://freebuff.com/login?auth_code=<24ch>",
  expiresAt: <epoch-ms str>, expiresInMs: 3600000}` (1h code lifetime).
- `GET /api/auth/cli/status?fingerprintId=…&fingerprintHash=…&expiresAt=…`
  polls ~every 5s → `401` while pending → `200 {user: {id, name, email,
  authToken, fingerprintId, fingerprintHash}, message:
  "Authentication successful!"}`. The `user.id` equals the
  `x-freebuff-acting-user-id` the codebuff leg sends.
- Session-start ads (right after login, BEFORE any chat):
  `POST /api/ads` with UA `Freebuff-CLI/<cli-version>` (NOT Bun):
  `{"provider":"gravity","messages":[],"sessionId":"<freebuff-session-uuid>",
  "device":{"os":"windows","timezone":"<IANA>","locale":"en-US"},
  "capabilityInspection":{"status":"unavailable",
  "reason":"windows_no_containment"},"surface":"cli_chat",
  "placementId":"Single-Ad-Unit-1",
  "userAgent":"Mozilla/5.0 (Windows NT 10.0; Win64; x64) ... Chrome/151.0.0.0 Safari/537.36",
  "cliDockArm":"control"}`
  → `200 {"ads":[],"provider":"gravity"}` (no fill on this session).
- The freebuff `sessionId` is a DIFFERENT uuid than the codebuff
  `freebuff_instance_id` (`cli:<uuid>`): two session identifiers, one per
  leg. No `freebuff/session` admission call exists on either leg — the
  codebuff side links via metadata, the freebuff side via the ads-call
  `sessionId` (client-minted at session start; absent from the login
  response).
