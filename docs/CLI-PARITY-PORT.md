# CLI Parity Port — genuine-CLI MITM capture → freebuff-proxy gaps

Definitive genuine-vs-proxy wire diff from the live `freebuff` CLI 0.2.12 capture,
for multi-account pooling + perfect tool-call translation.

- Capture: `/tmp/fb-mitm-flows.log` on `vps-us` (READ via ssh, secrets redacted at
  write time by the MITM addon; long values truncated at ~500 chars — see
  [Truncation caveat](#truncation-caveat)).
- Proxy baseline: `origin/main` @ `83e63be2` (PR #78, full CLI spoof).
- Prior art: `docs/OMP-TRANSLATION.md` (translation contract),
  `docs/decisions/*` (claim-rotate-closed, unified-store, smart-probe).
- Verdict up front: **no P0 gaps remain on the hot path** (admission / switch /
  chat / device / catalog / polling all MATCH when the catalog is held). What is
  left is one wire-visible fallback divergence (cold catalog), two unshipped
  peripheral legs of unknown server weight (`agents/validate`,
  `project-profile`), one rotation asymmetry (error-form `purchase_claim_released`
  does not rotate), one unparsed receipt id, and one re-capture need (chat body
  beyond `model` is past the truncation limit). Biggest single item: **P1-1
  cold-catalog fallback** — the only shape in which the proxy still emits a
  recognizably non-CLI request.

## Truncation caveat

The MITM addon caps logged lines at ~606 chars (values past ~500 chars end in
`…`). Consequences used consistently below:

- Header *names* and *presence* are fully reliable; long header *values*
  (`x-freebuff-catalog-fetch`, `x-freebuff-model` handle, `x-freebuff-env` tail)
  are prefix-only.
- One-line JSON bodies past ~500 chars (admission 200 tail incl. `prices` tail
  and any purchase fields, chat bodies past `{"model":"fbm1.…`, catalog `rows`)
  are NOT visible. Rows marked `[TRUNC]` below are grounded proxy-side instead.
- Short bodies are complete: auth/status/me/streak responses, DELETE receipts,
  `session_superseded` / 500 errors, START/FINISH, validate/project-profile
  responses, impression bodies.

## 1. Capture manifest

Log: **6938 lines, 722 requests / 722 responses** (the assignment's "1426 lines"
predates the healthz-polluted tail — 550 of the 722 requests are 1–3s healthz
polls). Window **2026-10-02 16:34:30 → 16:49:30 UTC (~15 min)**, egress via warp
exit `104.28.192.157` (`api.ipify.org` confirms). CLI self-updated
`0.1.2 → 0.2.12` through the chain at the start (`Range: bytes=2422041-` resume).

### 1.1 Flow census (request → response)

| # | Method + endpoint | × | Resp | Notes |
|---|---|---|---|---|
| 1 | `GET /api/healthz` | 550 | 200 | Bun/1.3.14 poll loop, ~1.6 s cadence, `{"status":"ok","timestamp"}` |
| 2 | `GET /api/v1/freebuff/session` | 32 | 200 | 15 full + 17 compact polls (§4.4) |
| 3 | `POST /api/logs` | 28 | 200 | `cli.*`, `message_sent`, `ads.*`, AI-SDK stream errors |
| 4 | `POST /api/v1/chat/completions` | 20 | 12×200 / 6×409 / 2×500 | §4; 409 = `session_superseded`, 500 = `Failed to process request` |
| 5 | `POST /api/v1/agent-runs` | 16 | 200 | 8×START + 8×FINISH (`base3-free-catalog`) |
| 6 | `POST /api/v1/ads/impression` | 11 | 200 | `Freebuff-CLI/0.2.12` UA + `X-Freebuff-*` |
| 7 | `POST freebuff.com/api/ads` | 10 | 200 | gravity auction, `Freebuff-CLI/0.2.12` UA |
| 8 | `POST /api/agents/validate` | 8 | 200 | ~731 KB `agentDefinitions` per model switch |
| 9 | `POST /api/v1/freebuff/session/admission` | 6 | 200 | §2, one per attempt uuid |
| 10 | `GET project-profile?project_key=local:…` | 6 | 200 | `{"due":true,"profile":null}` per switch |
| 11 | `GET freebuff.com/api/auth/cli/status` | 11 | 10×401 + 1×200 | fingerprint login polling → success |
| 12 | `POST freebuff.com/api/auth/cli/code` | 3 | 200 | `fingerprintId` → `fingerprintHash` |
| 13 | `DELETE /api/v1/freebuff/session/attempt` | 4 | 200 | §3, receipt bodies |
| 14 | `GET /api/v1/me?fields=id,email` | 3 | 200 | `{"id":"60144c72-…","email":"<operator mailbox>"}` |
| 15 | `GET /api/v1/freebuff/streak` | 2 | 200 | `streak:0`, tz `America/Los_Angeles` |
| 16 | `GET /api/v1/freebuff/models` | 2 | 200 | catalog v`v0.g1.e82914.full.3`, rotating fetch token |
| 17 | `GET /api/v1/ads/policy` | 2 | 200 | `arm:reduced_spotlight`, rotation 180 s |
| 18 | `POST /api/v1/ads` | 2 | 200 | website-route auction leg |
| 19 | `POST /api/v1/freebuff/device-keys` | 1 | 200 | `{"publicKey":"U5Ts…","client":"cli"}` → `{"keyId":"bXS5…"}` |
| 20 | `GET …/releases/download/0.2.12/…` | 2 | 301 + 302 | self-update redirect chain |

Single account throughout: `60144c72-…` (id also stamped as `x-freebuff-acting-user-id` on every
chat/agent-run; mailbox redacted). `project_key=local:646cbb82cc32e1d17785c4fab7688780`.

### 1.2 Model chain (6 admissions, not 3)

The assignment's "deepseek → luna → GLM" compresses the real chain. The capture
holds **6 admissions over 6 fresh `cli:<uuid>` claims** (4 deliberate DELETE
switches + 1 refund-kill re-admit with no DELETE):

| # | Attempt uuid (suffix) | Admitted `model` | Handle prefix (`x-freebuff-model` / chat `model`) | Turns |
|---|---|---|---|---|
| 1 | `23b9d3e2-…` | `m-096e75164d` (deepseek) | `fbm1.AAEAAUPjWWgu…` | 2×200 SSE (`deepseek/deepseek-v4-flash`) |
| 2 | `f672f4a7-…` | `m-22ff70c71…` [TRUNC] | `fbm1.AAEAAUPjTrV0…` | 2×200 SSE (`stealt…` [TRUNC]) |
| 3 | `bb39704b-…` | `m-a273b5e51…` [TRUNC] | `fbm1.AAEAAUPjPuL_…` | 1×500 then 3×409 refund-kill |
| 4 | `2ffa3ee0-…` | `m-916b95b33…` [TRUNC] | `fbm1.AAEAAUPj_PGzZ…` | 1×500 then 3×409 refund-kill |
| 5 | `088a57a3-…` | `m-5a5d0e255e` (luna) | `fbm1.AAEAAUPjkvg0…` | 4×200 SSE |
| 6 | `36759745-…` | `m-7e20df6765` (GLM) | `fbm1.AAEAAUPjuff0…` | 2×200 SSE (log ends mid-session) |

Admission-1 `freebucks` snapshot: `balance:85 daily{limit:100,spent:15,
remaining:85,resetAt:…07:00:00Z,resetTimeZone:America/Los_Angeles}
wallet{balance:0,monthlyBonus:0} planId:null` + `prices{m-7e20df6765:25,
m-00032eaeec:10, m-cb71f819fe:30, m-096e75164d:15, m-0a6f9dd646:20,
m-5a5d0e255e:20, m-79033aedfe:20, m-9a7e098cc1:10, m-69307952f8:5,
m-22ff…[TRUNC]}`. Post-kill full polls show `status:none` with `balance:85`
(spent 15 on the deepseek turns). Middle models (`m-22ff…`, `m-a273…`,
`m-916b…`) are never named past their key prefix in the visible log — do NOT
quote display names for them.

### 1.3 Catalog rotation in-capture

Two distinct fetch tokens: `fbf1.AAF-hAZX…` (admissions 1–4, chats 1–14) →
`fbf1.AAFbku_…` (admissions 5–6, late chats). The `/models` 200s carry
`{protocol:1, version:"v0.g1.e82914.full.3", issuedAt:1790958870356,
refreshAt:1790960670356, rows:[{key…[TRUNC]}]}` early and
`issuedAt:1790959305584` late — a server-side catalog refresh the CLI picked up
without restarting. `x-freebuff-catalog-protocol: 1` on every session-scoped
request.

## 2. Admission table

`POST https://www.codebuff.com/api/v1/freebuff/session/admission`,
`Content-Length: 0`, **no body**, UA `Bun/1.3.14`. Proxy refs are
`backend/internal/upstream/session.go` (admission `CreateSessionForModelWithClaim`
:152–192, stamper `stampSessionCall` :780–795), `catalog.go` (`stampCatalogModel`
:464–478), `client_chat.go` (`newRequest` :31–110), `client_env.go` (:56–130).

| # | Header (genuine) | Genuine value | Proxy | Verdict + ref |
|---|---|---|---|---|
| 1 | `Authorization` | `Bearer` + token | `Bearer` (`client_chat.go:58`) | MATCH |
| 2 | `x-freebuff-model` | `fbm1.` **handle** (never raw id) | handle when catalog held, else raw id; omitted when `model==""` (`catalog.go:464-478`) | MATCH when held / **GAP P1-1** when cold |
| 3 | `x-freebuff-instance-id` | `cli:<fresh-uuid>` == attempt uuid | manager claim `cli:<uuid>` (`session.go:183-184`; mint `generateCliInstanceID` :160–166) | MATCH |
| 4 | `x-freebuff-desktop-attempt-id` | uuid suffix of #3 | uuid suffix (`session.go:185-188`); TierOffer models skip #3–#5 (`:156`) | MATCH |
| 5 | `x-freebuff-multi-session` | `1` | `1` (`session.go:185-188`) | MATCH |
| 6 | `x-freebuff-purchase-continuity` | `1` | `1` (`session.go:185-188`) | MATCH |
| 7 | `x-freebuff-catalog-protocol` | `1` | `1` iff catalog held (`session.go:788-791`) | MATCH / cold GAP (same P1-1) |
| 8 | `x-freebuff-catalog-fetch` | `fbf1.…` | held fetch id (`session.go:788-791`) | MATCH / cold GAP (same P1-1) |
| 9 | `x-freebuff-device-key` | held key id | held key id (`session.go:792-794`) | MATCH / cold GAP (same P1-1) |
| 10 | `x-freebuff-device-sig` | 86 ch base64url, fresh per request | Ed25519 over `freebuff-device-v1\nMETHOD\npath\ntsMs\nhex(sha256(exact body, empty for bodyless))\nfetchId` (`device.go:84-106,219-227`), `ts=time.Now().UnixMilli()` (`:241,245,291`) | MATCH |
| 11 | `x-freebuff-device-ts` | ms epoch (`1790958…`, monotonic) | sign-time ms | MATCH |
| 12 | `x-freebuff-env` | `v1;in=1;out=1;tp=none;term=1;ct=0;sz=199x74;ci=0;ssh=1;l=1;p=shell;g=sshd;osc=0;tzo=0;px=loopback;t…[TRUNC]` | `v1;in=0;out=0;…;px={none\|loopback\|remote};tls=1;ca=0` (`client_env.go:56-124`), stamped on every session call (`session.go:783`) | shape MATCH, values honestly differ → **P2-3** |
| 13 | `x-freebuff-first-tab-discount` | `0` | `0` (`session.go:69-77,782`) | MATCH |
| 14 | `x-freebuff-wallet-spend-limit` | `0` | `WALLET_SPEND_LIMIT`, default `"0"` (`session.go:56-58,182`; `client.go:451-455`) | MATCH |
| 15 | `x-fb-timezone` | (present on stamper path) | stamped (`session.go:59-68,781`) | MATCH |
| 16 | `Content-Type` | absent (no body) | absent — nil body → no `Content-Type` (`client_chat.go:65-70`; `session.go:146-147,174`) | MATCH |
| 17 | retry / takeover | — | admission is NEVER transport-retried, never sends a takeover header (`session.go:147-151,39-51`) | MATCH (deliberate divergence: none observed) |

Admission 200 body keys (all MATCH parsed or carried): `status, accessTier,
instanceId, model` (raw `m-…` key — server answers the handle with the key),
`admittedAt, expiresAt, remainingMs, freebucks{balance, daily{limit,spent,
remaining,resetAt,resetTimeZone}, wallet{balance,monthlyBonus}, planId,
prices{…}}`. Any purchase fields ride past the truncation limit — `[TRUNC]`,
see P1-4. Same-claim stale-handle retry (same claim, exactly once, never
rotates: `session.go:203-222`) mirrors the CLI holding its uuid across a
catalog refresh.

## 3. Switch lifecycle

Genuine sequence per deliberate switch (4 observed: §1.2 transitions 1→2, 2→3,
3→4, 5→6):

1. `DELETE /api/v1/freebuff/session/attempt`, `Content-Length: 0`, no body —
   headers = admission set **minus** `x-freebuff-model` / `x-freebuff-wallet-
   spend-limit`, with the OLD uuid in both `x-freebuff-instance-id`
   (`cli:<old>`) and `x-freebuff-desktop-attempt-id` (`<old>`).
2. `200 {"status":"ended","desktopAttemptId":"<old>","refundReceiptId":"<uuid>"}`
   (4 receipts observed: `f77b5a23-…`, `843e1d61-…`, `be8a9451-…`, `857424f3-…`).
3. `POST admission` with a NEW uuid in both fields + the new model handle
   (DELETE→re-admit gap is sub-second: e.g. DELETE ts `1790959108510` → admission
   ts `1790959108695`).

Proxy: **MATCH** on headers/path/no-body (`upstream/session.go:519-569`; `cli:`
ids use `/attempt`, else legacy `/session` :524–527; UA Bun; nil body, no
`Content-Type`). Triggers cover the CLI shape: model-switch
`releaseHeldSlotForTarget` (`session/session_admission.go:417-448,507`),
`model_locked` DELETE+rotate (:770–800), `EndSession` (:926–958), `Shutdown`
(:969–1029), pool token remove/drop (`pool/lifecycle.go:394-396,413-414,
460-463`). 404 tolerated (nil receipt :549–551); 400 expired/superseded on
DELETE swallowed. Claim rotation is single-use CLI-parity: mint once
(`session/session_claim.go:90-138`), rotate on DELETE success (reason model_lock
/ model switch) or dead-claim/status-form release, persisted via store
(`session_manager.go:36-45`).

Refund-kill re-admit (the mid-chat case, transitions 4→5): after the 3rd 409 on
attempt `2ffa3ee0` the CLI sent FINISH/failed, polled full-session
(`status:none`, balance 85), then ~3 min later `POST admission` with a FRESH
uuid (`088a57a3`) and **no preceding DELETE** (nothing to release — the server
already ended it). Proxy matches this shape: chat-path `session_superseded` is
terminal in-request (drop cached session, honest 409, no `Retry-After`:
`server/errors.go:297-311`; `engine_attempt.go:386-390`), the refund-worded
variant rejoins fresh exactly once even with auto-retry off
(`engine_attempt.go:75-86,166-169`), and poll/refresh superseded rotates the
claim (`session_poll.go:505-506`; `session_admission.go:699-702`).

Remaining switch-path gaps: receipt id not parsed (**P2-1**), error-form
`purchase_claim_released` records but does not rotate (**P1-2**).

## 4. Chat envelope table

`POST /api/v1/chat/completions` (20 observed). UA is the outlier leg:
`ai-sdk/openai-compatible/0.0.0-test/codebuff ai-sdk/provider-utils/3.0.25
runtime/browser` (the TUI shells to the AI SDK; session legs stay `Bun/1.3.14`,
ads legs stay `Freebuff-CLI/0.2.12` — proxy pins the same three-way split:
`upstream/chat.go:158`; `client.go:138-155`; `ads.go:246-249`).

| # | Header (genuine) | Genuine | Proxy (`upstream/chat.go:138-190`) | Verdict |
|---|---|---|---|---|
| 1 | `Authorization` | Bearer | Bearer | MATCH |
| 2 | `Content-Type` | `application/json` (body always present, 49–55 KB) | always set, body always present | MATCH |
| 3 | `User-Agent` | ai-sdk string above | identical literal (`chat.go:158`) | MATCH |
| 4 | `x-freebuff-acting-user-id` | `60144c72-…` (== `/v1/me` id) on all 20 | sent iff `ACTING_USER_ID` == token's own id (`chat.go:176-190`; `session.go:571-583`) | MATCH (conditional by design) |
| 5 | `x-freebuff-catalog-fetch` | `fbf1.…` (tracks rotation) | held fetch id (`chat.go:164-175`) | MATCH / cold GAP (P1-1) |
| 6–8 | device `key`/`sig`/`ts` | fresh sig per POST over exact bytes | trio over exact enveloped bytes iff held (`chat.go:164-175`) | MATCH / cold GAP (P1-1) |
| 9 | attempt/instance/model headers | **absent** (attempt is bound at admission, not per chat) | explicitly absent (`chat.go:159-163`); no `Accept` (`:150-154`) | MATCH |
| 10 | body `model` | `fbm1.` handle (prefix matches admission handle per attempt) | handle rewrite in catalog mode (`rewriteChatModel`, `chatHandleFor`, `chat.go:127-132,562-608`), raw-id fallback | MATCH / cold GAP (P1-1) |

Body beyond `model` is `[TRUNC]` (49–55 KB single-line JSON) — the proxy's
envelope is grounded from code, not from the capture: `codebuff_metadata` keys
`run_id, client_id` (per-run, never per-call), `trace_session_id?,
freebuff_instance_id (+freebuff_multi_session="1", surface="cli" when cli:),
llm_step_number?, n?, cache_debug_correlation?, freebuff_reasoning_effort?,
cost_mode? (+surface="cli" when free), repo_snapshot?`; `provider
{data_collection:deny, order?, allow_fallbacks?, only:[amazon-bedrock]}` for
anthropic; `stream:true` (`chat.go:610-748`). Re-capture need: **P1-4**.

### 4.1 Error pattern (the refund-kill signature)

- Attempts 3 and 4 die identically: first chat → `500 {"error":"Failed to
  process request"}`, then **byte-identical retries** (equal `Content-Length`:
  3×50684 on `bb39704b`, 3×50736 on `2ffa3ee0`) → `409
  {"error":"session_superseded","message":"This model purchase was
  refunded. Start a new session to try again."}` each time. CLI then
  FINISH/failed the run and moved on — it never mints a 4th retry, never
  DELETEs the dead attempt.
- Healthy turns: `200` SSE (`: connected <ts>` + `data: {"id", "object":
  "chat.completion.chunk", "created", "model", …}` chunks). SSE `id`/`model`
  shape differs by backend: `chatcmpl-<hex8>` / `deepseek/deepseek-v4-flash` on
  the deepseek turn vs `gen-<ts>-<alnum16>` / `stealt…[TRUNC]` later — the proxy
  passes chunks through (`SanitizeChunkMapped`, float64 invariant) and MUST NOT
  normalize these ids.

### 4.2 Per-turn peripheral legs (every switch)

1. `POST /agents/validate` (~731 KB `agentDefinitions`, first def
   `publisher:codebuff, model:deepseek/deepseek-v4-flash`) → `{"success":true,
   "configs":["base3-free-deepseek-flash","researcher-web","researcher-docs",
   "thinker-selector-opus",…[TRUNC]}` — proxy NEVER sends it (tools gate-pinned
   locally via `clitools.go`): **P2-2**.
2. `POST /agent-runs {"action":"START","agentId":"base3-free-catalog",
   "ancestorRunIds":[]}` → `{"runId":"…"}` — MATCH (`upstream/session.go:585-637`,
   never retried).
3. Chats… then `POST /agent-runs {"action":"FINISH","runId","status":
   completed|failed,"totalSteps":2|1,"directCre…[TRUNC]}` → `{"success":true}` —
   MATCH (`:675-721`, incl. `directCredits:0, totalCredits:0, steps[],
   errorMessage?≤5000 runes`). Completed runs report `totalSteps:2`, killed runs
   `totalSteps:1, status:failed` (8 START / 8 FINISH pairs observed).
4. `GET /api/v1/project-profile?project_key=local:…` → `{"due":true,
   "profile":null}` — proxy NEVER fetches it: **P2-2**.
5. Ads legs on `Freebuff-CLI/0.2.12` + `x-freebuff-env`: gravity auction `POST
   freebuff.com/api/ads {provider:…, messages:[…], sessionId, device:{os:linux…}}`,
   `/api/v1/ads` website leg, 11× `/impression` (`X-Freebuff-Event-Id`,
   `X-Freebuff-Render-Delay-Ms: 9–10`), `ads.first_party_view_ack` log records —
   MATCH in shape (`upstream/ads.go:161-249,295-299,345-347,460-511`;
   `ads_chat.go:52-66`); no click leg exists in the capture either, so the
   proxy's no-click is parity, and `X-Freebuff-Render-Delay-Ms` is honestly
   omitted (nothing rendered).

### 4.3 Device registration (once per boot)

`POST /api/v1/freebuff/device-keys {"publicKey":"U5Ts…(43ch
base64url)","client":"cli"}` → `{"keyId":"bXS5…"}` — MATCH
(`upstream/device.go:321-362`, consts :49–64). Proxy holder is in-memory
per-(token,host), restart mints fresh + re-registers, unknown-key errors forget
the keyId (keep keypair) → next call re-registers (`:364-383`); backoff 5 min
fail / 1 h unsupported-endpoint (`:76-81,280-284`).

### 4.4 Session polling (32 GETs: 15 full + 17 compact)

Every poll carries device trio + catalog pair + env + `first-tab-discount:0` +
`instance-id` + `multi-session:1` + `purchase-continuity:1` + `heartbeat:1`
(`upstream/session.go:252-275` `GetSessionWithOpts`). Full polls add
`x-freebuff-include-unused-rate-limits:1`; compact polls send
`x-freebuff-compact-session:1` instead — MATCH, including the alternation
(`session/session_poll.go:369-400` compact/full via `forceFullPoll`, plus
post-admission `PollUntilActive` 1 s × 30 s `upstream/session.go:277-351`).
One doc-vs-code tension: `session_poll.go:369-372` comment claims compact Poll
sends no heartbeat ("CLI never beats, Desktop-only") but `GetSessionWithOpts`
sets `heartbeat:1` for `cli:` ids — code wins and matches capture: **P2-4**
(comment fix).

## 5. Tool loop + translation contract

How one pool account fans out to any client (OMP / advisor / pi) perfectly. The
wire is always the 16 canonical CLI defs + `end_turn` (`decide` dropped by the
upstream topUp; `docs/OMP-TRANSLATION.md:69-76`; fixture
`backend/internal/convert/testdata/cli-tools.json`: `read_files, str_replace,
write_file, run_terminal_command, code_search, glob, list_directory, write_todos,
web_search, read_url, ask_user, suggest_followups, gravity_index, render_ui,
skill, report_project_profile`).

- **Detect** (`convert/tools_floor.go:49-215`): OMP iff any schema has `intent-i`
  OR single-hit `advise` (:198–205) OR 2+ `ompSignatureNames`; pi iff subset of
  `piToolVocabulary` or batch-edits fingerprint; OMP checked first; invalid body
  → `familyNone`. The wire `agents/validate` leg (§4.2-1) carries the same 16 —
  the proxy gate-pins them locally instead of echoing 731 KB per turn.
- **Request leg** (`OMP-TRANSLATION.md:22-26`): normalize → `ToUpstream` rename
  (`toolmap_request.go:458-544`, first-claim-wins dedupe) → substitute only the
  11 `substitutedWireNames` (`tools_normalize.go:56-120`) →
  `floorOnlyOMP` (:224–268, deep-copied canonical defs, preserves
  `end_turn`/`decide` pins) → rename `tool_choice` (:546–575). OMP renames
  (`toolmap_request.go:41-199`; `OMP-TRANSLATION.md:44-58`): `bash/powershell`→
  `run_terminal_command`, `read`→`read_files`, `edit`→`str_replace`,
  `write`→`write_file`, `grep`→`code_search`, `todo`→`write_todos`,
  `find`→`glob`, `edit-diff`→`apply_patch`; `wait/context_notes/new_context/
  eval/learn/manage_skill/task` never map. History calls renamed symmetrically
  (`RenameMessagesToolCalls` :577–617, `task`→`mcp__task`) and resumed
  dropped-tool calls fold into assistant text (`foldResumedHistoryCalls`,
  `tools_floor.go:271-416`, #75).
- **Response leg**: reshape BEFORE restore (`tools_reshape.go:369-459`):
  `read_files{paths}`→N `read{path}` (OMP drops offset/limit, pi keeps),
  `str_replace{replacements}`→N OMP `edit{…}` vs ONE pi batch
  `{path,edits:[{oldText,newText}]}`, `glob`→registered `glob{path}` never
  `find`; then `FromUpstreamChunk` restore (`toolmap_response.go`, map-first /
  `mcp__`-conditional).
- **Reminder** (`upstream/chat.go:464-518`, gated on `ChatOptions.OMPFloorOnly`
  `:617-618`): prose-only task `tasks[]`-nonempty + todo op-machine + ask ids +
  `eval/learn/manage_skill` + hub `wait` + advise shapes; sentinel `:464-466`;
  wire untouched, other families byte-identical.
- **Unroutable fallback** (`tools_textfallback.go`; doc S3a): wire riders with
  no OMP target (`suggest_followups/render_ui/gravity_index/
  report_project_profile`) ride the wire (gate needs all 16) but the response
  leg suppresses + renders as text (list / link / one-liner / absorb) and flips
  `finish_reason` `tool_calls`→`stop` when nothing is dispatchable.
  `familyNone` (no floor, defs verbatim) 503s on foreign riders by the upstream
  gate — pre-#73 advisor (`advise+read+grep+glob`, no `intent-i`) died exactly
  this way; now `advise` forces OMP and streams verbatim (S3b).
- Fan-out is per-family response reshape, NOT pool routing: OMP floor+reshape+
  fallback; advisor = OMP family via `advise`; pi = no floor, 7 core renames +
  pi-shaped args + `piFloorRenderers/piRoutable` (doc S6).

## 6. Multi-account pooling guide

Add a token via `POST /admin/tokens/add` — authed admin session (`fb_admin`) +
double-submit CSRF (`fb_csrf`/`X-CSRF-Token`;
`backend/internal/server/admin_auth.go:41-44,393-422`), handler
`backend/internal/server/admin_tokens_ops.go:96-180` (8 KB cap, form-or-JSON,
rejects Bearer-prefix/empty/comma-CR-LF, `adminSaveMu`-serialized, divergence
guard cfg-vs-pool, zero-cost `probeTokenGate` refuses banned/country/auth with
no pool mutation, overlay persist + rollback `RemoveLastToken`).
`pool.AddToken` (`pool/pool.go:999-1015`) → `buildTokenEntry` (:976–997) builds
the per-token triple (upstream client + `session.Manager` + `runs.RunManager` +
ledger + async info/streak fetch). Lazy per design — NOT initialized at add:
claim minted on first admission (`session_claim.go:90-138` via
`session_admission.go:225`), device key empty until catalog held
(`device.go:188-202`), catalog nil = fallback until `ensureModelCatalog`
(`catalog.go:358-407`, first admission waits ≤4 s then falls back), quota only
via probe/admission `UpdateQuotaFromProbe`.

Keying (isolation map — the thing that makes N accounts safe):

| State | Scope | Ref |
|---|---|---|
| catalog holder (fetch token + handles) | per-(token,host) global map, `sha256(token\x00baseURL)` | `catalog.go:230-233` |
| device keypair + keyId | per-(token,host), same hash; memory-only, re-register per boot | `device.go:14-19,191-193` |
| session claim `cli:<uuid>` + instance-id | per-token (one `Manager` per `tokenEntry`) | `session_manager.go:36-45`; `session_claim.go:21-24` |
| quarantine / cooldown / ban | per-token entry (`atomic.Pointer`), CAS-once; `walkGates` skips per lane | `pool/pool.go:498-508,699-706`; `acquire_route.go:256-388` |
| `released_models` fail-fast | per-(token,model), 5 min TTL, zero-POST short-circuit | `session_claim_released.go:41-116` |
| quota / tokenState memory | per-token; banned/country/auth/429 probes return BEFORE the write | `pool/probe.go:117-215` |

Model switch at runtime: the session layer does DELETE-old + rotate-fresh +
re-admit-new (CLI parity, §3); the pool's sticky `effectiveModel`
(`acquire_route.go:940-950`) only surfaces the held model when a switch
failed/locked, fail-loud via the served-model signal. `PIN_MODEL` is a static
fail-fast with no upstream traffic (`pool/pin.go:19-57`). Leased mint
(`runs/runs.go:370-400` `Acquire`/`Release`) keeps concurrent turns on one
token from FINISHing a live run.

What a fresh seat buys: the refund ledger is server-side (`desktopRefunds` with
`purchaseId/refundedAt`) — no rotation, header, or model switch admits without
a fresh purchase. A fresh seat (new official-client purchase on the account)
turns the next admission 409 into a 200 and clears the per-(token,model)
fail-fast at TTL expiry; everything else (handles, device, catalog, claim
plumbing) is already in place.

## 7. Ranked PORT LIST

### P0 — none open

Hot-path parity (admission / DELETE / re-admit / chat / device / catalog /
polling / START-FINISH) MATCHes the capture whenever the catalog is held. Do
not open new P0s from this doc without a fresh failing capture.

### P1 — wire-visible or behavior-visible

- **P1-1 Cold-catalog fallback is the only non-CLI-shaped request left.**
  `stampCatalogModel` (`catalog.go:464-478`) sends the raw model id with NO
  `catalog-protocol/fetch` and NO device trio when the holder is nil; same
  fallback on chat (`chat.go:164-175`). The capture NEVER shows this shape —
  the genuine CLI always holds a catalog (it blocks on `/models` at startup).
  Close it: (a) keep the ≤4 s first-admission join (`catalog.go:358-407`) but
  fail the turn LOUD instead of falling back when the join times out (no silent
  raw-id POST); (b) add a startup `/models` prefetch on token add / boot so the
  holder is warm before the first turn; (c) metric + dashboard signal for
  fallback sends. Tests: cold-holder admission asserts no request without trio;
  prefetch warms holder. Files: `upstream/catalog.go`, `session/
  session_admission.go`, `pool/pool.go` (add path), dashboard probe-status.
- **P1-2 Error-form `purchase_claim_released` records but never rotates.**
  Status-form rotates + retries once (`session_admission.go:712-726`);
  error-form (`{"error":"purchase_claim_released"}`) only writes fail-fast
  memory (`:525-533`; `session_claim_released.go:49-59`). Single-use CLI parity
  (claims retire on release) says both forms should retire the claim; rotation
  on the error form is safe because the body already proves the claim dead.
  Files: `session/session_admission.go:525-533`, test with the error-form body.
- **P1-3 Confirm chat-500 semantics.** Capture: first chat on a dying purchase
  → `500 Failed to process request`, CLI retries byte-identical → 409s. Verify
  the proxy surfaces a 500 as retryable-once-then-honest (never silent, never
  infinite) on the chat path, matching `client_chat.go` no-retry guards for
  session legs. Files: `upstream/client_chat.go`, `upstream/chat.go`,
  `server/errors.go`. Likely verify-only.
- **P1-4 Re-capture with a higher truncation limit.** Chat bodies past
  `{"model"`, catalog `rows`, admission-200 tails (prices tail + any purchase
  fields), full `x-freebuff-env` — all past the ~500-char cap. One MITM run
  with `--flow-detail 5`-style full logging (or a targeted `body:` dump for
  `/chat/completions` + `/models` + admission) closes every `[TRUNC]` in §§2–4
  and either promotes P2-2 to P1 or kills it.

### P2 — fidelity / hygiene

- **P2-1 Parse the DELETE receipt ids.** `200 {status:ended,
  desktopAttemptId, refundReceiptId}` — proxy parses `freebucksRefund` but has
  no `refundReceiptId`/`desktopAttemptId` struct fields
  (`upstream/session.go:109-118,564-568`; `session_parse.go:91-97,348-352`).
  Record the receipt id on the token ledger (refund accounting, dispute
  evidence). Small, well-scoped.
- **P2-2 `agents/validate` + `project-profile` unshipped (unknown server
  weight).** Zero backend hits for either route; proxy gate-pins tools locally
  (`clitools.go`) and never GETs the profile. Live turns succeed without them,
  but a strict server gate could key on validate presence (731 KB/turn is
  exactly the kind of admission-ticket shape a gate would check). Decide:
  implement cheap cached validate echo (refresh per catalog version, not per
  turn) + profile fetch, or record a capture-backed decision not to (this doc
  is the evidence). Needs P1-4 first for the full validate body schema.
- **P2-3 Env descriptor honestly differs.** Proxy sends zeros/unknowns
  (`client_env.go:56-124`); CLI sends real tty (`in=1;out=1;sz=199x74;
  p=shell;…`). No observed gating on it; keep honest zeros (NEVER spoof a fake
  tty) — item is document-only unless a gate appears.
- **P2-4 Fix the heartbeat comment.** `session/session_poll.go:369-372` claims
  compact Poll sends no heartbeat, but `GetSessionWithOpts`
  (`upstream/session.go:266-272`) sets `heartbeat:1` for `cli:` ids — and the
  capture shows `heartbeat:1` on all 32 polls. One-line comment fix.

### Already shipped (this capture confirms the design)

| PR | What | Capture confirmation |
|---|---|---|
| #77 | catalog handles + protocol/fetch + same-claim stale retry | every admission + chat carries handle + pair; rotation picked up without restart |
| #78 | device trio + chat handles + `wallet-spend-limit:0` + env descriptor | trio on all session legs + chats; `wallet-spend-limit:0`; env on session + ads legs |
| #75 | resume fold (`foldResumedHistoryCalls`) | N/A (no resume in capture; listed for completeness) |
| #73 | advise single-hit OMP detection | N/A (no advisor turn in capture) |
| leased mint | `Acquire`/`Release` | matches START/FINISH pairing discipline (8/8, no cross-FINISH) |
| fail-fast | per-(token,model) released memory | matches CLI abandoning dead attempts after 3×409 (no 4th retry) |
| #76 | dashboard sync | N/A |

## 8. Known non-causes — what no port can fix

1. **Refund ledger is server-side and final.** `session_superseded … purchase
   was refunded` + `desktopRefunds{ purchaseId, refundedAt }` is a money
   reversal, not a claim expiry. No rotation, header set, handle, device key,
   model switch, or re-capture admits without a fresh purchase. The mass
   pool-wide wipe stands until operators re-seat via the official client.
2. **Withdrawn models.** A model the vendor withdraws (e.g. spark) has no
   catalog row and no handle — perfect spoofing cannot conjure it.
3. **Trial / tier gates.** `accessTier`, `first-tab-discount`, `planId:null`,
   streak bonus (`freebucksDailyBonus:15`), `prices{…}` are server-computed per
   account; the proxy reports them, never mints them.
4. **500s on dying purchases.** `Failed to process request` is the server's
   leading edge of a refund kill (observed twice, both followed by 409s), not a
   proxy bug to chase with retries.
5. **`agents/validate` weight is bounded.** 8 validates accompany 6 admissions
   (per switch, not per turn) — even if P2-2 ships, it ships per-switch, never
   per-chat.
