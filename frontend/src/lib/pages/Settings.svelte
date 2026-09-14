<script>
  import { onMount, onDestroy } from "svelte";
  import { recordPageVisit } from "../stores/pageState.js";
  import { RefreshCw, Save, X } from "@lucide/svelte";
  import PageShell from "../components/PageShell.svelte";
  import Button from "../components/Button.svelte";
  import Alert from "../components/Alert.svelte";
  import EmptyState from "../components/EmptyState.svelte";
  import AccessSecurityCard from "./settings/AccessSecurityCard.svelte";
  import CommandCenterCard from "../components/CommandCenterCard.svelte";
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
    saving,
    result,
    dirty,
    changedKeysCount,
    restartKeys,
    liveKeys,
    fetchData,
    resetSetting,
    overlaySaved,
    saveConfig,
    setField,
    discard,
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

  function handleBeforeUnload(e) {
    if ($dirty) {
      e.preventDefault();
      e.returnValue = "";
    }
  }

  function handleKeyDown(e) {
    if ((e.ctrlKey || e.metaKey) && e.key === "s") {
      e.preventDefault();
      if ($dirty && !$saving) {
        saveConfig(null, { confirm: false });
      }
    }
  }

  onMount(() => {
    recordPageVisit("settings");
    fetchData();
    window.addEventListener("beforeunload", handleBeforeUnload);
    window.addEventListener("keydown", handleKeyDown);
  });

  onDestroy(() => {
    window.removeEventListener("beforeunload", handleBeforeUnload);
    window.removeEventListener("keydown", handleKeyDown);
  });
</script>

<PageShell
  crumb="freebuff-proxy / Admin / settings.conf"
  title={$tr("Settings")}
  description={$tr(
    "Access, protection, and remaining tunables. Live-applying keys take effect on save without restart; restart-marked keys need a container restart.",
  )}
  loading={$loading}
  error={$error}
  onRetry={fetchData}
>
  {#snippet actions()}
    {#if $dirty}
      <Button variant="ghost" onclick={discard} disabled={$saving}>
        <X size={15} />
        {$tr("Discard")}
      </Button>
    {:else}
      <Button variant="ghost" onclick={fetchData}>
        <RefreshCw size={15} />
        {$tr("Refresh")}
      </Button>
    {/if}
    <Button
      variant="primary"
      onclick={saveConfig}
      disabled={$saving || !$dirty}
      loading={$saving}
    >
      <Save size={15} />
      {$tr("Save Changes")}
    </Button>
  {/snippet}

  {#if $result}
    <Alert
      tone={$result.ok
        ? $result.restart_only.length
          ? "warning"
          : "success"
        : "error"}
    >
      <div class="flex items-start justify-between gap-3">
        <div>
          {$result.message}
          {#if $result.ok && $result.restart_only.length}
            <p class="mt-1 text-xs">
              {$tr("Applies after restart: {keys}", {
                keys: $result.restart_only.join(", "),
              })}
            </p>
          {/if}
        </div>
        <button
          type="button"
          onclick={() => result.set(null)}
          class="text-[var(--fp-dim)] hover:text-[var(--fp-text)] transition-colors shrink-0"
          aria-label={$tr("Dismiss alert")}
        >
          <X size={14} />
        </button>
      </div>
    </Alert>
  {/if}

  {#if $dirty}
    <Alert tone="warning" title={$tr("Unsaved changes")}>
      <div
        class="flex flex-col sm:flex-row sm:items-center justify-between gap-2"
      >
        <span
          >{$tr(
            "{count} setting(s) modified. Click Save Changes to apply them immediately.",
            { count: $changedKeysCount },
          )}</span
        >
        <div class="flex items-center gap-2 shrink-0">
          <Button variant="secondary" size="sm" onclick={discard}>
            <X size={14} />
            {$tr("Discard")}
          </Button>
        </div>
      </div>
      {#if $restartKeys.length > 0 || $liveKeys.length > 0}
        <div class="flex flex-col gap-0.5 mt-2 text-xs">
          {#if $restartKeys.length > 0}
            <span
              >{$tr("Needs restart ({n}): {keys}", {
                n: $restartKeys.length,
                keys: $restartKeys.join(", "),
              })}</span
            >
          {/if}
          {#if $liveKeys.length > 0}
            <span class="text-[var(--fp-dim)]"
              >{$tr("Live-applying ({n}): {keys}", {
                n: $liveKeys.length,
                keys: $liveKeys.join(", "),
              })}</span
            >
          {/if}
        </div>
      {/if}
    </Alert>
  {/if}

  {#if $settingsDegraded}
    <Alert tone="warning" title={$tr("DB overlay unavailable")}>
      {$tr(
        "The settings store is offline — the dashboard runs live-only. Overlay saves and resets will fail; .env saves below still apply.",
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

  <!-- 7. Command Center (Lifecycle, updates & rollback) -->
  <CommandCenterCard />
</PageShell>
