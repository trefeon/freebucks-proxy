<script>
  import { onMount } from "svelte";
  import { RefreshCw } from "@lucide/svelte";
  import Card from "./Card.svelte";
  import {
    push as pushToast,
    dismiss as dismissToast,
  } from "../stores/toast.js";
  import Button from "./Button.svelte";
  import { fetchAPI } from "../api/client.js";
  import { adminApi } from "../api/paths.js";
  import { tr } from "../i18n.js";

  let { cursor = 0 } = $props();

  // Per-client API key usage (#604): fed by
  // GET /admin/api/usage?group_by=key → { keys: [...] }.
  // Same poll cadence as MetricsPanel (mount + shared Refresh-all cursor);
  // no new SSE event type. Key labels are short hashes only — raw API keys
  // are never displayed or logged. Missing payload shapes render as the
  // empty state, never as fabricated data.
  let payload = $state(null);
  let loading = $state(true);
  let error = $state("");
  let errorToast = $state(0);
  function notifyError(msg) {
    if (errorToast) dismissToast(errorToast);
    errorToast = msg ? pushToast({ tone: "error", title: msg }) : 0;
  }

  async function fetchTeam() {
    try {
      const res = await fetchAPI(
        `${adminApi.usage}?group_by=${encodeURIComponent("key")}`,
      );
      payload = res ?? null;
      error = "";
      notifyError("");
    } catch (e) {
      error = e.message || $tr("Failed to load team usage");
      notifyError(error);
    } finally {
      loading = false;
    }
  }

  onMount(fetchTeam);

  // Shared time cursor from the Activity page ("Refresh all"): refetch when
  // it advances. fetchTeam reads no reactive state, so cursor is the only
  // dependency.
  $effect(() => {
    if (cursor >= 0) fetchTeam();
  });

  function formatRate(rate) {
    const n = Number(rate);
    if (!Number.isFinite(n)) return "—";
    const pct = n <= 1 ? n * 100 : n;
    return `${pct.toFixed(1)}%`;
  }

  function formatFreebucks(cost) {
    const n = Number(cost ?? 0);
    if (!Number.isFinite(n) || n === 0) return "0 Freebucks";
    const s = Number.isInteger(n) ? String(n) : n.toFixed(1);
    return `${s} Freebucks`;
  }

  function formatTime(v) {
    if (v == null || v === "") return "—";
    const n = typeof v === "number" ? v : Number(v);
    const ms =
      Number.isFinite(n) && v !== "" && typeof v !== "string"
        ? n < 1e12
          ? n * 1000
          : n
        : Date.parse(v);
    if (!Number.isFinite(ms)) return "—";
    return new Date(ms).toLocaleString();
  }

  function modelRows(byModel) {
    if (!byModel || typeof byModel !== "object") return [];
    return Object.entries(byModel);
  }
</script>

<div class="space-y-6">
  {#if loading && !payload}
    <div class="space-y-6" aria-busy="true">
      <div class="skeleton skeleton-card h-64"></div>
      <span class="sr-only">{$tr("Loading team usage")}</span>
    </div>
  {:else if error && !payload}
    <div class="space-y-4">
      <Button variant="secondary" onclick={fetchTeam}>
        <RefreshCw size={15} />
        {$tr("Retry")}
      </Button>
    </div>
  {:else}
    {@const keys = Array.isArray(payload?.keys) ? payload.keys : []}
    <Card
      title={$tr("Team usage")}
      description={$tr(
        "Per-client API key usage — key labels are short hashes, never raw keys.",
      )}
      pad="none"
    >
      {#if keys.length}
        <div class="overflow-x-auto">
          <table class="fp-table">
            <caption class="sr-only"
              >{$tr(
                "Per-client API key usage — requests, tokens, window and Freebucks",
              )}</caption
            >
            <thead>
              <tr>
                <th scope="col">{$tr("Person / key")}</th>
                <th scope="col" class="num">{$tr("Requests")}</th>
                <th scope="col" class="num">{$tr("Total tokens")}</th>
                <th scope="col">{$tr("Window")}</th>
                <th scope="col" class="num">{$tr("Freebucks")}</th>
              </tr>
            </thead>
            <tbody>
              {#each keys as k (k.key_id)}
                <tr>
                  <td class="font-mono text-xs whitespace-nowrap"
                    ><span class="block truncate max-w-40" title={k.key_id}
                      >{k.key_id ?? "—"}</span
                    ></td
                  >
                  <td class="num whitespace-nowrap">
                    {Number(k.requests ?? 0).toLocaleString()}
                    <span class="text-[11px] text-[var(--fp-muted)]"
                      >· {formatRate(k.success_rate)}</span
                    >
                  </td>
                  <td class="num">
                    <span class="whitespace-nowrap"
                      >{Number(k.total_tokens ?? 0).toLocaleString()}</span
                    >
                    {#if modelRows(k.by_model).length}
                      <span class="mt-1 block space-y-0.5">
                        {#each modelRows(k.by_model) as [model, m] (model)}
                          <span
                            class="block truncate max-w-56 font-mono text-[11px] font-normal text-[var(--fp-muted)]"
                            title={`${model}: ${Number(m?.requests ?? 0).toLocaleString()} req · ${Number(m?.tokens ?? 0).toLocaleString()} tok (in ${Number(m?.prompt ?? 0).toLocaleString()} / out ${Number(m?.completion ?? 0).toLocaleString()} / think ${Number(m?.reasoning ?? 0).toLocaleString()})`}
                            >{model} · {Number(
                              m?.tokens ?? 0,
                            ).toLocaleString()}</span
                          >
                        {/each}
                      </span>
                    {/if}
                  </td>
                  <td class="font-mono text-xs whitespace-nowrap">
                    {formatTime(k.first_seen)} → {formatTime(k.last_seen)}
                  </td>
                  <td class="num whitespace-nowrap"
                    >{formatFreebucks(k.freebucks)}</td
                  >
                </tr>
              {/each}
            </tbody>
          </table>
        </div>
      {:else}
        <div class="px-5 py-6">
          <p class="text-sm text-[var(--fp-muted)]">
            {$tr("No per-client usage yet.")}
          </p>
        </div>
      {/if}
    </Card>
  {/if}
</div>
