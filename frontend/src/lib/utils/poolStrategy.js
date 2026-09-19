/**
 * Pool strategy presets (Pool → Controls → Pool Strategy card).
 *
 * Two named postures over the five final Ordered Queue-Spill knobs; anything
 * else reads as Custom (auto-detected, never selectable). Preset switches
 * write ONLY the keys whose value actually differs from the preset through
 * the shared instant-save overlay flow — every other knob keeps its value.
 *
 * - Drain: deep queues per account-model lane (300s / 1024 waiters). Safest
 *   for a few accounts: each account drains fully before the pool spills.
 * - Balance: shallow queues (16 waiters) with a tunable threshold wait
 *   (5–300s, default 15s, persisted as QUEUE_WAIT). Best for busy pools.
 *
 * PIN_MODEL is owned but never preset-written: pins are per-account routing
 * owned by the token drawer, not queue posture, so a pinned account must not
 * flip the badge to Custom on its own.
 *
 * Detection is a pure function over the owned set so the badge, the
 * threshold slider, and the e2e contract all read one source of truth.
 * Values the loader would not keep are resolved to the loader's own
 * fallbacks BEFORE classification — QUEUE_WAIT falls back to 30s,
 * QUEUE_DEPTH to 16, SLOTS_PER_ACCOUNT to 2 and MAX_SPILL_ACCOUNTS to 0
 * (backend/internal/config/config_load.go), so a stock install whose rows
 * are blank, whitespace, or unparseable still reads Balance instead of
 * Custom. Balance accepts any in-range threshold (not just the 60s preset),
 * so moving the slider never flips the badge to Custom.
 */

/** The five final Ordered Queue-Spill knobs the badge classifies over. */
export const STRATEGY_OWNED_KEYS = [
  "SLOTS_PER_ACCOUNT",
  "QUEUE_WAIT",
  "QUEUE_DEPTH",
  "PIN_MODEL",
  "MAX_SPILL_ACCOUNTS",
];

/** Exact values the MASQ preset writes (60s deferred scale-out). */
export const STRATEGY_MASQ = {
  SLOTS_PER_ACCOUNT: "2",
  QUEUE_WAIT: "60s",
  QUEUE_DEPTH: "32",
  MAX_SPILL_ACCOUNTS: "0",
};

/** Exact values the Drain preset writes (PIN_MODEL excluded by design). */
export const STRATEGY_DRAIN = {
  SLOTS_PER_ACCOUNT: "2",
  QUEUE_WAIT: "300s",
  QUEUE_DEPTH: "1024",
  MAX_SPILL_ACCOUNTS: "0",
};

/** Exact values the Balance preset writes (fast 15s spill). */
export const STRATEGY_BALANCE = {
  SLOTS_PER_ACCOUNT: "2",
  QUEUE_WAIT: "15s",
  QUEUE_DEPTH: "16",
  MAX_SPILL_ACCOUNTS: "0",
};

/**
 * Balance threshold slider bounds (seconds). The preset's own threshold
 * lives in STRATEGY_BALANCE.QUEUE_WAIT; the slider just has to admit it.
 */
export const BALANCE_THRESHOLD_MIN_SECS = 5;
export const BALANCE_THRESHOLD_MAX_SECS = 300;

/**
 * Loader fallbacks for the queue keys, mirrored from
 * backend/internal/config/config_load.go so the badge classifies on the
 * values the gateway actually runs with:
 * - QUEUE_WAIT is zero-tolerant: blank or non-positive → 30s.
 * - QUEUE_DEPTH defaults to 16 when absent or unparseable (0 is a real
 *   value: fail over at once, no queueing).
 * - SLOTS_PER_ACCOUNT defaults to 2 when absent or unparseable, floors
 *   negative values to 0 (0 is a real value: unlimited, no slot gating).
 * - MAX_SPILL_ACCOUNTS defaults to 0 when absent or unparseable (0 is a
 *   real value: unbounded, the full index chain).
 */
export const QUEUE_WAIT_DEFAULT_SECS = 30;
export const QUEUE_DEPTH_DEFAULT = 16;
export const SLOTS_PER_ACCOUNT_DEFAULT = 2;
export const MAX_SPILL_ACCOUNTS_DEFAULT = 0;

/**
 * Parse a Go duration (or a bare number = seconds) to seconds.
 * Accepts compound forms exactly as time.Duration.String emits them
 * ("1m0s", "5m0s", "1h2m3s", "1m30s") — the settings GET tier serves the
 * normalized effective value for file/env rows, and older overlay rows may
 * still hold a compound literal. Returns NaN when unparseable —
 * queueWaitSecs() resolves that to the loader fallback before the badge
 * classifies, never to a preset match.
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

/**
 * QUEUE_WAIT in seconds as the loader would resolve it: blank, unparseable,
 * or non-positive values fall back to the 30s default.
 */
export function queueWaitSecs(raw) {
  const secs = parseWaitSecs(raw);
  if (!Number.isFinite(secs) || secs <= 0) return QUEUE_WAIT_DEFAULT_SECS;
  return secs;
}

/**
 * QUEUE_DEPTH as the loader would resolve it: blank or unparseable values
 * fall back to 16. 0 is returned as-is (a real posture: fail over at once).
 */
export function queueDepth(raw) {
  const v = String(raw ?? "").trim();
  if (v === "") return QUEUE_DEPTH_DEFAULT;
  const n = Number(v);
  return Number.isFinite(n) ? n : QUEUE_DEPTH_DEFAULT;
}

/**
 * SLOTS_PER_ACCOUNT as the loader would resolve it: blank or unparseable
 * values fall back to 2, negatives floor to 0 (unlimited, no slot gating).
 */
export function slotsPerAccount(raw) {
  const v = String(raw ?? "").trim();
  if (v === "") return SLOTS_PER_ACCOUNT_DEFAULT;
  const n = Number(v);
  if (!Number.isFinite(n)) return SLOTS_PER_ACCOUNT_DEFAULT;
  return n < 0 ? 0 : n;
}

/**
 * MAX_SPILL_ACCOUNTS as the loader would resolve it: blank or unparseable
 * values fall back to 0 (unbounded, the full index chain).
 */
export function maxSpillAccounts(raw) {
  const v = String(raw ?? "").trim();
  if (v === "") return MAX_SPILL_ACCOUNTS_DEFAULT;
  const n = Number(v);
  if (!Number.isFinite(n) || n < 0) return MAX_SPILL_ACCOUNTS_DEFAULT;
  return n;
}

/**
 * Detect the strategy badge from the owned values (raw form strings).
 * Missing keys and values the loader would not keep fall back to the same
 * defaults the gateway runs with: 2 slots, 30s wait, depth 16, unbounded
 * spill. PIN_MODEL never participates: pins are per-account routing owned
 * by the token drawer, not queue posture.
 *
 * @param {Record<string, string>} values
 * @returns {"masq" | "drain" | "balance" | "custom"}
 */
export function detectStrategy(values = {}) {
  const slots = slotsPerAccount(values.SLOTS_PER_ACCOUNT);
  const spill = maxSpillAccounts(values.MAX_SPILL_ACCOUNTS);
  const depth = queueDepth(values.QUEUE_DEPTH);
  const waitSecs = queueWaitSecs(values.QUEUE_WAIT);
  const waitRaw = String(values.QUEUE_WAIT ?? "").trim();
  if (
    slots === 2 &&
    spill === 0 &&
    depth === 32 &&
    (waitRaw === "60s" ||
      waitRaw === "1m" ||
      waitRaw === "1m0s" ||
      Math.abs(waitSecs - 60) < 0.01)
  ) {
    return "masq";
  }
  if (slots === 2 && spill === 0 && depth === 1024 && waitSecs === 300) {
    return "drain";
  }
  if (
    slots === 2 &&
    spill === 0 &&
    depth === 16 &&
    waitSecs >= BALANCE_THRESHOLD_MIN_SECS &&
    waitSecs <= BALANCE_THRESHOLD_MAX_SECS
  ) {
    return "balance";
  }
  return "custom";
}

/**
 * Clamp a QUEUE_WAIT value to the Balance slider range (seconds).
 * Values the loader would not keep fall back to the same 30s default, so
 * the slider never shows a threshold the gateway is not running.
 */
export function thresholdSecs(raw) {
  return Math.min(
    BALANCE_THRESHOLD_MAX_SECS,
    Math.max(BALANCE_THRESHOLD_MIN_SECS, Math.round(queueWaitSecs(raw))),
  );
}
