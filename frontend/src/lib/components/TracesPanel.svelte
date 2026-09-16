<script>
  import { onMount } from "svelte";
  import { ChevronDown, ChevronRight, RefreshCw } from "@lucide/svelte";
  import Card from "./Card.svelte";
  import {
    push as pushToast,
    dismiss as dismissToast,
  } from "../stores/toast.js";
  import Button from "./Button.svelte";
  import EmptyState from "./EmptyState.svelte";
  import { fetchAPI } from "../api/client.js";
  import { adminApi } from "../api/paths.js";
  import { tr } from "../i18n.js";
  import { formatTime } from "../utils/format.js";

  let {
    cursor = 0,
    focusReqId = "",
    onOpenToken = null,
    onOpenLogs = null,
  } = $props();

  let data = $state(null);
  let loading = $state(true);
  let error = $state("");
  let errorToast = $state(0);
  function notifyError(msg) {
    if (errorToast) dismissToast(errorToast);
    errorToast = msg
      ? pushToast({
          tone: "error",
          title: $tr("Could not load this page"),
          body: msg,
        })
      : 0;
  }
  // Local focus dismissal: the parent sets focusReqId (log→trace link); the
  // "Clear" chip below resets to the full list without round-tripping.
  let clearedFocus = $state(false);

  // Trace-log display cap: the backend serves up to 200 rows; the table
  // shows the first 20 with a footer control to reveal the rest.
  const TRACE_DEFAULT_LIMIT = 20;
  let showAll = $state(false);
  let effectiveFocus = $derived(focusReqId && !clearedFocus ? focusReqId : "");

  // A newly arrived focus always re-arms, even after a previous Clear.
  // A new focus also collapses the list back to the default 20 rows.
  $effect(() => {
    void focusReqId;
    clearedFocus = false;
    showAll = false;
  });

  async function fetchData() {
    try {
      data = await fetchAPI(adminApi.traces);
      error = "";
      notifyError("");
    } catch (e) {
      error = e.message || $tr("Failed to load traces");
      notifyError(error);
    } finally {
      loading = false;
    }
  }

  onMount(fetchData);

  // Shared time cursor from the Activity page ("Refresh all"): refetch when
  // it advances. fetchData reads no reactive state, so cursor is the only
  // dependency.
  $effect(() => {
    if (cursor) fetchData();
  });

  const rowReqId = (t) => t.req_id ?? t.reqId ?? "";
  let rowsHaveReqId = $derived((data?.traces || []).some((t) => rowReqId(t)));
  let visibleTraces = $derived.by(() => {
    const all = data?.traces || [];
    if (!effectiveFocus || !rowsHaveReqId) return all;
    return all.filter((t) => String(rowReqId(t)) === String(effectiveFocus));
  });

  // Display slice only — the full dataset stays in memory (no fetch or
  // ring change); existing order/filter are untouched, only the row count
  // rendered is capped until the footer control reveals the rest.
  let traceTotal = $derived(visibleTraces.length);
  let traceShown = $derived(
    showAll ? traceTotal : Math.min(TRACE_DEFAULT_LIMIT, traceTotal),
  );
  let displayTraces = $derived(
    showAll ? visibleTraces : visibleTraces.slice(0, TRACE_DEFAULT_LIMIT),
  );
  let traceDescription = $derived(
    $tr("Showing {shown} of {total} chat traces from the in-memory log ring.", {
      shown: traceShown,
      total: traceTotal,
    }),
  );
  // Fallback highlight when rows carry no req_id: match token/time instead.
  function highlightRow(t) {
    if (!effectiveFocus || rowsHaveReqId) return false;
    return (
      String(t.token ?? "") === String(effectiveFocus) ||
      String(t.time ?? "").includes(String(effectiveFocus))
    );
  }

  function accIndex(tok) {
    const t = String(tok ?? "").trim();
    return /^\d+$/.test(t) ? Number(t) : null;
  }

  // Per-trace LLM token counts ride the Contract UsageRecord keys verbatim
  // (input/output/cached/reasoning/total); older rows without them render
  // no token line. Never confuse these with the Account column: that value
  // is the serving pool account index ("bridge" for client-supplied
  // tokens), not LLM token usage.
  function num(n) {
    const v = Number(n);
    return Number.isFinite(v) ? v.toLocaleString() : "0";
  }
  function hasUsage(t) {
    return (
      t != null &&
      (t.input != null ||
        t.output != null ||
        t.cached != null ||
        t.total != null)
    );
  }
  function tokLine(t) {
    if (!hasUsage(t)) return "";
    const parts = [
      `${num(t.input)} in`,
      `${num(t.cached)} cached`,
      `${num(t.output)} out`,
      `${num(t.total)} total`,
    ];
    if (Number(t.reasoning) > 0) parts.push(`${num(t.reasoning)} reasoning`);
    return parts.join(" · ");
  }
  // Rate-limited error rows may carry no serving token but a `rate_tokens`
  // list (comma-joined 1-based pool indices, e.g. "2" or "1,3"). Show the
  // first binding token as #N with a title naming every limited account.
  // `agent` names the serving agent when the backend supplies it.
  function rateList(t) {
    const raw = String(t?.rate_tokens ?? "").trim();
    if (!raw) return [];
    return raw
      .split(",")
      .map((s) => s.trim())
      .filter((s) => /^\d+$/.test(s));
  }
  function rateFirst(t) {
    const l = rateList(t);
    return l.length ? Number(l[0]) : null;
  }

  // ── Dense cells ──────────────────────────────────────────────────────
  // The ring is 200 rows deep, so every cell is one line: the visible form
  // stays short and the exact values ride `title` plus the row's expanded
  // detail, which is reachable by keyboard and by touch.

  // 276,467 → 276.5k. Locale-grouped digits would triple the cell width.
  function compactNum(n) {
    const v = Number(n);
    if (!Number.isFinite(v)) return "0";
    if (Math.abs(v) < 1000) return String(v);
    const [div, suffix] = Math.abs(v) >= 1e6 ? [1e6, "M"] : [1e3, "k"];
    return `${(v / div).toFixed(1).replace(/\.0$/, "")}${suffix}`;
  }
  // Total LLM tokens for the row; older rows that logged no total fall back
  // to input + output.
  function tokCompact(t) {
    if (t?.total != null) return compactNum(t.total);
    return compactNum(Number(t?.input ?? 0) + Number(t?.output ?? 0));
  }

  // Phases arrive as an ordered list built by the backend (acquire_ms,
  // session_refresh_ms, run_acquire_ms, upstream_ttfb_ms, total_ms, and
  // whatever a later release adds): never keyed to a fixed set here, so a new
  // phase renders on its own.
  const PHASES_INLINE = 2;
  function phaseLine(ph) {
    return `${ph.name} ${ph.ms}ms`;
  }
  function phaseList(t) {
    return (t?.phases || []).filter((ph) => ph?.name);
  }
  // Pipeline order, every phase: what the title and the expanded row show.
  function phasesFull(t) {
    return phaseList(t).map(phaseLine).join(" · ");
  }
  // Dominant first, so a summary the cell has to clip keeps the phases that
  // actually ate the wall clock; the tail is one expand (or `+N`) away.
  function phasesSummary(t) {
    return phaseList(t)
      .map((ph, i) => ({ ph, i }))
      .sort((a, b) => Number(b.ph.ms) - Number(a.ph.ms) || a.i - b.i)
      .slice(0, PHASES_INLINE)
      .map(({ ph }) => phaseLine(ph))
      .join(" · ");
  }
  function phasesRest(t) {
    return Math.max(0, phaseList(t).length - PHASES_INLINE);
  }
  // Whether the row's detail has anything the one-line cells left out.
  function hasDetail(t) {
    return Boolean(
      phaseList(t).length || hasUsage(t) || t?.agent || t?.error || rowReqId(t),
    );
  }

  // One row's detail open at a time. The table and the stacked cards share
  // the state, so a resize keeps the open row open.
  let expandedIndex = $state(-1);
  function toggleRow(i) {
    expandedIndex = expandedIndex === i ? -1 : i;
  }
</script>

<div class="space-y-6">
  <!-- Everything the one-line cells leave out: the phase list in pipeline
       order, the exact token counts, the serving agent and the request id,
       rendered by the desktop detail row and the mobile card alike. -->
  {#snippet traceDetail(t)}
    <div
      class="flex flex-wrap items-center gap-x-4 gap-y-1 font-mono text-[11px] text-[var(--fp-muted)]"
    >
      {#if phaseList(t).length}
        <span
          >{$tr("Phases (pipeline order): {list}", {
            list: phasesFull(t),
          })}</span
        >
      {/if}
      {#if hasUsage(t)}
        <span>{$tr("Tokens: {list}", { list: tokLine(t) })}</span>
      {/if}
      {#if t.agent}
        <span>{$tr("Agent: {name}", { name: t.agent })}</span>
      {/if}
      {#if rowReqId(t)}
        <span>{$tr("req_id: {id}", { id: rowReqId(t) })}</span>
      {/if}
      {#if t.error}
        <span class="text-[var(--fp-error)]"
          >{$tr("Error: {msg}", { msg: t.error })}</span
        >
      {/if}
    </div>
  {/snippet}
  {#snippet tracesFooter()}
    <div
      class="flex flex-col sm:flex-row sm:items-center justify-between gap-2.5 px-4 py-3"
    >
      <span class="fp-num text-xs text-[var(--fp-muted)]" role="status">
        {$tr("Showing {shown} of {total}", {
          shown: traceShown,
          total: traceTotal,
        })}
      </span>
      <Button
        variant="secondary"
        size="sm"
        aria-expanded={showAll}
        onclick={() => (showAll = !showAll)}
      >
        {showAll
          ? $tr("Show fewer")
          : $tr("Show all {total}", { total: traceTotal })}
      </Button>
    </div>
  {/snippet}
  {#if loading}
    <div class="space-y-6" aria-busy="true">
      <div class="skeleton skeleton-card h-64"></div>
      <span class="sr-only">{$tr("Loading traces")}</span>
    </div>
  {:else if error}
    <div>
      <Button variant="secondary" onclick={fetchData}>
        <RefreshCw size={15} />
        {$tr("Retry")}
      </Button>
    </div>
  {:else if data?.enabled}
    {#if effectiveFocus}
      <div class="flex flex-wrap items-center gap-2">
        <span
          class="inline-flex items-center rounded border border-[var(--fp-accent)]/30 bg-[var(--fp-accent-dim)] px-2 py-1 font-mono text-xs text-[var(--fp-accent)]"
        >
          {$tr("Filtered to {id}", { id: effectiveFocus })}
        </span>
        <Button
          variant="ghost"
          size="sm"
          onclick={() => (clearedFocus = true)}
          class="!h-7 !text-xs"
        >
          {$tr("Clear")}
        </Button>
      </div>
    {/if}
    <Card
      title={$tr("Trace log")}
      description={traceDescription}
      footer={traceTotal > TRACE_DEFAULT_LIMIT ? tracesFooter : undefined}
      pad="none"
    >
      {#if visibleTraces?.length}
        <!-- table-fixed holds the card off a horizontal scroller at every
             width: each column owns its content, so the wide cells (model,
             phases) truncate instead of widening the table. Below lg the
             stacked cards take over. -->
        <div class="hidden lg:block">
          <table
            class="fp-table w-full table-fixed [&_td:not([colspan])]:!px-2 [&_th]:!px-2"
          >
            <caption class="sr-only"
              >{$tr(
                "Chat traces — time, account, model, tokens, status, latency and phases",
              )}</caption
            >
            <thead>
              <tr>
                <th scope="col" class="w-[70px]">{$tr("Time")}</th>
                <th scope="col" class="w-[44px]">{$tr("Account")}</th>
                <th scope="col" class="w-[18%]">{$tr("Model")}</th>
                <th scope="col" class="num w-[64px]">{$tr("Tokens")}</th>
                <th scope="col" class="w-[160px] whitespace-nowrap"
                  >{$tr("Status")}</th
                >
                <th scope="col" class="num w-[68px]">{$tr("Latency")}</th>
                <th scope="col">{$tr("Phases")}</th>
                <th scope="col" class="w-[44px] text-right"
                  ><span class="sr-only">{$tr("Links")}</span></th
                >
              </tr>
            </thead>
            <tbody>
              {#each displayTraces as t, i (t.time + "|" + (rowReqId(t) ?? "") + "|" + i)}
                {@const tidx = accIndex(t.token)}
                {@const reqId = rowReqId(t)}
                {@const limited = rateFirst(t)}
                {@const usage = tokLine(t)}
                {@const open = expandedIndex === i}
                <tr class={highlightRow(t) ? "bg-amber-500/5" : ""}>
                  <td
                    class="whitespace-nowrap font-mono text-[11px] text-[var(--fp-muted)]"
                    >{formatTime(t.time)}</td
                  >
                  <td class="whitespace-nowrap">
                    {#if tidx !== null}
                      <button
                        type="button"
                        onclick={() => onOpenToken?.(tidx)}
                        title={$tr("Pool account {idx} — not LLM token usage", {
                          idx: tidx,
                        })}
                        aria-label={$tr(
                          "Pool account {idx} — not LLM token usage",
                          { idx: tidx },
                        )}
                        class="fp-num font-mono text-xs text-[var(--fp-accent)] hover:underline cursor-pointer bg-transparent border-0 p-0"
                        >#{t.token}</button
                      >
                    {:else if limited !== null}
                      <button
                        type="button"
                        onclick={() => onOpenToken?.(limited)}
                        title={$tr(
                          "Rate-limited accounts {list} — first binding token shown",
                          { list: rateList(t).join(", ") },
                        )}
                        aria-label={$tr(
                          "Rate-limited accounts {list} — first binding token shown",
                          { list: rateList(t).join(", ") },
                        )}
                        class="fp-num font-mono text-xs text-[var(--fp-accent)] hover:underline cursor-pointer bg-transparent border-0 p-0"
                        >#{limited}</button
                      >
                    {:else if t.token && t.token !== "—"}
                      <span
                        class="fp-num font-mono text-xs"
                        title={$tr(
                          "Serving account {name} — not LLM token usage",
                          { name: t.token },
                        )}>#{t.token}</span
                      >
                    {:else}
                      <span class="fp-num font-mono text-xs">—</span>
                    {/if}
                  </td>
                  <td class="font-mono text-[11px]">
                    <span
                      class="block truncate max-w-full"
                      title={t.agent
                        ? $tr("{model} · agent {agent}", {
                            model: t.model || "—",
                            agent: t.agent,
                          })
                        : t.model}>{t.model || "—"}</span
                    >
                  </td>
                  <td
                    class="num whitespace-nowrap font-mono text-[11px] text-[var(--fp-muted)]"
                    title={usage
                      ? $tr("Input / cached / output / total LLM tokens")
                      : ""}
                  >
                    {#if usage}<span class="sr-only">{usage}</span>{tokCompact(
                        t,
                      )}{:else}—{/if}
                  </td>
                  <!-- The error rides the status cell, so a row without one
                       reserves no space and the other cells never move. -->
                  <td class="whitespace-nowrap">
                    <span class="flex items-center gap-1.5 min-w-0">
                      <span
                        class={t.status === "error"
                          ? "text-[var(--fp-error)] font-semibold"
                          : "text-[var(--fp-success)]"}
                      >
                        {t.status || "ok"}
                      </span>
                      {#if t.error}
                        <span
                          class="truncate text-[11px] text-[var(--fp-error)]"
                          title={t.error}>{t.error}</span
                        >
                      {/if}
                    </span>
                  </td>
                  <td class="num whitespace-nowrap">{t.ms ? t.ms : "—"}</td>
                  <td class="min-w-0">
                    {#if phaseList(t).length}
                      <button
                        type="button"
                        onclick={() => toggleRow(i)}
                        aria-expanded={open}
                        title={$tr("All phases in pipeline order: {list}", {
                          list: phasesFull(t),
                        })}
                        class="flex w-full min-w-0 items-center gap-1 text-left font-mono text-[11px] text-[var(--fp-muted)] cursor-pointer bg-transparent border-0 p-0 hover:text-[var(--fp-text)]"
                      >
                        {#if open}
                          <ChevronDown size={11} class="shrink-0" />
                        {:else}
                          <ChevronRight size={11} class="shrink-0" />
                        {/if}
                        <span class="truncate min-w-0">{phasesSummary(t)}</span>
                        {#if phasesRest(t)}
                          <span class="shrink-0 text-[var(--fp-dim)]"
                            >+{phasesRest(t)}</span
                          >
                        {/if}
                      </button>
                    {:else}
                      <span class="text-[var(--fp-dim)]">—</span>
                    {/if}
                  </td>
                  <td class="whitespace-nowrap text-right">
                    <button
                      type="button"
                      onclick={() => onOpenLogs?.(reqId ? String(reqId) : "")}
                      title={reqId
                        ? $tr("Open logs for trace {id}", { id: reqId })
                        : $tr("No request id for this entry")}
                      aria-label={reqId
                        ? $tr("Open logs for trace {id}", { id: reqId })
                        : $tr("Open logs")}
                      class="font-mono text-[11px] text-[var(--fp-accent)] hover:underline cursor-pointer bg-transparent border-0 p-0 whitespace-nowrap"
                      >{$tr("Logs")}</button
                    >
                  </td>
                </tr>
                {#if open}
                  <tr class="bg-[var(--fp-surface-2)]">
                    <td colspan={8}>{@render traceDetail(t)}</td>
                  </tr>
                {/if}
              {/each}
            </tbody>
          </table>
        </div>
        <ul
          class="lg:hidden flex flex-col gap-2.5 p-3.5"
          aria-label={$tr("Chat traces")}
        >
          {#each displayTraces as t, i (t.time + "|" + (rowReqId(t) ?? "") + "|" + i)}
            {@const tidx = accIndex(t.token)}
            {@const reqId = rowReqId(t)}
            {@const limited = rateFirst(t)}
            {@const usage = tokLine(t)}
            {@const open = expandedIndex === i}
            <li class="fp-inset rounded px-3 py-2 flex flex-col gap-1 min-w-0">
              <div class="flex items-center justify-between gap-2 min-w-0">
                <span
                  class="whitespace-nowrap font-mono text-[11px] text-[var(--fp-muted)]"
                  >{formatTime(t.time)}</span
                >
                <span class="flex items-center gap-2 min-w-0">
                  {#if t.error}
                    <span
                      class="truncate text-[11px] text-[var(--fp-error)]"
                      title={t.error}>{t.error}</span
                    >
                  {/if}
                  <span
                    class={t.status === "error"
                      ? "text-[var(--fp-error)] font-semibold text-xs"
                      : "text-[var(--fp-success)] text-xs"}
                  >
                    {t.status || "ok"}
                  </span>
                </span>
              </div>
              <div class="flex items-center gap-1.5 min-w-0 text-xs">
                {#if tidx !== null}
                  <button
                    type="button"
                    onclick={() => onOpenToken?.(tidx)}
                    title={$tr("Pool account {idx} — not LLM token usage", {
                      idx: tidx,
                    })}
                    aria-label={$tr(
                      "Pool account {idx} — not LLM token usage",
                      { idx: tidx },
                    )}
                    class="fp-num font-mono text-[var(--fp-accent)] hover:underline cursor-pointer bg-transparent border-0 p-0 shrink-0"
                    >#{t.token}</button
                  >
                {:else if limited !== null}
                  <button
                    type="button"
                    onclick={() => onOpenToken?.(limited)}
                    title={$tr(
                      "Rate-limited accounts {list} — first binding token shown",
                      { list: rateList(t).join(", ") },
                    )}
                    aria-label={$tr(
                      "Rate-limited accounts {list} — first binding token shown",
                      { list: rateList(t).join(", ") },
                    )}
                    class="fp-num font-mono text-[var(--fp-accent)] hover:underline cursor-pointer bg-transparent border-0 p-0 shrink-0"
                    >#{limited}</button
                  >
                {:else if t.token && t.token !== "—"}
                  <span
                    class="fp-num font-mono shrink-0"
                    title={$tr("Serving account {name} — not LLM token usage", {
                      name: t.token,
                    })}>#{t.token}</span
                  >
                {:else}
                  <span class="fp-num font-mono shrink-0">—</span>
                {/if}
                <code
                  class="fp-num truncate min-w-0 text-[11px] text-[var(--fp-muted)]"
                  >{t.model || "—"}{#if t.agent}
                    <span class="text-[var(--fp-dim)]">
                      · {t.agent}</span
                    >{/if}</code
                >
              </div>
              {#if phaseList(t).length}
                <div
                  class="flex items-center gap-1 min-w-0 font-mono text-[11px] text-[var(--fp-muted)]"
                >
                  <span class="truncate min-w-0" title={phasesFull(t)}
                    >{phasesSummary(t)}</span
                  >
                  {#if phasesRest(t)}
                    <span class="shrink-0 text-[var(--fp-dim)]"
                      >+{phasesRest(t)}</span
                    >
                  {/if}
                </div>
              {/if}
              <div
                class="flex items-center gap-2 min-w-0 font-mono text-[11px] text-[var(--fp-muted)]"
              >
                <span class="ml-auto flex shrink-0 items-center gap-2">
                  {#if usage}
                    <span
                      class="fp-num"
                      title={$tr("Input / cached / output / total LLM tokens")}
                      ><span class="sr-only">{usage}</span>{tokCompact(t)}</span
                    >
                  {/if}
                  <span class="fp-num">{t.ms ? t.ms : "—"}</span>
                  <button
                    type="button"
                    onclick={() => onOpenLogs?.(reqId ? String(reqId) : "")}
                    title={reqId
                      ? $tr("Open logs for trace {id}", { id: reqId })
                      : $tr("No request id for this entry")}
                    aria-label={reqId
                      ? $tr("Open logs for trace {id}", { id: reqId })
                      : $tr("Open logs")}
                    class="font-mono text-[11px] text-[var(--fp-accent)] hover:underline cursor-pointer bg-transparent border-0 p-0 whitespace-nowrap"
                    >{$tr("Logs")}</button
                  >
                  {#if hasDetail(t)}
                    <button
                      type="button"
                      onclick={() => toggleRow(i)}
                      aria-expanded={open}
                      aria-label={$tr("Show trace detail")}
                      class="flex items-center cursor-pointer bg-transparent border-0 p-0 text-[var(--fp-muted)] hover:text-[var(--fp-text)]"
                    >
                      {#if open}
                        <ChevronDown size={12} />
                      {:else}
                        <ChevronRight size={12} />
                      {/if}
                    </button>
                  {/if}
                </span>
              </div>
              {#if open}
                <div class="pt-1">{@render traceDetail(t)}</div>
              {/if}
            </li>
          {/each}
        </ul>
      {:else}
        <div class="px-5 py-6">
          <p class="text-sm text-[var(--fp-muted)]">
            {$tr("No traces recorded yet.")}
          </p>
        </div>
      {/if}
    </Card>
  {:else if data}
    <EmptyState
      title={$tr("Traces disabled")}
      description={$tr(
        "Trace collection is off — no log ring is wired to this dashboard.",
      )}
    />
  {/if}
</div>
