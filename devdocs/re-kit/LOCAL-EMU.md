# LOCAL-EMU — local CLI lifecycle emulation (login → token → model → ping → delete)

`local-emu.sh` replays the freebuff CLI wire lifecycle against the live server
from Git Bash: device-code login (you complete it in the browser) → token saved
locally like `credentials.json` → model select → session admission → one chat
turn → automatic `DELETE`. Single session per run; the seat is always released.

Wire sources: `LOGIN-TUI.md`, `MODEL-SELECT.md`, `ACCOUNT-REQUEST.md`,
`SESSION.md`, `HEADERS.md`, `capture/01-auth.sh` + `02-session.sh` + `03-chat.sh`,
`scripts/gen-freebuff-token.sh` (auth_code → onboard rewrite, web/API split).

## Prerequisites

- Git Bash on Windows, `curl`, `jq`, `openssl` (script aborts if missing).
- A browser for the one-time login step (run with `MSYS_NO_PATHCONV=1` so the
  `auth_code` query string is not mangled by path conversion).
- No repo checkout state needed; nothing is written into the repo.

## Run (exact user steps)

1. Run the script:
   `MSYS_NO_PATHCONV=1 ./devdocs/re-kit/local-emu.sh`
2. Open the printed link (`https://freebuff.com/onboard?auth_code=...`) in your
   browser and complete the login. The script polls `GET .../api/auth/cli/status`
   every 5 s (401 = pending, silent) for up to 10 min, then saves the token.
3. Pick a model: numbered menu, `[1]` = recommended hero
   (`z-ai/glm-5.3-flash`). Type a number + Enter, or empty Enter for the default.
4. Watch the ping: the script prints the SSE chunk count and the first text
   bytes only (never the full stream).
5. Auto-delete: on exit the trap `DELETE`s the same instance id (404 tolerated).
   Re-runs reuse the stored token and skip step 2.

```text
./local-emu.sh            # LIVE (default): login -> pick -> admit -> ping -> DELETE
./local-emu.sh --dry-run  # print every curl + body shape, send nothing, store nothing
./local-emu.sh --logout   # delete C:/tmp/fb-local-emu/credentials.json and exit
```

Env overrides: `FREEBUFF_WEB_URL` (auth origin, default `https://freebuff.com`;
`FREEBUFF_BASE_URL` is a legacy alias), `FREEBUFF_API_URL` (session/chat origin,
default `https://www.codebuff.com`), `FREEBUFF_TZ` (override `x-fb-timezone`).

## Token file + permissions

- Location: `C:/tmp/fb-local-emu/credentials.json` (untracked, outside the repo).
- Shape: `{authToken, fingerprintId, fingerprintHash, obtainedAt}`.
- Permissions: directory `0700`, file `0600`, written atomically (tmp + rename).
- Reuse: a present file with a non-empty `.authToken` skips the browser login.
- Display: every print masks secrets as `first2...last2` (short values redacted
  whole). Tokens never appear in output, logs, or the repo.

## Model list source

Static default in the script (snapshot of `MODEL-SELECT.md` / upstream
`FREEBUFF_MODELS`): `z-ai/glm-5.3-flash` (hero = `DEFAULT_FREEBUFF_MODEL_ID`,
menu default), `deepseek/deepseek-v4-flash`, `mimo/mimo-v2.5`
(`FALLBACK_FREEBUFF_MODEL_ID`, always-joinable). Extend the `MODELS=` block from
`MODEL-SELECT.md` when the served set moves. Fingerprint is the isolated variant
(`enhanced-` + 32 random base64url bytes per login, never hardware).
Header/UA mirror: Bun `Bun/1.3.14` on auth + session legs, ai-sdk
`ai-sdk/openai-compatible/1.0.0/codebuff` on chat only, `x-fb-timezone` (tzutil
Windows→IANA map, else `UTC`), `x-freebuff-model` + wallet `0` + first-tab `0`
on admission (no body), `codebuff_metadata{run_id, client_id, trace_session_id,
freebuff_instance_id, llm_step_number:"1" (wire string), cost_mode:free}` on chat.

## Cleanup guarantees

- Exactly one session per run; the `EXIT` trap `DELETE`s the held instance id.
- `DELETE` 404/transport failure is tolerated (logged, exit code unaffected).
- `--dry-run` sends nothing and creates nothing (no token file, no seat).
- Temp files (status bodies, SSE dump) are removed on exit.

## Troubleshooting

- Link expired / stale page → re-run; each run mints a fresh one-time code.
- `401` while polling → normal pending state, keep the browser tab open.
- Login timeout (10 min) → re-run for a fresh code.
- `model_locked` / `model_unavailable` → re-run and pick another number.
- `rate_limited` / `spend_limited` / `ip_capped` → wait for the quota window.
- `consent_required` / `first_tab_discount_changed` → complete the step the
  response keys describe, then re-run.
- `country_blocked` / `banned` → terminal for that account.
- `bash -n` / LF: both files are LF, `bash -n local-emu.sh` clean, secret-sweep
  clean (only `<REDACTED>` / `<TOKEN …>` / `<FP>` placeholders).
