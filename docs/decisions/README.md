# Architecture decisions (public subset)

Status: index. This directory carries the decisions that are safe to publish;
the numbered series cited from code comments (ADR-0016, ADR-0019, ADR-0022,
ADR-0027, …) lives in the project's private dev trail (`trefeon/freebucks-proxy-dev`),
so a code comment naming one of those numbers will not resolve to a file here.

| Document | Subject |
|---|---|
| `data-architecture.md` | DB vs env vs JSON vs log vs mem: where each datum lives, and the crash/update/backup law. |
| `smart-probe.md` | The quota prober: what shipped, why it is trigger-based rather than sweeping, and the knobs. |
| `tool-name-translation.md` | Client tool names on the wire: one mapper per request, ownership by the ordered pass, and universal wire-name legalization. |

House style for a decision record: a status line, non-goals, the rules or
eligibility conditions, the constants and knobs with their defaults, the files
it touches, and how it is verified. Citations are `path:line` into this repo (or
the gitignored vendor clone, named as such) — a decision record that cannot be
checked against the code is a bug in the record.
