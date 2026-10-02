import { describe, it } from "node:test";
import assert from "node:assert/strict";
import {
  banBadge,
  quarantineBadge,
  statusFor,
  resetTimeFor,
} from "./tokenStatus.js";

// Card predicates must read the pool's own quarantine/ban fields: the
// backend never emits a "quarantined" session_status (statuses stay
// wire-faithful), so keying on the status string leaves banned accounts
// looking idle.

describe("quarantine / ban chips", () => {
  it("ban_type hard claims priority with a critical pulsing chip", () => {
    const badge = banBadge({ ban_type: "hard" });
    assert.equal(badge.tone, "critical");
    assert.equal(badge.pulse, true);
  });

  it("quarantineBadge names the pool marker with its reason", () => {
    assert.equal(quarantineBadge({}), null, "healthy account has no chip");
    assert.equal(
      quarantineBadge({ quarantined: false }),
      null,
      "explicit false has no chip",
    );
    const badge = quarantineBadge({
      quarantined: true,
      quarantine_reason: "banned",
    });
    assert.equal(badge.tone, "bad");
    assert.match(badge.label, /quarantined/);
    assert.match(badge.label, /banned/);
  });

  it("statusFor orders ban, quarantine, lock, exhaustion, cooldown", () => {
    assert.equal(
      statusFor({ ban_type: "hard", quarantined: true, locked: true }).tone,
      "critical",
      "ban outranks quarantine",
    );
    const quar = statusFor({
      quarantined: true,
      quarantine_reason: "banned",
      locked: true,
      session_status: "active",
    });
    assert.match(quar.label, /quarantined/, "quarantine outranks lock/active");
    assert.equal(
      statusFor({ quarantined: true }).label.includes("idle"),
      false,
      "quarantined never renders idle",
    );
    assert.equal(statusFor({ locked: true }).label, "locked");
  });

  it("resetTimeFor prefers the server-truth daily anchor", () => {
    assert.equal(
      resetTimeFor({
        freebucks: { daily: { reset_at_utc: "2026-09-24T00:00:00Z" } },
        cooldown_until: "2026-09-25T00:00:00Z",
      }),
      "2026-09-24T00:00:00Z",
    );
    assert.equal(resetTimeFor({}), "", "no stamps, no reset");
  });
});
