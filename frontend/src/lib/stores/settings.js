import { writable, get } from "svelte/store";
import { fetchAPI, deleteAPI } from "../api/client.js";
import { adminApi } from "../api/paths.js";
import { refreshTokens } from "./tokens.js";
import { parseEnv } from "../utils/env.js";
import { push as pushToast, dismiss as dismissToast } from "./toast.js";
import { tr } from "../i18n.js";

function t(key, params) {
  try {
    return get(tr)(key, params);
  } catch {
    return key;
  }
}

// ---------------------------------------------------------------------------
// Shared settings state: the .env document — displayed, never written here.
// Writes go through the DB-overlay instant-save path (DbOverrideSave POSTs the
// one key it owns); the whole-file .env write lives only in the Client API
// Keys editor (POST /admin/config, used to add/remove API_KEYS). Every tunable
// row writes its key straight to the overlay the moment it is touched — there
// is no batched draft, so setField only updates the live display value and
// never marks anything dirty.
// ---------------------------------------------------------------------------
export const meta = writable([]);
export const configData = writable(null);
export const loading = writable(true);
export const error = writable("");

export const rawText = writable("");

export const formValues = writable({});
export const effectiveMap = writable(new Map());
export const settingSources = writable({});
export const settingsDegraded = writable(false);
const baseContent = writable("");

// Last-known overlay values (key -> saved display value) for source=db
// rows. Applied over the file-derived display values so a row always shows
// what is saved, even when the settings endpoint is unreachable on a
// background refresh. Refreshed on every successful settings fetch.
let lastOverlayValues = {};

// The settings result toast: the global toast mirrors every result the
// pages used to render as an inline <Alert> (reset/save outcomes).
// Only one is ever live — a new outcome dismisses the previous receipt.
let lastResultToast = 0;
function notifyResult(r) {
  if (lastResultToast) {
    dismissToast(lastResultToast);
    lastResultToast = 0;
  }
  if (!r) return;
  const restartKeys = Array.isArray(r.restart_only) ? r.restart_only : [];
  lastResultToast = pushToast({
    tone: !r.ok ? "error" : restartKeys.length > 0 ? "warning" : "success",
    title: r.message,
    body:
      r.ok && restartKeys.length > 0
        ? t("Applies after restart: {keys}", { keys: restartKeys.join(", ") })
        : "",
  });
}

function isTruthy(v) {
  return v === "true" || v === "1" || v === "on" || v === "yes";
}

function displayFor(entry, raw) {
  if (entry && entry.kind === "bool") return isTruthy(raw) ? "true" : "false";
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

// Overlay wins the display: a key with a saved row shows the saved value,
// not the stale file value underneath it. Without this a refetch would both
// show the wrong value and re-trigger the row's instant save with it.
function applyOverlayWins(vals) {
  const byKey = new Map((get(meta) ?? []).map((e) => [e.key, e]));
  for (const [key, value] of Object.entries(lastOverlayValues)) {
    vals[key] = displayFor(byKey.get(key), value);
  }
  return vals;
}
// In-flight instant-save writes (key -> display value). A row registers
// here before POSTing and unregisters after its post-save refetch settles,
// so fetchData preserves the display value of a pending key instead of
// applying file/overlay values. Without this, a refetch triggered by one
// row's save can observe a GET that ran before a concurrent row's POST
// landed, revert the display to the file default — and the reverted row
// would then re-POST the default, clobbering the just-saved value
// (the Drain preset tap wrote 300s/1024, then re-posted 30s/16 ~400ms
// later). Entries are transient (pre-POST to post-refetch); everything
// else keeps the wholesale-apply semantics.
const pendingSaves = {};
export function notePendingSave(key, value) {
  pendingSaves[key] = value;
}
export function clearPendingSave(key) {
  delete pendingSaves[key];
}
function applyDisplayValues(base) {
  const vals = applyOverlayWins(deriveValues(base));
  for (const [key, value] of Object.entries(pendingSaves)) vals[key] = value;
  return vals;
}

export function setField(key, value) {
  formValues.update((vals) => ({ ...vals, [key]: value }));
}

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
    formValues.set(applyDisplayValues($base));
    try {
      const setRes = await fetchAPI(adminApi.settings);
      const next = {};
      const overlayVals = {};
      for (const e of setRes.settings ?? []) {
        next[e.key] = e.source;
        if (e.source === "db") overlayVals[e.key] = e.value;
      }
      settingSources.set(next);
      settingsDegraded.set(setRes.degraded === true);
      lastOverlayValues = overlayVals;
      formValues.set(applyDisplayValues($base));
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
  // A save in flight for this key must not survive the delete: its guard
  // would pin the display to the just-deleted value through the refetch.
  clearPendingSave(key);
  try {
    const res = await deleteAPI(adminApi.settingsDelete(key));
    const resetOutcome = {
      ok: true,
      message: res?.message || t("Saved value removed."),
      restart_only: [],
    };

    notifyResult(resetOutcome);
    await fetchData();
    refreshTokens();
  } catch (e) {
    const resetFailure = {
      ok: false,
      message: e.message || t("Failed to reset saved value"),
      restart_only: [],
    };

    notifyResult(resetFailure);
  }
}

export async function overlaySaved() {
  await fetchData();
  refreshTokens();
}
