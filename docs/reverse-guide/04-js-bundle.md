# 04 — JS bundle analysis (web chunks, read-only)

> **Zero mutations.** Web work is cookie-authenticated GETs + static
> chunk reads only. No chat ping, no POST/PUT/PATCH/DELETE ever invoked —
> mutation routes documented from static code (method + body key names).
> See [scope + evidence rules](01-scope-evidence.md), [static method](02-static-wire-method.md).

## What the vendor clone lacks

No `web/` dir in `upstream/freebuff` — everything about freebuff.com
is live observation, not source. Source of record:
`devdocs/re-kit/WEB.md` (cookie-observed shapes, types only, values redacted).

## js-reverse sign-chain, ported to web chunks

Port of the zhaoxuya `js-reverse` Observe→Capture→Rebuild→Patch→DeepDive
chain to freebuff web analysis (Patch stage FORBIDDEN here):

1. **Observe**: `GET /chat`, `GET /account` page shells (status/time/bytes,
   `self.__next_f` flight markers). No `__NEXT_DATA__`, no `/api/*` refs
   in HTML — routes live in page-unique chunks.
2. **Capture**: fetch `/_next/static/chunks/*.js` (34 chat / 38 account /
   26 shared, Turbopack-hashed). `06-web-shapes.sh pages` counts
   flight-markers + chunk-refs; that is the only inventory step.
3. **Rebuild**: route table from the 8 chat-only + 12 account-only chunks —
   method+path+body-key shapes into `WEB.md` §§3–5. Guessed RSC flight
   (`RSC: 1` + hand-built state) → 500; not pursued, chunks sufficed.
4. **Patch**: NEVER. No request mutation, no response tampering, no
   replayed POST. Shapes only.
5. **DeepDive**: web-vs-CLI table (`WEB.md` §7) — cookie vs Bearer auth,
   server-side threads + fetch-stream vs SSE completions, meter reads
   (`usage-summary`, `freebuff-session`) vs session poll.

## Read-only probe rules (`06-web-shapes.sh`)

- Jar via `COOKIE_JAR` env pointer (12-col or 7-col Netscape TSV);
  `Cookie` header built at runtime, values never printed/logged/stored.
- Subcommands: `pages session threads usage account shapes` — all GET.
- Output redacted: `jq walk` scalars→types; key→type trees only.
- Jar file untouched in place (size + mtime verified post-run), untracked.

## Vendor dist chunk reading (our own dashboard)

Same technique applies to `backend/internal/dashboard/dist`
(committed build output, never hand-edited):

- Read chunks to confirm what the served UI actually calls — bundle
  wins over `frontend/src` when they disagree (rebuild first).
- Grep for route strings (`/api/...`) in `dist/assets/*.js` to audit
  endpoint drift without running the UI.
- Never edit `dist` directly; fix in `frontend/src`, rebuild, commit.
## Key web-vs-CLI divergences (detail in WEB.md §7)

- Auth: `__Secure-next-auth.session-token` cookie + `GET /api/auth/session`
  (incl `stripe_customer_id`) vs per-request Bearer.
- Chat: `POST /api/chat/stream {threadId,content,model,reasoningEffort,...}`
  + fetch-reader framing vs `POST /api/v1/chat/completions` + SSE.
- Session: server-minted threads on first stream POST vs explicit
  admission/poll/DELETE lifecycle ([dynamic capture](03-dynamic-capture.md)).
- Ads: `POST /api/ads {adSequenceId,gravity_context,messages,sessionId,surface}`
  server-rendered path vs CLI ads legs (verdict: mirror waiting-room chain
  only — `devdocs/re-kit/ADS-VERDICT.md`).

## Sources

- `devdocs/re-kit/WEB.md`, `devdocs/re-kit/capture/06-web-shapes.sh`
- zhaoxuya `js-reverse/SKILL.md` (Observe→Capture→Rebuild→Patch→DeepDive)
