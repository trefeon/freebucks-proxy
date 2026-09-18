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

/** Confirm-dialog line for a confirm intent (mirrors askLineFor). */
export function intentAskLine(intent, activeModel) {
  if (intent.kind !== "confirm") return null;
  if (intent.walletSpend > 0)
    return `Today's Freebucks pool is spent — this uses ${intent.walletSpend} from the wallet${activeModel ? " and ends the live session" : ""}.`;
  return `This ends the live session and starts a new one (price ${intent.price}).`;
}

export const MODEL_METADATA = {
  "upstage/solar-pro4": {
    displayName: "Solar Pro 4",
    tagline: "0 Freebucks",
    badges: [],
  },
  "z-ai/glm-5.3-flash": {
    displayName: "GLM 5.3 Flash",
    tagline: "Deep reasoning",
    badges: ["Reasoning: max*", "Images", "NEW"],
  },
  "mimo/mimo-v2.5": {
    displayName: "MiMo 2.5",
    tagline: "Balanced",
    badges: ["Images"],
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
  "openai/gpt-5.6-luna": {
    displayName: "GPT-5.6 Luna",
    tagline: "Strong all-around",
    badges: ["Reasoning: high", "Images"],
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
    bonus > 0 ? `+${bonus} Freebucks every day` : "+1 bonus session every day";
  if (days < 7) {
    const remaining = 7 - days;
    return `🎁 ${remaining} more ${remaining === 1 ? "day" : "days"} to unlock ${perk}`;
  }
  return `🎁 Streak perk: ${perk}`;
}
