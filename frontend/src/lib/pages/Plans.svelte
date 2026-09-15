<script>
  import { onMount } from "svelte";
  import { X } from "@lucide/svelte";
  import PageShell from "../components/PageShell.svelte";
  import SegmentedControl from "../components/SegmentedControl.svelte";
  import Alert from "../components/Alert.svelte";
  import ModelsPanel from "../components/ModelsPanel.svelte";
  import AllowancesPanel from "../components/AllowancesPanel.svelte";
  import ModelRoutingSettings from "./settings/ModelRoutingSettings.svelte";
  import AdvancedSettings from "./settings/AdvancedSettings.svelte";
  import { tr } from "../i18n.js";
  import { recordPageVisit } from "../stores/pageState.js";
  import {
    meta as settingsMeta,
    formValues as settingsFormValues,
    rawText as settingsRawText,
    settingSources as settingsSources,
    settingsDegraded,
    result as settingsResult,
    fetchData as fetchSettings,
    resetSetting as resetSettingsKey,
    overlaySaved as settingsOverlaySaved,
    setField as setSettingsField,
  } from "../stores/settings.js";
  let tab = $state("accounts");

  onMount(() => {
    recordPageVisit("models");
    // Shared settings draft (same store as Settings): hydrates the inline
    // Usage controls; silent when Settings already loaded it.
    fetchSettings();
    try {
      const pending = sessionStorage.getItem("fp-page-tab:plans");
      sessionStorage.removeItem("fp-page-tab:plans");
      if (
        pending === "models" ||
        pending === "accounts" ||
        pending === "controls"
      )
        tab = pending;
    } catch {
      // storage unavailable — stay on the default tab
    }
  });
</script>

<PageShell
  crumb="freebuff-proxy / Admin / usage.conf"
  title={$tr("Usage")}
  description={$tr("Serving accounts and served models.")}
>
  <div class="flex flex-wrap items-center gap-2">
    <SegmentedControl
      bind:value={tab}
      options={[
        { id: "accounts", label: $tr("Accounts") },
        { id: "models", label: $tr("Models") },
        { id: "controls", label: $tr("Controls") },
      ]}
      ariaLabel={$tr("Catalog section")}
    />
  </div>

  {#if tab === "controls"}
    {#if $settingsDegraded}
      <Alert tone="warning" title={$tr("DB overlay unavailable")}>
        {$tr(
          "The settings store is offline — per-key saves are disabled. Changes cannot be saved right now.",
        )}
      </Alert>
    {/if}
    {#if $settingsResult}
      <Alert
        tone={$settingsResult.ok
          ? $settingsResult.restart_only.length
            ? "warning"
            : "success"
          : "error"}
      >
        <div class="flex items-start justify-between gap-3">
          <div>
            {$settingsResult.message}
            {#if $settingsResult.ok && $settingsResult.restart_only.length}
              <p class="mt-1 text-xs">
                {$tr("Applies after restart: {keys}", {
                  keys: $settingsResult.restart_only.join(", "),
                })}
              </p>
            {/if}
          </div>
          <button
            type="button"
            onclick={() => settingsResult.set(null)}
            class="text-[var(--fp-dim)] hover:text-[var(--fp-text)] transition-colors shrink-0"
            aria-label={$tr("Dismiss alert")}
          >
            <X size={14} />
          </button>
        </div>
      </Alert>
    {/if}
    <ModelRoutingSettings
      cardTitle="Usage Controls"
      formValues={$settingsFormValues}
      rawText={$settingsRawText}
      onField={setSettingsField}
      sources={$settingsSources}
      onReset={resetSettingsKey}
      onSaved={settingsOverlaySaved}
      degraded={$settingsDegraded}
    />
    <AdvancedSettings
      meta={$settingsMeta}
      formValues={$settingsFormValues}
      rawText={$settingsRawText}
      onField={setSettingsField}
      sources={$settingsSources}
      onReset={resetSettingsKey}
      onSaved={settingsOverlaySaved}
      onlyGroups={["upstream", "quota"]}
      cardTitle="Upstream & Quota"
      cardDescription="Upstream behavior and quotas."
      degraded={$settingsDegraded}
    />
  {:else}
    <div class="flex flex-col gap-5">
      {#if tab === "models"}
        <ModelsPanel />
      {:else}
        <AllowancesPanel />
      {/if}
    </div>
  {/if}
</PageShell>
