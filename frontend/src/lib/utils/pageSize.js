/**
 * Global page-size memory (Phase 4 R2): one `localStorage` key remembered
 * across every table. The LiveConsole pager is the only paged table today;
 * any future table reads the same key, so the operator sets it once.
 */

export const PAGE_SIZE_KEY = "fp-page-size";

/** The row-count options the pager offers (must match the select). */
export const PAGE_SIZE_OPTIONS = [10, 50, 100];

export const DEFAULT_PAGE_SIZE = 10;

/**
 * Load the remembered page size. Unknown, missing, or out-of-option values
 * (a hand-edited key, a removed option) fall back to the default — never a
 * broken pager.
 * @returns {number}
 */
export function loadPageSize() {
  try {
    const raw = window?.localStorage?.getItem(PAGE_SIZE_KEY);
    const n = Number(raw);
    if (Number.isInteger(n) && PAGE_SIZE_OPTIONS.includes(n)) return n;
  } catch {
    // Storage unavailable (private mode, SSR) — default stands.
  }
  return DEFAULT_PAGE_SIZE;
}

/**
 * Remember a page size. Invalid values are ignored (never persisted), so a
 * stray state can neither poison the key nor throw.
 * @param {number} n
 */
export function savePageSize(n) {
  try {
    if (Number.isInteger(n) && PAGE_SIZE_OPTIONS.includes(n)) {
      window?.localStorage?.setItem(PAGE_SIZE_KEY, String(n));
    }
  } catch {
    // Storage unavailable — the pager just forgets on nav.
  }
}
