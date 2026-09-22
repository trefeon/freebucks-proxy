/**
 * Shared touch-model option helpers (Warming → Streak Maintenance →
 * MATURITY_TOUCH_MODEL select). Priced labels come straight from
 * /admin/api/models rows (price_label/quota/pool) — never invented.
 */

/**
 * Served touch candidates, cheapest-Freebucks-cost first: rows the gateway
 * can admit (live agent binding, served, never the referral grant). Server
 * order already sorts cheapest-first, so priced rows stay ahead without
 * re-sorting; the premium pool trails.
 */
export function touchCandidates(modelRows) {
  const rows = (modelRows ?? []).filter(
    (m) => m?.agent && m?.served !== false && m?.pool !== "referral",
  );
  return [
    ...rows.filter((m) => m.pool !== "premium"),
    ...rows.filter((m) => m.pool === "premium"),
  ];
}

/** Server-reported cost class for one candidate row (never invented).
 * Prefers the regular list price so nightly streak touch automation reflects
 * the actual recurring rate rather than a temporary first-tab promotional discount. */
export function touchCostClass(m, listPrices = null) {
  if (!m) return "";
  if (listPrices && listPrices[m.id] !== undefined) {
    const p = listPrices[m.id];
    return p === 0 ? "0 Freebucks/hr" : `${p} Freebucks/hr`;
  }
  return m.list_price_label || m.price_label || m.quota || "";
}

export function touchLabel(m, listPrices = null) {
  const cls = touchCostClass(m, listPrices);
  return cls ? `${m.id} (${cls})` : m.id;
}

/**
 * Fail-open options: live candidates when the catalog loaded, else the
 * current value alone so the select never empties. The current saved value
 * is always appended when the catalog omits it (retired/priced since save,
 * or a stale snapshot): otherwise the select falls back to the first
 * option and displays a model that was never saved.
 */
export function touchOptions(modelRows, currentId = "") {
  const cands = touchCandidates(modelRows);
  if (cands.length === 0) {
    if (currentId) {
      return [{ id: currentId, price_label: "", quota: "", pool: "unlimited" }];
    }
    return [];
  }
  if (
    currentId &&
    currentId !== "auto" &&
    !cands.some((m) => m?.id === currentId)
  ) {
    return [
      ...cands,
      { id: currentId, price_label: "", quota: "", pool: "unlimited" },
    ];
  }
  return cands;
}
