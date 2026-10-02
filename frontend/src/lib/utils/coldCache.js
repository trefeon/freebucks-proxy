/**
 * Cold-render cache: a tiny localStorage snapshot per page so Overview and
 * Tokens paint instantly on mount (Phase 4 R3) instead of skeleton-loading
 * until the first poll lands. The query store replaces the seeded value on
 * the first successful poll and the UI carries a `stale` badge meanwhile.
 *
 * Never throws: storage may be blocked (private mode) or full — callers get
 * null/false and fall back to the skeleton path.
 */

const PREFIX = "fp-cache:";

// Upper bound per entry (~128KB serialized): pool snapshots stay well under
// it; anything bigger is skipped rather than risking quota eviction of the
// pageState drafts that share this storage.
const MAX_BYTES = 131072;

/**
 * Read a cached snapshot. Returns `{ savedAt, value }` or null when absent,
 * blocked, unparsable, or the wrong shape.
 * @param {string} key
 * @returns {{ savedAt: number, value: any } | null}
 */
export function readColdCache(key) {
  try {
    if (typeof localStorage === "undefined") return null;
    const raw = localStorage.getItem(PREFIX + key);
    if (!raw) return null;
    const parsed = JSON.parse(raw);
    if (!parsed || typeof parsed !== "object") return null;
    if (typeof parsed.savedAt !== "number" || !("value" in parsed)) return null;
    if (parsed.value === null || typeof parsed.value !== "object") return null;
    return parsed;
  } catch {
    return null;
  }
}

/**
 * Write a cached snapshot (timestamped). Oversized payloads and blocked/full
 * storage resolve false without throwing.
 * @param {string} key
 * @param {any} value - JSON-serializable snapshot
 * @returns {boolean} true when stored
 */
export function writeColdCache(key, value) {
  try {
    if (typeof localStorage === "undefined") return false;
    if (value === null || typeof value !== "object") return false;
    const raw = JSON.stringify({ savedAt: Date.now(), value });
    if (raw.length > MAX_BYTES) return false;
    localStorage.setItem(PREFIX + key, raw);
    return true;
  } catch {
    return false;
  }
}

/**
 * Drop a cached snapshot (used by tests and logout paths).
 * @param {string} key
 */
export function clearColdCache(key) {
  try {
    if (typeof localStorage === "undefined") return;
    localStorage.removeItem(PREFIX + key);
  } catch {
    /* blocked storage: nothing to clear */
  }
}
