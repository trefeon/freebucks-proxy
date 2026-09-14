/**
 * Pool strategy presets (Pool → Controls → Pool Strategy card).
 *
 * Two named postures over exactly five owned keys; anything else reads as
 * Custom (auto-detected, never selectable). Preset switches write ONLY the
 * five owned keys through the shared batched .env flow — every other knob
 * keeps its value.
 *
 * - Drain: deep queues per token (300s / 1024 waiters). Safest for a few
 *   accounts: each account drains fully before the pool fails over.
 * - Balance: shallow queues (16 waiters) with a tunable threshold wait
 *   (5–300s, default 60s, persisted as QUEUE_WAIT). Best for busy pools.
 *
 * Detection is a pure function over the owned set so the badge, the
 * threshold slider, and the e2e contract all read one source of truth.
 * Balance accepts any in-range threshold (not just the 60s default), so
 * moving the slider never flips the badge to Custom; the shipped catalog
 * defaults (30s / 16) already read as Balance.
 */

/** Keys a preset switch writes — nothing else. */
export const STRATEGY_OWNED_KEYS = [
  "ROUTING_SMART",
  "TOKEN_ROTATION",
  "RATE_LIMIT_FAILOVER",
  "QUEUE_WAIT",
  "QUEUE_DEPTH",
];

/** Exact values the Drain preset writes. */
export const STRATEGY_DRAIN = {
  ROUTING_SMART: "true",
  TOKEN_ROTATION: "drain",
  RATE_LIMIT_FAILOVER: "true",
  QUEUE_WAIT: "300s",
  QUEUE_DEPTH: "1024",
};

/** Exact values the Balance preset writes (threshold at its default). */
export const STRATEGY_BALANCE = {
  ROUTING_SMART: "true",
  TOKEN_ROTATION: "drain",
  RATE_LIMIT_FAILOVER: "true",
  QUEUE_WAIT: "60s",
  QUEUE_DEPTH: "16",
};

/** Balance threshold slider bounds (seconds) + default. */
export const BALANCE_THRESHOLD_MIN_SECS = 5;
export const BALANCE_THRESHOLD_MAX_SECS = 300;
export const BALANCE_THRESHOLD_DEFAULT_SECS = 60;

/**
 * Parse a Go duration (or a bare number = seconds) to seconds.
 * Returns NaN when unparseable — callers treat that as Custom, never as a
 * preset match.
 */
export function parseWaitSecs(raw) {
  const v = String(raw ?? "")
    .trim()
    .toLowerCase();
  if (v === "") return NaN;
  const m = /^(\d+(\.\d+)?)(ns|us|µs|ms|s|m|h)$/.exec(v);
  if (m) {
    const n = Number(m[1]);
    switch (m[3]) {
      case "h":
        return n * 3600;
      case "m":
        return n * 60;
      case "s":
        return n;
      case "ms":
        return n / 1000;
      default:
        return n / 1e9;
    }
  }
  const n = Number(v);
  return Number.isFinite(n) ? n : NaN;
}

function isOn(raw, fallback) {
  if (raw === undefined || raw === null || String(raw).trim() === "")
    return fallback;
  return String(raw).trim().toLowerCase() !== "false";
}

function normRotation(raw) {
  const v = String(raw ?? "drain")
    .trim()
    .toLowerCase();
  return ["drain", "round_robin", "least_used", "random"].includes(v)
    ? v
    : "drain";
}

/**
 * Detect the strategy badge from the five owned values (raw form strings;
 * missing keys fall back to the catalog defaults: smart on, drain,
 * failover on, 30s, 16).
 *
 * @param {Record<string, string>} values
 * @returns {"drain" | "balance" | "custom"}
 */
export function detectStrategy(values = {}) {
  const smart = isOn(values.ROUTING_SMART, true);
  const rotation = normRotation(values.TOKEN_ROTATION);
  const failover = isOn(values.RATE_LIMIT_FAILOVER, true);
  const depth = String(values.QUEUE_DEPTH ?? "16").trim();
  const waitSecs = parseWaitSecs(values.QUEUE_WAIT ?? "30s");
  if (
    smart &&
    rotation === "drain" &&
    failover &&
    depth === "1024" &&
    waitSecs === 300
  ) {
    return "drain";
  }
  if (
    smart &&
    rotation === "drain" &&
    failover &&
    depth === "16" &&
    Number.isFinite(waitSecs) &&
    waitSecs >= BALANCE_THRESHOLD_MIN_SECS &&
    waitSecs <= BALANCE_THRESHOLD_MAX_SECS
  ) {
    return "balance";
  }
  return "custom";
}

/**
 * Clamp a QUEUE_WAIT value to the Balance slider range (seconds).
 * Unparseable values fall back to the 60s default.
 */
export function thresholdSecs(raw) {
  const secs = parseWaitSecs(raw);
  if (!Number.isFinite(secs)) return BALANCE_THRESHOLD_DEFAULT_SECS;
  return Math.min(
    BALANCE_THRESHOLD_MAX_SECS,
    Math.max(BALANCE_THRESHOLD_MIN_SECS, Math.round(secs)),
  );
}
