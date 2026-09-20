import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { formatLocalDateTime, zoneLabel } from "./format.js";

// 19:00Z: still Sep 20 in UTC/PDT, already Sep 21 in UTC+7 — the viewer's
// zone is the one that decides which reset day they read.
const AT = "2026-09-20T19:00:00Z";

describe("formatLocalDateTime (instant + short zone)", () => {
  it("renders the instant in the named zone with its label", () => {
    assert.equal(
      formatLocalDateTime(AT, { timeZone: "UTC" }),
      "Sep 20, 07:00 PM (UTC)",
    );
  });

  it("keeps a Pacific reset on its own day with the PST/PDT label", () => {
    assert.equal(
      formatLocalDateTime(AT, {
        timeZone: "America/Los_Angeles",
        locale: "en-US",
      }),
      "Sep 20, 12:00 PM (PDT)",
    );
  });

  it("shifts the clock and the date into a UTC+7 zone", () => {
    assert.equal(
      formatLocalDateTime(AT, { timeZone: "Asia/Jakarta", locale: "id-ID" }),
      "Sep 21, 02:00 AM (WIB)",
    );
  });

  it("honors an explicit UTC offset instead of reading it as local", () => {
    assert.equal(
      formatLocalDateTime("2026-09-21T02:00:00+07:00", { timeZone: "UTC" }),
      "Sep 20, 07:00 PM (UTC)",
    );
  });

  it("defaults to the viewer's own zone and never echoes the raw stamp", () => {
    const out = formatLocalDateTime(AT);
    assert.match(out, /^Sep 2\d, \d{2}:\d{2} [AP]M \(.+\)$/);
    assert.doesNotMatch(out, /T19:00:00Z/);
  });

  it("empty input renders empty", () => {
    assert.equal(formatLocalDateTime(""), "");
    assert.equal(formatLocalDateTime(), "");
  });

  it("passes an unparseable value through verbatim", () => {
    assert.equal(formatLocalDateTime("not-a-time"), "not-a-time");
  });
});

describe("zoneLabel (viewer locale, vendor display strings)", () => {
  it("names an explicit zone in its own locale", () => {
    assert.equal(
      zoneLabel(AT, { timeZone: "America/Los_Angeles", locale: "en-US" }),
      "PDT",
    );
    assert.equal(
      zoneLabel(AT, { timeZone: "Asia/Jakarta", locale: "id-ID" }),
      "WIB",
    );
  });

  it("still labels the zone of a stamp the runtime cannot parse", () => {
    assert.equal(
      zoneLabel("15:04 Jan 2", { timeZone: "Asia/Jakarta", locale: "id-ID" }),
      "WIB",
    );
    assert.equal(zoneLabel(undefined, { timeZone: "UTC" }), "UTC");
  });

  it("reads the offset the named date actually carries (January → PST)", () => {
    assert.equal(
      zoneLabel("15:04 Jan 2", {
        timeZone: "America/Los_Angeles",
        locale: "en-US",
      }),
      "PST",
    );
  });
});
