# freebuff reverse-engineering guide

How we reverse-engineer freebuff: static-first against our own traffic,
pins before code, every claim backed by fixed evidence.
[Scope + evidence discipline](01-scope-evidence.md) is required reading;
everything else is on-demand.

## Router: target × intent → read first

| Target | Intent | Read first |
|---|---|---|
| Upstream TS export / model catalog / notice string | Port a behavior change | [02](02-static-wire-method.md) |
| Wire snapshot / drift row / pin refresh | Re-pin vendor version | [02](02-static-wire-method.md), then [01](01-scope-evidence.md) §4 |
| Live session / chat SSE / ads chain | Confirm a wire hypothesis | [03](03-dynamic-capture.md) |
| freebuff.com web shape / static JS chunk | Trace web-only surface | [04](04-js-bundle.md) |
| Stripped symbol / struct layout / native fragment | Recover binary meaning | [05](05-binary-analysis.md) |
| Unknown starting point | Triage any target | [01](01-scope-evidence.md), then ask the loop below |

## The freebuff RE loop

1. **Vendor clone is truth.** Diff inside `upstream/freebuff`
   (gitignored); never reason from memory about upstream behavior.
2. **Pin verbatim.** Snapshots land in
   `backend/internal/wirefacts/testdata/wire/snapshots.json` (+ sha256
   manifest); registry pins in
   `backend/internal/registry/testdata/upstream/` (6 files).
3. **Codegen derives behavior.** `wiregen` emits `wirefacts_gen.go`,
   catalog, notices, toolmap from pins — hand-write nothing the
   generator owns.
4. **Classify before refresh.** `scripts/drift-exact.sh` labels each row
   SAME / COMMENT_ONLY / NOTICE_ONLY / FUNCTIONAL; UNKNOWN aborts the
   chain (`scripts/repin-all.sh:103-107`).
5. **Serial re-pin chain.** Wire PR → registry PR → dashboard PR →
   re-pin (`scripts/repin-all.sh`), one green merge before the next
   opens. Never batch layers into one PR.
6. **Live traffic is confirm-only.** Dry-run first, single session,
   always DELETE; see [01](01-scope-evidence.md) §2.
7. **Two observations to validate.** Static read + replay (or any two
   independent sources) before a finding moves pins; see
   [01](01-scope-evidence.md) §3.
8. **Journal the reusable shape.** Desensitized commands only, no
   secrets; see [01](01-scope-evidence.md) §5.

## File index

- [01-scope-evidence.md](01-scope-evidence.md) — scope gate, live-traffic discipline, Evidence→Finding→Path, review checklist, journal rule.
- [02-static-wire-method.md](02-static-wire-method.md) — pins, codegen, drift classification, serial re-pin chain.
- [03-dynamic-capture.md](03-dynamic-capture.md) — dry-run-first live capture, session lifecycle, confirm-only replay.
- [04-js-bundle.md](04-js-bundle.md) — web-shape tracing via static chunks, zero mutations.
- [05-binary-analysis.md](05-binary-analysis.md) — symbol/struct recovery and fragment emulation for native targets.

## Attribution

- [zhaoxuya520/reverse-skill](https://github.com/zhaoxuya520/reverse-skill) (MIT) — ops discipline: scope gate, Evidence→Finding→Path chain with ≥2-observation validated rule, case-review fixity (sha256 + artifact path), desensitized field journal. Ported in [01](01-scope-evidence.md); router-table idea shapes the table above.
- [P4nda0s/reverse-skills](https://github.com/P4nda0s/reverse-skills) (MIT) — binary techniques: magic-constant/paired-call symbol recovery, offset-aggregation struct rebuild, Frida/unicorn dynamic confirmation. Ported in [05](05-binary-analysis.md).
- Local method (`devdocs/re-kit/`, `scripts/`, `backend/internal/wirefacts/`) — the static-first wire loop in 8 steps above; our own work, not upstream's.

## Sources

- Local kit: `devdocs/re-kit/CLI-FLOW.md`, `scripts/repin-all.sh`, `backend/internal/wirefacts/wirefacts.go:68-78`
- Upstream repos: https://github.com/zhaoxuya520/reverse-skill, https://github.com/P4nda0s/reverse-skills
