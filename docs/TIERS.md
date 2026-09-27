# Tiers — full vs limited accounts, and what the proxy spoofs per tier

Vendor tip `57943aa71`. Two different "tiers" exist: account tier
(`accessTier: full|limited`, decided server-side by egress IP) and model
tiers (served rows). This doc covers both + the proxy handling.

## 1. How the server decides your tier

Egress IP → country → tier. Full-access allowlist = Tier 1 (US) + Tier 2
(CA GB AU NZ IE NO SE DK FI NL AT LU IS) + Tier 3 (DE FR ES IT PT BE CH
LI MT KR) (`freebuff-countries.ts:19-62`). Everyone else, **and any VPN**,
is limited (`:12-14`). SG + IL left full access 2026-09-15 and are
uniformly limited since 2026-09-18, plan or no plan (`:37-42`) — any proxy
copy naming SG as Tier-1 is stale (fixed 2026-09-27 in `server/errors.go`,
`error_taxonomy.go`).

## 2. What full gets that limited doesn't

| | Full | Limited |
|---|---|---|
| Catalog | Everything served (`FREEBUFF_MODELS`) | Explicit allowlist only: GLM 5.3 Flash (hero), DeepSeek V4 Flash (coercion target), MiMo 2.5, Solar Mini 4 / Pro 4 where entitled (`freebuff-models.ts:3760-3773`) |
| Out-of-tier pick | Served if recognized | Coerced to DeepSeek V4 Flash, never refused (`:4337-3340`) — unless paused (coerced to fallback) or unknown (refused; the #1801 2.5x-admission retry loop) |
| Concurrency | Multi-tab 3 (free) / 8 (subscriber); slot-bound models 1 / 3 | Slot-bound 1, multi-tab **0** (`freebuff-desktop-sessions.ts:37-47`) — one seat, and EVERY model is slot-bound without a plan (`:61`) |
| Slot-bound models | GPT-6 Luna, Gemini 3.8 Flash, MiMo 2.6 Pro, Muse Spark (`:15-30`) | All of the above, plus everything else (limited-no-plan rule) |
| Premium pool | Base sessions/day + earned; extra session at full (`:1135-1138`) | 6 limited sessions, same 24h window (`:1103`) |
| Reward sessions | GLM 5.3 Flash is unmetered at full (no reward needed) | Reward buys GLM 5.3 Flash (`:3371-3380`); Fable 5.1 offer: 1 session/user, 500 global cap (`:3244-3247`) |
| Plan-only rows | Open (Gemini 3.8 Flash is every-surface Pro-only; Luna/MiMo-2.6-Pro open since 2026-09-25) | Plan-only: Luna + MiMo 2.6 Pro + all catalog Pro rows (`:3966-3971`); pickers draw them LOCKED |
| Hero/default | GLM 5.3 Flash default (unmetered since 2026-09-02) | GLM 5.3 Flash hero, DeepSeek V4 Flash coercion target (`:3722-3735`) |
| Ads | Same always-on posture (subscription removes NO ads on freebuff builds) | Same |
| Standing/signup | Same 9 signup-block reasons, same standing scale | Same |

## 3. What the proxy does per tier (spoof checklist)

- **Tier detection:** verbatim from admission/probe (`pool.go:157-162`
  `AccessTier`, `SubscriptionTierID` kept unparsed; `snapshot.go:237-241`).
  Pool-effective tier is optimistic-any (`server/models.go:371-389`: one
  `full` token opens full rows) — matches "any token satisfying a branch
  admits the row".
- **Catalog gating:** `modelcat` tiers (`TierFull/TierPaid/TierOffer`),
  `isModelAllowedForTier` (limited catalog), tier rows listed WITH reasons
  (`plan_required`/`offer_unavailable`/`trial_used`) so pickers render
  honestly; admission stays strict (`server/models.go:136-260`,
  `tier_gate_test.go`).
- **Limited annotation:** `/v1/models` stamps `current_access_tier`,
  demotes full rows to `region_limited`
  (`server_models_test.go:1401-1469`).
- **Identity switch:** TierOffer flips session identity mode
  (`server/engine_attempt.go:86-106`, `upstream/session.go:107`) — the only
  tier-driven wire change; everything else is tier-agnostic (same headers,
  same UA, same cadence on both tiers).
- **Slot discipline:** single-seat bound respected on limited (slot-bound 1);
  `superseded` terminal, never re-fight.
- **Fallback:** `model_unavailable` → cheapest served unmetered fallback for
  the token's live meter (`session/session_admission.go:659`); withdrawn ids
  stay recognized + coerced (never deleted — the #1801 lesson).

## 4. Spoof rules per tier (what to never get wrong)

1. Never name a web-only or plan-only row from a CLI envelope on a planless
   account — admission refuses planless starts regardless.
2. Never route the full-access default to the reward pool (GLM 5.3 Flash is
   unmetered at full; `freebuff-models.ts:4467-4471`).
3. On limited: one seat, one auction, no multi-tab behavior; coerce (don't
   refuse) out-of-tier picks to the tier default.
4. Keep paused ids resolvable (count tokens, resolve aliases) but never
   served; withdrawn copy names the upstream default (GLM 5.3 Flash).
5. Prices/off-peak: the wire `prices` map is the sole cost source —
   charge-once at session start; pool counters bucket on the Pacific day.
