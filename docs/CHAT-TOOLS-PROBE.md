# Chat + tool-calling probe

End-to-end map of how a client tool call travels through the proxy to
upstream and back, how to exercise it with
`scripts/chat-tools-probe.py`, and what to fix when it breaks.

Status labels: **live** = verified against the real free tier on
2026-09-28/29 (shapes only, no tokens or ids below); **code** = derived
from the repo paths cited (verify before trusting after refactors).

Companion probes: `scripts/free-tier-gate-probe.py` (gate isolation:
direct-vs-proxy) and `scripts/live-pool-smoke.py` (pooled smoke with one
foreign-named tool). This probe goes one step further: it forces a real
tool round-trip through the proxy and checks the client name survives it.

## 1. Method map (client → upstream → client)

Each hop names the code that owns it.

1. **Client sends OpenAI tools.** `POST /v1/chat/completions` with
   `{model, messages, tools: [{type: function, function: {name, ...}}],
   tool_choice}` — any names, including foreign or dotted ones.
2. **Server normalizes (code).**
   `handleChat` in `backend/internal/server/openai.go` calls
   `convert.NormalizeRequestMappedOpts` (`backend/internal/convert/`,
   see `convert_request.go`). The ordered pass maps foreign names to
   official signatures, virtualizes collisions/duplicates to `mcp__*`,
   and legalizes grammar (`^[A-Za-z0-9_-]{1,64}$`). One request gets one
   `ToolMapper`; never rebuild it from the body on the way back.
   Rules and invariants: [tool-name-translation](decisions/tool-name-translation.md).
   OMP-family exception: OMP/pi-family tool defs are REPLACED floor-only
   (exactly the 16 canonical CLI defs + `end_turn` pin — zero `mcp__`
   riders on the wire, not virtualized); client surface still restores
   OMP names on the way back.
3. **Pool admission + run START (live + code).** `chatCore`
   (`backend/internal/server/engine.go`) acquires a session slot, then
   the upstream leg runs admission (`backend/internal/upstream/session.go`)
   and agent-runs `START`
   (`{action: START, agentId: base3-free-<model-slug>}`), per
   [LIVE-CAPTURE](LIVE-CAPTURE.md) §2 and [FREE-TIER-GATE](FREE-TIER-GATE.md) §3.
4. **Upstream chat with CLI envelope (live + code).**
   `POST /api/v1/chat/completions` with the ai-sdk UA and the envelope
   stamped by `injectEnvelope` (`backend/internal/upstream/chat.go`):
   16 official tools with FULL schemas (the load-bearing part — only
   tool definitions gate the 503, system content exonerated by the
   2026-09-30 live bisect: OMP-tools+neutral-system 503 vs
   official-tools+full-77KB-OMP-system 200) + `tool_choice:
   auto` + `codebuff_metadata` (run/client/trace ids, `llm_step_number`,
   `cost_mode: free`; `repo_snapshot` optional, proven NOT required
   2026-09-29) + `provider: {data_collection:
   deny}` + `stream: true`. Gate shape: [FREE-TIER-GATE](FREE-TIER-GATE.md) §4.
   Bare→`www` 307 preserves `Authorization`; chat UA is the ai-sdk
   string, every other leg sends `Bun/1.3.14`.
5. **Upstream SSE tool deltas (live).** `text/event-stream` chunks shaped
   `choices[].delta.tool_calls[]` with `{index, id, function: {name
   (official), arguments}}`, assembled across frames, then a terminal
   chunk with `finish_reason: tool_calls`. Multi-step turns replay the
   transcript: each step appends `assistant{tool_calls} + role:tool`
   pairs and bumps `llm_step_number` 1→2→3 — the 3-POST loop in
   [LIVE-CAPTURE](LIVE-CAPTURE.md) (`list_directory` → `read_files`).
6. **Proxy relay + restore + reshape (code).** Streaming (`relayStream`,
   `backend/internal/server/openai_stream.go`) and non-streaming
   (`relayJSON`) accumulate deltas, then `ToolMapper.FromUpstreamChunk`
   / `RestoreName` renames official/wire names back to the EXACT client
   names before writing — and for OMP-family turns the relay also
   reshapes CLI args to OMP shape (`backend/internal/convert/tools_reshape.go`,
   not names-only). Client sees
   `choices[].message.tool_calls[]` with CLIENT names and
   `finish_reason: tool_calls`, followed by the message replay. Same
   mapper threads through `/v1/messages` and `/v1/responses` surfaces.

## 2. Running `scripts/chat-tools-probe.py`

Stdlib only (`urllib/json/argparse/os/sys/time`), shebang +
executable bit. Proxy-only (direct upstream legs live in
`scripts/free-tier-gate-probe.py --mode direct`). Key ONLY from the
`FP_API_KEY` env (no flag); missing key prints usage on stderr and
exits `2`. `--help` works with no keys. Exits: `0` PASS, `1` FAIL,
`2` usage-or-key-gate. Secrets never printed (`****last4` in
`--verbose` only).

Flags: `--model <id>` (required), `--base` (default
`http://127.0.0.1:3457`), `--tool-set MODE...`: `classic` (default —
`bash`/`read`/`edit`/`todo` skeletons), `official` (16 official names
with generic object schemas), or `custom NAME...` (1+ bare names, e.g.
`--tool-set custom 'acme.weather?'` to exercise virtualization),
`--stream` / `--no-stream` (default: run BOTH tool legs; `--stream` =
stream leg only, `--no-stream` = non-stream leg only — health, key
gate, and plain-chat legs always run), `--force-tool NAME` (pin
`tool_choice` to one offered tool), `--timeout SEC`, `--verbose`.

```sh
# Full pass: health + key gate + plain chat + both tool legs
FP_API_KEY=<key> python3 scripts/chat-tools-probe.py --model deepseek/deepseek-v4-flash
# PASS lines: health: ok ..., gate bogus: 401, gate noauth: 401,
#   plain: 200 finish_reason=stop content_len=N usage=...,
#   tools: 200 finish_reason=tool_calls calls=[bash] usage=...,
#   tools-stream: 200 frames=N t_first_ms=N finish_reason=tool_calls terminal=[DONE],
#   matrix: bash=>bash RESTORED ..., VERDICT: PASS, exit 0

# Official 16-tool set, non-stream leg only, verbose:
FP_API_KEY=<key> python3 scripts/chat-tools-probe.py --model z-ai/glm-5.3-flash \
  --tool-set official --no-stream --verbose
# PASS shape: tools: 200 finish_reason=tool_calls, matrix 16/16 RESTORED

# Custom foreign names to exercise mcp__* virtualization + restore:
FP_API_KEY=<key> python3 scripts/chat-tools-probe.py --tool-set custom 'acme.weather?' 'demo.get-time'
# PASS shape: matrix acme.weather?=>acme.weather? RESTORED (wire was grammar-legalized)
```

Reading the report. Per leg: status, `finish_reason` (`tool_calls` =
the model emitted a call; `stop` with empty `tool_calls` = it declined,
see §3), `choices[0].message.tool_calls[]` names + args preview, usage
triple (`prompt`/`completion`/`total`), and the `tool_choice` sent.
Restore matrix, one line per tool: `offered=>called RESTORED`, or
`LEAK: official signature name leaked — see convert.ToolMapper`
(mapper bug, see §4 P1). Stream notes: first-frame latency
(`t_first_ms`), `delta.tool_calls` assembly by `index` (`id`/`name` /
`arguments` stitching across frames), terminal frame (`[DONE]`
expected; a `response.completed`-style ending is noted as an anomaly).


## 3. Known failure signatures

| Signal | Meaning | Action |
|---|---|---|
| 502 `auth-rejected` on admission | Proxy sent a stale/expired upstream token | Rotate `AUTH_TOKENS` / discovered credentials, not a gate bug |
| 503 `model-unavailable` on chat | Envelope gate tripped (tool definitions only — system content exonerated 2026-09-30) | Checklist ([FREE-TIER-GATE](FREE-TIER-GATE.md) §4): 16 tools with FULL schemas (skeletons fail), `codebuff_metadata` keys, `stream: true`, ai-sdk UA (`repo_snapshot` NOT required) |
| 409 `purchase_capacity` | Slot held (`slotLimit 1`) | `DELETE` the holder first; never force a second admission |
| 428 seat gone mid-chat | `waiting_room_required` | Re-admit, then retry the turn |
| restore-LEAK (official/`mcp__*` name reaches client) | Response mapper rebuilt or wrong surface | See P1: one mapper per request, threaded via `relayStats.toolMap` |
| Empty `tool_calls` + `stop` | Model declined the call | Prompt not forceful (`Call it now... no text reply`) or tool schema uninviting; not a relay bug |

## 4. Backend fix plan — LANDED (live proof 2026-09-30)

- **P0 — Envelope completeness on every tool turn — LANDED via PR #34 (virtualize) / #35 (substitute) / #36 (floor-only+reshape)** (`backend/internal/upstream/chat.go`, `injectEnvelope`; floor: `backend/internal/convert/tools_floor.go`, reshape: `backend/internal/convert/tools_reshape.go` — live proof 2026-09-30).
  OMP-family turns ride floor-only (16 canonical CLI defs + `end_turn`
  pin, zero `mcp__` riders) with FULL schemas on every tool turn, even
  when the client turn is tool-less or carries foreign tools.
  `repo_snapshot` deliberately NOT stamped (proven unnecessary 2026-09-29).
  Proof: `chat-tools-probe --model <each served model>` prints
  `chat: 200` instead of 503. Risk: each probe turn admits a session
  and bills Freebucks — keep probes `--spend-gated`, one turn per
  model, nightly only.
- **P1 — Convert round-trip audit on all surfaces — LANDED via PRs #34/#35/#36 (floor-only+reshape)** (`backend/internal/convert/` + relay files `openai_stream.go`, `anthropic_stream.go`, `responses_stream.go`, `engine.go` — live proof 2026-09-30).
  Request paths substitute OMP-family defs floor-only and relay paths
  reshape CLI args back to OMP shape; no rebuilt mappers, no
  virtualized `mcp__*` leaks on OMP turns (the #685 class). Acceptance: restore
  matrix `1/1 OK`, zero `LEAK` rows, on all three surfaces.
  Risk: none wire-side (read-only audit + probe turns only).
- **P2 — Gate-aware errors** (`backend/internal/server/errors.go`, `backend/internal/server/error_taxonomy.go`).
  Map an upstream 503-envelope refusal to proxy 503 plus a hint naming
  the missing envelope part (tools/metadata per the §4
  checklist) instead of a bare passthrough. Acceptance: forced 503 shows
  `hint: <missing-part>` in the probe report. Risk: error text must not
  leak tokens/ids — shapes only.
- **P3 — Probe-gated CI** (new nightly lane, record-only first).
  Wire `chat-tools-probe --model <matrix>` into a nightly job; store
  reports, alert on FAIL, no auto-merge gating until the signal is
  stable. Acceptance: first green nightly report committed as baseline.
  Risk: nightly Freebucks burn — cap the model matrix, reuse one slot
  per model, `--release` every session.

## Links

- Gate shape + modes: [FREE-TIER-GATE](FREE-TIER-GATE.md)
- Per-turn chain + 3-POST tool loop: [LIVE-CAPTURE](LIVE-CAPTURE.md)
- CLI wire shapes: [CLI-WIRE-TRACE](CLI-WIRE-TRACE.md), [UPSTREAM-CLI](UPSTREAM-CLI.md)
- Mapper rules: [tool-name-translation](decisions/tool-name-translation.md)
- Code: `backend/internal/server/openai.go`, `backend/internal/server/engine.go`, `backend/internal/convert/`, `backend/internal/upstream/chat.go`, `backend/internal/upstream/session.go`
- Scripts: `scripts/chat-tools-probe.py`, `scripts/free-tier-gate-probe.py`, `scripts/live-pool-smoke.py`
