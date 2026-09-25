import { getEnvValue } from "./env.js";

/**
 * DEVTOOLS_ENABLED gate (issue #287): the single predicate shared by the
 * sidebar's Dev Tools tab, the DevTools page self-check, and the Tokens
 * per-token session-spawn toolbar. True when the .env value is the literal
 * 'true' or '1' (case-insensitive), matching the old regex/trims at each site.
 *
 * @param {string} envContent - Raw .env document text.
 * @returns {boolean}
 */
export function isDevToolsEnabled(envContent) {
  const val = (getEnvValue(envContent, "DEVTOOLS_ENABLED") || "").toLowerCase();
  return val === "true" || val === "1";
}

/**
 * Live-config DEVTOOLS_ENABLED gate (unified store): prefers the effective[]
 * snapshot from GET /admin/api/config (mem truth, so an overlay save flips
 * the gate without a restart) and falls back to the .env export parse when
 * the snapshot carries no DEVTOOLS_ENABLED row (older gateway).
 *
 * @param {any} cfgRes - Parsed GET /admin/api/config payload.
 * @returns {boolean}
 */
export function isDevToolsEnabledFromConfig(cfgRes) {
  const effective = cfgRes?.effective;
  if (Array.isArray(effective)) {
    const row = effective.find((kv) => kv?.key === "DEVTOOLS_ENABLED");
    if (row !== undefined) {
      const val = String(row.value ?? "").toLowerCase();
      return val === "true" || val === "1";
    }
  }
  return isDevToolsEnabled(cfgRes?.env_content || "");
}
