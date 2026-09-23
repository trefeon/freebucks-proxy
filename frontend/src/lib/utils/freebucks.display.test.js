import { describe, it } from "node:test";
import assert from "node:assert/strict";
import {
  formatFreebucks,
  firstTabListPriceFor,
  freebucksDisplayModel,
  freebucksResetLine,
  offPeakCopy,
  streakBonusNote,
} from "./freebucks.js";

const LUNA = "openai/gpt-6-luna";

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

describe("freebucksResetLine (absolute instant vs vendor display string)", () => {
  const NOW = Date.parse("2026-09-20T07:00:00Z");
  const win = (extra) => ({
    resetAt: "",
    resetZone: null,
    resetAbsolute: false,
    ...extra,
  });

  it("display-only: labels the vendor clock, never claims a reset happened", () => {
    // "15:04 Jan 2" is a past LOCAL instant to V8's lenient parser; without an
    // absolute stamp it must still render as the refill zone's wall clock.
    const line = freebucksResetLine(
      win({ resetAt: "15:04 Jan 2", resetZone: "UTC" }),
      NOW,
    );
    assert.equal(line.shape, "clock");
    assert.equal(line.clock, "15:04 Jan 2 (UTC)");
  });

  it("display-only: names the zone it was formatted in, whatever it is", () => {
    const line = freebucksResetLine(
      win({ resetAt: "15:04 Jan 2", resetZone: "Asia/Jakarta" }),
      NOW,
    );
    assert.equal(line.shape, "clock");
    assert.match(line.clock, /^15:04 Jan 2 \(.+\)$/);
  });

  it("display-only without a zone still shows the stamp, never a countdown", () => {
    const line = freebucksResetLine(win({ resetAt: "15:04 Jan 2" }), NOW);
    assert.equal(line.shape, "clock");
    assert.equal(line.rel, "");
  });

  it("only an absolute instant that passed reads as pending", () => {
    assert.equal(
      freebucksResetLine(
        win({ resetAt: "2026-09-20T06:00:00Z", resetAbsolute: true }),
        NOW,
      ).shape,
      "pending",
    );
  });

  it("absolute ahead: counts down and re-anchors the clock", () => {
    const twoDays = freebucksResetLine(
      win({ resetAt: "2026-09-22T07:00:00Z", resetAbsolute: true }),
      NOW,
    );
    assert.equal(twoDays.shape, "countdown");
    assert.equal(twoDays.rel, "2d");
    assert.match(twoDays.clock, /^Sep 22, \d{2}:\d{2} [AP]M \(.+\)$/);

    const fiveHours = freebucksResetLine(
      win({ resetAt: "2026-09-20T12:32:00Z", resetAbsolute: true }),
      NOW,
    );
    assert.equal(fiveHours.rel, "5h 32m");
  });

  it("no stamp at all renders nothing", () => {
    assert.deepEqual(freebucksResetLine(win({}), NOW), {
      shape: "none",
      rel: "",
      clock: "",
    });
  });
});

describe("streakBonusNote (bonus branches only with a value present)", () => {
  it("7+ days: '+N Freebucks every Pacific day' perk line", () => {
    assert.equal(
      streakBonusNote({ streak: 7, freebucks_daily_bonus: 3 }),
      "🎁 Streak perk: +3 Freebucks every Pacific day",
    );
  });

  it("below the week: unlock countdown", () => {
    assert.equal(
      streakBonusNote({ streak: 3, freebucks_daily_bonus: 3 }),
      "🎁 4 more days to unlock +3 Freebucks every Pacific day",
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

describe("freebucksDisplayModel (allowances card, one figure per fact)", () => {
  const NOW = Date.parse("2026-09-23T12:00:00Z");
  // The served daily pool both live accounts carried on 2026-09-23.
  const DAILY = {
    limit: 25,
    spent: 10,
    remaining: 15,
    percent_used: 40,
    reset_at: "2026-09-24T00:00:00Z",
    reset_time_zone: "UTC",
  };
  // Live pair: akmalrzn15 40 = 15 daily + 25 wallet; hermescresioa
  // 30 = 15 daily + 15 wallet — the streak perk's 15/day is already inside
  // the wallet figure, so the daily limit stays at the server's 25.
  const AKMAL = {
    streak: 8,
    freebucks_daily_bonus: 15,
    freebucks: { balance: 40, daily: DAILY, wallet: { balance: 25 } },
  };
  const HERMES = {
    streak: 7,
    freebucks_daily_bonus: 15,
    freebucks: { balance: 30, daily: DAILY, wallet: { balance: 15 } },
  };

  it("akmalrzn15: spendable 40 = 15 daily + 25 wallet", () => {
    const m = freebucksDisplayModel(AKMAL, NOW);
    assert.equal(m.spendable, 40);
    assert.equal(m.dailyLeft, 15);
    assert.equal(m.dailyLimit, 25);
    assert.equal(m.dailySpent, 10);
    assert.equal(m.dailyPct, 40);
    assert.equal(m.wallet, 25);
    assert.equal(m.decomposition, "15 daily + 25 wallet");
  });

  it("hermescresioa: spendable 30 = 15 daily + 15 wallet", () => {
    const m = freebucksDisplayModel(HERMES, NOW);
    assert.equal(m.spendable, 30);
    assert.equal(m.wallet, 15);
    assert.equal(m.decomposition, "15 daily + 15 wallet");
  });

  it("never folds the perk into the daily limit", () => {
    // The bonus is credited to the wallet, so the pool the card draws stays
    // the server's own: 25, not 25 + 15.
    const m = freebucksDisplayModel(AKMAL, NOW);
    assert.equal(m.dailyLimit, 25);
    assert.equal(m.dailySpent, 10);
    assert.equal(m.dailyLeft, 15);
  });

  it("drops the decomposition when the served figures do not add up", () => {
    // Spendable includes a bucket this card cannot name (claimable grants are
    // excluded from spendable) — state the total, invent nothing.
    const diverging = {
      freebucks: { balance: 40, daily: DAILY, wallet: { balance: 20 } },
    };
    const m = freebucksDisplayModel(diverging, NOW);
    assert.equal(m.spendable, 40);
    assert.equal(m.decomposition, null);
  });

  it("no freebucks block at all renders nothing (older payloads)", () => {
    assert.equal(freebucksDisplayModel({ streak: 4 }, NOW), null);
    assert.equal(freebucksDisplayModel(null, NOW), null);
  });

  it("gap-fills only the missing spent figure, from the served pair", () => {
    const m = freebucksDisplayModel(
      {
        freebucks: {
          balance: 40,
          daily: { limit: 25, remaining: 15 },
          wallet: { balance: 25 },
        },
      },
      NOW,
    );
    assert.equal(m.dailySpent, 10);
    assert.equal(m.dailyPct, 40);
    assert.equal(m.decomposition, "15 daily + 25 wallet");
  });

  it("a missing figure never becomes a stated zero", () => {
    const m = freebucksDisplayModel(
      { freebucks: { daily: { limit: 25, spent: 10, remaining: 15 } } },
      NOW,
    );
    assert.equal(m.spendable, null);
    assert.equal(m.wallet, null);
    assert.equal(m.decomposition, null);
  });

  it("daily reset line: absolute instant counts down in the reset zone", () => {
    const { resetLine } = freebucksDisplayModel(AKMAL, NOW);
    assert.equal(resetLine.shape, "countdown");
    assert.equal(resetLine.rel, "12h 0m");
    assert.equal(resetLine.clock, "2026-09-24T00:00:00Z (UTC)");
  });

  it("daily reset line: no stamp renders no line", () => {
    const m = freebucksDisplayModel(
      { freebucks: { balance: 40, daily: { limit: 25, remaining: 15 } } },
      NOW,
    );
    assert.equal(m.resetLine, null);
  });

  it("perk note: the vendor copy verbatim, only with a positive bonus", () => {
    assert.equal(
      freebucksDisplayModel(AKMAL, NOW).perkNote,
      "🎁 Streak perk: +15 Freebucks every Pacific day",
    );
    // Below the week the vendor unlock countdown rides the same note.
    const early = { ...HERMES, streak: 3 };
    assert.equal(
      freebucksDisplayModel(early, NOW).perkNote,
      "🎁 4 more days to unlock +15 Freebucks every Pacific day",
    );
    // No bonus on the meter (or a session-only bonus): nothing lands on the
    // wallet line at all.
    assert.equal(
      freebucksDisplayModel({ freebucks: AKMAL.freebucks }, NOW).perkNote,
      null,
    );
    assert.equal(
      freebucksDisplayModel({ ...AKMAL, freebucks_daily_bonus: 0 }, NOW)
        .perkNote,
      null,
    );
  });

  it("keeps the perk note when the wallet figure is absent", () => {
    const m = freebucksDisplayModel(
      { streak: 8, freebucks_daily_bonus: 15, freebucks: { daily: DAILY } },
      NOW,
    );
    assert.equal(m.wallet, null);
    assert.equal(m.decomposition, null);
    assert.equal(m.perkNote, "🎁 Streak perk: +15 Freebucks every Pacific day");
  });
});
