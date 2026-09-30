# Dead-claim rotation: `admission_attempt_closed` gets one fresh-claim retry

Status: shipped · 2026-09-30 · refresh rotates the persisted purchase claim and retries admission exactly once on the dead-start 409.

Non-goals: no change to CLI-parity claim stability (same claim on every other path), no new wire code, no cooldown or tokenhealth change, no G5 same-claim retry change.

## Context

- Failing admissions carried the persisted-claim attempt headers (`Desktop-Attempt-Id` + `Purchase-Continuity`) while the last 200 (GLM legacy path) did not: the claim on the wire referred to a purchase start upstream had already closed.
- Upstream answers that POST with 409 `admission_attempt_closed` (live 409 body carries `{"error":"admission_attempt_closed",...}` with no `status` field, so it classifies to `UpstreamError`, never a session state).
- Rotation previously happened only on DELETE/superseded paths (`session_admission.go` create comment, `session_claim.go` header): retrying the same dead claim 409s forever — the wedge survives restarts via the persisted claim row (`SaveClaim`/`LoadClaim`) and survives silence (no timer heals it).

## Decision

- Detect the dead start in one place: `isAdmissionAttemptClosed` (`backend/internal/session/session_claim.go`) matches the marker against the `UpstreamError` body. No wire-code constant: the marker is absent from the pinned snapshots, and the wiregen guard must keep failing loud on new snapshot literals.
- On that error, refresh rotates (`rotateClaim`, fresh `cli:` UUID per `newClaimID`, persisted through the existing store write so restarts adopt the fresh identity) and retries the admission exactly once, bounded by the `claimRetried` flag shared with the `purchase_claim_released` path (`backend/internal/session/session_admission.go` refresh error branch).
- A retry that still fails surfaces the error unchanged (falls through to the existing `noteAdmissionErr` + return).

## Why once-bounded rotation is safe

- At most one extra admission POST per user request, and only after this exact error: the flag is per `refresh` call, so the dead-start retry and the released-claim retry share a single rotation budget — never a loop, never a per-attempt mint.
- No purchase-identity churn on any other path: G5 same-claim retries are untouched (`admission_attempt_closed` is not `admissionRetryable`, so the same dead claim is never re-POSTed), and ended/none/quota paths still rejoin on the stable claim.
- No cooldown or failover interaction: the error is not a quota refusal, carries no token cooldown, and the pool sees either an adopted session or the honest 409.

## Files it touches

- `backend/internal/session/session_admission.go` (refresh error branch + G4 comment), `backend/internal/session/session_claim.go` (marker, matcher, header note), `backend/internal/session/session.go` (`reasonAttemptClosed`).
- `backend/internal/session/session_claim_rotate_closed_test.go` (wedge battery: rotate-and-adopt, still-409 surfaces, store handoff).
- `docs/decisions/claim-rotate-closed.md` (this note).

## Verification

- `TestAdmissionRotatesClaimOnAttemptClosed`: stale claim 409s, second POST carries a different `cli:` id with `desktop-attempt-id == uuid suffix`, 200 adopted — exactly 2 upstream hits.
- `TestAdmissionAttemptClosedRetryStill409Surfaces`: permanent 409 surfaces as the unchanged 409 `UpstreamError` after exactly 2 hits.
- `TestAttemptClosedRotationPersistsAcrossRestart`: `LoadClaim` answers the rotated claim and a rebuilt manager rejoins on it.
- `go test ./backend/internal/session/ ./backend/internal/pool/ ./backend/internal/server/` green; `gofmt`/`go vet` clean; no new internal imports (stdlib `strings` only — archtest matrix untouched).
