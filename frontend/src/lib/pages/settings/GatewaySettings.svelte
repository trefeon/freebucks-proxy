<script>
  import SettingsCard from "../../components/SettingsCard.svelte";
  import SettingsRow from "../../components/SettingsRow.svelte";
  import ToggleSwitch from "../../components/ToggleSwitch.svelte";
  import DbOverrideSave from "../../components/DbOverrideSave.svelte";
  import { ShieldCheck } from "@lucide/svelte";
  import { tr } from "../../i18n.js";
  import { parseEnv } from "../../utils/env.js";

  /**
   * Gateway & Protection settings card (General group).
   * Built using the SettingsCard and SettingsRow template components.
   * Safe Mode instant-saves to the DB overlay on edit (DbOverrideSave).
   * HTTP_READ_TIMEOUT is env-only: its row renders the effective value
   * read-only with an env-note (no editor, no save), like the hidden keys.
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
   * @prop {boolean} [degraded=false] - settings store offline: rows render
   *   an honest offline note and stay read-only for saves
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
    degraded = false,
  } = $props();

  let env = $derived(parseEnv(rawText));
  let safeMode = $derived(formValues.SAFE_MODE !== "false");
  let httpReadTimeout = $derived(formValues.HTTP_READ_TIMEOUT || "60s");

  // Key search: row copy lives in consts so rendering + matching share one
  // source (case-insensitive key + label/description substring).
  const SAFE_MODE_LABEL = "Anti-Ban Safe Mode";
  const SAFE_MODE_DESC =
    "Enforces 200ms request jitter and 30-minute idle session rotation to match official CLI behavior and avoid upstream account flagging.";
  const HTTP_TIMEOUT_LABEL = "HTTP Read Timeout";
  const HTTP_TIMEOUT_DESC =
    "How long the server waits for slow clients uploading request bodies (far-away harnesses, images). Takes effect after a container restart; 0 disables the timeout.";
  let q = $derived(query.trim().toLowerCase());
  function hit(...parts) {
    if (!q) return true;
    return parts.join("\n").toLowerCase().includes(q);
  }
  let showSafeMode = $derived(
    hit("SAFE_MODE", SAFE_MODE_LABEL, SAFE_MODE_DESC),
  );
  let showHttpTimeout = $derived(
    hit("HTTP_READ_TIMEOUT", HTTP_TIMEOUT_LABEL, HTTP_TIMEOUT_DESC),
  );
  let visibleKeys = $derived(
    [
      showSafeMode ? "SAFE_MODE" : null,
      showHttpTimeout ? "HTTP_READ_TIMEOUT" : null,
    ].filter((k) => k !== null),
  );
  let visible = $derived(visibleKeys.length);
  $effect(() => {
    onMatchCount?.(visible);
  });
</script>

{#if !q || visible > 0}
  <SettingsCard
    title={$tr("General")}
    description={$tr(
      "Gateway runtime behavior and account protection. Most keys apply live without restart; restart-marked keys apply after a container restart.",
    )}
  >
    {#snippet icon()}
      <ShieldCheck size={20} />
    {/snippet}
    {#snippet actions()}
      {#if q}
        <span
          role="status"
          class="text-[11px] font-mono text-[var(--fp-dim)] shrink-0"
          >{$tr("{visible} of {total}", { visible, total: 2 })}</span
        >
      {/if}
    {/snippet}

    <!-- Safe Mode -->
    {#if showSafeMode}
      <SettingsRow
        first={visibleKeys[0] === "SAFE_MODE"}
        label={$tr(SAFE_MODE_LABEL)}
        description={$tr(SAFE_MODE_DESC)}
      >
        {#snippet badge()}
          <code
            class="text-[10px] px-1.5 py-0.5 rounded bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-mono"
            >SAFE_MODE</code
          >
          {#if !env.SAFE_MODE}
            <span
              class="text-[10px] px-1.5 py-0.5 rounded-[var(--fp-radius-sm)] border border-[var(--fp-border)] bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-semibold uppercase tracking-wider shrink-0"
              >{$tr("default")}</span
            >
          {/if}
        {/snippet}

        {#snippet extra()}
          <DbOverrideSave
            settingKey="SAFE_MODE"
            value={formValues.SAFE_MODE ?? "true"}
            source={sources.SAFE_MODE}
            {onReset}
            {onSaved}
            {degraded}
          />
        {/snippet}

        <div class="flex items-center gap-2.5">
          <ToggleSwitch
            checked={safeMode}
            ariaLabel="SAFE_MODE"
            onchange={(v) => onField("SAFE_MODE", v ? "true" : "false")}
          />
        </div>
      </SettingsRow>
    {/if}

    <!-- HTTP Read Timeout -->
    {#if showHttpTimeout}
      <SettingsRow
        first={visibleKeys[0] === "HTTP_READ_TIMEOUT"}
        last={visibleKeys[visibleKeys.length - 1] === "HTTP_READ_TIMEOUT"}
        label={$tr(HTTP_TIMEOUT_LABEL)}
        description={$tr(HTTP_TIMEOUT_DESC)}
      >
        {#snippet badge()}
          <code
            class="text-[10px] px-1.5 py-0.5 rounded bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-mono"
            >HTTP_READ_TIMEOUT</code
          >
          {#if !env.HTTP_READ_TIMEOUT}
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
            settingKey="HTTP_READ_TIMEOUT"
            value={httpReadTimeout}
            restartOnly
            source={sources.HTTP_READ_TIMEOUT}
            {onReset}
            {onSaved}
            {degraded}
          />
        {/snippet}

        <!-- Env-only: the reader never consults the overlay, so there is no
          editor and no save — the effective value renders read-only with
          the gateway's verbatim pointer, like the hidden keys. -->
        <div class="w-full sm:w-48">
          <code
            class="fp-mono text-xs text-[var(--fp-text)] break-all block text-right select-all"
            title={httpReadTimeout}>{httpReadTimeout}</code
          >
          <p
            class="text-[10px] text-[var(--fp-dim)] leading-relaxed mt-1 text-right"
          >
            {$tr(
              "HTTP_READ_TIMEOUT is set in the environment or .env file, not as a knob (the reader never consults the overlay).",
            )}
          </p>
        </div>
      </SettingsRow>
    {/if}
  </SettingsCard>
{/if}
