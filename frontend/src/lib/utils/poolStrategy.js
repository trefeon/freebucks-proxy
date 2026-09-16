/**
 * Pool strategy presets (Pool → Controls → Pool Strategy card).
 *
 * Two named postures over exactly five owned keys; anything else reads as
 * Custom (auto-detected, never selectable). Preset switches write ONLY the
 * five owned keys through the shared instant-save overlay flow — every
 * other knob keeps its value.
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
 * Accepts compound forms exactly as time.Duration.String emits them
 * ("1m0s", "5m0s", "1h2m3s", "1m30s") — the settings GET tier serves the
 * normalized effective value for file/env rows, and older overlay rows may
 * still hold a compound literal. Returns NaN when unparseable — callers
 * treat that as Custom, never as a preset match.
 */
const DURATION_GROUP_RE = /(\d+(?:\.\d+)?)(ns|us|µs|μs|ms|s|m|h)/g;
const DURATION_UNIT_SECS = {
  h: 3600,
  m: 60,
  s: 1,
  ms: 0.001,
  us: 1e-6,
  µs: 1e-6,
  μs: 1e-6,
  ns: 1e-9,
};
export function parseWaitSecs(raw) {
  const v = String(raw ?? "")
    .trim()
    .toLowerCase();
  if (v === "") return NaN;
  let body = v;
  let neg = false;
  if (body.startsWith("-")) {
    neg = true;
    body = body.slice(1);
  }
  if (body !== "") {
    DURATION_GROUP_RE.lastIndex = 0;
    let total = 0;
    let consumed = 0;
    let matched = false;
    let m;
    while ((m = DURATION_GROUP_RE.exec(body)) !== null) {
      // Gap or overlap: not a clean duration ("1.5.2s", "10x").
      if (m.index !== consumed) break;
      matched = true;
      consumed = m.index + m[0].length;
      total += Number(m[1]) * DURATION_UNIT_SECS[m[2]];
    }
    if (matched && consumed === body.length) return neg ? -total : total;
  }
  const n = Number(neg ? body : v);
  return Number.isFinite(n) ? (neg ? -n : n) : NaN;
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
