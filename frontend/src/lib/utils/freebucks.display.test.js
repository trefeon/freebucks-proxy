import { describe, it } from "node:test";
import assert from "node:assert/strict";
import {
  formatFreebucks,
  firstTabListPriceFor,
  offPeakCopy,
  streakBonusNote,
} from "./freebucks.js";

const LUNA = "openai/gpt-5.6-luna";

describe("firstTabListPriceFor (struck-only-when-moved)", () => {
  it("returns the list price when the offer moved the row", () => {
    const fb = {
      prices: { [LUNA]: 5 },
      list_prices: { [LUNA]: 15 },
      first_tab_discount: { amount: 10, available: true },
    };
    assert.equal(firstTabListPriceFor(fb, LUNA), 15);
  });

  it("clamped-0-no-strike: a row already at 0 is not '0 off 0'", () => {
    const fb = {
      prices: { [LUNA]: 0 },
      list_prices: { [LUNA]: 0 },
      first_tab_discount: { amount: 3, available: true },
    };
    assert.equal(firstTabListPriceFor(fb, LUNA), undefined);
  });

  it("pre-listPrices-no-strike: a quote without listPrices never guesses", () => {
    const fb = {
      prices: { [LUNA]: 5 },
      first_tab_discount: { amount: 10, available: true },
    };
    assert.equal(firstTabListPriceFor(fb, LUNA), undefined);
  });

  it("no strike when the offer is in use by another session", () => {
    const fb = {
      prices: { [LUNA]: 15 },
      list_prices: { [LUNA]: 15 },
      first_tab_discount: { amount: 10, available: false },
    };
    assert.equal(firstTabListPriceFor(fb, LUNA), undefined);
  });

  it("no strike when the list price did not move the row", () => {
    const fb = {
      prices: { [LUNA]: 15 },
      list_prices: { [LUNA]: 15 },
      first_tab_discount: { amount: 10, available: true },
    };
    assert.equal(firstTabListPriceFor(fb, LUNA), undefined);
  });

  it("reads the camelCase wire keys too", () => {
    const fb = {
      prices: { [LUNA]: 5 },
      listPrices: { [LUNA]: 15 },
      firstTabDiscount: { amount: 10, available: true },
    };
    assert.equal(firstTabListPriceFor(fb, LUNA), 15);
  });
});

describe("offPeakCopy (active vs upcoming)", () => {
  // Window 00:00-08:00 UTC at price 5 against regular 15.
  const offer = {
    startHourUtc: 0,
    endHourUtc: 8,
    price: 5,
    regularPrice: 15,
  };
  const at = (iso) => Date.parse(iso);

  it("active: the resolved quote owns the price, detail names the regular", () => {
    const fb = {
      prices: { [LUNA]: 5 },
      off_peak: { [LUNA]: offer },
    };
    const copy = offPeakCopy(fb, LUNA, {
      now: at("2026-09-18T04:00:00Z"),
      timeZone: "UTC",
    });
    assert.equal(copy.active, true);
    assert.equal(copy.badge, "Off-peak");
    assert.match(copy.detail, /normally 15\/hr/);
  });

  it("upcoming: detail names the off-peak price and window", () => {
    const fb = {
      prices: { [LUNA]: 15 },
      off_peak: { [LUNA]: offer },
    };
    const copy = offPeakCopy(fb, LUNA, {
      now: at("2026-09-18T12:00:00Z"),
      timeZone: "UTC",
    });
    assert.equal(copy.active, false);
    assert.match(copy.detail, /Off-peak 5\/hr/);
  });

  it("undefined when the model carries no offer or no price", () => {
    assert.equal(
      offPeakCopy({ prices: { [LUNA]: 5 } }, LUNA, { now: 0 }),
      undefined,
    );
    assert.equal(
      offPeakCopy({ off_peak: { [LUNA]: offer } }, LUNA, { now: 0 }),
      undefined,
    );
  });
  it("snake_case dashboard keys: live quote shape renders, never throws", () => {
    // Live /admin/api/tokens ships start_hour_utc/end_hour_utc/regular_price
    // (2026-09-18: camelCase-only read threw RangeError mid-render, freezing
    // the Usage Accounts/Models tabs on "Loading…").
    const fb = {
      prices: { "deepseek/deepseek-v4-flash": 10 },
      off_peak: {
        "deepseek/deepseek-v4-flash": {
          start_hour_utc: 22,
          end_hour_utc: 6,
          price: 10,
          regular_price: 15,
        },
      },
    };
    const copy = offPeakCopy(fb, "deepseek/deepseek-v4-flash", {
      now: at("2026-09-18T23:00:00Z"),
      timeZone: "UTC",
    });
    assert.equal(copy.active, true);
    assert.match(copy.detail, /normally 15\/hr/);
  });

  it("missing window hours: undefined, never throws", () => {
    const fb = {
      prices: { [LUNA]: 5 },
      off_peak: { [LUNA]: { price: 5, regularPrice: 15 } },
    };
    assert.equal(
      offPeakCopy(fb, LUNA, { now: at("2026-09-18T04:00:00Z") }),
      undefined,
    );
  });
});

describe("formatFreebucks grouping", () => {
  it("groups thousands", () => {
    assert.equal(formatFreebucks(1234), (1234).toLocaleString());
    assert.match(formatFreebucks(1234567), /1.234.567|1,234,567/);
  });

  it("clamps negatives to zero like upstream", () => {
    assert.equal(formatFreebucks(-5), "0");
  });

  it("rounds fractional units to integers like upstream", () => {
    assert.equal(formatFreebucks(1.5), "2");
    assert.equal(formatFreebucks(7.5), "8");
    assert.equal(formatFreebucks(2.5), "3");
  });
});

describe("streakBonusNote (bonus branches only with a value present)", () => {
  it("7+ days: '+N Freebucks every day' perk line", () => {
    assert.equal(
      streakBonusNote({ streak: 7, freebucks_daily_bonus: 3 }),
      "🎁 Streak perk: +3 Freebucks every day",
    );
  });

  it("below the week: unlock countdown", () => {
    assert.equal(
      streakBonusNote({ streak: 3, freebucks_daily_bonus: 3 }),
      "🎁 4 more days to unlock +3 Freebucks every day",
    );
  });

  it("null on older data: absent bonus keeps the session copy", () => {
    assert.equal(streakBonusNote({ streak: 9 }), null);
    assert.equal(
      streakBonusNote({ streak: 9, freebucks_daily_bonus: null }),
      null,
    );
  });

  it("null with no streak at all", () => {
    assert.equal(
      streakBonusNote({ streak: 0, freebucks_daily_bonus: 3 }),
      null,
    );
  });
});
