<script>
  import { onMount } from "svelte";
  import { RefreshCw } from "@lucide/svelte";
  import PageShell from "../components/PageShell.svelte";
  import Alert from "../components/Alert.svelte";
  import Button from "../components/Button.svelte";
  import SegmentedControl from "../components/SegmentedControl.svelte";
  import LiveConsole from "../components/LiveConsole.svelte";
  import MetricsPanel from "../components/MetricsPanel.svelte";
  import TracesPanel from "../components/TracesPanel.svelte";
  import LogLevelSettings from "./settings/LogLevelSettings.svelte";
  import AdvancedSettings from "./settings/AdvancedSettings.svelte";
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
  import { tr } from "../i18n.js";
  import { recordPageVisit } from "../stores/pageState.js";

  let tab = $state("live");
  // Shared time cursor: "Refresh all" stamps it; every panel refetches when
  // it advances (each panel also fetches on its own mount).
  let cursor = $state(0);
  // Log→trace link target, set by LiveConsole's Trace buttons.
  let traceFocus = $state("");
  // Trace/metric→log link text, applied as LiveConsole's one-shot
  // initialFilter through a {#key} remount.
  let liveInitial = $state("");
  let liveKey = $state(0);

  function onOpenTrace(reqId, token) {
    void token;
    traceFocus = reqId ?? "";
    tab = "traces";
  }

  function onOpenLogs(text) {
    liveInitial = text ?? "";
    liveKey += 1;
    tab = "live";
  }

  function onOpenToken(idx) {
    try {
      sessionStorage.setItem("fp-tokens-expand", String(idx));
    } catch {
      // Storage unavailable (private mode) — the Tokens page just won't
      // auto-expand; navigation still works.
    }
    location.hash = "tokens";
  }

  onMount(() => {
    recordPageVisit("logs");
    // Shared settings draft (same store as Settings): hydrates the inline
    // Logging card; silent when Settings already loaded it.
    fetchSettings();
    // One-shot deep-link tab (set by the shell's legacy-hash redirect);
    // consumed on mount so back-navigation keeps the operator's own tab.
    try {
      const t = sessionStorage.getItem("fp-page-tab:activity") || "";
      sessionStorage.removeItem("fp-page-tab:activity");
      if (t === "live" || t === "metrics" || t === "traces" || t === "logging")
        tab = t;
    } catch {
      // Storage unavailable — stay on the default Live tab.
    }
  });
</script>

<PageShell
  crumb="freebuff-proxy / Admin / logs.conf"
  title={$tr("Logs")}
  description={$tr("Live traffic, metrics, and traces.")}
>
  {#snippet actions()}
    <div class="flex flex-wrap items-center gap-2">
      <SegmentedControl
        bind:value={tab}
        options={[
          { id: "live", label: $tr("Live") },
          { id: "metrics", label: $tr("Metrics") },
          { id: "traces", label: $tr("Traces") },
          { id: "logging", label: $tr("Logging") },
        ]}
        ariaLabel={$tr("Activity view")}
      />
      <Button
        variant="secondary"
        size="sm"
        onclick={() => (cursor = Date.now())}
        class="!h-8 !text-xs !px-2.5"
      >
        <RefreshCw size={13} />
        <span class="hidden min-[480px]:inline">{$tr("Refresh all")}</span>
      </Button>
    </div>
  {/snippet}
  {#if tab === "logging"}
    {#if $settingsDegraded}
      <Alert tone="warning" title={$tr("DB overlay unavailable")}>
        {$tr(
          "The settings store is offline — per-key saves are disabled. Changes cannot be saved right now.",
        )}
      </Alert>
    {/if}
    <LogLevelSettings
      cardTitle="Logging"
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
      onlyGroups={["general"]}
      cardTitle="Logging & Diagnostics"
      cardDescription="Log output and diagnostics."
      degraded={$settingsDegraded}
    />
  {:else}
    {#if tab === "live"}
      {#key liveKey}
        <LiveConsole
          {cursor}
          initialFilter={liveInitial}
          {onOpenTrace}
          {onOpenToken}
        />
      {/key}
    {:else if tab === "metrics"}
      <MetricsPanel {cursor} {onOpenToken} {onOpenLogs} />
    {:else}
      <TracesPanel
        {cursor}
        focusReqId={traceFocus}
        {onOpenToken}
        {onOpenLogs}
      />
    {/if}
  {/if}
</PageShell>
