/**
 * Freebucks meter intents for spawn pickers (mirrors freebucksRowIntent in
 * upstream cli/src/utils/freebucks.ts, issue #350 port).
 *
 * Three outcomes, ordered by what the pick costs the reader:
 * - paywall — pool AND wallet together cannot cover the price: refuse where
 *   the balance is already on screen (disable the option/button).
 * - confirm — affordable but spends something that does not come back (a
 *   live session, or wallet Freebucks): enrich the confirm dialog.
 * - allow — everything else, including every row on an unmetered account.
 */

import { formatLocalDateTime, zoneLabel } from "./format.js";

export function freebucksOf(token) {
  return token?.freebucks ?? null;
}

export function modelPrice(fb, modelId) {
  return fb?.prices?.[modelId];
}

export function spawnIntent(token, modelId) {
  const fb = freebucksOf(token);
  const price = modelPrice(fb, modelId);
  if (!fb || price === undefined)
    return { kind: "allow", price, walletSpend: 0 };
  // Spendable counts claimable earned grants toward admission (vendor
  // af898dc getFreebucksModelMeter: balance + claimable >= price).
  const spendable =
    (fb.balance ?? fb.Balance ?? 0) +
    (fb.claimableGrantFreebucks ?? fb.ClaimableGrant ?? 0);
  if (fb.quota_exempt ?? fb.quotaExempt ?? false)
    return { kind: "allow", price, walletSpend: 0 };
  // `<` not `<=`: a spendable amount that exactly equals the price BUYS
  // the session.
  if (spendable < price) return { kind: "paywall", price, walletSpend: 0 };
  const dailyRemaining = fb.daily?.remaining ?? fb.Daily?.Remaining ?? 0;
  const walletSpend = Math.max(0, price - dailyRemaining);
  const activeModel = token?.session_model;
  const endsSession = activeModel !== undefined && activeModel !== modelId;
  if (endsSession || walletSpend > 0)
    return { kind: "confirm", price, walletSpend };
  return { kind: "allow", price, walletSpend: 0 };
}

/** Upstream FREEBUFF_PLAN_REQUIRED_LABEL / _LINE: the badge and sentence a
 * plan-locked row draws (common/src/util/freebuff-model-selection.ts). The
 * row stays visible, carries no price, and the server refuses admission. */
export const PLAN_REQUIRED_LABEL = "Paid plan";
export const PLAN_REQUIRED_LINE = "Included with a paid plan.";

/** Whether a catalog row (or a model id, via the served set) is plan-locked. */
export function isPlanRequired(row) {
  return Boolean(row?.plan_required ?? row?.planRequired);
}

/** Confirm-dialog line for a confirm intent (mirrors askLineFor). */
export function intentAskLine(intent, activeModel) {
  if (intent.kind !== "confirm") return null;
  if (intent.walletSpend > 0)
    return `Today's Freebucks pool is spent — this uses ${intent.walletSpend} from the wallet${activeModel ? " and ends the live session" : ""}.`;
  return `This ends the live session and starts a new one (price ${intent.price}).`;
}

export const MODEL_METADATA = {
  // Solar Pro 4 left every picker on 2026-09-23 (replaced by Solar Mini 4 in
  // FREEBUFF_MODELS, supersededBy Mini 4) but stays recognized: sessions
  // admitted before the swap drain on it, so its copy stays for the rows
  // that still name it. Never a picker row.
  "upstage/solar-pro4": {
    displayName: "Solar Pro 4",
    tagline: "0 Freebucks",
    badges: [],
  },
  "upstage/solar-mini4": {
    displayName: "Solar Mini 4",
    tagline: "Fast and light",
    badges: ["NEW"],
  },
  // Space Bunny Alpha is served but experimental (BETA): upstream renders the
  // experimental flag as a badge chip, and the row retains prompts on an
  // anonymous host — hence the BETA chip and the disclaimer, mirroring the
  // backend Notice. Never the automatic default (backend Experimental gate).
  "stealth/space-bunny-alpha": {
    displayName: "Space Bunny Alpha",
    tagline: "1M context",
    badges: ["Reasoning: high", "Images", "NEW", "BETA"],
    disclaimer: "Anonymous provider retains prompts",
  },
  "z-ai/glm-5.3-flash": {
    displayName: "GLM 5.3 Flash",
    tagline: "Deep reasoning",
    badges: ["Reasoning: max*", "Images", "NEW"],
  },
  // The wire id keeps its v2.5 spelling; upstream serves MiMo 2.6 Flash under
  // it (freebuff-models.ts: the row's displayName moved 2026-09-21).
  "mimo/mimo-v2.5": {
    displayName: "MiMo 2.6 Flash",
    tagline: "Balanced",
    badges: ["Images", "NEW"],
  },
  "deepseek/deepseek-v4-flash": {
    displayName: "DeepSeek V4.1 Flash",
    tagline: "Smart & Fast",
    badges: ["Reasoning: high", "Images", "NEW"],
    disclaimer: "May use data for AI training",
  },
  "meta/muse-spark-1.2-contributor": {
    displayName: "Muse Spark 1.2",
    tagline: "Queue",
    badges: ["Reasoning: xhigh"],
    disclaimer: "May use data for AI training",
  },
  // GPT-5.6 Luna left every picker on 2026-09-22 (replaced by GPT-6 Luna in
  // FREEBUFF_MODELS) but stays recognized and admissible: sessions admitted
  // before the swap drain on it, so its copy stays for the rows that still
  // name it (token quota cards, drained session display). Never a picker row.
  "openai/gpt-5.6-luna": {
    displayName: "GPT-5.6 Luna",
    tagline: "Strong all-around",
    badges: ["Reasoning: high", "Images"],
  },
  "openai/gpt-6-luna": {
    displayName: "GPT-6 Luna",
    tagline: "Strong all-around",
    badges: ["Reasoning: high", "Images", "NEW"],
  },
  "z-ai/glm-5.2": {
    displayName: "GLM 5.2",
    tagline: "Referral reward",
    badges: ["Referral only"],
  },
};

export function formatFreebucks(v) {
  if (v == null || v === "") return "0";
  const n = Number(v);
  if (Number.isNaN(n)) return String(v);
  // Whole units with locale grouping, never negative (mirrors upstream
  // formatFreebucks: Math.max(0, Math.round(amount)).toLocaleString()).
  return Math.max(0, Math.round(n)).toLocaleString();
}

// "4h 12m", "38m", "2d 5h" — until the daily pool refills; "now" once it
// has passed. Port of upstream freebucksResetCountdown (same shape as the
// Web/Desktop pickers; issue #354).
export function freebucksResetCountdown(resetAt, nowMs) {
  const remainingMs = Date.parse(resetAt) - nowMs;
  if (!Number.isFinite(remainingMs) || remainingMs <= 0) return "now";
  const totalMinutes = Math.ceil(remainingMs / 60000);
  const days = Math.floor(totalMinutes / 1440);
  const hours = Math.floor((totalMinutes % 1440) / 60);
  const minutes = totalMinutes % 60;
  if (days > 0) return `${days}d ${hours}h`;
  if (hours > 0) return `${hours}h ${minutes}m`;
  return `${minutes}m`;
}

// "2d 5h", "5h 32m", "38m", "12s" — until one Freebucks window refills;
// "now" once the instant has passed, and an unparseable stamp is echoed back
// rather than guessed at.
export function freebucksWindowRel(at, nowMs) {
  if (!at) return "—";
  const t = new Date(at).getTime();
  if (isNaN(t)) return at;
  const ms = t - nowMs;
  if (ms <= 0) return "now";
  const mins = Math.floor(ms / 60000);
  const h = Math.floor(mins / 60);
  const m = mins % 60;
  if (h >= 24) {
    const d = Math.floor(h / 24);
    const hr = h % 24;
    return hr > 0 ? `${d}d ${hr}h` : `${d}d`;
  }
  if (h > 0) return `${h}h ${m}m`;
  if (m > 0) return `${m}m`;
  return `${Math.max(1, Math.floor(ms / 1000))}s`;
}

/**
 * The reset line one Freebucks window renders, from its wire stamps. Only the
 * absolute instant can carry a countdown or an expiry claim; the vendor's own
 * display string ("15:04 Jan 2") is a foreign wall clock with no year, so it
 * renders as-is with the zone it was formatted in and never claims a reset
 * already happened. Returns plain data — the component owns the wording.
 * @param {{ resetAt?: string|null, resetZone?: string|null, resetAbsolute?: boolean }} win
 * @param {number} nowMs
 * @returns {{ shape: "none"|"pending"|"countdown"|"clock", rel: string, clock: string }}
 */
export function freebucksResetLine(win, nowMs) {
  const at = win?.resetAt ?? "";
  if (!at) return { shape: "none", rel: "", clock: "" };
  const zone = win?.resetZone ?? null;
  const clock = zone
    ? `${at} (${zoneLabel(at, { timeZone: zone })})`
    : formatLocalDateTime(at) || "—";
  if (!win?.resetAbsolute) return { shape: "clock", rel: "", clock };
  const ms = Date.parse(at);
  if (Number.isFinite(ms) && ms <= nowMs)
    return { shape: "pending", rel: "", clock };
  return { shape: "countdown", rel: freebucksWindowRel(at, nowMs), clock };
}

/** A wire figure, kept as a number only when the server actually sent one:
 * an absent figure must never become the 0 the card then states as fact. */
function wireNum(v) {
  if (v == null || v === "") return null;
  const n = Number(v);
  return Number.isFinite(n) ? n : null;
}

/**
 * The Allowances card's display model for one account's `freebucks` block:
 * every figure the card draws, resolved once from the wire so no number is
 * derived twice, plus the identity of the spendable total.
 *
 * `balance` IS the spendable figure — vendor
 * common/src/types/freebuff-session.ts defines it as
 * `daily.remaining + wallet.balance` — and `decomposition` states that
 * relationship in words. It renders only when the three served figures
 * cohere after rounding: a mismatch means the server counts something this
 * card cannot name (claimable grants are deliberately excluded from
 * spendable), so the card states the total alone rather than inventing a
 * bucket for the difference.
 *
 * `resetLine` is the daily pool's own refill stamp: only a parseable instant
 * carries a countdown, a vendor display string renders as a clock with the
 * zone it was formatted in. `perkNote` is the streak perk — server-credited
 * to the WALLET every Pacific day, never folded into `dailyLimit` — and stays
 * null unless a positive Freebucks bonus is served, so the session-bonus copy
 * never lands on a wallet line.
 *
 * @param {{ freebucks?: object|null }} token pooled-token row
 * @param {number} nowMs clock for the reset line (tests pin it)
 * @returns {null | {
 *   spendable: number|null, dailyLeft: number|null, dailyLimit: number|null,
 *   dailySpent: number|null, dailyPct: number, wallet: number|null,
 *   decomposition: string|null, resetLine: object|null, perkNote: string|null,
 * }} null for a token with no freebucks block (older payloads)
 */
export function freebucksDisplayModel(token, nowMs = Date.now()) {
  const fb = token?.freebucks ?? null;
  if (fb == null) return null;
  const daily = fb.daily ?? null;
  const dailyLimit = wireNum(daily?.limit);
  const dailyLeft = wireNum(daily?.remaining);
  // Gap-fill only — the served figures stay authoritative. Older payloads
  // send `remaining` without `spent`; the daily window's own meter mirrors
  // this in FreebucksQuotaBar.
  const dailySpent =
    wireNum(daily?.spent) ??
    (dailyLimit != null && dailyLeft != null
      ? Math.max(0, dailyLimit - dailyLeft)
      : null);
  const pctRaw =
    wireNum(daily?.percent_used) ??
    (dailyLimit > 0 && dailySpent != null
      ? (dailySpent / dailyLimit) * 100
      : 0);
  const dailyPct = Math.min(100, Math.max(0, pctRaw));
  const wallet = wireNum(fb.wallet?.balance);
  const spendable = wireNum(fb.balance);
  const coheres =
    spendable != null &&
    dailyLeft != null &&
    wallet != null &&
    Math.round(spendable) === Math.round(dailyLeft) + Math.round(wallet);
  const decomposition = coheres
    ? `${formatFreebucks(dailyLeft)} daily + ${formatFreebucks(wallet)} wallet`
    : null;
  const resetAt = daily?.reset_at ?? "";
  const resetLine = resetAt
    ? freebucksResetLine(
        {
          resetAt,
          resetZone: daily?.reset_time_zone ?? daily?.resetTimeZone ?? null,
          resetAbsolute: Number.isFinite(Date.parse(resetAt)),
        },
        nowMs,
      )
    : null;
  const bonus = wireNum(
    token?.freebucks_daily_bonus ?? token?.freebucksDailyBonus,
  );
  const perkNote = bonus != null && bonus > 0 ? streakBonusNote(token) : null;
  return {
    spendable,
    dailyLeft,
    dailyLimit,
    dailySpent,
    dailyPct,
    wallet,
    decomposition,
    resetLine,
    perkNote,
  };
}

// "$25", "$4.20", "$0" — whole dollars until the figure is small enough
// that the cents are the story. Port of upstream formatAllowanceUsd
// (issue #354).
export function formatAllowanceUsd(usd) {
  const safe = Math.max(0, Number(usd) || 0);
  if (safe >= 10) return `$${Math.round(safe)}`;
  if (safe >= 1) return `$${safe.toFixed(1).replace(/\.0$/, "")}`;
  return `$${safe.toFixed(2)}`;
}

export function sortModelsByPrice(modelIds, freebucks, names) {
  if (!Array.isArray(modelIds)) return [];
  const priceOf = (id) => freebucks?.prices?.[id] ?? Number.POSITIVE_INFINITY;
  const nameOf = (id) => names?.[id] || MODEL_METADATA[id]?.displayName || id;
  return [...modelIds].sort(
    (a, b) => priceOf(a) - priceOf(b) || nameOf(a).localeCompare(nameOf(b)),
  );
}
/** Session price after a first-tab discount, floored at zero (mirrors
 * discountedSessionPrice in freebuff-first-tab-discount.ts). */
export function discountedSessionPrice(price, discount) {
  return Math.max(0, price - discount);
}

/**
 * The list price to draw crossed out beside `modelId`'s discounted price,
 * or undefined when there is nothing to cross out: no offer, the offer in
 * use by another session, an unpriced row, or a row the discount did not
 * move (a row already at 0 is not "0 off 0"). A quote from a server that
 * predates `listPrices` answers undefined for every row rather than
 * guessing — `price + amount` is wrong for every row the zero floor
 * clamped. Port of firstTabListPriceFor (vendor 3420c99); reads both the
 * snake_case dashboard keys and the camelCase wire keys.
 */
export function firstTabListPriceFor(info, modelId) {
  const discount = info?.first_tab_discount ?? info?.firstTabDiscount ?? null;
  if (!discount?.available) return undefined;
  const price = info?.prices?.[modelId];
  const listPrice = (info?.list_prices ?? info?.listPrices)?.[modelId];
  if (price === undefined || listPrice === undefined || listPrice <= price)
    return undefined;
  return listPrice;
}

/** Resolve a server-owned daily off-peak policy, including windows crossing
 * midnight. Port of offPeakPriceAt (vendor freebuff-price-changes.ts).
 * Reads the camelCase wire keys and the snake_case dashboard keys (the live
 * /admin/api/tokens quote ships start_hour_utc/end_hour_utc). Returns null
 * when the window hours are absent or non-numeric instead of building an
 * Invalid Date — Intl formatting on one throws RangeError mid-render, which
 * froze the Usage Accounts/Models tabs on "Loading…" (2026-09-18). */
export function offPeakPriceAt(offer, now) {
  const startHour = offer?.startHourUtc ?? offer?.start_hour_utc;
  const endHour = offer?.endHourUtc ?? offer?.end_hour_utc;
  if (!Number.isFinite(startHour) || !Number.isFinite(endHour)) return null;
  const start = new Date(now);
  start.setUTCHours(startHour, 0, 0, 0);
  if (+start > now) start.setUTCDate(start.getUTCDate() - 1);
  const end = new Date(start);
  end.setUTCHours(endHour, 0, 0, 0);
  if (+end <= +start) end.setUTCDate(end.getUTCDate() + 1);
  const active = now < +end;
  if (!active) {
    start.setUTCDate(start.getUTCDate() + 1);
    end.setUTCDate(end.getUTCDate() + 1);
  }
  return { start, end, active };
}

function resolveWindowTimeZone(timeZone) {
  if (timeZone) return timeZone;
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone ?? "UTC";
  } catch {
    return "UTC";
  }
}

function formatWindowTimeZoneLabel(on, timeZone) {
  const zone = resolveWindowTimeZone(timeZone);
  const named = new Intl.DateTimeFormat(undefined, {
    timeZone: zone,
    hour: "numeric",
    timeZoneName: "short",
  })
    .formatToParts(on)
    .find((part) => part.type === "timeZoneName")?.value;
  return named ?? zone;
}

/**
 * Presentation copy for one model id's off-peak offer (port of
 * freebucksOffPeakCopy, vendor 3420c99): { active, badge, detail, tooltip },
 * or undefined when the model carries no offer or no price. Prices come
 * from the server quote, never a local rate card. The resolved quote owns
 * the active badge too — no second pricing clock is run.
 */
export function offPeakCopy(
  info,
  modelId,
  { now = Date.now(), timeZone } = {},
) {
  const offer = info?.off_peak?.[modelId] ?? info?.offPeak?.[modelId] ?? null;
  if (!offer || info?.prices?.[modelId] === undefined) return undefined;
  const window = offPeakPriceAt(offer, now);
  if (!window) return undefined;
  const { start, end } = window;
  const zone = resolveWindowTimeZone(timeZone);
  const fmt = new Intl.DateTimeFormat(undefined, {
    hour: "numeric",
    minute: "2-digit",
    timeZone: zone,
  });
  const startZone = formatWindowTimeZoneLabel(start, zone);
  const endZone = formatWindowTimeZoneLabel(end, zone);
  const hours = `${fmt.format(start)}${startZone === endZone ? "" : ` ${startZone}`}–${fmt.format(end)} ${endZone}`;
  const discount = info?.first_tab_discount ?? info?.firstTabDiscount;
  const regularPrice = offer.regularPrice ?? offer.regular_price;
  const active =
    info.prices[modelId] ===
    discountedSessionPrice(
      offer.price,
      discount?.available ? discount.amount : 0,
    );
  return {
    active,
    badge: "Off-peak",
    detail: active
      ? `Off-peak · normally ${regularPrice}/hr · until ${fmt.format(end)} ${endZone}`
      : `Off-peak ${offer.price}/hr · ${hours}`,
    tooltip:
      `Off-peak: ${offer.price} Freebucks/hour, daily ${hours}. ` +
      `Regular price: ${regularPrice} Freebucks/hour. ` +
      "The price at session start is locked for the full hour." +
      (discount?.available
        ? " Your first-tab discount is also included in the displayed price."
        : ""),
  };
}

/**
 * Short perk note for an active streak (port of getFreebuffStreakBonusNote,
 * vendor freebuff-streak-line.ts, Freebucks-meter branches only). Returns
 * null unless a freebucks_daily_bonus value is present: a positive number
 * means the account is on the meter and the perk is Freebucks; null or
 * absent (older servers) keeps the session copy — the caller draws no
 * Freebucks bonus note. No streak polling here; pure copy of the payload.
 */
export function streakBonusNote(token) {
  const bonus =
    token?.freebucks_daily_bonus ?? token?.freebucksDailyBonus ?? null;
  if (bonus == null) return null;
  const days = Number(token?.streak) || 0;
  if (days <= 0) return null;
  const perk =
    bonus > 0
      ? `+${bonus} Freebucks every Pacific day`
      : "+1 bonus session every day";
  if (days < 7) {
    const remaining = 7 - days;
    return `🎁 ${remaining} more ${remaining === 1 ? "day" : "days"} to unlock ${perk}`;
  }
  return `🎁 Streak perk: ${perk}`;
}
