<script>
  import { onMount } from "svelte";
  import PageShell from "../components/PageShell.svelte";
  import SegmentedControl from "../components/SegmentedControl.svelte";
  import Alert from "../components/Alert.svelte";
  import ModelsPanel from "../components/ModelsPanel.svelte";
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
    fetchData as fetchSettings,
    resetSetting as resetSettingsKey,
    overlaySaved as settingsOverlaySaved,
    setField as setSettingsField,
  } from "../stores/settings.js";
  let tab = $state("models");

  onMount(() => {
    recordPageVisit("models");
    // Shared settings draft (same store as Settings): hydrates the inline
    // Usage controls; silent when Settings already loaded it.
    fetchSettings();
    try {
      const pending = sessionStorage.getItem("fp-page-tab:plans");
      if (pending !== null) {
        sessionStorage.removeItem("fp-page-tab:plans");
        if (pending === "catalog" || pending === "models") tab = "models";
        else if (pending === "routing" || pending === "controls")
          tab = "controls";
      }
    } catch {
      // storage unavailable — stay on the default tab
    }
  });
</script>

<PageShell
  crumb="freebucks-proxy / Admin / models.conf"
  title={$tr("Models")}
  description={$tr("Served models catalog, list prices, and routing controls.")}
>
  <div class="flex flex-wrap items-center gap-2">
    <SegmentedControl
      bind:value={tab}
      options={[
        { id: "models", label: $tr("Catalog") },
        { id: "controls", label: $tr("Routing") },
      ]}
      ariaLabel={$tr("Models sections")}
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
      <ModelsPanel />
    </div>
  {/if}
</PageShell>
