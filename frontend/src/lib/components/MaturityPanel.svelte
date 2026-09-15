<script>
  import { onMount } from "svelte";
  import { recordPageVisit } from "../stores/pageState.js";
  import Card from "./Card.svelte";
  import Alert from "./Alert.svelte";
  import StatusBadge from "./StatusBadge.svelte";
  import ToggleSwitch from "./ToggleSwitch.svelte";
  import DbOverrideSave from "./DbOverrideSave.svelte";
  import { postAPI, fetchAPI } from "../api/client.js";
  import { adminApi } from "../api/paths.js";
  import {
    tokensData as tokensStore,
    tokensError as tokensErrorStore,
    ensureTokensStore,
    refreshTokens,
  } from "../stores/tokens.js";
  import {
    touchOptions as sharedTouchOptions,
    touchLabel,
  } from "../utils/touchModels.js";
  import { tr } from "../i18n.js";

  /**
   * Streak Maintenance board: universal automatic, one switch plus the
   * touch-model row below it. The global kill-switch is the master control;
   * touch-model (MATURITY_TOUCH_MODEL) moved here from Pool Tuning so the
   * whole streak surface sits on the Warming tab. The row instant-saves to
   * the DB overlay on edit (DbOverrideSave); it dims while the kill-switch
   * is off. Ledger rows stay read-only status.
   * @prop {Record<string, string>} [formValues={}] - live settings values
   * @prop {(key: string, value: string) => void} [onField] - value edit
   * @prop {Record<string, string>} [sources={}] - ADR-0019 source tiers
   * @prop {(key: string) => Promise<void>} [onReset=null] - saved-value reset
   * @prop {(() => Promise<void>) | null} [onSaved=null] - parent refetch
   * @prop {boolean} [degraded=false] - settings store offline note
   */
  let {
    formValues = {},
    onField = null,
    sources = {},
    onReset = null,
    onSaved = null,
    degraded = false,
  } = $props();

  let data = $state(null);
  let loading = $state(true);
  let error = $state("");
  let unsubStore = null;
  let unsubErr = null;

  // Global kill-switch (MATURITY_ENABLED, default true): the master streak
  // control. The editable touch-model row reads the shared settings draft.
  let globalEnabled = $state(true);
  let globalLoaded = $state(false);
  let savingGlobal = $state(false);
  // Editable tuning row (instant overlay save beside the control):
  // touch-model defaults "" (= auto, cheapest unmetered). The select shows
  // "auto" for both "" and a literal "auto" (older overlays stored the
  // word), while edits canonicalize Auto back to "" so the draft always
  // matches the catalog default. The row dims while the kill-switch is off.
  let touchVal = $derived(formValues.MATURITY_TOUCH_MODEL ?? "");
  let touchSelectVal = $derived(
    touchVal === "" || touchVal === "auto" ? "auto" : touchVal,
  );
  let maturityOff = $derived(globalLoaded && !globalEnabled);
  let modelRows = $state([]);
  function touchOpts() {
    return sharedTouchOptions(modelRows, touchSelectVal);
  }
  // Tonight's maintenance window (RFC3339 absolute instants from the
  // payload): the next-run countdown formats these, so the window math
  // lives in one DST-safe place server-side.
  let windowStart = $state("");
  let windowEnd = $state("");
  // Wall clock for the next-run countdown (30s tick; the 10s poll also
  // refreshes it). One interval for the whole panel, cleared on unmount.
  let nowMs = $state(Date.now());
  let countdownTimer = null;

  function applyTokens(v) {
    if (!v) return;
    data = v;
    if (v.maturity_enabled !== undefined) {
      globalEnabled = Boolean(v.maturity_enabled);
    }
    if (typeof v.maturity_window_start === "string") {
      windowStart = v.maturity_window_start;
    }
    if (typeof v.maturity_window_end === "string") {
      windowEnd = v.maturity_window_end;
    }
    globalLoaded = true;
    error = "";
    loading = false;
  }

  // Universal on/off switch writes the global kill-switch through the
  // settings overlay (same path as Settings → Advanced, hot-applied).
  async function setGlobalEnabled(next) {
    if (savingGlobal) return;
    savingGlobal = true;
    try {
      const res = await postAPI(adminApi.settingsSave, {
        key: "MATURITY_ENABLED",
        value: next ? "true" : "false",
      });
      if (res && res.ok === false)
        throw new Error(res.message || "Save rejected");
      globalEnabled = next;
      await refreshTokens();
    } catch {
      globalEnabled = !next;
      await refreshTokens();
    } finally {
      savingGlobal = false;
    }
  }
  function fmtTime(iso) {
    if (!iso) return "—";
    const d = new Date(iso);
    return isNaN(d) ? "—" : d.toLocaleString();
  }

  function fmtCountdown(ms) {
    if (!isFinite(ms) || ms <= 0) return "due now";
    const s = Math.floor(ms / 1000);
    const h = Math.floor(s / 3600);
    const m = Math.floor((s % 3600) / 60);
    if (h >= 24) {
      const d = Math.floor(h / 24);
      const hr = h % 24;
      return hr > 0 ? `in ${d}d ${hr}h` : `in ${d}d`;
    }
    if (h > 0) return `in ${h}h ${m}m`;
    if (m > 0) return `in ${m}m`;
    return `in ${s}s`;
  }

  // --- Pacific-midnight fallback (old servers without the window keys) ---
  // Next Pacific midnight via Intl wall-clock math (DST-safe: the offset is
  // re-resolved for the target date, never a fixed hour).
  function laOffsetMinutes(ts) {
    const dtf = new Intl.DateTimeFormat("en-US", {
      timeZone: "America/Los_Angeles",
      hour12: false,
      year: "numeric",
      month: "2-digit",
      day: "2-digit",
      hour: "2-digit",
      minute: "2-digit",
      second: "2-digit",
    });
    const parts = {};
    for (const p of dtf.formatToParts(new Date(ts))) parts[p.type] = p.value;
    const asUTC = Date.UTC(
      Number(parts.year),
      Number(parts.month) - 1,
      Number(parts.day),
      Number(parts.hour) % 24,
      Number(parts.minute),
      Number(parts.second),
    );
    return Math.round((asUTC - ts) / 60000);
  }

  function pacificMidnight(ts, addDays) {
    const dtf = new Intl.DateTimeFormat("en-CA", {
      timeZone: "America/Los_Angeles",
      year: "numeric",
      month: "2-digit",
      day: "2-digit",
    });
    const [y, m, d] = dtf.format(new Date(ts)).split("-").map(Number);
    const base = Date.UTC(y, m - 1, d) + addDays * 86400000;
    const bd = new Date(base);
    const off = laOffsetMinutes(base + 8 * 3600000);
    return (
      Date.UTC(bd.getUTCFullYear(), bd.getUTCMonth(), bd.getUTCDate()) -
      off * 60000
    );
  }

  function fallbackWindow(ts) {
    let end = pacificMidnight(ts, 0);
    if (end <= ts) end = pacificMidnight(ts, 1);
    return { start: end - 60 * 60000, end };
  }

  function runWindow() {
    const s = Date.parse(windowStart);
    const e = Date.parse(windowEnd);
    if (isFinite(s) && isFinite(e) && e > s) return { start: s, end: e };
    return fallbackWindow(nowMs);
  }

  // Universal automatic: every pooled account is covered, no enrollment
  // filter anywhere on this board.
  function coveredTokens() {
    return tokens;
  }

  function touchedToday(t) {
    const m = t?.maturity;
    if (m?.touch_day && m?.slot_day) return m.touch_day === m.slot_day;
    return !!t?.today_used;
  }
  // Single shared skipped definition for rows AND the header count: any
  // ledger skip:* code (including skip:touch-model) reads Skipped.
  function isSkipped(t) {
    return String(t?.maturity?.last_result ?? "").startsWith("skip:");
  }

  function slotPast(t) {
    const slot = Date.parse(t?.maturity?.slot ?? "");
    return isFinite(slot) && slot <= nowMs;
  }

  // Pacific-day label for an instant ("Sep 11"): the day key the streak
  // walk counts, so run times read against the reset that matters.
  function fmtPacificDay(iso) {
    if (!iso) return "";
    const d = new Date(iso);
    if (isNaN(d)) return "";
    return new Intl.DateTimeFormat("en-US", {
      timeZone: "America/Los_Angeles",
      month: "short",
      day: "numeric",
    }).format(d);
  }

  // Where today's usage happened: proxy-routed traffic lands in the local
  // day ledger (requests_per_day); upstream-dated use with none here means
  // the account was used outside this proxy (app, CLI, or direct). A day
  // carrying only the nightly touch reads as automation, not outside use:
  // touches bypass Pool.Chat so they never increment the local ledger.
  function usageSource(t) {
    const n = Number(t?.requests_per_day) || 0;
    if (n > 0) return $tr("used here ({n} today)", { n });
    if (t?.last_usage || !t?.maturity?.last_touch)
      return $tr("used outside this proxy");
    return $tr("nightly touch only");
  }

  function lastActivity(t) {
    return t?.last_usage || t?.maturity?.last_touch || "";
  }

  // Per-account tonight status for the board rows (universal: no enabled
  // gate — a missing ledger simply reads Pending). Skipped carries the
  // human reason plus the exact ledger code plus when/where the account
  // was last active; touched carries the touch time, eligible means
  // due now inside the window, pending means waiting for the slot/window.
  function rowStatus(t) {
    const m = t?.maturity;
    const result = m?.last_result ?? "";
    // Shared skipped gate (see isSkipped): every skip:* code reads Skipped.
    if (isSkipped(t)) {
      if (result === "skip:today-used") {
        const when = fmtPacificDay(lastActivity(t));
        return {
          kind: "skipped",
          text: `${$tr("Skipped")} · ${$tr("day already used")}${when ? ` · ${$tr("last activity {day}", { day: when })}` : ""} · ${usageSource(t)} · ${result}`,
        };
      }
      if (result === "skip:client-active") {
        return {
          kind: "skipped",
          text: `${$tr("Skipped")} · ${$tr("you used it today via this proxy")} · ${result}`,
        };
      }
      return { kind: "skipped", text: `${$tr("Skipped")} · ${result}` };
    }
    if (touchedToday(t)) {
      return {
        kind: "touched",
        text: `${$tr("Touched")} ${fmtTime(m?.last_touch)}`,
      };
    }
    const w = runWindow();
    if (nowMs >= w.start && nowMs < w.end && slotPast(t)) {
      return { kind: "eligible", text: $tr("Eligible tonight") };
    }
    return { kind: "pending", text: $tr("Pending") };
  }

  // Exact model id tonight's touch will admit, resolved server-side from
  // the live meter (manual override → premium-short pool head → auto
  // pick → global fallback). Read path only: no probing here.
  function resolvedModel(t) {
    const m = t?.maturity;
    return m?.effective_touch_model || m?.auto_touch_model || "";
  }

  // Last-run ledger summary across covered accounts: latest touch time,
  // touch count, and skip counts grouped by exact reason.
  function ledgerSummary(list) {
    let touched = 0;
    let latest = "";
    const skips = {};
    for (const t of list) {
      const m = t.maturity;
      if (!m) continue;
      if (m.last_result === "ok") touched += 1;
      else if ((m.last_result ?? "").startsWith("skip:")) {
        skips[m.last_result] = (skips[m.last_result] ?? 0) + 1;
      }
      if (m.last_touch && (!latest || m.last_touch > latest)) {
        latest = m.last_touch;
      }
    }
    const skipped = Object.values(skips).reduce((a, b) => a + b, 0);
    const reasons = Object.entries(skips)
      .sort(([a], [b]) => (a < b ? -1 : 1))
      .map(([reason, n]) => (n > 1 ? `${reason} ×${n}` : reason));
    return { touched, skipped, reasons, latest };
  }
  function nextReset() {
    const r = pacificMidnight(nowMs, 0);
    return r > nowMs ? r : pacificMidnight(nowMs, 1);
  }
  function countdownText() {
    const w = runWindow();
    const skipped = coveredTokens().filter(isSkipped).length;
    const eligible = coveredTokens().length - skipped;
    const counts = `${eligible} eligible · ${skipped} skipped`;
    const reset = ` · ${$tr("reset")} ${fmtCountdown(nextReset() - nowMs)}`;
    if (nowMs >= w.start && nowMs < w.end) {
      return `In window · ends ${fmtCountdown(w.end - nowMs)} · ${counts}${reset}`;
    }
    return `Next run ${fmtCountdown(w.start - nowMs)} · ${counts}${reset}`;
  }

  onMount(() => {
    recordPageVisit("maturity");
    // Served-model catalog for the touch-model select (shared
    // utils/touchModels.js, priced labels kept).
    (async () => {
      try {
        const res = await fetchAPI(adminApi.models);
        modelRows = res?.models ?? [];
      } catch {
        modelRows = [];
      }
    })();
    countdownTimer = setInterval(() => {
      nowMs = Date.now();
    }, 30000);
    const release = ensureTokensStore();
    unsubStore = tokensStore.subscribe(applyTokens);
    unsubErr = tokensErrorStore.subscribe((err) => {
      if (err) {
        error = err;
        loading = false;
      }
    });
    function onConfigSaved() {
      refreshTokens();
    }
    window.addEventListener("fp-config-saved", onConfigSaved);
    return () => {
      if (countdownTimer) clearInterval(countdownTimer);
      countdownTimer = null;
      release();
      unsubStore?.();
      unsubErr?.();
      window.removeEventListener("fp-config-saved", onConfigSaved);
    };
  });

  const tokens = $derived(data?.tokens ?? []);
  const summary = $derived(ledgerSummary(coveredTokens()));
</script>

{#if loading}
  <div class="flex flex-col gap-3" aria-hidden="true">
    <div class="skeleton skeleton-text w-1/3"></div>
    <div class="skeleton skeleton-line"></div>
    <div class="skeleton skeleton-line"></div>
  </div>
{:else if error}
  <div class="flex flex-col items-start gap-2">
    <Alert tone="error" title={error} />
  </div>
{:else}
  <Card
    title={$tr("Streak Maintenance")}
    description={$tr(
      "Fully automatic: every account is touched nightly. The switch plus the touch-model row below are the only controls.",
    )}
  >
    {#snippet actions()}
      <span class="flex shrink-0 flex-nowrap items-center gap-1.5">
        {#if globalLoaded && !globalEnabled}
          <StatusBadge tone="bad" status={$tr("Off")} />
        {/if}
      </span>
    {/snippet}
    <div class="flex flex-col gap-2.5">
      <div class="flex flex-wrap items-center gap-x-4 gap-y-2">
        <ToggleSwitch
          checked={globalEnabled}
          disabled={!globalLoaded || !!savingGlobal}
          saving={!!savingGlobal}
          ariaLabel={$tr("Streak maintenance")}
          onchange={(next) => setGlobalEnabled(next)}
        />
        <span class="text-xs font-medium text-[var(--fp-text)]"
          >{$tr("Streak maintenance")}</span
        >
      </div>
      <div
        class="flex flex-col gap-2 border-t border-[var(--fp-border)]/60 pt-2.5 {maturityOff
          ? 'opacity-60'
          : ''}"
      >
        <div class="flex flex-wrap items-center gap-x-3 gap-y-2">
          <span class="text-xs font-medium text-[var(--fp-text)]"
            >{$tr("Touch model")}</span
          >
          <code
            class="text-[10px] px-1.5 py-0.5 rounded bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-mono"
            >MATURITY_TOUCH_MODEL</code
          >
          <select
            class="fp-select"
            value={touchSelectVal}
            aria-label="MATURITY_TOUCH_MODEL"
            title={touchSelectVal}
            onchange={(e) => {
              const raw = e.currentTarget.value;
              onField?.("MATURITY_TOUCH_MODEL", raw === "auto" ? "" : raw);
            }}
          >
            <option value="auto">Auto (cheapest unmetered)</option>
            {#each touchOpts() as opt (opt.id)}
              <option value={opt.id}>{touchLabel(opt)}</option>
            {/each}
          </select>
          <span class="ml-auto">
            <DbOverrideSave
              settingKey="MATURITY_TOUCH_MODEL"
              value={touchVal}
              source={sources.MATURITY_TOUCH_MODEL}
              {onReset}
              {onSaved}
              {degraded}
            />
          </span>
        </div>
      </div>
      <p class="fp-num text-[11px] leading-relaxed text-[var(--fp-dim)]">
        {$tr("Nightly window 23:45–00:00 Pacific")}
        ·
        {$tr(
          "one touch per Pacific day, placed in the final 15 minutes before reset to rescue the expiring day",
        )}
        ·
        {$tr("client request activity since the last Pacific reset skips")}
      </p>
      <p
        class="fp-num text-xs text-[var(--fp-muted)]"
        aria-label={$tr("Next maintenance run")}
      >
        {countdownText()}
      </p>
      {#if globalLoaded && !globalEnabled}
        <Alert
          tone="warning"
          title={$tr("Maturity automation is globally off")}
        >
          {$tr(
            "Turn Streak maintenance on in Settings — the row below stays put while the kill-switch is off.",
          )}
        </Alert>
      {/if}
      <div
        class="flex flex-col gap-1 border-t border-[var(--fp-border)]/60 pt-2.5"
        aria-label={$tr("Last maintenance run")}
      >
        <p class="fp-num text-[11px] text-[var(--fp-dim)]">
          {$tr("Last run")}
          {fmtTime(summary.latest)}{fmtPacificDay(summary.latest)
            ? ` · ${$tr("for the {day} Pacific day", { day: fmtPacificDay(summary.latest) })}`
            : ""} · {$tr("touched")}
          {summary.touched}
          · {$tr("skipped")}
          {summary.skipped}
        </p>
        {#if summary.reasons.length > 0}
          <p class="fp-num text-[11px] text-[var(--fp-dim)]">
            {summary.reasons.join(" · ")}
          </p>
        {/if}
      </div>
      {#if tokens.length === 0}
        <p class="text-sm text-[var(--fp-dim)]">{$tr("No pooled tokens")}</p>
      {:else}
        <div class="flex flex-col gap-1.5">
          {#each tokens as t (t.index ?? t.email)}
            {@const idx = t.index ?? 0}
            {@const st = rowStatus(t)}
            {@const model = resolvedModel(t)}
            <div
              class="flex flex-col gap-1 border-t border-[var(--fp-border)]/60 pt-2"
            >
              <div class="flex flex-wrap items-center gap-x-3 gap-y-1">
                <span class="min-w-0">
                  <span
                    class="fp-num text-xs font-semibold text-[var(--fp-text)]"
                    >{$tr("Account #{idx}", { idx: idx + 1 })}</span
                  >
                  {#if t.email}
                    <span
                      class="ml-1.5 text-[11px] text-[var(--fp-muted)] truncate"
                      title={t.email}>{t.email}</span
                    >
                  {/if}
                </span>
                {#if t.locked}
                  <StatusBadge tone="warn" status={$tr("Locked")} />
                {/if}
                {#if model}
                  <code
                    class="fp-num ml-auto text-[11px] text-[var(--fp-muted)]"
                    title={$tr("Touch model for tonight")}>{model}</code
                  >
                {/if}
              </div>
              {#if st.kind === "skipped"}
                <StatusBadge tone="warn" status={st.text} />
              {:else}
                <span class="fp-num text-[11px] text-[var(--fp-dim)]"
                  >{st.text}</span
                >
              {/if}
            </div>
          {/each}
        </div>
      {/if}
    </div>
  </Card>
{/if}
