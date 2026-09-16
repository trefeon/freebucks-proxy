<script>
  import { onMount } from "svelte";
  import { RefreshCw } from "@lucide/svelte";
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
  let effectiveFocus = $derived(focusReqId && !clearedFocus ? focusReqId : "");

  // A newly arrived focus always re-arms, even after a previous Clear.
  $effect(() => {
    void focusReqId;
    clearedFocus = false;
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
</script>

<div class="space-y-6">
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
      description={$tr("Last 200 chat traces from the in-memory log ring.")}
      pad="none"
    >
      {#if visibleTraces?.length}
        <div class="overflow-x-auto hidden md:block">
          <table class="fp-table w-full min-w-[640px]">
            <caption class="sr-only"
              >{$tr(
                "Chat traces — time, account, model, tokens, status, latency and phases",
              )}</caption
            >
            <thead>
              <tr>
                <th scope="col" class="w-[1%]">{$tr("Time")}</th>
                <th scope="col" class="w-[1%]">{$tr("Account")}</th>
                <th scope="col" class="w-48">{$tr("Model")}</th>
                <th scope="col" class="w-[1%]">{$tr("Tokens")}</th>
                <th scope="col" class="w-[1%] whitespace-nowrap"
                  >{$tr("Status")}</th
                >
                <th scope="col" class="num w-[1%]">{$tr("Latency")}</th>
                <th scope="col">{$tr("Phases")}</th>
                <th scope="col">{$tr("Error")}</th>
                <th scope="col" class="text-right w-[1%]"
                  ><span class="sr-only">{$tr("Links")}</span></th
                >
              </tr>
            </thead>
            <tbody>
              {#each visibleTraces as t, i (t.time + "|" + (rowReqId(t) ?? "") + "|" + i)}
                {@const tidx = accIndex(t.token)}
                {@const reqId = rowReqId(t)}
                {@const limited = rateFirst(t)}
                {@const usage = tokLine(t)}
                <tr class={highlightRow(t) ? "bg-amber-500/5" : ""}>
                  <td
                    class="whitespace-nowrap font-mono text-[11px] text-[var(--fp-muted)] w-[1%]"
                    >{formatTime(t.time)}</td
                  >
                  <td class="w-[1%] whitespace-nowrap">
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
                  <td class="font-mono text-[11px]"
                    ><span class="block truncate max-w-full" title={t.model}
                      >{t.model || "—"}</span
                    >
                    {#if t.agent}
                      <span
                        class="block text-[10px] text-[var(--fp-dim)]"
                        title={$tr("Serving agent")}>{t.agent}</span
                      >
                    {/if}
                  </td>
                  <td
                    class="whitespace-nowrap font-mono text-[11px] text-[var(--fp-muted)] w-[1%]"
                    title={usage
                      ? $tr("Input / cached / output / total LLM tokens")
                      : ""}>{usage || "—"}</td
                  >
                  <td class="w-[1%] whitespace-nowrap">
                    <span
                      class={t.status === "error"
                        ? "text-[var(--fp-error)] font-semibold"
                        : "text-[var(--fp-success)]"}
                    >
                      {t.status || "ok"}
                    </span>
                  </td>
                  <td class="num w-[1%] whitespace-nowrap"
                    >{t.ms ? t.ms : "—"}</td
                  >
                  <td>
                    {#if t.phases?.length}
                      <div class="flex flex-wrap gap-1">
                        {#each t.phases as ph, j (ph.name + "|" + j)}
                          <span
                            class="px-1.5 py-0.5 rounded-[var(--fp-radius-sm)] bg-[var(--fp-surface-2)] text-[10px] font-mono text-[var(--fp-muted)]"
                          >
                            {ph.name}
                            {ph.ms}ms
                          </span>
                        {/each}
                      </div>
                    {:else}
                      <span class="text-[var(--fp-dim)]">—</span>
                    {/if}
                  </td>
                  <td
                    class="text-[var(--fp-error)] text-[11px] max-w-[200px] truncate"
                    >{t.error || ""}</td
                  >
                  <td class="w-[1%] whitespace-nowrap text-right">
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
              {/each}
            </tbody>
          </table>
        </div>
        <ul
          class="md:hidden flex flex-col gap-2.5 p-3.5"
          aria-label={$tr("Chat traces")}
        >
          {#each visibleTraces as t, i (t.time + "|" + (rowReqId(t) ?? "") + "|" + i)}
            {@const tidx = accIndex(t.token)}
            {@const reqId = rowReqId(t)}
            {@const limited = rateFirst(t)}
            {@const usage = tokLine(t)}
            <li class="fp-inset rounded p-3 flex flex-col gap-2 min-w-0">
              <div class="flex items-center justify-between gap-2 min-w-0">
                <span
                  class="whitespace-nowrap font-mono text-[11px] text-[var(--fp-muted)]"
                  >{formatTime(t.time)}</span
                >
                <span
                  class={t.status === "error"
                    ? "text-[var(--fp-error)] font-semibold text-xs"
                    : "text-[var(--fp-success)] text-xs"}
                >
                  {t.status || "ok"}
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
              {#if usage}
                <div
                  class="fp-num text-[11px] text-[var(--fp-muted)]"
                  title={$tr("Input / cached / output / total LLM tokens")}
                  aria-label={$tr("LLM token usage: {usage}", { usage })}
                >
                  {usage}
                </div>
              {/if}
              <div class="flex items-center justify-between gap-2 text-xs">
                <span class="fp-num text-[var(--fp-muted)]"
                  >{t.ms ? t.ms : "—"}</span
                >
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
              </div>
              {#if t.phases?.length}
                <div class="flex flex-wrap gap-1">
                  {#each t.phases as ph, j (ph.name + "|" + j)}
                    <span
                      class="px-1.5 py-0.5 rounded-[var(--fp-radius-sm)] bg-[var(--fp-surface-2)] text-[10px] font-mono text-[var(--fp-muted)]"
                    >
                      {ph.name}
                      {ph.ms}ms
                    </span>
                  {/each}
                </div>
              {/if}
              {#if t.error}
                <p class="text-[var(--fp-error)] text-[11px] break-words">
                  {t.error}
                </p>
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
