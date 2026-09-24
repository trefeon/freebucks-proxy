<script>
  /**
   * Ads — upstream ad firing telemetry, server-side proofing only.
   * Data: GET /admin/api/ads/summary (totals, credits, breakdowns) plus
   * GET /admin/api/ads/legs?limit=N (newest-first leg events). One fetch per
   * mount plus manual Refresh; no polling (legs are proofing telemetry, not
   * hot counters). Titles/brands only — impUrl/clickUrl never reach this
   * page (the ledger carries none by contract).
   */
  import { onMount } from "svelte";
  import { RefreshCw } from "@lucide/svelte";
  import PageShell from "../components/PageShell.svelte";
  import KpiGrid from "../components/KpiGrid.svelte";
  import Card from "../components/Card.svelte";
  import Button from "../components/Button.svelte";
  import Alert from "../components/Alert.svelte";
  import EmptyState from "../components/EmptyState.svelte";
  import StatusBadge from "../components/StatusBadge.svelte";
  import { fetchAPI } from "../api/client.js";
  import { adminApi } from "../api/paths.js";
  import { push as pushToast } from "../stores/toast.js";
  import { tr } from "../i18n.js";
  import { recordPageVisit } from "../stores/pageState.js";

  const LEGS_LIMIT = 50;

  let summary = $state(null);
  let legs = $state(null);
  let loading = $state(true);
  let error = $state("");

  function num(v) {
    const n = Number(v);
    return Number.isFinite(n) ? n : 0;
  }

  function countRows(obj) {
    if (!obj || typeof obj !== "object" || Array.isArray(obj)) return [];
    return Object.entries(obj)
      .map(([name, v]) => ({ name: String(name), count: num(v) }))
      .sort((a, b) => b.count - a.count);
  }

  function errorTotal(v) {
    if (Array.isArray(v)) return v.length;
    return num(v);
  }

  // Ledger ts is epoch millis or an ISO instant (lane B owns the writer);
  // accept both, never render a raw value.
  function formatTs(v) {
    if (v == null || v === "") return "\u2014";
    let ms;
    if (typeof v === "number") {
      ms = v < 1e12 ? v * 1000 : v;
    } else if (/^\d+$/.test(String(v).trim())) {
      const n = Number(v);
      ms = n < 1e12 ? n * 1000 : n;
    } else {
      ms = Date.parse(v);
    }
    if (!Number.isFinite(ms)) return "\u2014";
    return new Date(ms).toLocaleString();
  }

  function normalizeLeg(e) {
    const creditsRaw = e?.credits == null ? null : Number(e.credits);
    return {
      ts: e?.ts ?? null,
      surface: e?.surface != null ? String(e.surface) : "\u2014",
      provider: e?.provider != null ? String(e.provider) : "\u2014",
      leg: e?.leg != null ? String(e.leg) : "\u2014",
      title: e?.title != null ? String(e.title) : "",
      brand: e?.brand != null ? String(e.brand) : "",
      credits:
        creditsRaw != null && Number.isFinite(creditsRaw) ? creditsRaw : null,
      error: e?.error != null ? String(e.error) : "",
    };
  }

  function legTone(leg) {
    if (leg === "impression" || leg === "streak") return "good";
    return "info";
  }

  async function fetchAds() {
    if (summary === null && legs === null) loading = true;
    try {
      const [s, l] = await Promise.all([
        fetchAPI(adminApi.adsSummary),
        fetchAPI(`${adminApi.adsLegs}?limit=${LEGS_LIMIT}`),
      ]);
      summary = s ?? null;
      legs = Array.isArray(l) ? l : Array.isArray(l?.legs) ? l.legs : [];
      error = "";
    } catch (e) {
      const msg = e?.message || $tr("Failed to load ads telemetry");
      if (summary === null && legs === null) {
        error = msg;
      } else {
        pushToast({ tone: "error", title: msg });
      }
    } finally {
      loading = false;
    }
  }

  onMount(() => {
    recordPageVisit("ads");
    fetchAds();
  });

  let totals = $derived({
    auction: num(summary?.totals?.auction),
    impression: num(summary?.totals?.impression),
    streak: num(summary?.totals?.streak),
  });
  let creditsGranted = $derived(num(summary?.creditsGranted));
  let providerRows = $derived(countRows(summary?.byProvider));
  let surfaceRows = $derived(countRows(summary?.bySurface));
  let legRows = $derived((legs ?? []).map(normalizeLeg));
  let summaryErrors = $derived(errorTotal(summary?.errors));
  let legErrors = $derived(legRows.filter((r) => r.error));
  let errorLines = $derived.by(() => {
    const lines = [];
    if (summaryErrors > 0)
      lines.push($tr("{count} recorded in summary", { count: summaryErrors }));
    for (const r of legErrors.slice(0, 3))
      lines.push(`${r.leg} \u00b7 ${r.surface} \u00b7 ${r.error}`);
    return lines;
  });
  let showErrorNote = $derived(
    summary !== null && (summaryErrors > 0 || legErrors.length > 0),
  );
  let allQuiet = $derived(
    totals.auction + totals.impression + totals.streak + creditsGranted === 0 &&
      legRows.length === 0,
  );
  let empty = $derived(
    summary !== null && legs !== null && allQuiet
      ? {
          title: $tr("No ad legs yet"),
          description: $tr(
            "Auction and impression legs will appear here once the proxy fires them.",
          ),
        }
      : null,
  );
</script>

<PageShell
  crumb="freebucks-proxy / Admin / ads.conf"
  title={$tr("Ads")}
  description={$tr(
    "Upstream ad auction and impression legs fired by the proxy",
  )}
  {loading}
  {error}
  {empty}
  onRetry={fetchAds}
>
  {#snippet actions()}
    <div class="flex flex-wrap items-center gap-2">
      {#if summary?.lastEventAt}
        <span class="fp-num text-xs text-[var(--fp-dim)]"
          >{$tr("last leg {time}", {
            time: formatTs(summary.lastEventAt),
          })}</span
        >
      {/if}
      <Button variant="secondary" size="sm" onclick={fetchAds}>
        <RefreshCw size={13} />
        <span class="hidden min-[480px]:inline">{$tr("Refresh")}</span>
      </Button>
    </div>
  {/snippet}

  {#if showErrorNote}
    <Alert tone="warning" sticky title={$tr("Ad legs reporting errors")}>
      <ul class="mt-1 flex flex-col gap-0.5">
        {#each errorLines as line (line)}
          <li class="min-w-0 break-words font-mono text-[11px]">{line}</li>
        {/each}
      </ul>
    </Alert>
  {/if}

  <div data-testid="ads-kpis">
    <KpiGrid
      items={[
        {
          label: $tr("Auctions"),
          value: totals.auction,
          hint: $tr("auction legs fired"),
        },
        {
          label: $tr("Impressions"),
          value: totals.impression,
          hint: $tr("impression legs fired"),
        },
        {
          label: $tr("Streak"),
          value: totals.streak,
          hint: $tr("credit-granting streak legs"),
        },
        {
          label: $tr("Credits granted"),
          value: creditsGranted,
          hint: $tr("Freebucks granted"),
          tone: creditsGranted > 0 ? "good" : "default",
        },
      ]}
    />
  </div>

  <div class="grid grid-cols-1 gap-4 md:grid-cols-2">
    <Card
      title={$tr("Legs by provider")}
      description={$tr("Auction, impression, and streak legs per provider")}
      pad="none"
    >
      {#if providerRows.length}
        <div class="overflow-x-auto">
          <table class="fp-table w-full" data-testid="ads-by-provider">
            <caption class="sr-only">{$tr("Ad legs by provider")}</caption>
            <thead>
              <tr>
                <th scope="col">{$tr("Provider")}</th>
                <th scope="col" class="text-right">{$tr("Legs")}</th>
              </tr>
            </thead>
            <tbody>
              {#each providerRows as row (row.name)}
                <tr>
                  <td class="font-mono text-xs">{row.name}</td>
                  <td class="fp-num text-right text-xs">{row.count}</td>
                </tr>
              {/each}
            </tbody>
          </table>
        </div>
      {:else}
        <div class="px-5 py-6">
          <p class="text-sm text-[var(--fp-muted)]">
            {$tr("No provider breakdown yet.")}
          </p>
        </div>
      {/if}
    </Card>

    <Card
      title={$tr("Legs by surface")}
      description={$tr("Where the proxy fired each leg")}
      pad="none"
    >
      {#if surfaceRows.length}
        <div class="overflow-x-auto">
          <table class="fp-table w-full" data-testid="ads-by-surface">
            <caption class="sr-only">{$tr("Ad legs by surface")}</caption>
            <thead>
              <tr>
                <th scope="col">{$tr("Surface")}</th>
                <th scope="col" class="text-right">{$tr("Legs")}</th>
              </tr>
            </thead>
            <tbody>
              {#each surfaceRows as row (row.name)}
                <tr>
                  <td class="font-mono text-xs">{row.name}</td>
                  <td class="fp-num text-right text-xs">{row.count}</td>
                </tr>
              {/each}
            </tbody>
          </table>
        </div>
      {:else}
        <div class="px-5 py-6">
          <p class="text-sm text-[var(--fp-muted)]">
            {$tr("No surface breakdown yet.")}
          </p>
        </div>
      {/if}
    </Card>
  </div>

  <Card
    title={$tr("Recent legs")}
    description={$tr("Newest first; titles and brands only, never ad URLs")}
  >
    {#if legRows.length}
      <ul class="flex flex-col gap-2">
        {#each legRows as r, i (`${r.ts ?? i}-${r.leg}-${r.surface}-${i}`)}
          <li
            data-testid="ads-leg"
            class="flex flex-col gap-1 border-b border-[var(--fp-border)] pb-2 last:border-0 last:pb-0"
          >
            <div class="flex flex-wrap items-center gap-x-3 gap-y-1">
              <span class="fp-num shrink-0 text-[11px] text-[var(--fp-dim)]"
                >{formatTs(r.ts)}</span
              >
              <StatusBadge status={r.leg} tone={legTone(r.leg)} />
              <span class="font-mono text-[11px] text-[var(--fp-muted)]"
                >{r.surface}</span
              >
              <span class="font-mono text-[11px] text-[var(--fp-muted)]"
                >{r.provider}</span
              >
              {#if r.credits != null}
                <span
                  class="fp-num text-[11px] font-medium text-[var(--fp-success)]"
                  >+{r.credits} {$tr("Freebucks")}</span
                >
              {/if}
            </div>
            {#if r.title || r.brand}
              <div class="min-w-0 break-words text-xs text-[var(--fp-text)]">
                {r.title}{#if r.title && r.brand}
                  ·
                {/if}{#if r.brand}<span class="text-[var(--fp-muted)]"
                    >{r.brand}</span
                  >{/if}
              </div>
            {/if}
            {#if r.error}
              <div
                class="min-w-0 break-words font-mono text-[11px] text-[var(--fp-warning)]"
              >
                {r.error}
              </div>
            {/if}
          </li>
        {/each}
      </ul>
    {:else}
      <EmptyState
        title={$tr("No legs recorded yet")}
        description={$tr(
          "The summary has counts but no recent leg events were returned.",
        )}
      />
    {/if}
  </Card>
</PageShell>
