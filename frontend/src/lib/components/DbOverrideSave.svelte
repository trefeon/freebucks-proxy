<script>
  import { onDestroy } from "svelte";
  import { RotateCcw } from "@lucide/svelte";
  import { postAPI } from "../api/client.js";
  import { notePendingSave, clearPendingSave } from "../stores/settings.js";
  import { adminApi } from "../api/paths.js";
  import { tr } from "../i18n.js";

  /**
   * Instant per-key save (global instant-save migration).
   *
   * Watches `value`: once the row's control stops changing it for
   * WRITE_DELAY_MS, the value is POSTed to /admin/api/settings and the
   * parent refetches, so badges and effective values agree again. There is
   * no Save button — the inline status (Saving… / the server message /
   * error + Retry) is the only write affordance besides Reset.
   *
   * A rejected value (POST 400, e.g. a malformed lock map) surfaces inline
   * on the row, never as a page error. Restart-only keys keep their honest
   * copy: the server message already says the value applies after restart.
   *
   * Also hosts the saved-value reset + source note for `db`/`env` rows, so
   * each row shows Reset + note + status as one right-aligned cluster.
   * Reset issues DELETE /admin/api/settings/:key through the parent's
   * onReset and the parent refetches. Default/file rows (empty source)
   * render the status alone.
   *
   * @prop {string} settingKey - catalog key (e.g. "LOG_LEVEL")
   * @prop {string} value - current form value for the key
   * @prop {boolean} [restartOnly=false] - key applies after restart; the
   *   success note says so honestly instead of implying live-apply
   * @prop {(() => Promise<void>) | null} [onSaved=null] - parent refetch
   * @prop {string} [source=""] - settings source tier ("db", "env", or "")
   * @prop {(key: string) => Promise<void>} [onReset=null] - parent reset handler
   * @prop {boolean} [degraded=false] - settings store offline: writes stay
   *   disabled and an honest offline note renders instead of the status
   */
  let {
    settingKey,
    value,
    restartOnly = false,
    onSaved = null,
    source = "",
    onReset = null,
    degraded = false,
  } = $props();

  // Coalescing window: one POST per interaction burst (typed text, slider
  // drags, stepper taps), not one per keystroke.
  const WRITE_DELAY_MS = 400;

  let saving = $state(false);
  let resetting = $state(false);
  let status = $state(null); // { ok, text } — inline row outcome
  // Baseline set on mount and never POSTed; every later change writes.
  let lastSent = $state(undefined);
  // Set around Reset so the refetch-driven display revert is adopted, not
  // re-posted (which would resurrect the just-deleted overlay row).
  let suppress = false;
  let timer = null;
  let pending = null;
  let inflight = false;
  let queued = null;

  $effect(() => {
    const v = value;
    if (lastSent === undefined) {
      lastSent = v;
      return;
    }
    if (suppress) {
      lastSent = v;
      return;
    }
    if (degraded) return;
    if (v === lastSent) return;
    // A blank display with no overlay row is the post-reset state (or an
    // untouched default): POSTing it can never succeed — the gateway 400s
    // empty writes — so adopt it instead of firing a doomed save that
    // would resurrect the just-deleted row as an error. Rows that still
    // hold a saved value keep the honest 400 path in fire() below.
    if ((v ?? "").trim() === "" && source !== "db") {
      lastSent = v;
      status = null;
      return;
    }
    schedule(v);
  });

  function schedule(v) {
    clearTimeout(timer);
    pending = v;
    timer = setTimeout(() => {
      timer = null;
      pending = null;
      if (!suppress && !degraded && v !== lastSent) fire(v);
    }, WRITE_DELAY_MS);
  }
  async function fire(v) {
    if (degraded) return;
    // Same guard as the watcher: a blank with no overlay row (e.g. the
    // source tier flipped db -> default between schedule and fire) must
    // never POST — adopt it instead.
    if ((v ?? "").trim() === "" && source !== "db") {
      lastSent = v;
      status = null;
      return;
    }
    if (inflight) {
      queued = v;
      return;
    }
    inflight = true;
    saving = true;
    status = null;
    // Pin the display through the post-save refetch: a concurrent row's
    // refetch can observe a GET that ran before this POST landed and
    // revert the display to the file default, which this row would then
    // re-POST. Cleared in the finally once the refetch has settled.
    notePendingSave(settingKey, v);
    try {
      const res = await postAPI(adminApi.settingsSave, {
        key: settingKey,
        value: v ?? "",
      });
      lastSent = v;
      status = {
        ok: true,
        text: res?.message || $tr("Saved."),
      };
      await onSaved?.();
    } catch (e) {
      status = { ok: false, text: e.message || $tr("Save failed") };
    } finally {
      clearPendingSave(settingKey);
      saving = false;
      inflight = false;
      if (queued !== null && queued !== lastSent) {
        const q = queued;
        queued = null;
        fire(q);
      } else {
        queued = null;
      }
    }
  }

  function retry() {
    status = null;
    fire(value);
  }

  async function reset() {
    if (resetting || !onReset) return;
    resetting = true;
    suppress = true;
    try {
      await onReset(settingKey);
      lastSent = value;
    } finally {
      suppress = false;
      resetting = false;
    }
  }

  onDestroy(() => {
    clearTimeout(timer);
    timer = null;
    // Flush a pending write: navigating away mid-debounce must not drop it.
    if (pending !== null && !suppress && !degraded && pending !== lastSent) {
      const v = pending;
      pending = null;
      fire(v);
    }
  });

  let showRestartCopy = $derived(
    restartOnly && !saving && status?.ok && !/restart/i.test(status.text ?? ""),
  );
</script>

<div class="flex flex-wrap items-center justify-end gap-2">
  {#if degraded}
    <span class="text-[10px] text-[var(--fp-dim)] lowercase shrink-0"
      >{$tr("overlay offline — per-key save unavailable")}</span
    >
  {:else}
    {#if onReset && source === "db"}
      <button
        type="button"
        class="text-[10px] font-semibold uppercase tracking-wider shrink-0 inline-flex items-center gap-1 text-[var(--fp-muted)] hover:text-[var(--fp-text)] disabled:opacity-50 cursor-pointer"
        onclick={reset}
        disabled={resetting}
        title={$tr(
          "Remove the saved value; the setting falls back to its default",
        )}
      >
        <RotateCcw size={11} />
        {$tr("Reset")}
      </button>
    {/if}
    {#if source === "db"}
      <span class="text-[10px] text-[var(--fp-dim)] lowercase shrink-0"
        >{$tr("saved value")}</span
      >
    {:else if source === "env"}
      <span
        class="text-[10px] text-[var(--fp-dim)] lowercase shrink-0"
        title={$tr(
          "A saved value would lose: the process environment wins until it is unset.",
        )}>{$tr("overridden by process env")}</span
      >
    {/if}
    {#if saving}
      <span role="status" class="text-[11px] text-[var(--fp-dim)]"
        >{$tr("Saving…")}</span
      >
    {:else if status}
      <span
        role="status"
        class="text-[11px] {status.ok
          ? 'text-[var(--fp-success)]'
          : 'text-[var(--fp-error)]'}"
        >{status.text}{#if showRestartCopy}
          {$tr("Applies after restart.")}{/if}</span
      >
      {#if !status.ok}
        <button
          type="button"
          class="text-[10px] font-semibold uppercase tracking-wider shrink-0 inline-flex items-center gap-1 text-[var(--fp-muted)] hover:text-[var(--fp-text)] cursor-pointer"
          onclick={retry}
        >
          {$tr("Retry")}
        </button>
      {/if}
    {/if}
  {/if}
</div>
