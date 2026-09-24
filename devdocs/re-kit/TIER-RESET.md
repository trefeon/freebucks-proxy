# TIER-RESET — limited-tier lock mechanics + release paths

Research-only. No mutations performed, no identifiers recorded, probes were
read-only GETs (shapes/enums only). Server evaluation order is UNVERIFIED
(`access-cache.ts` is server-only, absent from the public clone).

## Decision chain (fail-closed union; ANY negative input → limited)

1. IP resolvable — `missing`/`unresolved_client_ip`/`ip_privacy_lookup_failed`
   → limited (`freebuff-models.ts:3935-3937` fails CLOSED;
   `freebuff-session.ts:736-738`).
2. No privacy signals (`vpn`/`proxy`/`tor`/`relay`/`res_proxy`/`hosting`/
   `anonymous`/`service`, `freebuff-session.ts:740-748`) — "everywhere else,
   and any VPN, is limited access" (`freebuff-countries.ts:13-14`).
3. Resolved country in allowlist union Tier1{US} + Tier2(14) + Tier3(10)
   (`freebuff-countries.ts:19-62`); else `country_not_allowed` → limited.
4. Account floor: "request's own network passed, but the ACCOUNT was seen
   from a not-allowed country within the access-floor window, so it keeps
   that country's limits. Set by the floor in `access-cache.ts`, never by an
   IP lookup" (`freebuff-session.ts:727-733`) → `recent_limited_country`.
5. Country resolved server-side from the authenticated request, "never from
   anything a client sends" (`freebuff-models.ts:3930-3933`) — no header or
   body knob exists; `x-fb-timezone` is "never proof of country".

Subscription is NOT in this chain: it widens WHAT a limited account may pick
(`freebuff-models.ts:4210-4257`), never the tier. US-or-paid is a per-MODEL
exemption (GPT-6 Luna + MiMo 2.6 Pro, `freebuff-models.ts:3945-3948`).

## Same-host split verdicts

Same egress IP holds signals (1)–(3) constant, so one US/full + one limited
MUST come from per-account state: the `recent_limited_country` floor
(persists after moving) and/or `subscription.tierId` and/or verification
state (`GET /api/account/country`).

## Release table

| Lock | Release |
|---|---|
| `country_not_allowed` | Egress from an allowlist country on EVERY admission+poll+chat call; web re-verify after relocating (user browser) |
| `recent_limited_country` | Floor-window expiry ("for a while", exact duration UNVERIFIED) and/or verify at `freebuff.com/account?tab=country` (user browser) |
| `anonymous_network` / anonymized | Direct connection, drop VPN/proxy/Tor/relay/hosting egress |
| `missing` / `unresolved_client_ip` / `ip_privacy_lookup_failed` | Resolvable direct egress; transient cases retry later (partially UNVERIFIED) |
| `country_blocked` terminal | Proxy parks 15m only; server verdict stands |
| No live plan (`tierId` null) | Purchase widens MODELS at limited, does NOT flip tier |
| `verification` null | Human web verify flow (write method UNVERIFIED, never script it) |

## User steps (browser, nothing automated)

1. Log in at `https://freebuff.com`.
2. Open `https://freebuff.com/account?tab=country` (URL is vendor copy,
   `freebuff-model-availability.ts:102`).
3. Complete the verify control on that tab (in-page shape UNVERIFIED).
4. For network locks: direct residential/mobile egress, then a fresh session
   (new admission re-resolves).
5. Re-check `GET /api/web/freebuff-session` for `accessTier` +
   `countryBlockReason` change.

## Compare checklist (same machine/network, back-to-back, one account at a time)

1. `.accessTier` (full vs limited).
2. `.countryBlockReason` + whether `.countryCode` is UNKNOWN.
3. `.ipPrivacySignals` (empty vs non-empty).
4. `.subscription.tierId` (null vs present).
5. `GET /api/account/country` `{verification, active, nextVerifyAt}`.

If (2)+(3) match but (1) differs → per-account floor/verification is the
splitter (the "stuck" signature). If (3) differs on one host → egress paths
differ between the two sides.
