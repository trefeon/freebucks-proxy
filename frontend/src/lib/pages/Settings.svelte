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
  import HiddenKeysCard from "./settings/HiddenKeysCard.svelte";
  import GatewaySettings from "./settings/GatewaySettings.svelte";
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

  // The general-group catalog keys that are not owned by a curated card
  // (LOG_LEVEL has its own card; the rest of `general` is covered or
  // hidden): exactly the four rows the Logs page's Logging & Diagnostics
  // card rendered before the tab was removed, so the card moves with its
  // contents unchanged.
  const LOGGING_KEYS = [
    "DEBUG_DUMP",
    "DEVTOOLS_ENABLED",
    "LOG_ACCESS",
    "LOG_FORMAT",
  ];

  // Key search across the rows this page actually renders. The count is the
  // sum of the sections' own match counts with an empty query (the link-out
  // stubs count the keys they name) — 13 catalog keys plus the non-catalog
  // admin-password row plus the hidden-keys disclosure rows today, never the
  // whole 65-key catalog, most of which is homed on the Pool/Usage/Logs/
  // Warming surfaces.
  let filterQuery = $state("");
  let searching = $derived(filterQuery.trim().length > 0);
  // Per-section visible-row counts (bound from the section components, -1
  // until mounted) for the global search empty state.
  let gatewayMatches = $state(-1);
  let logLevelMatches = $state(-1);
  let accessMatches = $state(-1);
  let advancedMatches = $state(-1);
  let loggingMatches = $state(-1);
  let hiddenMatches = $state(-1);
  let searchableKeys = $state(0);
  let allEmpty = $derived(
    searching &&
      gatewayMatches === 0 &&
      logLevelMatches === 0 &&
      accessMatches === 0 &&
      advancedMatches === 0 &&
      loggingMatches === 0 &&
      hiddenMatches === 0,
  );
  // Capture the empty-query total once the sections have reported: while a
  // query is typed those counts shrink, and the placeholder must keep naming
  // the page's real searchable key count.
  $effect(() => {
    if (searching) return;
    const sum = [
      accessMatches,
      gatewayMatches,
      logLevelMatches,
      advancedMatches,
      loggingMatches,
      hiddenMatches,
    ].reduce((n, c) => (c > 0 ? n + c : n), 0);
    if (sum > searchableKeys) searchableKeys = sum;
  });

  onMount(() => {
    recordPageVisit("settings");
    fetchData();
  });
</script>

<PageShell
  crumb="freebucks-proxy / Admin / settings.conf"
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
        "The settings store is offline — the dashboard runs live-only. Per-key saves and resets will fail; the values below come from the file and the running process.",
      )}
    </Alert>
  {/if}

  <!-- Key search across the keys this page renders -->
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
      placeholder={searchableKeys > 0
        ? $tr("Search {n} settings…", { n: searchableKeys })
        : $tr("Search settings…")}
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

  <!-- 3. Logging (the working home of the log keys; the Logs page no
       longer hosts a Logging tab) -->
  <LogLevelSettings
    formValues={$formValues}
    rawText={$rawText}
    onField={setField}
    sources={$settingSources}
    onReset={resetSetting}
    onSaved={overlaySaved}
    query={filterQuery}
    onMatchCount={(n) => (logLevelMatches = n)}
    cardTitle="Server Log Level"
    degraded={$settingsDegraded}
  />

  <!-- 5b. Logging & Diagnostics (every other general-group key) -->
  <AdvancedSettings
    meta={$meta}
    formValues={$formValues}
    rawText={$rawText}
    onField={setField}
    sources={$settingSources}
    onReset={resetSetting}
    onSaved={overlaySaved}
    query={filterQuery}
    onMatchCount={(n) => (loggingMatches = n)}
    onlyKeys={LOGGING_KEYS}
    cardTitle="Logging & Diagnostics"
    cardDescription="Log output and diagnostics."
    degraded={$settingsDegraded}
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
        "Try a key name like PIN_MODEL, or clear the search to see all sections.",
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

  <!-- 7. Hidden keys (catalog keys with no card of their own) -->
  <HiddenKeysCard
    meta={$meta}
    formValues={$formValues}
    sources={$settingSources}
    onReset={resetSetting}
    onSaved={overlaySaved}
    query={filterQuery}
    onMatchCount={(n) => (hiddenMatches = n)}
    degraded={$settingsDegraded}
  />

  <!-- 8. Command Center (Lifecycle, updates & rollback) -->
  <CommandCenterCard />
</PageShell>
