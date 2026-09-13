<script>
  import { RotateCcw } from "@lucide/svelte";
  import { tr } from "../i18n.js";

  /**
   * Saved-value reset + source note (ADR-0019).
   *
   * Rendered inside a SettingsRow badge slot when the settings source map
   * reports `db` or `env` for the row's key. For `db` rows the effective
   * value is saved and a Reset control clears it back to the default;
   * reset issues DELETE /admin/api/settings/:key through the parent's
   * onReset and the parent refetches, so this component holds no data
   * state of its own. For `env` rows there is nothing to reset, so only
   * the muted source note renders. Default/file rows render nothing and
   * should not mount this component.
   *
   * @prop {string} settingKey - catalog key (e.g. "SAFE_MODE")
   * @prop {string} [source="db"] - settings source tier ("db" or "env")
   * @prop {(key: string) => Promise<void>} [onReset] - parent reset handler
   */
  let { settingKey, source = "db", onReset = null } = $props();

  let resetting = $state(false);

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

{#if onReset && source === "db"}
  <button
    type="button"
    class="text-[10px] font-semibold uppercase tracking-wider shrink-0 inline-flex items-center gap-1 text-[var(--fp-muted)] hover:text-[var(--fp-text)] disabled:opacity-50 cursor-pointer"
    onclick={reset}
    disabled={resetting}
    title={$tr("Remove the saved value; the setting falls back to its default")}
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
