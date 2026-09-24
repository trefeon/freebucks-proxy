# Pool-only removal: bridge/hybrid excision

Status: shipped · 2026-09-25 · pool-only excision of bridge/hybrid routing.

Non-goals: no change to pool scheduling, quota metering, or the untracked-bundle note; no new auth scheme.

## Context

- The gateway multiplexed many client keys onto a small upstream account pool; per-request passthrough tokens widened the wire ban-shape (one credential shape per account vs one per request).
- An instant-ban report against passthrough-style routing confirmed the risk.
- Owner order: remove bridge/hybrid entirely; serve the pool only.

## Decision

- Routing: pool-only. A request whose credential matches `API_KEYS` is served from the `AUTH_TOKENS` pool; anything else gets `401`. With no `AUTH_TOKENS` configured the gateway serves errors — configure pool tokens.
- Config: `BRIDGE_ENABLED` and related bridge knobs/APIs are deleted, not deprecated.
- UI/tests: bridge-mode UI, fixtures, and e2e coverage are deleted; remaining suites pin pooled-only behavior.

## Change surface

- Routing: `EffectiveMode`/bridge relay removed; non-pool credentials `401`.
- Config: bridge knobs removed from loader, catalog, dotenv keys, and fixtures.
- API: bridge token headers/endpoints removed.
- UI: bridge controls removed from the dashboard.
- Tests: bridge/hybrid cases removed; pooled-only cases (401 on unknown credential, errors on empty pool) retained.

## Consequences

- Old bridge clients (per-request upstream tokens) now get `401`; they must move to pool `API_KEYS` credentials.
- VPS operators re-save `API_KEYS` (and confirm `AUTH_TOKENS`) after deploy so the pool serves traffic.
- Ban-shape narrows to one credential shape per pooled account.

## Files it touches

- `README.md` (pooled-only statement), `.env.minimal` (pool-token comment).
- `docs/decisions/data-architecture.md` (bridge rows annotated superseded), `docs/decisions/locality-timezone.md` (bridge-clients line fixed).
- Code/UI/test excision lives on `fix/remove-bridge-mode` (pool/server/config + dashboard lanes).

## Verification

- `grep -ri 'bridge\|hybrid\|EffectiveMode' README.md .env.minimal docs/decisions/data-architecture.md docs/decisions/locality-timezone.md docs/decisions/pool-only-removal.md` shows only historical/superseded mentions.
- Full-suite verification is the coordinator's job after both lanes land.
