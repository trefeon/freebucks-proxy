import { writable, derived, get } from "svelte/store";
import { fetchAPI, postForm, deleteAPI } from "../api/client.js";
import { adminApi, adminActions } from "../api/paths.js";
import { confirmAction } from "./confirm.js";
import { refreshTokens } from "./tokens.js";
import { parseEnv, setEnvValue as setEnvLine } from "../utils/env.js";
import { tr } from "../i18n.js";

function t(key, params) {
  try {
    return get(tr)(key, params);
  } catch {
    return key;
  }
}

// ---------------------------------------------------------------------------
// Shared settings state machine (lifted from Settings.svelte).
// Single ownership for the .env document + DB-overlay save paths so the
// Settings page and the Pool page edit the same draft.
// ---------------------------------------------------------------------------
export const meta = writable([]);
export const configData = writable(null);
export const loading = writable(true);
export const error = writable("");

export const rawText = writable("");
export const baseContent = writable("");
export const formValues = writable({});
export const changedKeys = writable(new Set());
export const effectiveMap = writable(new Map());
export const settingSources = writable({});
export const settingsDegraded = writable(false);
export const saving = writable(false);
export const result = writable(null);

function isTruthy(v) {
  return v === "true" || v === "1" || v === "on" || v === "yes";
}

function serializeFor(entry, val) {
  if (entry.kind === "bool") return isTruthy(val) ? "true" : "false";
  if (entry.kind === "list") {
    return String(val ?? "")
      .split(",")
      .map((s) => s.trim())
      .join(",");
  }
  return String(val ?? "");
}

function displayFor(entry, raw) {
  if (entry.kind === "bool") return isTruthy(raw) ? "true" : "false";
  return raw;
}

function deriveValues(content) {
  const env = parseEnv(content);
  const $meta = get(meta);
  const $effective = get(effectiveMap);
  const vals = {};
  for (const entry of $meta) {
    let raw = env[entry.key];
    if (raw === undefined) {
      raw = $effective.get(entry.key)?.value ?? entry.default ?? "";
    }
    vals[entry.key] = displayFor(entry, raw);
  }
  return vals;
}

function rebuildRaw() {
  const $meta = get(meta);
  const $changed = get(changedKeys);
  const $vals = get(formValues);
  let out = get(rawText);
  for (const entry of $meta) {
    if (!$changed.has(entry.key)) continue;
    out = setEnvLine(out, entry.key, serializeFor(entry, $vals[entry.key]));
  }
  rawText.set(out);
}

export function setField(key, value) {
  formValues.update((vals) => ({ ...vals, [key]: value }));
  const next = new Set(get(changedKeys));
  next.add(key);
  changedKeys.set(next);
  rebuildRaw();
}

export function discard() {
  const $base = get(baseContent);
  rawText.set($base);
  formValues.set(deriveValues($base));
  changedKeys.set(new Set());
  result.set(null);
}

export const dirty = derived(
  [rawText, baseContent],
  ([$raw, $base]) => $raw !== $base,
);

export const changedKeysCount = derived(
  [baseContent, rawText],
  ([$base, $raw]) => {
    const a = parseEnv($base);
    const b = parseEnv($raw);
    const keys = new Set([...Object.keys(a), ...Object.keys(b)]);
    let n = 0;
    for (const k of keys) {
      if ((a[k] ?? "") !== (b[k] ?? "")) n++;
    }
    return n;
  },
);

export const dirtyKeys = derived([baseContent, rawText], ([$base, $raw]) => {
  const a = parseEnv($base);
  const b = parseEnv($raw);
  const keys = new Set([...Object.keys(a), ...Object.keys(b)]);
  return [...keys].filter((k) => (a[k] ?? "") !== (b[k] ?? ""));
});

export function needsRestart(key) {
  return (get(meta) ?? []).some((e) => e.key === key && e.restart_only);
}

export const restartKeys = derived(dirtyKeys, ($keys) =>
  $keys.filter(needsRestart),
);
export const liveKeys = derived(dirtyKeys, ($keys) =>
  $keys.filter((k) => !needsRestart(k)),
);

export async function fetchData() {
  const firstLoad = get(configData) == null;
  if (firstLoad) loading.set(true);
  error.set("");
  try {
    const [metaRes, cfgRes] = await Promise.all([
      fetchAPI(adminApi.configMeta),
      fetchAPI(adminApi.config),
    ]);
    meta.set(Array.isArray(metaRes) ? metaRes : (metaRes?.entries ?? []));
    configData.set(cfgRes);
    baseContent.set(cfgRes.env_content || "");
    const nextMap = new Map();
    for (const kv of cfgRes.effective ?? []) {
      nextMap.set(kv.key, kv);
    }
    effectiveMap.set(nextMap);
    const $base = get(baseContent);
    rawText.set($base);
    formValues.set(deriveValues($base));
    changedKeys.set(new Set());
    try {
      const setRes = await fetchAPI(adminApi.settings);
      const next = {};
      for (const e of setRes.settings ?? []) next[e.key] = e.source;
      settingSources.set(next);
      settingsDegraded.set(setRes.degraded === true);
    } catch {
      // Keep last-known sources on background refresh failure.
    }
  } catch (e) {
    if (firstLoad) error.set(e.message || t("Failed to fetch configuration"));
  } finally {
    if (firstLoad) loading.set(false);
  }
}

export async function resetSetting(key) {
  try {
    const res = await deleteAPI(adminApi.settingsDelete(key));
    result.set({
      ok: true,
      message: res?.message || t("Saved value removed."),
      restart_only: [],
    });
    await fetchData();
    refreshTokens();
  } catch (e) {
    result.set({
      ok: false,
      message: e.message || t("Failed to reset saved value"),
      restart_only: [],
    });
  }
}

export async function overlaySaved() {
  await fetchData();
  refreshTokens();
}

export async function saveConfig(e, opts = {}) {
  if (get(saving) || !get(dirty)) return;
  if (opts.confirm !== false) {
    const ok = await confirmAction({
      title: t("Save Configuration"),
      message: t("Save these settings and reload the proxy with the changes?"),
      confirmText: t("Save & Reload"),
      tone: "warn",
    });
    if (!ok) return;
  }
  saving.set(true);
  result.set(null);
  try {
    const res = await postForm(adminActions.configSave, {
      content: get(rawText),
    });
    const json = await res.json();
    const ok = res.ok && json.ok;
    result.set({
      ok,
      message:
        json.message ||
        (res.ok ? t("Configuration saved and reloaded.") : t("Save failed")),
      restart_only: Array.isArray(json.restart_only) ? json.restart_only : [],
    });
    if (ok) {
      await fetchData();
      refreshTokens();
      if (typeof window !== "undefined") {
        window.dispatchEvent(new CustomEvent("fp-config-saved"));
      }
    } else {
      const $base = get(baseContent);
      rawText.set($base);
      formValues.set(deriveValues($base));
      changedKeys.set(new Set());
    }
  } catch (e) {
    result.set({
      ok: false,
      message: e.message || t("Network error saving configuration"),
      restart_only: [],
    });
  } finally {
    saving.set(false);
  }
}
