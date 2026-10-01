# Architecture decisions (public subset)

Status: index. This directory carries the decisions that are safe to publish;
the numbered series cited from code comments (ADR-0016, ADR-0019, ADR-0022,
ADR-0027, …) lives in the project's private dev trail (`trefeon/freebuff-proxy-dev`),
so a code comment naming one of those numbers will not resolve to a file here.

| Document | Subject |
|---|---|
| `data-architecture.md` | DB vs env vs JSON vs log vs mem: where each datum lives, and the crash/update/backup law. |
| `locality-timezone.md` | The zone the gateway declares on session calls (x-fb-timezone): the vendor facts, the boring-host rule, the country→zone table, and the privacy split between /healthz and the doctor. |
| `smart-probe.md` | The quota prober: what shipped, why it is trigger-based rather than sweeping, and the knobs. |
| `tool-name-translation.md` | Client tool names on the wire: one mapper per request, ownership by the ordered pass, and universal wire-name legalization. |
| `unified-store.md` | One runtime source per plane: mem-authoritative snapshot, DB-persisted, .env seed-only. |
| `pool-only-removal.md` | Bridge/hybrid excision: pool-only routing, bridge knobs deleted not deprecated. |
| `claim-rotate-closed.md` | Dead-claim wedge: 409 admission_attempt_closed rotates the persisted purchase claim with exactly one fresh-claim admission retry. |
| `ordered-account-placement.md` | POOL_ORDERED_PLACEMENT: prefer the lowest-index account that can serve a turn instead of letting a later warm lane take it; the Freebucks-seat tradeoff and why it is opt-in. |

House style for a decision record: a status line, non-goals, the rules or
eligibility conditions, the constants and knobs with their defaults, the files
it touches, and how it is verified. Citations are `path:line` into this repo (or
the gitignored vendor clone, named as such) — a decision record that cannot be
checked against the code is a bug in the record.
