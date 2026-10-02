/**
 * Hash-query table state (Phase 4 R1): URL-shareable Logs/Tokens table state
 * via `location.hash`, e.g. `#activity?view=table&level=error&msg=timeout`.
 *
 * The App shell owns the hash PAGE (`#activity`, `#tokens`); this module owns
 * the QUERY on it. Writes go through `history.replaceState` (never
 * `location.hash =`, which would fire `hashchange` and loop the shell's
 * tab-sync effect) and merge per-key, so two components sharing one page
 * (Activity's tab + LiveConsole's filters on `#activity`) never clobber each
 * other. Only non-default values are encoded, so plain `#tokens` stays bare.
 *
 * Pure cores (`parseHash`, `buildHash`, `applyHashPatch`) take/return strings
 * and are unit-tested; the `window`-touching wrappers are thin and SSR-safe.
 */

/**
 * Split a hash into its page segment and query params. The page is everything
 * before the first `?` (legacy ids and unknown pages pass through untouched —
 * the shell resolves those, not this module).
 * @param {string} hash e.g. `#activity?level=error` or `activity`
 * @returns {{ page: string, params: URLSearchParams }}
 */
export function parseHash(hash) {
  const raw = String(hash ?? "").replace(/^#/, "");
  const qi = raw.indexOf("?");
  if (qi === -1) return { page: raw, params: new URLSearchParams() };
  return {
    page: raw.slice(0, qi),
    params: new URLSearchParams(raw.slice(qi + 1)),
  };
}

/**
 * Page segment of a hash (anything before the first `?`).
 * @param {string} hash
 * @returns {string}
 */
export function hashPageId(hash) {
  return parseHash(hash).page;
}

/**
 * Render a hash from a page plus params. Empty params render the bare page
 * (`#tokens`, never `#tokens?`).
 * @param {string} page
 * @param {URLSearchParams} params
 * @returns {string}
 */
export function buildHash(page, params) {
  const qs = params?.toString() ?? "";
  return qs ? `#${page}?${qs}` : `#${page}`;
}

/**
 * Merge a patch into a hash's query (pure). Keys with null/undefined/empty
 * values are deleted; all other values are stringified. Returns the input
 * hash unchanged when its page does not match (a component must never
 * rewrite another page's query).
 * @param {string} hash current hash, e.g. `#activity?tab=traces`
 * @param {string} page owning page, e.g. `activity`
 * @param {Record<string, string | number | null | undefined>} patch
 * @returns {string} the merged hash
 */
export function applyHashPatch(hash, page, patch) {
  const { page: cur, params } = parseHash(hash);
  if (cur !== page) return String(hash ?? "");
  for (const [k, v] of Object.entries(patch ?? {})) {
    if (v === null || v === undefined || v === "") params.delete(k);
    else params.set(k, String(v));
  }
  return buildHash(page, params);
}

/**
 * Read the live hash: page segment plus query params. SSR-safe (no `window`
 * → empty page, empty params).
 * @returns {{ page: string, params: URLSearchParams }}
 */
export function readPageHash() {
  if (typeof window === "undefined" || !window.location) {
    return { page: "", params: new URLSearchParams() };
  }
  return parseHash(window.location.hash || "");
}

const hashTimers = new Map();

/**
 * Debounced hash-query merge for the owning page (default 300 ms, trailing).
 * Reads the LIVE hash at fire time and merges only the patched keys, so two
 * writers sharing a page compose instead of clobbering. Drops the write when
 * the operator has navigated to another page since. Uses `replaceState`: no
 * `hashchange` event, no history spam, no shell-sync loop.
 * @param {string} page owning page
 * @param {Record<string, string | number | null | undefined>} patch
 * @param {number} [delay=300]
 */
export function scheduleHashUpdate(page, patch, delay = 300) {
  if (typeof window === "undefined") return;
  clearTimeout(hashTimers.get(page));
  hashTimers.set(
    page,
    setTimeout(() => {
      hashTimers.delete(page);
      writeHashUpdate(page, patch);
    }, delay),
  );
}
/**
 * Immediate (non-debounced) hash-query merge. Prefer `scheduleHashUpdate`
 * from reactive effects; this is the flush path and the unit-test seam.
 * @param {string} page owning page
 * @param {Record<string, string | number | null | undefined>} patch
 */
export function writeHashUpdate(page, patch) {
  if (
    typeof window === "undefined" ||
    !window.location ||
    !window.history ||
    typeof window.history.replaceState !== "function"
  ) {
    return;
  }
  const cur = window.location.hash || "";
  if (hashPageId(cur) !== page) return;
  const next = applyHashPatch(cur, page, patch);
  // Normalize the comparison: `cur` may lack the `#` prefix in exotic
  // embeddings while `next` always carries it.
  const curNorm = cur.startsWith("#") ? cur : `#${cur}`;
  if (next !== curNorm) window.history.replaceState(null, "", next);
}
