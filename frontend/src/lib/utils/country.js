import { get } from "svelte/store";
import { tr } from "../i18n.js";

// Country helpers for the per-account region view. The pool snapshot carries
// the last admitted country plus the remembered 403 country_blocked state
// (CountryCode / CountryBlockReason); the dashboard serves them as the live
// keys `country_code` / `country_block_reason` (omitempty, so older servers
// simply omit both and every helper below degrades to "unknown").
//
// Server-side truth (NOT-FEASIBLE verdict): the server resolves country from
// the request egress IP only — no client-sent knob can force it. The UI
// therefore never offers a fix action, only the layer to fix: an anonymized
// or unresolvable egress (proxy/VPN/relay/hosting exit) vs the account-level
// floor that persists after moving (human web verify flow, surfaced as text
// only — never automated).

function t() {
  return get(tr);
}

/** Upper-cased country code, or "" when the payload carries none. */
export function countryCodeOf(token) {
  const raw = token?.country_code ?? token?.countryCode ?? "";
  return String(raw || "")
    .trim()
    .toUpperCase();
}

/** Raw block reason string, or "" when the account is not country-blocked. */
export function countryReasonOf(token) {
  const raw = token?.country_block_reason ?? token?.countryBlockReason ?? "";
  return String(raw || "").trim();
}

/** True once the server has ever resolved this account to a US egress. */
export function hasCountry(token) {
  return countryCodeOf(token) !== "";
}

/** True when the resolved country is anything but US. */
export function isNonUS(token) {
  const code = countryCodeOf(token);
  return code !== "" && code !== "US";
}

/** True when a country_blocked reason is remembered for this account. */
export function isCountryBlocked(token) {
  return countryReasonOf(token) !== "";
}

/**
 * Per-account country badge, or null when the server reported no country.
 * US reads good, non-US reads warn — a non-US egress loses the Tier-1
 * full-access seat and the US-or-paid models, so it must never read idle.
 */
export function countryBadgeFor(token) {
  const code = countryCodeOf(token);
  if (!code) return null;
  if (code === "US") {
    return {
      label: t()("US"),
      aria: t()("Upstream country: United States"),
      tone: "good",
    };
  }
  return {
    label: code,
    aria: t()("Upstream country: {code} (not US)", { code }),
    tone: "warn",
  };
}

/**
 * Actionable layer for one CountryBlockReason. Returns { layer, detail }:
 * layer is "egress" (fix the exit path: clean, directly-attributed egress
 * with no VPN/proxy/relay/hosting signals) or "account" (the access-cache
 * floor pinned the old country's limits — clear it in the human web flow).
 */
export function countryAdviceFor(reason) {
  const r = String(reason || "").trim();
  switch (r) {
    case "recent_limited_country":
      return {
        layer: "account",
        detail: t()(
          "Account floor: this account was seen from a limited country and keeps that country's limits for a window even on clean egress. Clear it in the human web verify flow at /account?tab=country (manual step in the browser — never automated).",
        ),
      };
    case "anonymized_or_unknown_country":
    case "anonymous_network":
      return {
        layer: "egress",
        detail: t()(
          "Egress path: upstream saw anonymized or relayed egress (VPN, proxy, relay, or hosting exit). Re-admit over a clean, directly-attributed egress IP with no privacy signals.",
        ),
      };
    case "missing_client_ip":
    case "unresolved_client_ip":
    case "ip_privacy_lookup_failed":
      return {
        layer: "egress",
        detail: t()(
          "Egress path: upstream could not resolve a client country from the egress IP, so it fails closed to limited. Check the egress/proxy chain preserves a directly-attributed IP.",
        ),
      };
    case "country_not_allowed":
      return {
        layer: "egress",
        detail: t()(
          "Egress region: the exit country is outside the upstream allowlist. Move egress to the US for the Tier-1 full-access seat.",
        ),
      };
    default:
      return {
        layer: "egress",
        detail: t()(
          "Egress path: upstream reported a region block ({reason}). Re-admit over clean US egress; if it persists on clean egress, clear the account floor in the human web verify flow at /account?tab=country.",
          { reason: r || t()("unknown") },
        ),
      };
  }
}

/**
 * Fleet summary for the Tokens-page banner. Returns { nonUS, blocked } with
 * per-account entries { idx, code, reason, advice } in pool order. Empty
 * arrays when there is nothing to warn about (all-US or all-unknown fleet).
 */
export function fleetCountrySummary(tokens) {
  const nonUS = [];
  const blocked = [];
  for (const token of tokens ?? []) {
    const idx = token?.index ?? -1;
    const code = countryCodeOf(token);
    const reason = countryReasonOf(token);
    if (code !== "" && code !== "US") nonUS.push({ idx, code });
    if (reason !== "")
      blocked.push({ idx, code, reason, advice: countryAdviceFor(reason) });
  }
  return { nonUS, blocked };
}
