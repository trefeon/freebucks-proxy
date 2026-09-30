# Universal Clients — custom-provider recipes

Point any open-source agentic CLI/harness at freebucks-proxy as a custom
provider and it reaches the upstream service looking like the official
upstream CLI.
The proxy renames foreign tool names to the official signature equivalents
on the upstream wire and restores the client's own names on every response
path; parameters are forwarded untouched after structural normalization —
only names are rewritten. This rename-only rule holds for NON-OMP clients.
OMP/pi-family requests are different: their tool defs are floor-only REPLACED
with the 16 canonical CLI defs + end_turn pin
(`backend/internal/convert/tools_floor.go`, hooked at
`convert_request.go:182-187`), and response args are reshaped to OMP shape
(`backend/internal/convert/tools_reshape.go`). Zero foreign riders reach the wire.

## Global rules

- OpenAI-compatible `baseUrl` MUST include `/v1`;
  Anthropic MUST NOT (the SDK appends `/v1/messages` itself).
- Auth: OpenAI-shape providers send `Authorization: Bearer <proxy-key>`;
  Anthropic and opencode-go send `x-api-key: <proxy-key>`.
- Model field: any served model id.
- Never forward vendor env names upstream (`ANTHROPIC_BASE_URL`,
  `KIMI_CODE_BASE_URL`, …) — they are harness-side config only.
- `tools`: non-OMP clients keep their defs with structural schema normalization (`backend/internal/convert/schemacache_store.go:normalizeToolSchemas`); missing `end_turn` sentinel is appended, never duplicated (`schemacache_endturn.go:injectEndTurnTool`). `decide` is stripped before egress (`backend/internal/upstream/clitools.go:28-30`). The gate keys on tool DEFINITIONS, not a foreign_toolset flag (live bisect 2026-09-30) — the foreign_toolset framing is superseded. OMP/pi-family defs are floor-only replaced (16 canonical CLI defs + end_turn pin). An empty `tools:[]` is the client's own choice except for OMP-family, where the floor forces the 16 defs. (A client that trims its tool list must still carry the matching tool history, or its turn has no tools to answer with.)
- Responses API: preserve strict item order
  `message(s) → function_call(s) → function_call_output(s)` and inject
  `reasoning_text` per thinking turn (opencode-go strict gateway).

## Per-client recipes

| Client | Base URL | Auth | Model field | Notes |
|---|---|---|---|---|
| OMP/pi (OpenAI) | `http://HOST:3457/v1` | `Authorization: Bearer <proxy-key>` | any served id | compat baked at build; rerouting needs `registerProvider` |
| OMP/pi (Anthropic) | `http://HOST:3457` (no `/v1`) | `x-api-key: <proxy-key>` | same | SDK appends `/v1/messages` |
| claude-code | `ANTHROPIC_BASE_URL=http://HOST:3457` | `ANTHROPIC_API_KEY=<proxy-key>` | served id | strip `cc_*` markers (also done server-side) |
| codex | `config.toml model_provider + base_url=http://HOST:3457/v1`, `wire_api=responses` | `env_key` → proxy key | served id | Responses ordering preserved; `wire_api="chat"` is a hard config error, not a fallback (`reference/agents/codex/WIRE-NOTES.md:17-19`) |
| opencode | `provider.<id>.options{baseURL:http://HOST:3457/v1,apiKey}` | `OPENCODE_API_KEY` or options | served id | keep item order + reasoning_text |
| crush/kimi/jcode/goose/hermes | `*_BASE_URL=http://HOST:3457/v1` (or provider JSON `base_url`) | corresponding key env → proxy key | served id | never send vendor env names upstream |
| openclaw/openhands/swe-agent | `baseUrl/base_url/api_base=http://HOST:3457/v1` | `apiKey/api_key` → proxy key | served id | keep tool history when trimming tools |

**gemini-cli is not usable this way.** Its gateway mode is
`GOOGLE_GEMINI_BASE_URL` (`reference/agents/gemini-cli/packages/core/src/core/contentGenerator.ts:87-89`;
`WIRE-NOTES.md:12,52-56`), which switches auth to `AuthType.GATEWAY` and then
sends `{base}/v1beta/models/{model}:streamGenerateContent?alt=sse` with an
`x-goog-api-key` header (empty under GATEWAY with no key). This gateway exposes
no `/v1beta` surface (`backend/internal/server/`, live routes are `/v1/chat/completions`,
`/v1/responses`, `/v1/models`, `/v1/messages` — `/v1/embeddings` answers 400
`unsupported_endpoint` by design), so that env var lands on a 404 — a
Google-API-shaped translation shim would be needed.
