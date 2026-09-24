import { describe, it } from "node:test";
import assert from "node:assert/strict";
import {
  countryCodeOf,
  countryReasonOf,
  hasCountry,
  isNonUS,
  isCountryBlocked,
  countryBadgeFor,
  countryAdviceFor,
  fleetCountrySummary,
} from "./country.js";

describe("country utils", () => {
  it("normalizes codes and tolerates absent fields (old servers)", () => {
    assert.equal(countryCodeOf({}), "");
    assert.equal(countryCodeOf({ country_code: "us" }), "US");
    assert.equal(countryCodeOf({ country_code: " de " }), "DE");
    assert.equal(countryReasonOf({}), "");
    assert.equal(
      countryReasonOf({ country_block_reason: "anonymous_network" }),
      "anonymous_network",
    );
    assert.equal(hasCountry({}), false);
    assert.equal(hasCountry({ country_code: "US" }), true);
    assert.equal(isNonUS({}), false);
    assert.equal(isCountryBlocked({}), false);
  });

  it("badges US good, non-US warn, unknown null", () => {
    assert.equal(countryBadgeFor({}), null);
    assert.equal(countryBadgeFor({ country_code: "US" }).tone, "good");
    const de = countryBadgeFor({ country_code: "DE" });
    assert.equal(de.tone, "warn");
    assert.equal(de.label, "DE");
    assert.match(de.aria, /DE/);
  });

  it("routes reasons to the right layer", () => {
    assert.equal(countryAdviceFor("recent_limited_country").layer, "account");
    assert.match(
      countryAdviceFor("recent_limited_country").detail,
      /\/account\?tab=country/,
    );
    for (const r of [
      "anonymized_or_unknown_country",
      "anonymous_network",
      "missing_client_ip",
      "unresolved_client_ip",
      "ip_privacy_lookup_failed",
      "country_not_allowed",
      "something_new",
      "",
    ]) {
      assert.equal(countryAdviceFor(r).layer, "egress", r);
    }
    assert.match(countryAdviceFor("anonymous_network").detail, /[Ee]gress/);
  });

  it("summarizes the fleet in pool order", () => {
    const { nonUS, blocked } = fleetCountrySummary([
      { index: 0, country_code: "US" },
      { index: 1 },
      { index: 2, country_code: "DE" },
      {
        index: 3,
        country_code: "NL",
        country_block_reason: "recent_limited_country",
        cooldown_active: true,
      },
    ]);
    assert.deepEqual(nonUS, [
      { idx: 2, code: "DE" },
      { idx: 3, code: "NL" },
    ]);
    assert.equal(blocked.length, 1);
    assert.equal(blocked[0].idx, 3);
    assert.equal(blocked[0].advice.layer, "account");
    const empty = fleetCountrySummary([
      { index: 0, country_code: "US" },
      { index: 1 },
    ]);
    assert.deepEqual(empty, { nonUS: [], blocked: [] });
  });
});
