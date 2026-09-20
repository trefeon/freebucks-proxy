# Universal Clients — custom-provider recipes

Point any open-source agentic CLI/harness at freebuff-proxy as a custom
provider and it reaches the upstream service looking like the official
upstream CLI.
The proxy renames foreign tool names to the official signature equivalents
on the upstream wire and restores the client's own names on every response
path; parameters are forwarded untouched after structural normalization —
only names are rewritten.

## Global rules

- OpenAI-compatible `baseUrl` MUST include `/v1`;
  Anthropic MUST NOT (the SDK appends `/v1/messages` itself).
- Auth: OpenAI-shape providers send `Authorization: Bearer <proxy-key>`;
  Anthropic and opencode-go send `x-api-key: <proxy-key>`.
- Model field: any served model id.
- Never forward vendor env names upstream (`ANTHROPIC_BASE_URL`,
  `KIMI_CODE_BASE_URL`, …) — they are harness-side config only.
- `tools:[]` empty-tools sentinel: only with real tool history (a prior
  `tool_use`/`tool_calls` turn) AND zero active tools; never to
  Bedrock-routed paths.
- Responses API: preserve strict item order
  `message(s) → function_call(s) → function_call_output(s)` and inject
  `reasoning_text` per thinking turn (opencode-go strict gateway).

## Per-client recipes

| Client | Base URL | Auth | Model field | Notes |
|---|---|---|---|---|
| OMP/pi (OpenAI) | `http://HOST:3457/v1` | `Authorization: Bearer <proxy-key>` | any served id | compat baked at build; rerouting needs `registerProvider` |
| OMP/pi (Anthropic) | `http://HOST:3457` (no `/v1`) | `x-api-key: <proxy-key>` | same | SDK appends `/v1/messages` |
| claude-code | `ANTHROPIC_BASE_URL=http://HOST:3457` | `ANTHROPIC_API_KEY=<proxy-key>` | served id | strip `cc_*` markers (also done server-side) |
| codex | `config.toml model_provider + base_url=http://HOST:3457/v1`, `wire_api=responses` | `env_key` → proxy key | served id | Responses ordering preserved |
| opencode | `provider.<id>.options{baseURL:http://HOST:3457/v1,apiKey}` | `OPENCODE_API_KEY` or options | served id | keep item order + reasoning_text |
| gemini/crush/kimi/jcode/goose/hermes | `*_BASE_URL=http://HOST:3457/v1` (or provider JSON `base_url`) | corresponding key env → proxy key | served id | never send vendor env names upstream |
| openclaw/openhands/swe-agent | `baseUrl/base_url/api_base=http://HOST:3457/v1` | `apiKey/api_key` → proxy key | served id | `tools:[]` sentinel only with tool history |
