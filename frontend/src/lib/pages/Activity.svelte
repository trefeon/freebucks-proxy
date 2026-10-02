<script>
  import { onMount } from "svelte";
  import { RefreshCw } from "@lucide/svelte";
  import PageShell from "../components/PageShell.svelte";
  import Button from "../components/Button.svelte";
  import SegmentedControl from "../components/SegmentedControl.svelte";
  import LiveConsole from "../components/LiveConsole.svelte";
  import MetricsPanel from "../components/MetricsPanel.svelte";
  import TeamUsagePanel from "../components/TeamUsagePanel.svelte";
  import TracesPanel from "../components/TracesPanel.svelte";
  import { tr } from "../i18n.js";

  import { recordPageVisit } from "../stores/pageState.js";
  import { readPageHash, scheduleHashUpdate } from "../utils/tableUrlState.js";

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

  // R1 shareable URL: the activity tab rides the hash query
  // (#activity?tab=traces), merged per-key so LiveConsole's filter keys are
  // never clobbered. The default tab stays unencoded.
  $effect(() => {
    scheduleHashUpdate("activity", { tab: tab === "live" ? null : tab });
  });

  // Map a tab id from any source (legacy one-shot, hash) onto the page tab.
  // Returns true when it matched a known tab.
  function applyActivityTab(want) {
    if (want === "live" || want === "requests") tab = "live";
    else if (want === "metrics") tab = "metrics";
    else if (want === "team" || want === "keys") tab = "team";
    else if (want === "traces") tab = "traces";
    else return false;
    return true;
  }

  onMount(() => {
    recordPageVisit("logs");
    // One-shot deep-link tab (set by the shell's legacy-hash redirect);
    // consumed on mount so back-navigation keeps the operator's own tab.
    // A pasted shareable URL (?tab=...) fills the same slot when no
    // one-shot is present. Unknown values never switch the view.
    let consumedOneShot = false;
    try {
      const t = sessionStorage.getItem("fp-page-tab:activity") || "";
      if (t) {
        sessionStorage.removeItem("fp-page-tab:activity");
        consumedOneShot = applyActivityTab(t);
      }
    } catch {
      // Storage unavailable — fall through to the hash.
    }
    if (!consumedOneShot) {
      const { page: urlPage, params: urlParams } = readPageHash();
      if (urlPage === "activity") applyActivityTab(urlParams.get("tab") ?? "");
    }
  });
</script>

<PageShell
  crumb="freebuff-proxy / Admin / logs.conf"
  title={$tr("Logs")}
  description={$tr("Live traffic, metrics, team usage, and traces.")}
>
  {#snippet actions()}
    <div class="flex flex-wrap items-center gap-2">
      <SegmentedControl
        bind:value={tab}
        options={[
          { id: "live", label: $tr("Requests") },
          { id: "metrics", label: $tr("Metrics") },
          { id: "team", label: $tr("Client Keys") },
          { id: "traces", label: $tr("Traces") },
        ]}
        ariaLabel={$tr("Activity view")}
      />
      <Button
        variant="secondary"
        size="sm"
        onclick={() => (cursor = Date.now())}
      >
        <RefreshCw size={13} />
        <span class="hidden min-[480px]:inline">{$tr("Refresh all")}</span>
      </Button>
    </div>
  {/snippet}
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
  {:else if tab === "team"}
    <TeamUsagePanel {cursor} />
  {:else}
    <TracesPanel {cursor} focusReqId={traceFocus} {onOpenToken} {onOpenLogs} />
  {/if}
</PageShell>
