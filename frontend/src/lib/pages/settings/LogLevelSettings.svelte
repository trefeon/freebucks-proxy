<script>
  import SettingsCard from "../../components/SettingsCard.svelte";
  import SettingsRow from "../../components/SettingsRow.svelte";
  import DbOverrideSave from "../../components/DbOverrideSave.svelte";
  import { ScrollText } from "@lucide/svelte";
  import { tr } from "../../i18n.js";
  import { parseEnv } from "../../utils/env.js";

  /**
   * Logging settings card (single-key: LOG_LEVEL).
   * Built using the SettingsCard and SettingsRow template components.
   * Mirrors the TrafficSettings Pool contract: stub/degraded/cardTitle
   * props, key-search query + onMatchCount reporting.
   *
   * @prop {Record<string, string>} formValues
   * @prop {string} rawText
   * @prop {(key: string, value: string) => void} onField
   * @prop {Record<string, string>} [sources] - ADR-0019 source tiers
   * @prop {(key: string) => Promise<void>} [onReset] - saved-value reset
   * @prop {(() => Promise<void>) | null} [onSaved] - parent refetch after a
   *   per-key save
   * @prop {string} [query] - settings key-search text; hides non-matching rows
   * @prop {(n: number) => void} [onMatchCount] - reports the visible-row count to the parent
   *   global empty state
   * @prop {boolean} [stub=false] - link-out stub for the Settings page
   *   (same search matching + count, body links to #activity)
   * @prop {boolean} [degraded=false] - settings store offline: the row
   *   renders an honest offline note and stays read-only for saves
   * @prop {string} [cardTitle="Logging"] - card title override (page h1 is
   *   "Logs", so the title must not equal it)
   */
  let {
    formValues,
    rawText = "",
    onField,
    sources = {},
    onReset = null,
    onSaved = null,
    query = "",
    onMatchCount = null,
    stub = false,
    degraded = false,
    cardTitle = "Logging",
  } = $props();

  let env = $derived(parseEnv(rawText));
  let logLevel = $derived(formValues.LOG_LEVEL || "info");

  // Key search: row copy lives in consts so rendering + matching share one
  // source (case-insensitive key + label/description substring).
  const LOG_LEVEL_LABEL = "Server Log Level";
  const LOG_LEVEL_DESC =
    "Controls the detail level of server console output and the live Logs page.";
  let q = $derived(query.trim().toLowerCase());
  function hit(...parts) {
    if (!q) return true;
    return parts.join("\n").toLowerCase().includes(q);
  }
  let showLogLevel = $derived(
    hit("LOG_LEVEL", LOG_LEVEL_LABEL, LOG_LEVEL_DESC),
  );
  let visibleKeys = $derived(
    [showLogLevel ? "LOG_LEVEL" : null].filter((k) => k !== null),
  );
  let visible = $derived(visibleKeys.length);
  let total = 1;
  $effect(() => {
    onMatchCount?.(visible);
  });
</script>

{#if !q || visible > 0}
  <SettingsCard
    title={$tr(cardTitle)}
    description={$tr(
      "Server log verbosity. Saved instantly; applies after a container restart.",
    )}
  >
    {#snippet icon()}
      <ScrollText size={20} />
    {/snippet}
    {#snippet actions()}
      {#if q}
        <span
          role="status"
          class="text-[11px] font-mono text-[var(--fp-dim)] shrink-0"
          >{$tr("{visible} of {total}", { visible, total })}</span
        >
      {/if}
    {/snippet}

    {#if stub}
      <div class="py-4 flex flex-col items-start gap-2">
        <p class="text-xs text-[var(--fp-muted)] leading-relaxed">
          {$tr("Server log level now lives under the Logs page's Logging tab.")}
        </p>
        <a
          href="#activity"
          onclick={() => {
            try {
              sessionStorage.setItem("fp-page-tab:activity", "logging");
            } catch {
              // Storage unavailable — the Logs page opens on its default tab.
            }
          }}
          class="text-xs text-[var(--fp-accent)] hover:underline font-medium"
        >
          {$tr("Manage log level (Logs → Logging tab)")}
        </a>
      </div>
    {:else if showLogLevel}
      <SettingsRow
        first={visibleKeys[0] === "LOG_LEVEL"}
        last={visibleKeys[visibleKeys.length - 1] === "LOG_LEVEL"}
        label={$tr(LOG_LEVEL_LABEL)}
        description={$tr(LOG_LEVEL_DESC)}
      >
        {#snippet badge()}
          <code
            class="text-[10px] px-1.5 py-0.5 rounded bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-mono"
            >LOG_LEVEL</code
          >
          {#if !env.LOG_LEVEL}
            <span
              class="text-[10px] px-1.5 py-0.5 rounded-[var(--fp-radius-sm)] border border-[var(--fp-border)] bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-semibold uppercase tracking-wider shrink-0"
              >{$tr("default")}</span
            >
          {/if}
          <span class="text-[10px] text-[var(--fp-dim)] lowercase shrink-0"
            >{$tr("(needs restart)")}</span
          >
        {/snippet}
        {#snippet extra()}
          <DbOverrideSave
            settingKey="LOG_LEVEL"
            value={logLevel}
            restartOnly
            source={sources.LOG_LEVEL}
            {onReset}
            {onSaved}
            {degraded}
          />
        {/snippet}

        <div class="w-full sm:w-48">
          <select
            aria-label="LOG_LEVEL"
            class="fp-input w-full !text-xs !h-9 !pl-3 !pr-8 bg-[var(--fp-input-bg)] text-[var(--fp-text)] border border-[var(--fp-border-bright)] rounded-[var(--fp-radius-sm)] focus:border-[var(--fp-accent)] focus:outline-none"
            value={logLevel}
            onchange={(e) => onField("LOG_LEVEL", e.currentTarget.value)}
          >
            <option value="info" class="bg-[#141a25] text-[#e9edf3]"
              >info (recommended)</option
            >
            <option value="debug" class="bg-[#141a25] text-[#e9edf3]"
              >debug</option
            >
            <option value="warn" class="bg-[#141a25] text-[#e9edf3]"
              >warn</option
            >
            <option value="error" class="bg-[#141a25] text-[#e9edf3]"
              >error</option
            >
            <option value="trace" class="bg-[#141a25] text-[#e9edf3]"
              >trace</option
            >
          </select>
        </div>
      </SettingsRow>
    {/if}
  </SettingsCard>
{/if}
