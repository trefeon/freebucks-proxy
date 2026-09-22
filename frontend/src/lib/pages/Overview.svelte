<script>
  /**
   * Overview — live proxy status, KPI row, and at-risk token cards.
   * Data: the overview endpoint (pooled snapshot + token cards), polled every 15s.
   * All KPIs/cards map to real response fields only.
   */
  import { onMount, onDestroy } from "svelte";
  import PageShell from "../components/PageShell.svelte";
  import KpiGrid from "../components/KpiGrid.svelte";
  import ApiKeysEditor from "../components/ApiKeysEditor.svelte";
  import StatusBadge from "../components/StatusBadge.svelte";
  import Card from "../components/Card.svelte";
  import CopyButton from "../components/CopyButton.svelte";
  import {
    push as pushToast,
    dismiss as dismissToast,
  } from "../stores/toast.js";
  import AnnouncementsBanner from "../components/AnnouncementsBanner.svelte";
  import { fetchAPI } from "../api/client.js";
  import { adminApi } from "../api/paths.js";
  import { createQueryStore } from "../stores/query.js";
  import { tr } from "../i18n.js";
  import { recordPageVisit } from "../stores/pageState.js";
  import {
    formatTime,
    formatLocalDate,
    parseLogFields,
  } from "../utils/format.js";
  import { isExhausted, resetTimeFor } from "../utils/tokenStatus.js";
  let data = $state(null);
  let loading = $state(true);
  let error = $state("");

  // Issue #322: restart/deploy-only fields (mode, model_count, safe_mode,
  // transient_retries, upstream_sync) and account-stable card fields
  // (email, standing_*, referral_*) ride a once-per-mount full fetch; the
  // 15s hot poll hits ?view=live and merges over the cached static
  // snapshot. A full refresh every ~5min (or when the cache is empty) picks
  // up mid-session changes (mode switches, trust updates, registry syncs).
  const STATIC_TOP_KEYS = [
    "base_url",
    "mode",
    "in_bridge",
    "show_bridge",
    "models",
    "model_count",
    "safe_mode",
    "transient_retries",
    "fingerprint_rotations",
    "is_default_admin_token",
    "upstream_sync",
  ];
  const STATIC_TOKEN_KEYS = [
    "email",
    "account_id",
    "has_standing",
    "standing_level",
    "standing_label",
    "standing_score",
    "standing_next_level",
    "standing_next_level_at",
    "standing_capped_by",
    "standing_capped_reason",
    "standing_blurb",
    "standing_next_steps",
    "has_referral",
    "referral_code",
    "referral_qualified_count",
    "referral_github_linked",
    "referral_reset_at",
    "pinned_model",
  ];
  let staticPart = null;
  let staticTokensByIndex = {};

  function pick(obj, keys) {
    const out = {};
    for (const k of keys) if (k in obj) out[k] = obj[k];
    return out;
  }

  function rememberStatic(full) {
    staticPart = pick(full, STATIC_TOP_KEYS);
    staticTokensByIndex = {};
    for (const t of full.tokens ?? []) {
      staticTokensByIndex[t.index] = pick(t, STATIC_TOKEN_KEYS);
    }
  }

  function mergeLive(live) {
    // Old servers and hermetic mocks answer the live URL with the full shape:
    // refresh the static cache instead of rendering stale snapshots.
    if ("mode" in live && "model_count" in live) rememberStatic(live);
    return {
      ...staticPart,
      ...live,
      tokens: (live.tokens ?? []).map((lt) => ({
        ...(staticTokensByIndex[lt.index] ?? {}),
        ...lt,
      })),
    };
  }

  // Shared query store owns the poll loop, visibility gating, overlap guard,
  // and the full/live cadence (full on first poll, every 30 polls, every 5min,
  // on refresh). The static merge above stays page-local: full shapes refresh
  // the static cache in the data subscription below.
  const LIVE_QS = "?view=live";
  const overviewQuery = createQueryStore({
    intervalMs: 15000,
    fetchFull: () => fetchAPI(adminApi.overview),
    fetchLive: () => fetchAPI(adminApi.overview + LIVE_QS),
    merge: (_cached, live) => mergeLive(live),
  });

  let releaseQuery = null;
  let unsubData = null;
  let unsubError = null;
  onMount(() => {
    recordPageVisit("overview");
    fetchRecentErrors();
    releaseQuery = overviewQuery.ensure();
    unsubData = overviewQuery.data.subscribe((v) => {
      if (v) {
        // Old servers and hermetic mocks answer the live URL with the full
        // shape: refresh the static cache instead of rendering stale data.
        if ("mode" in v && "model_count" in v) rememberStatic(v);
        data = v;
        loading = false;
        error = "";
      }
    });
    unsubError = overviewQuery.error.subscribe((e) => {
      if (e) {
        error = e;
        loading = false;
      }
    });
    function onConfigSaved() {
      staticPart = null;
      overviewQuery.refresh();
    }
    window.addEventListener("fp-config-saved", onConfigSaved);
    return () => {
      window.removeEventListener("fp-config-saved", onConfigSaved);
      unsubData?.();
      unsubError?.();
      releaseQuery?.();
    };
  });
  // Worst-account callout: single token needing attention — banned first,
  // then cooldown active, then lowest requests/day headroom.
  function isBanned(t) {
    if (t.ban_type) return true;
    return t.session_status === "banned" || t.session_status === "quarantined";
  }
  function dayHeadroom(t) {
    const limit = t.requests_per_day_limit ?? 0;
    if (!(limit > 0)) return Number.POSITIVE_INFINITY;
    return limit - (t.requests_per_day ?? 0);
  }
  let worstAccount = $derived.by(() => {
    const tokens = data?.tokens ?? [];
    if (tokens.length === 0) return null;
    let worst = tokens[0];
    for (let i = 1; i < tokens.length; i++) {
      const a = tokens[i];
      const b = worst;
      if (!!isBanned(a) !== !!isBanned(b)) {
        if (isBanned(a)) worst = a;
        continue;
      }
      if (!!a.cooldown_active !== !!b.cooldown_active) {
        if (a.cooldown_active) worst = a;
        continue;
      }
      if (dayHeadroom(a) < dayHeadroom(b)) worst = a;
    }
    if (isBanned(worst) || worst.cooldown_active) {
      return worst;
    }
    return null;
  });
  // At-risk account callout surfaces as a toast (error when banned, warning
  // for cooldown); the inline slot keeps only the Open Tokens link. One toast
  // per account+condition — the key guard stops the 15s poll from re-pushing
  // and holds across the 10s fade, and clearing the condition dismisses it.
  let cooldownToast = $state(0);
  let cooldownKey = $state("");
  $effect(() => {
    const w = worstAccount;
    const cond = isBanned(w)
      ? "banned"
      : isExhausted(w)
        ? "exhausted"
        : "cooldown";
    const key = w ? `${w.index}-${cond}` : "";
    if (key === cooldownKey) return;
    if (cooldownToast) {
      dismissToast(cooldownToast);
      cooldownToast = 0;
    }
    cooldownKey = key;
    if (w) {
      let title = $tr("Account #{index} needs attention", { index: w.index });
      let body = w.email || w.session_status || "";
      if (isExhausted(w)) {
        const resetAt = resetTimeFor(w);
        title = $tr("Account #{index} exhausted", { index: w.index });
        body = resetAt
          ? $tr("Daily Freebucks limit reached. Resets at {time}", {
              time: formatLocalDate(resetAt),
            })
          : $tr("Daily Freebucks limit reached");
      }
      cooldownToast = pushToast({
        tone: isBanned(w) ? "error" : "warning",
        title,
        body,
      });
    }
  });
  onDestroy(() => {
    if (cooldownToast) dismissToast(cooldownToast);
  });

  // Recent-errors mini-list: one-shot logs fetch per mount (no polling).
  // Failures hide the section silently.
  const ERROR_MESSAGES = new Set([
    "request failed",
    "chat request refused",
    "messages request refused",
    "responses request refused",
    "rate limit exceeded",
  ]);
  let recentErrors = $state([]);
  function isErrorEntry(e) {
    if (!e) return false;
    if (e.level === "error") return true;
    if (ERROR_MESSAGES.has(e.message)) return true;
    if (e.message === "chat trace") {
      for (const f of parseLogFields(e.fields)) {
        if (f.key === "status" && f.value === "error") return true;
      }
    }
    return false;
  }
  function errorDetail(e) {
    const parsed = parseLogFields(e.fields);
    if (parsed.length === 0) return "";
    const pref =
      parsed.find((f) => f.key === "error" || f.key === "reason") ?? parsed[0];
    return pref.value ? `${pref.key}=${pref.value}` : pref.key;
  }
  async function fetchRecentErrors() {
    try {
      const res = await fetchAPI(adminApi.logs);
      const entries = res?.entries ?? [];
      // Entries arrive newest-first; keep the 5 most recent error-ish ones.
      recentErrors = entries.filter(isErrorEntry).slice(0, 5);
    } catch {
      recentErrors = [];
    }
  }

  function retry() {
    error = "";
    loading = true;
    overviewQuery.refresh();
  }
  let poolTotal = $derived(data?.tokens?.length ?? 0);
  let busyTokens = $derived(
    data?.tokens?.filter((t) => t.active_runs > 0).length ?? 0,
  );
  let cooldownTokens = $derived(
    data?.tokens?.filter((t) => t.cooldown_active).length ?? 0,
  );
  let bannedTokens = $derived(
    data?.tokens?.filter((t) => isBanned(t)).length ?? 0,
  );
  let requestsToday = $derived(
    data?.tokens?.reduce((s, t) => s + (t.requests || 0), 0) ?? 0,
  );

  // Dynamic Base URL follows the browser's current host (VPS IP, domain, VPN reverse proxy)
  // as computed dynamically by the backend from the request headers (Host, X-Forwarded-Host/Proto).
  let dynamicBaseURL = $derived.by(() => {
    if (data?.base_url) {
      return data.base_url;
    }
    if (typeof window !== "undefined" && window.location.host) {
      return `${window.location.protocol}//${window.location.host}/v1`;
    }
    return "http://127.0.0.1:3457/v1";
  });
</script>

<PageShell
  crumb="freebucks-proxy / Admin / overview.conf"
  title={$tr("Overview")}
  description={$tr("Live proxy status and token pool telemetry")}
  {loading}
  {error}
  onRetry={retry}
>
  {#snippet actions()}
    {#if data}
      <StatusBadge status={data.mode} tone={data.in_bridge ? "good" : "info"} />
      <span class="fp-num text-xs text-[var(--fp-dim)]">up {data.uptime}</span>
    {/if}
  {/snippet}

  <!-- Upstream announcements and broadcasts -->
  <AnnouncementsBanner />

  {#if data}
    {#if data.has_tokens}
      <!-- KPI row (pooled tokens active) -->
      <KpiGrid
        items={[
          { label: $tr("Pool total"), value: poolTotal },
          {
            label: $tr("Busy"),
            value: busyTokens,
            hint: $tr("tokens with active runs"),
            tone: busyTokens > 0 ? "good" : "default",
          },
          {
            label: $tr("Cooldown"),
            value: cooldownTokens,
            hint: cooldownTokens > 0 ? $tr("temporarily paused") : undefined,
            tone: cooldownTokens > 0 ? "warn" : "default",
          },
          {
            label: $tr("Banned"),
            value: bannedTokens,
            hint: $tr("banned accounts"),
            tone: bannedTokens > 0 ? "bad" : "default",
          },
          { label: $tr("Requests today"), value: requestsToday },
          { label: $tr("Models"), value: data.model_count ?? 0 },
        ]}
      />
      {#if worstAccount}
        {@const w = worstAccount}
        <div
          class="flex items-center justify-between gap-3 px-3 py-2 rounded-[3px] border border-[var(--fp-warning)]/30 bg-[var(--fp-warning)]/10 text-xs"
        >
          <div class="flex items-center gap-2 min-w-0">
            <span class="led led-warn shrink-0" aria-hidden="true"></span>
            <span class="text-[var(--fp-text)]">
              {#if isExhausted(w)}
                {@const resetAt = resetTimeFor(w)}
                {$tr("Account #{index} exhausted", { index: w.index })}
                {#if resetAt}
                  <span class="fp-num text-[var(--fp-warning)] font-medium">
                    · {$tr("resets at {time}", {
                      time: formatLocalDate(resetAt),
                    })}</span
                  >
                {/if}
              {:else}
                {$tr("Account #{index} needs attention", { index: w.index })}
              {/if}
            </span>
          </div>
          <a
            href="#tokens"
            class="text-[var(--fp-warning)] hover:underline shrink-0 font-medium"
          >
            {$tr("Open Tokens")} →
          </a>
        </div>
      {/if}

      <!-- Hybrid mode: pool summary above plus a compact bridge-relay card -->
      {#if data.mode === "hybrid"}
        {#if (data.bridge_tokens ?? 0) > 0 || data.bridge_token_cards?.length}
          <Card title={$tr("Bridge relay")}>
            <p class="text-sm text-[var(--fp-muted)]">
              {$tr(
                "{count} active bridge client(s) relaying their own FreeBuff tokens",
                { count: data.bridge_tokens ?? 0 },
              )}
            </p>
            {#if data.bridge_token_cards?.length}
              <ul class="mt-2 flex flex-col gap-1.5">
                {#each data.bridge_token_cards.slice(0, 4) as bc (bc.key)}
                  <li class="flex flex-wrap items-center gap-2 text-xs">
                    <StatusBadge status={bc.status} />
                    <code class="fp-num font-mono text-[var(--fp-text)]"
                      >{bc.key}</code
                    >
                    {#if bc.model}
                      <code class="fp-num font-mono text-[var(--fp-muted)]"
                        >{bc.model}</code
                      >
                    {/if}
                  </li>
                {/each}
              </ul>
            {/if}
          </Card>
        {:else}
          <div
            class="flex items-center justify-between px-3.5 py-2 rounded-[3px] border border-[var(--fp-border)] bg-[var(--fp-surface)] text-xs text-[var(--fp-muted)]"
          >
            <div class="flex items-center gap-2 min-w-0">
              <span class="led led-idle shrink-0" aria-hidden="true"></span>
              <span class="font-medium text-[var(--fp-text)]"
                >{$tr("Bridge relay")}</span
              >
              <span class="text-[var(--fp-dim)]">·</span>
              <span class="truncate"
                >{$tr("0 active bridge clients (hybrid relay ready)")}</span
              >
            </div>
            <span
              class="text-[11px] font-mono text-[var(--fp-dim)] shrink-0 hidden sm:inline"
              >{$tr("API_KEYS or token auth")}</span
            >
          </div>
        {/if}
      {/if}
    {:else}
      <!-- Bridge mode / empty pool summary -->
      <KpiGrid
        items={[
          {
            label: $tr("Relay Mode"),
            value: data.in_bridge ? "Bridge" : "Hybrid",
            hint: data.in_bridge
              ? $tr("client-supplied tokens")
              : $tr("shared pool + bridge"),
          },
          {
            label: $tr("Active Bridge Clients"),
            value: data.bridge_tokens ?? 0,
            hint: $tr("relaying upstream sessions"),
          },
          {
            label: $tr("Served Models"),
            value: data.model_count ?? 0,
            hint: $tr("OpenAI & Anthropic"),
          },
        ]}
      />

      <Card
        title={$tr("Gateway Ready — Bridge & Pooled Relay")}
        description={$tr(
          "The gateway is online and ready for traffic. Connect your tools directly in Bridge mode, or add FreeBuff accounts to create a shared token pool.",
        )}
      >
        {#snippet actions()}
          <a
            href="#tokens"
            class="fp-btn fp-btn-secondary fp-btn-sm inline-flex items-center gap-1.5"
          >
            <span>{$tr("Manage Tokens")}</span>
          </a>
        {/snippet}
        <div class="text-xs text-[var(--fp-muted)] space-y-2">
          <p>
            <strong class="text-[var(--fp-text)]"
              >{$tr("Bridge Mode (Active):")}</strong
            >
            {$tr(
              "Clients can send requests using their own FreeBuff token as the Bearer or x-api-key credential.",
            )}
          </p>
          <p>
            <strong class="text-[var(--fp-text)]"
              >{$tr("Pooled Mode (Ready):")}</strong
            >
            {$tr(
              "Add FreeBuff accounts in Tokens (via Device Login or pasting tokens) to enable shared pool rotation, admission coercion, and Client API Key routing.",
            )}
          </p>
        </div>
      </Card>

      {#if data.bridge_token_cards?.length}
        <Card title={$tr("Active Bridge Clients")}>
          <ul class="flex flex-col gap-1.5">
            {#each data.bridge_token_cards as bc (bc.key)}
              <li class="flex flex-wrap items-center gap-2 text-xs">
                <StatusBadge status={bc.status} />
                <code class="fp-num font-mono text-[var(--fp-text)]"
                  >{bc.key}</code
                >
                {#if bc.model}
                  <code class="fp-num font-mono text-[var(--fp-muted)]"
                    >{bc.model}</code
                  >
                {/if}
              </li>
            {/each}
          </ul>
        </Card>
      {/if}
    {/if}

    <!-- Universal Client Integration & Endpoints Card (Always Available) -->
    <section aria-label="Client integration">
      <div
        class="flex flex-col items-start gap-1 sm:flex-row sm:items-center sm:justify-between mb-3"
      >
        <h2 class="text-lg font-semibold text-[var(--fp-text)]">
          {$tr("Client Integration")}
        </h2>
        <span class="text-xs font-mono text-[var(--fp-muted)]"
          >OpenAI & Anthropic Compatible</span
        >
      </div>

      <div class="grid grid-cols-1 lg:grid-cols-2 gap-4">
        <!-- Gateway Base URL -->
        <Card
          title={$tr("Gateway Base URL")}
          description={$tr(
            "Universal base endpoint for any OpenAI or Anthropic client, SDK, or CLI tool.",
          )}
        >
          <div class="flex items-center gap-2">
            <div class="fp-inset flex-1 px-3 py-2 overflow-x-auto">
              <code
                class="fp-num text-xs text-[var(--fp-text)] font-mono font-medium"
                >{dynamicBaseURL}</code
              >
            </div>
            <CopyButton text={dynamicBaseURL} label={$tr("Copy URL")} />
          </div>
          <p class="mt-3 text-xs text-[var(--fp-muted)]">
            {$tr(
              "Authentication: Use any Client API Key below via Bearer token or x-api-key header.",
            )}
          </p>
        </Card>

        <!-- Supported Protocols & Routes -->
        <Card
          title={$tr("Supported Wire Protocols")}
          description={$tr(
            "Dual-protocol translation handled transparently by the gateway.",
          )}
        >
          <div class="space-y-2">
            <div
              class="fp-inset px-3 py-2 flex items-center justify-between gap-2 text-xs"
            >
              <div class="flex items-center gap-2 min-w-0">
                <span
                  class="px-1.5 py-0.5 rounded bg-[var(--fp-surface)] border border-[var(--fp-border)] font-mono text-[10px] text-[var(--fp-accent)]"
                  >OpenAI</span
                >
                <span class="font-mono text-[var(--fp-text)] min-w-0 break-all"
                  >POST /v1/chat/completions</span
                >
              </div>
              <span class="text-[var(--fp-dim)] text-[11px] shrink-0"
                >Cursor, Aider, OMP</span
              >
            </div>
            <div
              class="fp-inset px-3 py-2 flex items-center justify-between gap-2 text-xs"
            >
              <div class="flex items-center gap-2 min-w-0">
                <span
                  class="px-1.5 py-0.5 rounded bg-[var(--fp-surface)] border border-[var(--fp-border)] font-mono text-[10px] text-[#A78BFA]"
                  >Anthropic</span
                >
                <span class="font-mono text-[var(--fp-text)] min-w-0 break-all"
                  >POST /v1/messages</span
                >
              </div>
              <span class="text-[var(--fp-dim)] text-[11px] shrink-0"
                >Claude Code, Cline</span
              >
            </div>
          </div>
        </Card>
      </div>

      <div class="mt-4">
        <ApiKeysEditor />
      </div>
    </section>
    {#if recentErrors.length > 0}
      <Card title={$tr("Recent errors")}>
        {#snippet footer()}
          <a
            href="#activity"
            class="text-xs text-[var(--fp-accent)] hover:underline"
          >
            {$tr("Open Activity")}
          </a>
        {/snippet}
        <ul class="flex flex-col gap-2">
          {#each recentErrors as e, i (i)}
            <li
              class="flex flex-col gap-0.5 border-b border-[var(--fp-border)] pb-2 last:border-0 last:pb-0 sm:flex-row sm:flex-wrap sm:items-baseline sm:gap-x-3"
            >
              <span class="fp-num shrink-0 text-[11px] text-[var(--fp-dim)]"
                >{formatTime(e.time)}</span
              >
              <span class="min-w-0 break-words text-xs text-[var(--fp-text)]"
                >{e.message}</span
              >
              {#if errorDetail(e)}
                <span
                  class="min-w-0 flex-1 truncate font-mono text-[11px] text-[var(--fp-muted)]"
                  title={errorDetail(e)}>{errorDetail(e)}</span
                >
              {/if}
            </li>
          {/each}
        </ul>
      </Card>
    {/if}
  {/if}
</PageShell>
