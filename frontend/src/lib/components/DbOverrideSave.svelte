<script>
  import { RotateCcw } from "@lucide/svelte";
  import { postAPI } from "../api/client.js";
  import { adminApi } from "../api/paths.js";
  import { tr } from "../i18n.js";

  /**
   * Per-key save affordance (ADR-0019).
   *
   * Persists the row's current form value via POST /admin/api/settings
   * and asks the parent to refetch, so badges and effective values agree
   * again. A rejected value (POST 400, e.g. a malformed lock map)
   * surfaces inline on the row, never as a page error.
   *
   * Also hosts the saved-value reset + source note for `db`/`env` rows,
   * so each row shows Reset + note + Save as one right-aligned cluster.
   * Reset issues DELETE /admin/api/settings/:key through the parent's
   * onReset and the parent refetches. Default/file rows (empty source)
   * render Save alone.
   *
   * @prop {string} settingKey - catalog key (e.g. "LOG_LEVEL")
   * @prop {string} value - current form value for the key
   * @prop {boolean} [restartOnly=false] - key applies after restart; the
   *   success note says so honestly instead of implying live-apply
   * @prop {(() => Promise<void>) | null} [onSaved=null] - parent refetch
   * @prop {string} [source=""] - settings source tier ("db", "env", or "")
   * @prop {(key: string) => Promise<void>} [onReset=null] - parent reset handler
   */
  let {
    settingKey,
    value,
    restartOnly = false,
    onSaved = null,
    source = "",
    onReset = null,
  } = $props();

  let saving = $state(false);
  let resetting = $state(false);
  let status = $state(null); // { ok, text } — inline row outcome

  async function save() {
    if (saving) return;
    saving = true;
    status = null;
    try {
      const res = await postAPI(adminApi.settingsSave, {
        key: settingKey,
        value: value ?? "",
      });
      status = {
        ok: true,
        text: res?.message || $tr("Saved."),
      };
      await onSaved?.();
    } catch (e) {
      status = { ok: false, text: e.message || $tr("Save failed") };
    } finally {
      saving = false;
    }
  }

  async function reset() {
    if (resetting || !onReset) return;
    resetting = true;
    try {
      await onReset(settingKey);
    } finally {
      resetting = false;
    }
  }
</script>

<div class="flex flex-wrap items-center justify-end gap-2">
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
    <span class="text-[10px] text-[var(--fp-dim)] lowercase shrink-0"
      >{$tr("from environment")}</span
    >
  {/if}
  <button
    type="button"
    class="text-[10px] font-semibold uppercase tracking-wider shrink-0 inline-flex items-center gap-1 text-[var(--fp-muted)] hover:text-[var(--fp-text)] disabled:opacity-50 cursor-pointer"
    onclick={save}
    disabled={saving}
    title={$tr("Save this setting")}
  >
    {$tr("Save")}
  </button>
  {#if status}
    <span
      role="status"
      class="text-[11px] {status.ok
        ? 'text-[var(--fp-success)]'
        : 'text-[var(--fp-error)]'}"
      >{status.text}{#if status.ok && restartOnly}
        {$tr("Applies after restart.")}{/if}</span
    >
  {/if}
</div>
