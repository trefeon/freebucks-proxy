<script>
  import SettingsCard from "../../components/SettingsCard.svelte";
  import SettingsRow from "../../components/SettingsRow.svelte";
  import ToggleSwitch from "../../components/ToggleSwitch.svelte";
  import DbOverrideSave from "../../components/DbOverrideSave.svelte";
  import { Cpu } from "@lucide/svelte";
  import { tr } from "../../i18n.js";
  import { parseEnv } from "../../utils/env.js";

  /**
   * Model reasoning settings card (Upstream group).
   * Built using the SettingsCard and SettingsRow template components.
   * The key applies live on reload (not restart-only).
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
   *   (same search matching + count, body links to #plans)
   * @prop {boolean} [degraded=false] - settings store offline: per-key
   *   overlay saves render an honest offline note, .env flow stays usable
   * @prop {string} [cardTitle='Upstream'] - card title override (the Usage
   *   page embeds the full card as 'Usage Controls')
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
    cardTitle = "Upstream",
  } = $props();

  let env = $derived(parseEnv(rawText));
  let reasoningInContent = $derived(
    Boolean(
      formValues.REASONING_IN_CONTENT &&
      formValues.REASONING_IN_CONTENT !== "false" &&
      formValues.REASONING_IN_CONTENT !== "off",
    ),
  );
  // Key search: row copy lives in consts so rendering + matching share one
  // source (case-insensitive key + label/description substring).
  const REASON_LABEL = "Fold Reasoning into Message Content";
  const REASON_DESC =
    "Wraps internal reasoning inside <think>...</think> tags in the message body for older AI clients that do not support dedicated reasoning stream blocks.";

  let q = $derived(query.trim().toLowerCase());
  function hit(...parts) {
    if (!q) return true;
    return parts.join("\n").toLowerCase().includes(q);
  }
  let showReason = $derived(
    hit("REASONING_IN_CONTENT", REASON_LABEL, REASON_DESC),
  );
  let visibleKeys = $derived(
    [showReason ? "REASONING_IN_CONTENT" : null].filter((k) => k !== null),
  );
  let visible = $derived(visibleKeys.length);
  $effect(() => {
    onMatchCount?.(visible);
  });
</script>

{#if !q || visible > 0}
  <SettingsCard
    title={$tr(cardTitle)}
    description={$tr("Reasoning format. Changes apply live without restart.")}
  >
    {#snippet icon()}
      <Cpu size={20} />
    {/snippet}
    {#snippet actions()}
      {#if q}
        <span
          role="status"
          class="text-[11px] font-mono text-[var(--fp-dim)] shrink-0"
          >{$tr("{visible} of {total}", { visible, total: 1 })}</span
        >
      {/if}
    {/snippet}
    {#if stub}
      <div class="py-4 flex flex-col items-start gap-2">
        <p class="text-xs text-[var(--fp-muted)] leading-relaxed">
          {$tr(
            "Model reasoning format now lives under the Usage page's Controls tab.",
          )}
        </p>
        <a
          href="#plans"
          onclick={() => {
            try {
              sessionStorage.setItem("fp-page-tab:plans", "controls");
            } catch {
              // Storage unavailable — the Usage page opens on its default tab.
            }
          }}
          class="text-xs text-[var(--fp-accent)] hover:underline font-medium"
        >
          {$tr("Manage Usage controls (Usage → Controls tab)")}
        </a>
      </div>
    {:else}
      <!-- Fold Reasoning into Content -->
      {#if showReason}
        <SettingsRow
          first={visibleKeys[0] === "REASONING_IN_CONTENT"}
          last={visibleKeys[visibleKeys.length - 1] === "REASONING_IN_CONTENT"}
          label={$tr(REASON_LABEL)}
          description={$tr(REASON_DESC)}
        >
          {#snippet badge()}
            <code
              class="text-[10px] px-1.5 py-0.5 rounded bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-mono"
              >REASONING_IN_CONTENT</code
            >
            {#if !env.REASONING_IN_CONTENT}
              <span
                class="text-[10px] px-1.5 py-0.5 rounded-[var(--fp-radius-sm)] border border-[var(--fp-border)] bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-semibold uppercase tracking-wider shrink-0"
                >{$tr("default")}</span
              >
            {/if}
          {/snippet}

          {#snippet extra()}
            {#if degraded}
              <span class="text-[10px] text-[var(--fp-dim)]"
                >{$tr("Overlay offline — use .env save")}</span
              >
            {:else}
              <DbOverrideSave
                settingKey="REASONING_IN_CONTENT"
                value={formValues.REASONING_IN_CONTENT ?? ""}
                source={sources.REASONING_IN_CONTENT}
                {onReset}
                {onSaved}
              />
            {/if}
          {/snippet}

          <div class="flex items-center gap-2.5">
            <ToggleSwitch
              checked={reasoningInContent}
              ariaLabel="REASONING_IN_CONTENT"
              onchange={(v) => onField("REASONING_IN_CONTENT", v ? "true" : "")}
            />
          </div>
        </SettingsRow>
      {/if}
    {/if}
  </SettingsCard>
{/if}
