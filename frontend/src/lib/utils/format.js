/**
 * Format an ISO timestamp to a local short date.
 * @param {string} utcIso
 * @returns {string}
 */
export function formatLocalDate(utcIso) {
  if (!utcIso) return "";
  try {
    const d = new Date(utcIso);
    if (isNaN(d.getTime())) return utcIso;
    return d.toLocaleString(undefined, {
      month: "short",
      day: "numeric",
      hour: "2-digit",
      minute: "2-digit",
    });
  } catch {
    return utcIso;
  }
}

// The dashboard's keys are English-only (i18n.js), so the clock is pinned for
// shape and the unit test can assert exact clock strings. The zone NAME is
// not: it is resolved with the viewer's own locale, so an operator in Jakarta
// reads "WIB" while a US viewer reads "PDT".
const CLOCK_LOCALE = "en-US";

/**
 * Short zone label at an instant ("WIB", "PDT", "GMT+7"), resolved with the
 * viewer's locale. A wire display string like "15:04 Jan 2" names no year, so
 * its label is read at whatever instant the runtime makes of it — which is
 * that date's real offset in the zone it belongs to. A stamp the runtime
 * cannot parse at all is labeled as of now. "" when the runtime cannot name
 * the zone.
 * @param {Date|string|number} [at]
 * @param {{ timeZone?: string, locale?: string }} [opts] IANA zone (omitted =
 *   the viewer's own) and locale (tests pin it)
 * @returns {string}
 */
export function zoneLabel(at, opts = {}) {
  const { timeZone, locale } = opts;
  const d = at instanceof Date ? at : new Date(at);
  const when = isNaN(d.getTime()) ? new Date() : d;
  try {
    const parts = new Intl.DateTimeFormat(locale, {
      timeZone,
      timeZoneName: "short",
    }).formatToParts(when);
    return parts.find((p) => p.type === "timeZoneName")?.value ?? "";
  } catch {
    return "";
  }
}

/**
 * Format an ISO instant as a date + clock on the viewer's own wall clock plus
 * its short zone label, e.g. "Sep 21, 02:00 AM (WIB)". Reset/refill stamps
 * arrive as absolute UTC on the wire; the label travels with the clock so a
 * local time is never mistaken for the reset zone's.
 * @param {string} utcIso
 * @param {{ timeZone?: string, locale?: string }} [opts] explicit IANA zone
 *   and zone-name locale (tests)
 * @returns {string} "" for empty input, the input verbatim when unparseable
 */
export function formatLocalDateTime(utcIso, opts = {}) {
  if (!utcIso) return "";
  const { timeZone, locale } = opts;
  try {
    const d = new Date(utcIso);
    if (isNaN(d.getTime())) return utcIso;
    const stamp = d.toLocaleString(CLOCK_LOCALE, {
      month: "short",
      day: "numeric",
      hour: "2-digit",
      minute: "2-digit",
      timeZone,
    });
    const zone = zoneLabel(d, { timeZone, locale });
    return zone ? `${stamp} (${zone})` : stamp;
  } catch {
    return utcIso;
  }
}

/**
 * Format an ISO timestamp to a local time string (HH:MM:SS).
 * @param {string} ts
 * @returns {string}
 */
export function formatTime(ts) {
  if (!ts) return "";
  try {
    const d = new Date(ts);
    if (isNaN(d.getTime())) return ts;
    return d.toLocaleTimeString(undefined, {
      hour: "2-digit",
      minute: "2-digit",
      second: "2-digit",
    });
  } catch {
    return ts;
  }
}

/**
 * Parse structured log fields string into key-value pairs.
 * Fields are separated by double-space, key=value format.
 * @param {string} fields
 * @returns {Array<{key: string, value: string}>}
 */
export function parseLogFields(fields) {
  if (!fields) return [];
  return fields
    .split("  ")
    .filter(Boolean)
    .map((f) => {
      const [k, ...v] = f.split("=");
      return { key: k, value: v.join("=") };
    });
}

/**
 * Generate a random client API key with sk-fb- prefix.
 * @returns {string}
 */
export function generateRandomApiKey() {
  const bytes = new Uint8Array(16);
  crypto.getRandomValues(bytes);
  const hex = Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join(
    "",
  );
  return `sk-fb-${hex}`;
}
