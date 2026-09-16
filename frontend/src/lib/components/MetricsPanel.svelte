<script>
  import { onMount } from "svelte";
  import { RefreshCw } from "@lucide/svelte";
  import Card from "./Card.svelte";
  import Stat from "./Stat.svelte";
  import {
    push as pushToast,
    dismiss as dismissToast,
  } from "../stores/toast.js";
  import Button from "./Button.svelte";
  import SegmentedControl from "./SegmentedControl.svelte";
  import { fetchAPI } from "../api/client.js";
  import { adminApi } from "../api/paths.js";
  import { tr } from "../i18n.js";

  let { cursor = 0, onOpenToken = null, onOpenLogs = null } = $props();

  let data = $state(null);
  let loading = $state(true);
  let error = $state("");

  // Token usage overview (9Router-style): range tabs + overview/details
  // toggle, fed by GET /admin/api/usage?range=… with a fallback to the
  // `usage` key of GET /admin/api/metrics.
  let usageRange = $state("today");
  let usageView = $state("overview");
  let usage = $state(null);
  let usageLoading = $state(true);
  let usageError = $state("");
  let errorToast = $state(0);
  let usageToast = $state(0);
  function notifyError(msg) {
    if (errorToast) dismissToast(errorToast);
    errorToast = msg ? pushToast({ tone: "error", title: msg }) : 0;
  }
  function notifyUsageError(msg) {
    if (usageToast) dismissToast(usageToast);
    usageToast = msg ? pushToast({ tone: "error", title: msg }) : 0;
  }

  async function fetchData() {
    try {
      data = await fetchAPI(adminApi.metrics);
      error = "";
      notifyError("");
    } catch (e) {
      error = e.message || $tr("Failed to load metrics");
      notifyError(error);
    } finally {
      loading = false;
    }
  }

  async function fetchUsage(range) {
    usageLoading = true;
    try {
      try {
        usage = await fetchAPI(
          `${adminApi.usage}?range=${encodeURIComponent(range)}`,
        );
      } catch {
        // Older gateway without the dedicated route: usage rides along
        // under the `usage` key of the metrics payload.
        const m = await fetchAPI(adminApi.metrics);
        usage = m?.usage ?? null;
      }
      usageError = "";
      notifyUsageError("");
    } catch (e) {
      usageError = e.message || $tr("Failed to load usage");
      notifyUsageError(usageError);
    } finally {
      usageLoading = false;
    }
  }

  onMount(fetchData);

  // Shared time cursor from the Activity page ("Refresh all"): refetch when
  // it advances. fetchData reads no reactive state, so cursor is the only
  // dependency.
  $effect(() => {
    if (cursor) fetchData();
  });

  // Usage refetch: runs on mount, on range change, and on Refresh-all.
  // Both reads are in the effect body so they stay tracked; fetchUsage
  // takes the range as a parameter and reads no reactive state itself.
  $effect(() => {
    const range = usageRange;
    if (cursor >= 0) fetchUsage(range);
  });

  // Trend shorthand: up/down/flat arrow plus the magnitude, e.g. "↑ 12.5%".
  function trendText(trend) {
    if (!trend) return "—";
    const arrow =
      trend.direction === "up" ? "↑" : trend.direction === "down" ? "↓" : "→";
    const pct = Math.abs(Number(trend.percentage) || 0).toFixed(1);
    return `${arrow} ${pct}%`;
  }

  // Session-cost tally in Freebucks (per-session wire price summed per
  // entry, not a per-token USD estimate): mirrors freebucksPriceLabel.
  function formatCost(cost) {
    const n = Number(cost ?? 0);
    if (!Number.isFinite(n) || n === 0) return "0 Freebucks";
    const s = Number.isInteger(n) ? String(n) : n.toFixed(1);
    return `${s} Freebucks`;
  }
</script>

<div class="space-y-6">
  {#if loading}
    <div class="space-y-6" aria-busy="true">
      <div class="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-4 gap-4">
        {#each Array(4) as _, i (i)}
          <div class="skeleton skeleton-card h-24"></div>
        {/each}
      </div>
      <div class="skeleton skeleton-card h-64"></div>
      <span class="sr-only">{$tr("Loading metrics")}</span>
    </div>
  {:else if error}
    <div class="space-y-4">
      <Button variant="secondary" onclick={fetchData}>
        <RefreshCw size={15} />
        {$tr("Retry")}
      </Button>
    </div>
  {:else if data}
    <!-- Token usage overview (9Router-style): range tabs, overview/details
         toggle, five total cards, per-entry table in details mode. -->
    <Card
      title={$tr("Token usage")}
      description={$tr("Session cost in Freebucks — not a billing figure.")}
      pad="none"
    >
      {#snippet actions()}
        <div class="flex max-w-full flex-wrap items-center gap-2">
          <SegmentedControl
            bind:value={usageRange}
            options={[
              { id: "today", label: "Today" },
              { id: "24h", label: "24h" },
              { id: "7d", label: "7D" },
              { id: "30d", label: "30D" },
              { id: "60d", label: "60D" },
            ]}
            ariaLabel={$tr("Usage range")}
            size="xs"
          />
          <SegmentedControl
            bind:value={usageView}
            options={[
              { id: "overview", label: $tr("Overview") },
              { id: "details", label: $tr("Details") },
            ]}
            ariaLabel={$tr("Usage view")}
            size="xs"
          />
        </div>
      {/snippet}
      <div class="px-5 py-4">
        {#if usageLoading && !usage}
          <div
            class="grid grid-cols-2 sm:grid-cols-3 lg:grid-cols-5 gap-4"
            aria-busy="true"
          >
            {#each Array(5) as _, i (i)}
              <div class="skeleton skeleton-card h-24"></div>
            {/each}
          </div>
          <span class="sr-only">{$tr("Loading usage")}</span>
        {:else if usageError && !usage}
          <div class="space-y-3">
            <Button variant="secondary" onclick={() => fetchUsage(usageRange)}>
              <RefreshCw size={15} />
              {$tr("Retry")}
            </Button>
          </div>
        {:else}
          {@const totals = usage?.totals ?? {}}
          <div class="grid grid-cols-2 sm:grid-cols-3 lg:grid-cols-5 gap-4">
            <Card class="p-4">
              <Stat
                label={$tr("Total Requests")}
                value={Number(totals.requests ?? 0).toLocaleString()}
              />
            </Card>
            <Card class="p-4">
              <Stat
                label={$tr("Total Input Tokens")}
                value={Number(totals.input ?? 0).toLocaleString()}
              />
            </Card>
            <Card class="p-4">
              <Stat
                label={$tr("Cached Tokens")}
                value={Number(totals.cached ?? 0).toLocaleString()}
              />
            </Card>
            <Card class="p-4">
              <Stat
                label={$tr("Output Tokens")}
                value={Number(totals.output ?? 0).toLocaleString()}
              />
            </Card>
            <Card class="p-4">
              <Stat label={$tr("Est. Cost")} value={formatCost(totals.cost)} />
            </Card>
          </div>
          {#if usageView === "details"}
            {#if usage?.entries?.length}
              <div class="overflow-x-auto mt-4">
                <table class="fp-table">
                  <caption class="sr-only"
                    >{$tr(
                      "Per-request token usage — time, model and token counts",
                    )}</caption
                  >
                  <thead>
                    <tr>
                      <th scope="col" class="w-[1%]">{$tr("Time")}</th>
                      <th scope="col" class="w-48">{$tr("Model")}</th>
                      <th scope="col" class="num w-[1%]">{$tr("Input")}</th>
                      <th scope="col" class="num w-[1%]">{$tr("Cached")}</th>
                      <th scope="col" class="num w-[1%]">{$tr("Output")}</th>
                      <th scope="col" class="num w-[1%]">{$tr("Total")}</th>
                    </tr>
                  </thead>
                  <tbody>
                    {#each usage.entries as e (e.req_id ?? e.ts_ms)}
                      <tr>
                        <td class="font-mono text-xs whitespace-nowrap w-[1%]"
                          >{new Date(Number(e.ts_ms ?? 0)).toLocaleString()}</td
                        >
                        <td class="font-mono text-xs"
                          ><span
                            class="block truncate max-w-full"
                            title={e.model}>{e.model ?? "—"}</span
                          ></td
                        >
                        <td class="num"
                          >{Number(e.input ?? 0).toLocaleString()}</td
                        >
                        <td class="num"
                          >{Number(e.cached ?? 0).toLocaleString()}</td
                        >
                        <td class="num"
                          >{Number(e.output ?? 0).toLocaleString()}</td
                        >
                        <td class="num"
                          >{Number(e.total ?? 0).toLocaleString()}</td
                        >
                      </tr>
                    {/each}
                  </tbody>
                </table>
              </div>
            {:else}
              <p class="mt-4 text-sm text-[var(--fp-muted)]">
                {$tr("No usage in this range yet.")}
              </p>
            {/if}
          {/if}
        {/if}
      </div>
    </Card>

    <!-- KPI row -->
    <div class="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-4">
      <Card class="p-4">
        <Stat
          label={$tr("Requests served")}
          value={(data.requests_total ?? 0).toLocaleString()}
          hint={$tr("{count} sample(s)", { count: data.sample_count ?? 0 })}
        />
      </Card>
      <Card class="p-4">
        <Stat
          label={$tr("Transient retries")}
          value={(data.transient_retries ?? 0).toLocaleString()}
          hint={$tr("trend {trend}", { trend: trendText(data.retries_trend) })}
          tone={(data.retries_trend?.direction ?? "flat") === "up"
            ? "warn"
            : "default"}
        />
      </Card>
      <Card class="p-4">
        <Stat
          label={$tr("Fingerprint rotations")}
          value={(data.fingerprint_rotations ?? 0).toLocaleString()}
          tone={(data.fingerprint_rotations ?? 0) > 0 ? "warn" : "default"}
        />
      </Card>
    </div>

    <!-- Sparkline row: server-rendered SVG; content is numeric coordinates
         plus static colors/labels only (see sparklineSVG), so it carries no
         user-controlled data and is safe to embed. -->
    <div class="grid grid-cols-1 lg:grid-cols-2 gap-4">
      <Card
        title={$tr("Requests over time")}
        description={$tr("Samples appended per poll — rolling window of 120.")}
        pad="none"
      >
        <div class="px-5 py-4 h-16 w-full [&_svg]:w-full [&_svg]:h-full">
          {#if data.requests_spark}
            <!-- eslint-disable-next-line svelte/no-at-html-tags -- server-rendered SVG: numeric coords + static colors only (sparklineSVG), no user-controlled data -->
            {@html data.requests_spark}
          {:else}
            <p class="text-sm text-[var(--fp-muted)]">
              {$tr("Not enough samples yet.")}
            </p>
          {/if}
        </div>
      </Card>
      <Card
        title={$tr("Retries over time")}
        description={$tr("Transient retry activity across the same window.")}
        pad="none"
      >
        <div class="px-5 py-4 h-16 w-full [&_svg]:w-full [&_svg]:h-full">
          {#if data.retries_spark}
            <!-- eslint-disable-next-line svelte/no-at-html-tags -- server-rendered SVG: numeric coords + static colors only (sparklineSVG), no user-controlled data -->
            {@html data.retries_spark}
          {:else}
            <p class="text-sm text-[var(--fp-muted)]">
              {$tr("Not enough samples yet.")}
            </p>
          {/if}
        </div>
      </Card>
    </div>

    <!-- Per-token breakdown -->
    <Card
      title={$tr("Per-token metrics")}
      description={$tr("24h request counts for every pool token.")}
      pad="none"
    >
      {#if data.per_tokens?.length}
        <div class="overflow-x-auto">
          <table class="fp-table">
            <caption class="sr-only"
              >{$tr("Per-token metrics — requests")}</caption
            >
            <thead>
              <tr>
                <th scope="col">{$tr("Token")}</th>
                <th scope="col" class="num w-[1%]">{$tr("Requests (24h)")}</th>
                <th scope="col" class="text-right w-[1%]"
                  ><span class="sr-only">{$tr("Links")}</span></th
                >
              </tr>
            </thead>
            <tbody>
              {#each data.per_tokens as p (p.token)}
                <tr>
                  <td class="w-[1%] whitespace-nowrap">
                    <button
                      type="button"
                      onclick={() => onOpenToken?.(p.token)}
                      title={$tr("Open token {idx}", { idx: p.token })}
                      class="fp-num font-mono text-xs text-[var(--fp-accent)] hover:underline cursor-pointer bg-transparent border-0 p-0"
                      >#{p.token}</button
                    >
                  </td>
                  <td class="num"
                    >{Number(p.requests_24h ?? 0).toLocaleString()}</td
                  >
                  <td class="w-[1%] whitespace-nowrap text-right">
                    <button
                      type="button"
                      onclick={() => onOpenLogs?.("")}
                      class="font-mono text-[11px] text-[var(--fp-accent)] hover:underline cursor-pointer bg-transparent border-0 p-0 whitespace-nowrap"
                      >{$tr("Logs")}</button
                    >
                  </td>
                </tr>
              {/each}
            </tbody>
          </table>
        </div>
      {:else}
        <div class="px-5 py-6">
          <p class="text-sm text-[var(--fp-muted)]">
            {$tr("No pool tokens yet.")}
          </p>
        </div>
      {/if}
    </Card>
  {/if}
</div>
