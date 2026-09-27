# MITM capture rig + live findings (Windows; one UAC for the freebuff leg)

Reverse-proxy MITM (not a forward proxy — Bun ignores `HTTPS_PROXY`):
per-origin TLS listeners on localhost forward to the real upstreams with
full verification, logging flows with `Authorization` redacted.

## Rig (`devtools/mitm/`, gitignored lab-only, never pushed)

- `ca-cert.pem`/`ca-key.pem`: lab CA (30d). `leaf-cert.pem` (IP SAN
  127.0.0.1 + DNS SANs for localhost/codebuff/www/freebuff) +
  `leaf-key.pem`. (`*.pem`/`*.key` are gitignored globally.)
- `mitm.py`: listeners `127.0.0.1:8443→codebuff.com`,
  `127.0.0.1:8444→freebuff.com`; follows upstream redirects
  (case-insensitive `Location`); `flows.log` = JSON lines (redacted,
  bodies brotli-decoded for logging only — wire untouched, cap 300KB);
  `summarize.py` digests line ranges.
- Upstream resolution via DoH (1.1.1.1) with SNI-preserving dial, so a
  lab hosts-override can never loop the forwarder into itself; explicit
  `Host` header (http.client defaults to the dial IP → Cloudflare 403);
  upstream ALPN pinned `http/1.1` (h2 offer yields binary frames the
  parser chokes on).
- Codebuff leg (zero-admin): `NEXT_PUBLIC_CODEBUFF_APP_URL=
  https://127.0.0.1:8443` + `NODE_EXTRA_CA_CERTS=devtools/mitm/ca-cert.pem`.
- Freebuff leg (one UAC prompt, fully reverted after):
  `hosts-toggle.ps1 on` adds `127.0.0.1 freebuff.com` (+ backup) and
  `netsh portproxy 443->8444` (the login route ignores
  `NEXT_PUBLIC_FREEBUFF_APP_URL` and hits `:443`); `off` removes both.
  Browser login must happen OFF-PC (phone) while the override is on.

## Findings (official CLI 0.1.0, live 2026-09-27)

1. **Bun ignores `HTTPS_PROXY`/`HTTP_PROXY`** (listener got zero hits;
   version printed in 1s with a dead proxy configured).
2. **Trust works per-process**: `NODE_EXTRA_CA_CERTS` alone is honored
   (wrong-cert still handshakes server-side then the client aborts —
   validation happens post-handshake, so always confirm with real bytes).
   No admin (cert store/hosts) needed.
3. **Session/me/healthz honor `NEXT_PUBLIC_CODEBUFF_APP_URL`** at runtime.
   **Login ignores `NEXT_PUBLIC_FREEBUFF_APP_URL`** (binary reads the name
   but the route also needs `:443` — captured via hosts+portproxy instead).
4. **Live request shapes** — full chain in `docs/LIVE-CAPTURE.md`:
   validate (710KB agent defs) → agent-runs START → chat ×N (ai-sdk UA,
   `codebuff_metadata` session linkage, tool loop) → project-profile →
   FINISH; login code/status + session-start `/api/ads` (gravity,
   `cli_chat`, dock arm) on the freebuff leg.
5. **Update CDN 403s through the rig** (Range `freebuff-cli` UA
   download) — update outside MITM.
6. Opsec: captures contain a live `Bearer` token AND a login `authToken`
   in the status-200 body (`mitm.py` redacts only the `Authorization`
   header) — never paste raw flows anywhere; docs carry shapes only.

## Reuse

Codebuff leg: restart `mitm.py`, point env at it, drive the CLI.
Freebuff leg: `hosts-toggle.ps1 on` (UAC) → login/chat → `off`.
Full decoded chain lives in `docs/LIVE-CAPTURE.md`.
