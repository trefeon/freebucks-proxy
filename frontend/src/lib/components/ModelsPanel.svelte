<script>
  import { onMount } from "svelte";
  import Stat from "./Stat.svelte";
  import Card from "./Card.svelte";
  import Button from "./Button.svelte";
  import {
    push as pushToast,
    dismiss as dismissToast,
  } from "../stores/toast.js";
  import EmptyState from "./EmptyState.svelte";
  import StatusBadge from "./StatusBadge.svelte";
  import CopyButton from "./CopyButton.svelte";
  import { fetchAPI } from "../api/client.js";
  import { adminApi } from "../api/paths.js";
  import { tokensData, ensureTokensStore } from "../stores/tokens.js";
  import {
    sortModelsByPrice,
    formatFreebucks,
    firstTabListPriceFor,
    offPeakCopy,
  } from "../utils/freebucks.js";
  import { tr } from "../i18n.js";
  // Cheapest-first order on the meter (upstream picker revamp): merge the
  // live per-token price maps first-win, and sort only when at least one
  // price exists — unmetered accounts keep the deliberate catalog order.
  const meteredPrices = $derived(
    (() => {
      const map = {};
      for (const t of live?.tokens ?? []) {
        for (const [id, p] of Object.entries(t.freebucks?.prices ?? {})) {
          if (!(id in map)) map[id] = p;
        }
      }
      return map;
    })(),
  );
  const orderedModels = $derived(
    data == null || Object.keys(meteredPrices).length === 0
      ? (data?.models ?? [])
      : sortModelsByPrice(
          data.models.map((m) => m.id),
          { prices: meteredPrices },
        )
          .map((id) => data.models.find((m) => m.id === id))
          .filter(Boolean),
  );
  let data = $state(null);
  let live = $state(null);
  let loading = $state(true);
  let error = $state("");
  let errorToast = $state(0);
  function notifyError(msg) {
    if (errorToast) dismissToast(errorToast);
    errorToast = msg
      ? pushToast({
          tone: "error",
          title: $tr("Failed to load models"),
          body: msg,
        })
      : 0;
  }

  // Row state: withdrawn rows (admission-refused, replacement named) render
  // "withdrawn". Unserved rows that a tier still admits (paid plan, limited
  // trial) render "unserved" — never "referral", which is the grant-gated
  // referral pool only. Rows without a binding (and legacy payloads without
  // the served flag) stay "unbound".
  function modelState(m) {
    if (m.withdrawn) return "withdrawn";
    if (!m.agent) return "unbound";
    if (m.served !== false) return "served";
    if ((m.pool ?? "") === "referral") return "referral";
    return "unserved";
  }
  function modelTone(state) {
    if (state === "served") return "good";
    if (state === "referral" || state === "unserved") return "info";
    return "idle";
  }
  // Tier chips render the wire tier ids as operator-readable labels: paid
  // is the plan-metered catalog, offer the capacity-limited trial.
  function tierLabel(t) {
    if (t === "paid") return "paid plan";
    if (t === "offer") return "limited trial";
    return t;
  }
  // Replacement display name for withdrawn rows (raw id when the
  // replacement is not in this payload).
  function replacementName(id) {
    const rows = data?.models ?? [];
    return rows.find((m) => m.id === id)?.display_name || id;
  }
  // Live campaign copy for the offer row: the shared pool counts straight
  // from the wire block, plus trial-used when this account's slice is
  // spent; rows outside the campaign say so instead of showing zeroes.
  function offerLine(m) {
    const o = m.offer;
    if (!o) return "trial not offered right now";
    const left = `${o.remaining} of ${o.total} sessions left`;
    return o.user_remaining <= 0 ? `${left} · trial used` : left;
  }
  async function load() {
    loading = true;
    error = "";
    notifyError("");
    try {
      data = await fetchAPI(adminApi.models);
    } catch (e) {
      error = e.message || $tr("Failed to load models");
      notifyError(error);
    } finally {
      loading = false;
    }
  }
  // Live price join: Freebucks/hr comes from the shared tokens snapshot,
  // keyed by model id across pool tokens. Sub-1 prices read "p
  // Freebucks/hr" like every other row (a "$p/hr" misreads as dollars);
  // fractionals keep one decimal, integers render bare.
  function freebucksFor(id) {
    for (const t of live?.tokens ?? []) {
      if (t.freebucks?.prices?.[id] != null) return t.freebucks;
    }
    return null;
  }
  function priceText(p) {
    const rounded = Math.round(p * 10) / 10;
    const body = Number.isInteger(rounded)
      ? String(rounded)
      : rounded.toFixed(1);
    return `${body} Freebucks/hr`;
  }
  function priceLabel(id) {
    const fb = freebucksFor(id);
    if (!fb) return "";
    return priceText(fb.prices[id]);
  }
  // Crossed-out list price, drawn only on rows the first-tab offer actually
  // moved (offer available + list > price; clamped-0 and pre-listPrices
  // quotes answer "" via firstTabListPriceFor).
  function strikeLabel(id) {
    const fb = freebucksFor(id);
    if (!fb) return "";
    const list = firstTabListPriceFor(fb, id);
    return list === undefined ? "" : formatFreebucks(list);
  }
  // Off-peak active/upcoming line for one model row ("" when no offer).
  function offPeakLine(id) {
    const fb = freebucksFor(id);
    if (!fb) return "";
    return offPeakCopy(fb, id)?.detail ?? "";
  }
  // Price staleness: the displayed price came from the live join, but every
  // contributing token is quota_stale (pool restarted since last probe).
  function priceIsStale(id) {
    if (!priceLabel(id)) return false;
    const contributors = (live?.tokens ?? []).filter(
      (t) => t.freebucks?.prices?.[id] != null,
    );
    return contributors.length > 0 && contributors.every((t) => t.quota_stale);
  }
  const staleTitle = $derived(
    $tr("Price may be stale — pool restarted since last probe"),
  );
  onMount(() => {
    const release = ensureTokensStore();
    const unsub = tokensData.subscribe((v) => {
      if (v) live = v;
    });
    return () => {
      release();
      unsub();
    };
  });
  onMount(load);

  const servedCount = $derived(
    data ? data.models.filter((m) => modelState(m) === "served").length : 0,
  );
</script>

{#if loading}
  <p
    role="status"
    aria-label={$tr("Loading…")}
    class="text-xs text-[var(--fp-dim)] font-mono"
  >
    {$tr("Loading…")}
  </p>
{:else if error}
  <div class="flex flex-col gap-3">
    <p class="text-sm text-[var(--fp-error)]" data-testid="inline-error">
      {error}
    </p>
    <div>
      <Button variant="secondary" onclick={load}>{$tr("Retry")}</Button>
    </div>
  </div>
{:else if data && data.models.length === 0}
  <EmptyState
    title={$tr("No models registered")}
    description={$tr(
      "The model registry is empty. Add model-to-agent mappings in the gateway config and reload.",
    )}
  />
{:else if data}
  <Stat
    label={$tr("Served Models")}
    value={`${servedCount} of ${data.models.length}`}
    hint={$tr("{count} registered · {agents} agents", {
      count: data.count,
      agents: data.agents,
    })}
    tone={servedCount > 0 ? "good" : "idle"}
    big
  />
  <p class="text-xs text-[var(--fp-muted)]" data-testid="models-note">
    {$tr("Live upstream values — identical for every account in the region.")}
  </p>

  <Card title={$tr("Models")} pad="none">
    <!-- Desktop: table (md+) -->
    <div class="hidden md:block overflow-x-auto">
      <table class="fp-table w-full">
        <thead>
          <tr>
            <th scope="col">{$tr("Model ID")}</th>
            <th scope="col" class="w-[1%] whitespace-nowrap">{$tr("Served")}</th
            >
            <th scope="col" class="w-[1%] whitespace-nowrap">{$tr("Agent")}</th>
            <th scope="col" class="text-right w-[1%] whitespace-nowrap"
              >{$tr("Price")}</th
            >
            <th scope="col" class="w-[1%] whitespace-nowrap">{$tr("Tier")}</th>
          </tr>
        </thead><tbody>
          {#each orderedModels as m (m.id)}
            {@const bound = Boolean(m.agent)}
            {@const st = modelState(m)}
            {@const effectivePrice = priceLabel(m.id) || m.price_label || "—"}
            {@const strike = strikeLabel(m.id)}
            {@const offPeak = offPeakLine(m.id)}
            {@const stale = priceIsStale(m.id)}
            <tr class={m.withdrawn ? "opacity-60" : ""}>
              <td>
                <div class="flex flex-col gap-0.5 min-w-0">
                  <div class="flex items-center gap-1.5 flex-wrap">
                    <strong
                      class="text-xs font-semibold text-[var(--fp-text)] truncate"
                    >
                      {m.display_name || m.id}
                    </strong>
                    {#if m.badges?.length}
                      {#each m.badges as badge (badge)}
                        <span
                          class="px-1 py-0.2 rounded text-[9px] uppercase tracking-wider border text-[var(--fp-dim)] bg-[var(--fp-surface)] border-[var(--fp-border)]"
                        >
                          {badge}
                        </span>
                      {/each}
                    {/if}
                  </div>
                  <div
                    class="flex items-center gap-1.5 flex-wrap text-[11px] text-[var(--fp-dim)] min-w-0"
                  >
                    <code
                      class="fp-num truncate max-w-[220px] text-[var(--fp-muted)]"
                      title={m.id}
                    >
                      {m.id}
                    </code>
                    <CopyButton
                      text={m.id}
                      label={$tr("Copy model ID")}
                      iconOnly
                    />
                    {#if m.tagline}
                      <span class="text-[var(--fp-dim)]">·</span>
                      <span class="text-[var(--fp-muted)]">{m.tagline}</span>
                    {/if}
                    {#if m.notice}
                      <span class="text-[var(--fp-dim)]">·</span>
                      <span class="italic">{m.notice}</span>
                    {/if}
                    {#if m.efforts?.length}
                      <span class="text-[var(--fp-dim)]">·</span>
                      <span>{$tr("Reasoning")}: {m.efforts.join("/")}</span>
                    {/if}
                  </div>
                </div>
              </td>
              <td class="w-[1%] whitespace-nowrap">
                <StatusBadge status={$tr(st)} tone={modelTone(st)} />
              </td>
              <td class="w-[1%] whitespace-nowrap">
                {#if bound}
                  <span class="fp-mono text-[var(--fp-muted)]">{m.agent}</span>
                {:else}
                  <span class="text-[var(--fp-dim)]">—</span>
                {/if}
              </td>
              <td class="w-[1%] whitespace-nowrap text-right">
                <span class="inline-flex flex-col items-end gap-0.5">
                  <span class="inline-flex items-center gap-1.5">
                    {#if strike}
                      <s
                        class="fp-num text-[11px] text-[var(--fp-dim)]"
                        title={$tr("Regular price")}>{strike}</s
                      >
                    {/if}
                    <span
                      class="fp-num text-xs font-semibold {effectivePrice ===
                      '0 Freebucks/hr'
                        ? 'text-emerald-400'
                        : 'text-[var(--fp-accent)]'}">{effectivePrice}</span
                    >
                    {#if stale}
                      <span
                        class="led led-warn shrink-0"
                        title={staleTitle}
                        aria-label={staleTitle}
                        role="img"
                      ></span>
                    {/if}
                  </span>
                  {#if offPeak}
                    <span class="fp-num text-[10px] text-[var(--fp-muted)]"
                      >{offPeak}</span
                    >
                  {/if}
                </span>
              </td>
              <td class="w-[1%] whitespace-nowrap" data-testid="model-tier">
                {#if m.withdrawn}
                  <span
                    class="px-1 py-0.2 rounded text-[9px] uppercase tracking-wider border text-[var(--fp-dim)] bg-[var(--fp-surface)] border-[var(--fp-border)]"
                  >
                    withdrawn
                  </span>
                  <p
                    data-testid="model-withdrawn"
                    class="mt-1 text-[11px] whitespace-normal text-[var(--fp-muted)]"
                  >
                    Withdrawn — use {replacementName(m.replacement)}
                  </p>
                {:else}
                  <span class="inline-flex flex-wrap gap-1">
                    {#each m.tiers ?? [] as t (t)}
                      <span
                        class="px-1 py-0.2 rounded text-[9px] uppercase tracking-wider border text-[var(--fp-muted)] bg-[var(--fp-surface)] border-[var(--fp-border)]"
                      >
                        {tierLabel(t)}
                      </span>
                    {/each}
                  </span>
                  {#if m.tiers?.includes("offer")}
                    <p
                      data-testid="model-offer"
                      class="mt-1 fp-num text-[11px] whitespace-normal text-[var(--fp-muted)]"
                    >
                      {offerLine(m)}
                    </p>
                  {/if}
                {/if}
              </td>
            </tr>
          {/each}
        </tbody>
      </table>
    </div>
    <!-- Mobile: stacked cards (< md) — no horizontal scrolling -->
    <ul
      class="md:hidden flex flex-col gap-2.5 p-3.5"
      aria-label={$tr("Models")}
    >
      {#each orderedModels as m (m.id)}
        {@const bound = Boolean(m.agent)}
        {@const st = modelState(m)}
        {@const effectivePrice = priceLabel(m.id) || m.price_label || "—"}
        {@const strike = strikeLabel(m.id)}
        {@const offPeak = offPeakLine(m.id)}
        {@const stale = priceIsStale(m.id)}
        <li
          class="fp-inset rounded p-3 flex flex-col gap-2 min-w-0 {m.withdrawn
            ? 'opacity-60'
            : ''}"
        >
          <div class="flex items-start justify-between gap-2 min-w-0">
            <div class="min-w-0">
              <strong
                class="text-sm font-bold text-[var(--fp-text)] block truncate"
              >
                {m.display_name || m.id}
              </strong>
              <div
                class="flex items-center gap-1.5 flex-wrap text-xs text-[var(--fp-dim)] pt-0.5 min-w-0"
              >
                <code class="fp-num truncate max-w-[200px]">{m.id}</code>
                <span class="shrink-0 -mr-1">
                  <CopyButton
                    text={m.id}
                    label={$tr("Copy model ID")}
                    iconOnly
                  />
                </span>
                {#if m.tagline}
                  <span>·</span>
                  <span class="text-[var(--fp-muted)]">{m.tagline}</span>
                {/if}
                {#each m.badges ?? [] as badge (badge)}
                  <span>·</span>
                  <span
                    class="px-1.5 py-0.2 rounded text-[10px] uppercase tracking-wider border bg-[var(--fp-surface)] border-[var(--fp-border)]"
                  >
                    {badge}
                  </span>
                {/each}
                {#if m.notice}
                  <span>·</span>
                  <span class="italic">{m.notice}</span>
                {/if}
                {#if m.efforts?.length}
                  <span>·</span>
                  <span>{$tr("Reasoning")}: {m.efforts.join("/")}</span>
                {/if}
              </div>
            </div>
            <StatusBadge status={$tr(st)} tone={modelTone(st)} />
          </div>
          <div
            class="flex items-center justify-between gap-2 text-xs pt-1 border-t border-[var(--fp-border)]/60"
          >
            <span class="inline-flex flex-col items-start gap-0.5">
              <span class="inline-flex items-center gap-1.5">
                {#if strike}
                  <s
                    class="fp-num text-[11px] text-[var(--fp-dim)]"
                    title={$tr("Regular price")}>{strike}</s
                  >
                {/if}
                <span
                  class="font-semibold {effectivePrice === '0 Freebucks/hr'
                    ? 'text-emerald-400'
                    : 'text-[var(--fp-accent)]'}"
                >
                  {effectivePrice}
                </span>
                {#if stale}
                  <span
                    class="led led-warn shrink-0"
                    title={staleTitle}
                    aria-label={staleTitle}
                    role="img"
                  ></span>
                {/if}
              </span>
              {#if offPeak}
                <span class="fp-num text-[10px] text-[var(--fp-muted)]"
                  >{offPeak}</span
                >
              {/if}
            </span>
          </div>
          <div
            class="flex flex-wrap items-center gap-1 pt-1 border-t border-[var(--fp-border)]/60"
            data-testid="model-tier"
          >
            {#if m.withdrawn}
              <span
                class="px-1.5 py-0.2 rounded text-[10px] uppercase tracking-wider border text-[var(--fp-dim)] bg-[var(--fp-surface)] border-[var(--fp-border)]"
              >
                withdrawn
              </span>
            {:else}
              {#each m.tiers ?? [] as t (t)}
                <span
                  class="px-1.5 py-0.2 rounded text-[10px] uppercase tracking-wider border text-[var(--fp-muted)] bg-[var(--fp-surface)] border-[var(--fp-border)]"
                >
                  {tierLabel(t)}
                </span>
              {/each}
            {/if}
          </div>
          {#if m.withdrawn}
            <p
              data-testid="model-withdrawn"
              class="text-xs text-[var(--fp-muted)]"
            >
              Withdrawn — use {replacementName(m.replacement)}
            </p>
          {:else if m.tiers?.includes("offer")}
            <p
              data-testid="model-offer"
              class="fp-num text-xs text-[var(--fp-muted)]"
            >
              {offerLine(m)}
            </p>
          {/if}
          <div class="flex items-center justify-between gap-2 text-xs min-w-0">
            <span class="text-[var(--fp-dim)] shrink-0">{$tr("Agent")}</span>
            {#if bound}
              <span
                class="fp-mono text-[var(--fp-muted)] text-right break-all min-w-0"
                >{m.agent}</span
              >
            {:else}
              <span class="text-[var(--fp-dim)]">—</span>
            {/if}
          </div>
        </li>
      {/each}
    </ul>
  </Card>
{/if}
