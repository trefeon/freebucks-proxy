# 01 — Scope and evidence discipline

Ops discipline ported from reverse-skill to freebuff work: nothing runs
against live traffic until the scope gate passes, and no finding ships
without fixed evidence behind it.

## 1. Scope gate (adapted from `skills/ops/scope-contract.md`)

- **Authorized target only.** In-scope: the vendor clone
  (`upstream/freebuff`, gitignored), our own proxy traffic, our own
  accounts. Never third-party accounts, sessions, or traffic.
- **Network profile first.** Every live capture declares one of:
  `offline` (static clone + pins only), `lab_only` (local-emu replay),
  `authorized_target_only` (own single session, dry-run first).
- **Gate record.** Before any ACT, note three lines: target (upstream
  commit/version), auth (own token, never pasted), network profile.
  No record, no capture. Mirrors the `scope.md` hard gate — `-Force`
  never bypasses it.

## 2. Live-traffic discipline

- **Dry-run first.** Capture scripts under `devdocs/re-kit/capture/`
  default to dry-run; prove the shape offline before touching the wire.
- **Single session.** One session at a time; poll/backoff constants per
  `devdocs/re-kit/SESSION.md`.
- **Always DELETE.** Every admitted session is explicitly ended; see
  `devdocs/re-kit/ENDPOINTS.md` session-DELETE route. Never leave live
  sessions behind, never commit live artifacts.

## 3. Evidence → Finding → Path (adapted from `skills/ops/evidence-finding-path.md`)

- **Evidence** = one observation + artifact path + sha256. Raw rows, no
  conclusions. E.g. a wire snapshot row in
  `backend/internal/wirefacts/testdata/wire/snapshots.json`.
- **Finding** = conclusion drawn from evidence. Status ladder:
  `candidate` (single observation) → `validated` (≥2 independent
  observations, e.g. static clone read + local-emu replay agreeing).
- **Path** = what the finding unlocks (port row, drift classification,
  re-pin step). Findings without a path are trivia; drop them.
- **Sufficiency rule.** Only `validated` findings move pins or code.
  One-source claims stay `candidate` no matter how plausible.

## 4. Case-review checklist (adapted from `skills/case-review/SKILL.md`)

- **Fixity:** every pinned artifact carries sha256 + stable path. The
  wiregen gate enforces it: upstream-sha match at
  `backend/internal/wirefacts/wirefacts.go:68` and per-snapshot hash
  check at `backend/internal/wirefacts/wirefacts.go:77-78`.
- **Classify before refresh:** `scripts/repin-all.sh:103-107` aborts on
  any UNKNOWN-BASELINE row — review the drift, never force the pin.
- **Read-only audit:** re-verify hashes at review time
  (`--verify-hashes --strict` analogue: rerun the wirefacts + registry
  parity tests), fix the artifact path, not the hash.

## 5. Desensitized field-journal rule

- Journals record **commands and shapes, never secrets**: strip tokens,
  cookies, hostnames, IPs, user paths before writing. Follows the
  redaction rules in `devdocs/re-kit/capture/`.
- Reusable commands go in the journal as copy-paste precedent
  (cf. `skills/field-journal/precedent-reverse.md`); anything that only
  works with a live secret stays out of the repo.

## Sources

- reverse-skill ops contracts: `skills/ops/scope-contract.md`, `skills/ops/evidence-finding-path.md`, `skills/case-review/SKILL.md` — https://github.com/zhaoxuya520/reverse-skill
- Local pins + gates: `backend/internal/wirefacts/wirefacts.go:68-78`, `scripts/repin-all.sh:103-107`
- Capture discipline: `devdocs/re-kit/capture/`, `devdocs/re-kit/SESSION.md`, `devdocs/re-kit/ENDPOINTS.md`
