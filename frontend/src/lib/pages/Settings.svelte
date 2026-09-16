<script>
  import { onMount } from "svelte";
  import { recordPageVisit } from "../stores/pageState.js";
  import { RefreshCw } from "@lucide/svelte";
  import PageShell from "../components/PageShell.svelte";
  import Button from "../components/Button.svelte";
  import Alert from "../components/Alert.svelte";
  import EmptyState from "../components/EmptyState.svelte";
  import AccessSecurityCard from "./settings/AccessSecurityCard.svelte";
  import CommandCenterCard from "../components/CommandCenterCard.svelte";
  import RawEnvEditor from "../components/RawEnvEditor.svelte";
  import GatewaySettings from "./settings/GatewaySettings.svelte";
  import TrafficSettings from "./settings/TrafficSettings.svelte";
  import ModelRoutingSettings from "./settings/ModelRoutingSettings.svelte";
  import AdvancedSettings from "./settings/AdvancedSettings.svelte";
  import LogLevelSettings from "./settings/LogLevelSettings.svelte";
  import {
    meta,
    loading,
    error,
    rawText,
    formValues,
    settingSources,
    settingsDegraded,
    fetchData,
    resetSetting,
    overlaySaved,
    setField,
  } from "../stores/settings.js";
  import { tr } from "../i18n.js";

  // Key search across all catalog sections (70 keys).
  let filterQuery = $state("");
  let searching = $derived(filterQuery.trim().length > 0);
  // Per-section visible-row counts (bound from the section components, -1
  // until mounted) for the global search empty state. trafficMatches is
  // reported by the Pool stub card that links out to #tokens.
  let gatewayMatches = $state(-1);
  let trafficMatches = $state(-1);
  let routingMatches = $state(-1);
  let logLevelMatches = $state(-1);
  let accessMatches = $state(-1);
  let advancedMatches = $state(-1);
  let allEmpty = $derived(
    searching &&
      gatewayMatches === 0 &&
      trafficMatches === 0 &&
      routingMatches === 0 &&
      logLevelMatches === 0 &&
      accessMatches === 0 &&
      advancedMatches === 0,
  );

  onMount(() => {
    recordPageVisit("settings");
    fetchData();
  });
</script>

<PageShell
  crumb="freebuff-proxy / Admin / settings.conf"
  title={$tr("Settings")}
  description={$tr(
    "Access, protection, and remaining tunables. Every row saves instantly to the DB overlay; restart-marked keys apply after a container restart.",
  )}
  loading={$loading}
  error={$error}
  onRetry={fetchData}
>
  {#snippet actions()}
    <Button variant="ghost" onclick={fetchData}>
      <RefreshCw size={15} />
      {$tr("Refresh")}
    </Button>
  {/snippet}

  {#if $settingsDegraded}
    <Alert tone="warning" title={$tr("DB overlay unavailable")}>
      {$tr(
        "The settings store is offline — the dashboard runs live-only. Per-key saves and resets will fail; the emergency .env editor below still applies.",
      )}
    </Alert>
  {/if}

  <!-- Key search across all 70 catalog keys -->
  <div class="flex flex-col gap-1.5">
    <label
      for="settings-search"
      class="text-xs font-semibold text-[var(--fp-muted)]"
      >{$tr("Search settings")}</label
    >
    <input
      id="settings-search"
      type="search"
      autocomplete="off"
      placeholder={$tr("Search 70 keys…")}
      bind:value={filterQuery}
      class="fp-input w-full sm:max-w-md"
    />
  </div>

  <!-- 1. Access and Security (password + dashboard access) -->
  <AccessSecurityCard
    formValues={$formValues}
    rawText={$rawText}
    onField={setField}
    sources={$settingSources}
    onReset={resetSetting}
    onSaved={overlaySaved}
    query={filterQuery}
    onMatchCount={(n) => (accessMatches = n)}
    onPasswordSuccess={fetchData}
    degraded={$settingsDegraded}
  />

  <!-- 2. Gateway & Protection (General - live reload) -->
  <GatewaySettings
    formValues={$formValues}
    rawText={$rawText}
    onField={setField}
    sources={$settingSources}
    onReset={resetSetting}
    onSaved={overlaySaved}
    query={filterQuery}
    onMatchCount={(n) => (gatewayMatches = n)}
    degraded={$settingsDegraded}
  />

  <!-- 3. Pool (moved to the Pool page - stub links out to #tokens) -->
  <TrafficSettings
    formValues={$formValues}
    rawText={$rawText}
    onField={setField}
    sources={$settingSources}
    onReset={resetSetting}
    onSaved={overlaySaved}
    query={filterQuery}
    onMatchCount={(n) => (trafficMatches = n)}
    stub
  />

  <!-- 4. Model Routing & Aliases (moved to the Usage page - stub links out to #plans) -->
  <ModelRoutingSettings
    formValues={$formValues}
    rawText={$rawText}
    onField={setField}
    sources={$settingSources}
    onReset={resetSetting}
    onSaved={overlaySaved}
    query={filterQuery}
    onMatchCount={(n) => (routingMatches = n)}
    stub
  />

  <!-- 5. Logging (moved to the Logs page - stub links out to #activity) -->
  <LogLevelSettings
    formValues={$formValues}
    rawText={$rawText}
    onField={setField}
    sources={$settingSources}
    onReset={resetSetting}
    onSaved={overlaySaved}
    query={filterQuery}
    onMatchCount={(n) => (logLevelMatches = n)}
    stub
  />

  <!-- 6. Advanced (every remaining catalog key with its default) -->
  <AdvancedSettings
    meta={$meta}
    formValues={$formValues}
    rawText={$rawText}
    onField={setField}
    sources={$settingSources}
    onReset={resetSetting}
    onSaved={overlaySaved}
    query={filterQuery}
    onMatchCount={(n) => (advancedMatches = n)}
    onlyGroups={["security"]}
    cardTitle="Security"
    cardDescription="Remaining security tunables."
    degraded={$settingsDegraded}
  />

  {#if allEmpty}
    <EmptyState
      title={$tr('No settings match "{q}"', { q: filterQuery.trim() })}
      description={$tr(
        "Try a key name like TOKEN_ROTATION, or clear the search to see all sections.",
      )}
    >
      {#snippet action()}
        <Button
          variant="secondary"
          size="sm"
          onclick={() => (filterQuery = "")}
        >
          {$tr("Clear search")}
        </Button>
      {/snippet}
    </EmptyState>
  {/if}

  <!-- 7. Emergency raw .env editor (break-glass whole-file path) -->
  <RawEnvEditor />

  <!-- 8. Command Center (Lifecycle, updates & rollback) -->
  <CommandCenterCard />
</PageShell>
