# fb-re-kit -- capture scripts (untracked, outside repo)

Static kit for freebuff CLI **0.0.194** against repo pin **0.0.193**.
Gap is explicit: scripts encode the 0.0.193 wire (scout-verified file:line);
any 0.0.193 to 0.0.194 delta is UNVERIFIED until the vendor clone is re-pinned.

## Prerequisites

- A **dedicated test token** with no production seats. Token travels in
  `FREEBUFF_TOKEN` env only -- never argv, never file, never log.
- An **off-prod host/pool**: never run against the same account pool as a live
  gateway (supersede/takeover risk). One seat per run.
- Tools: `bash`, `curl`, `jq` (`openssl` optional for fingerprint minting).
- Default `FREEBUFF_API_URL=https://www.codebuff.com` is an UNVERIFIED
  placeholder -- override per run to the host under test.

## Partitioned order (single session, always cleaned up)

1. `capture/01-auth.sh` -- device-code login (no token). DRY-RUN prints curl;
   `--send` POSTs `{fingerprintId}` and polls `GET status` (401=pending,
   5s interval / 5min timeout defaults). Report `loginUrl` shape only
   (`auth_code` present, value never logged); open it verbatim in a browser.
2. `capture/02-session.sh` -- admission POST (no body) then one GET poll
   (instance + compact) then trap DELETEs the SAME instance (404 tolerated).
   Single session per run. Token redacted in logs (first 2 / last 2 chars).
3. `capture/03-chat.sh` -- one chat turn on the 02 seat:
   `POST /api/v1/chat/completions {model, stream:true}` with ai-sdk UA and
   `codebuff_metadata {run_id, client_id, trace_session_id,
   freebuff_instance_id, llm_step_number, cost_mode}`. SSE relays verbatim;
   quota/balance come from the NEXT session GET poll, never the stream.

Dry-run first, every time:

```sh
./capture/01-auth.sh
./capture/02-session.sh
./capture/03-chat.sh --model '<id>'
```

Live (each step needs explicit per-use approval + env):

```sh
export FREEBUFF_API_URL='https://<test-host>'
./capture/01-auth.sh --send
export FREEBUFF_TOKEN  # value lives in env only, never written here
./capture/02-session.sh --send --model '<id>'
export FREEBUFF_INSTANCE_ID='<instance>'
./capture/03-chat.sh --send  # needs FREEBUFF_TOKEN + FREEBUFF_MODEL
```

## Redaction + gitignore rules

- Token: env-only (`FREEBUFF_TOKEN`), redacted function in 02/03, `<REDACTED>`
  in every dry-run print. Secret grep must be clean (only the `<REDACTED>`
  placeholder near `Bearer `).
- Hosts/IPs/keys: no captures, tokens, `.env`, hostnames, IPs, or key names
  committed anywhere. This kit lives ONLY under `C:/tmp/fb-re-kit/` (untracked,
  outside the repo) -- never copy it into the repo.
- Login values: `loginUrl`, `fingerprintHash`, `expiresAt`, `instanceId`
  values are never logged -- shape/presence only (`jq has(...)` checks).
- Never commit captures: no `*.json` responses, no transcripts, no
  `/tmp/fb-*.json` contents. Suggested global ignore if reused:
  `*.json`, `.env`, `capture/*.log`.
