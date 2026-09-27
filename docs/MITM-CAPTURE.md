# MITM capture rig + live findings (Windows, zero-admin)

Reverse-proxy MITM (not a forward proxy — Bun ignores `HTTPS_PROXY`):
per-origin TLS listeners on localhost forward to the real upstreams with
full verification, logging flows with `Authorization` redacted.

## Rig (`devtools/mitm/`, gitignored lab-only, never pushed)

- `ca-cert.pem`/`ca-key.pem`: lab CA (30d). `leaf-cert.pem` (IP SAN
  127.0.0.1) + `leaf-key.pem`. (`*.pem`/`*.key` are gitignored globally.)
- `mitm.py`: listeners `127.0.0.1:8443→codebuff.com`,
  `127.0.0.1:8444→freebuff.com`; follows upstream redirects
  (case-insensitive `Location`); `flows.log` = JSON lines (redacted).
- Drive the CLI: `NEXT_PUBLIC_CODEBUFF_APP_URL=https://127.0.0.1:8443
  NEXT_PUBLIC_FREEBUFF_APP_URL=https://127.0.0.1:8444
  NODE_EXTRA_CA_CERTS=devtools/mitm/ca-cert.pem freebuff.exe`

## Findings (official CLI 0.1.0, live 2026-09-27)

1. **Bun ignores `HTTPS_PROXY`/`HTTP_PROXY`** (listener got zero hits;
   version printed in 1s with a dead proxy configured).
2. **Trust works per-process**: `NODE_EXTRA_CA_CERTS` alone is honored
   (wrong-cert still handshakes server-side then the client aborts —
   validation happens post-handshake, so always confirm with real bytes).
   No admin (cert store/hosts) needed.
3. **Session/me/healthz honor `NEXT_PUBLIC_CODEBUFF_APP_URL`** at runtime.
   **Login does NOT honor `NEXT_PUBLIC_FREEBUFF_APP_URL`** in this build
   (code POST went direct; binary bakes the name but the login route
   ignores the override) — login capture needs admin hosts-override.
4. **Live request shapes** (redacted, `flows.log`):
   - `GET /api/v1/me?fields=id%2Cemail` — exact fields `id,email`
     (proxy requested `id,email,discord_id`; aligned 2026-09-27,
     `tokenhealth.go:224`, `discord_id` parsing removed as dead weight).
     Headers: `Authorization: Bearer`, `Connection: keep-alive`,
     `User-Agent: Bun/1.3.14`, `Accept: */*`,
     `Accept-Encoding: gzip, deflate, br, zstd`.
   - `GET /api/healthz` on the session base, repeatedly (~2s cadence while
     the TUI sits at login-gate) — CLI health-probes upstream, 301→www
     followed fine.
   - Stored credential returns **401** on `/me` → TUI "API key invalid";
     TUI re-polls `/me`+`/healthz` in a loop while gated.
5. **Not capturable without login**: session admission, chat, polls, ads
   (TUI gates at login; stored token is dead). npm-registry update check
   is not env-overridable (constant host) — out of scope (boring shape).
6. Opsec: captures contain a live `Bearer` token — `mitm.py` redacts
   `Authorization` in `flows.log`; never paste raw flows anywhere.

## Reuse

Restart `mitm.py`, point env at it, drive the CLI. After a fresh login on
an unbanned account, a single chat attempt captures admission → chat →
polls → ads end-to-end (use a price-0 model; refusal costs nothing).
