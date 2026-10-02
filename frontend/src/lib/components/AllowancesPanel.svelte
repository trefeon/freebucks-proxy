<script>
  import { onMount } from "svelte";
  import { SvelteSet } from "svelte/reactivity";
  import { Flame } from "@lucide/svelte";
  import { recordPageVisit } from "../stores/pageState.js";
  import Button from "./Button.svelte";
  import {
    push as pushToast,
    dismiss as dismissToast,
  } from "../stores/toast.js";
  import EmptyState from "./EmptyState.svelte";
  import RefundLines from "./RefundLines.svelte";
  import {
    tokensData,
    tokensError,
    ensureTokensStore,
    refreshTokens,
  } from "../stores/tokens.js";
  import { postAPI } from "../api/client.js";
  import { adminActions, tokenActions } from "../api/paths.js";
  import { tr } from "../i18n.js";
  import {
    formatFreebucks,
    formatAllowanceUsd,
    freebucksResetCountdown,
    freebucksDisplayModel,
    isAbsoluteResetTimestamp,
    offPeakCopy,
  } from "../utils/freebucks.js";
  import { localeRegionCode, viewerTimeZone } from "../utils/format.js";
  import { streakBadgeFor } from "../utils/tokenStatus.js";

  let data = $state(null);
  let loading = $state(true);
  let error = $state("");
  let errorToast = $state(0);
  let lastErrorMsg = "";
  function notifyError(msg) {
    // The shared tokens poll re-fails with the same message: only replace
    // the toast when it actually changes, so it never flickers and a
    // manual dismiss is respected until the next distinct failure.
    if (msg === lastErrorMsg) return;
    lastErrorMsg = msg;
    if (errorToast) dismissToast(errorToast);
    errorToast = msg
      ? pushToast({
          tone: "error",
          title: $tr("Could not load this page"),
          body: msg,
        })
      : 0;
  }
  // Countdown tick: the global reset strip re-renders "resets in" against
  // this clock every second. Refetches nothing on its own.
  let now = $state(Date.now());
  // Probe-all (POST /admin/tokens/test-all, zero-cost: the pool probes every
  // token with a session-less GET and claims no slot). Progress + disabled
  // state on the button, one summary toast, then a list refetch so the
  // account cards render the fresh probe data.
  let probing = $state(false);

  // Short per-status labels for the summary toast, mirroring the backend
  // pool.ProbeTokenOutcome status set (backend/internal/pool/probe.go).
  const PROBE_STATUS_LABELS = {
    ok: "ok",
    banned: "banned",
    rate_limited: "limited",
    freebucks_exhausted: "exhausted",
    auth_rejected: "rejected",
    country_blocked: "blocked",
    error: "error",
  };

  function probeSummary(outcomes) {
    const counts = {};
    for (const o of outcomes) {
      const s = String(o?.status ?? "error");
      counts[s] = (counts[s] ?? 0) + 1;
    }
    const parts = [`${counts.ok ?? 0} ok`];
    delete counts.ok;
    for (const s of Object.keys(counts))
      parts.push(`${counts[s]} ${PROBE_STATUS_LABELS[s] ?? s}`);
    return $tr("Probed {total}: {parts}", {
      total: outcomes.length,
      parts: parts.join(", "),
    });
  }

  async function probeAll() {
    if (probing) return;
    probing = true;
    try {
      const res = await postAPI(adminActions.testAll, {});
      if (Array.isArray(res)) {
        const allOk = res.every((o) => o?.status === "ok");
        pushToast({
          tone: allOk ? "success" : "warning",
          title: probeSummary(res),
        });
      } else if (res && res.ok === false) {
        pushToast({
          tone: "error",
          title: res.message || $tr("Probe all failed"),
        });
      } else {
        pushToast({ tone: "error", title: $tr("Probe all failed") });
      }
      refreshTokens();
    } catch (e) {
      pushToast({
        tone: "error",
        title: e?.message || $tr("Network error probing tokens"),
      });
    } finally {
      probing = false;
    }
  }
  // Per-account probe (POST /admin/tokens/{id}/test, zero-cost like
  // probe-all): the affordance behind the never-probed copy, so one dark
  // account need not wait for a fleet-wide probe.
  async function probeOne(idx) {
    if (probing) return;
    probing = true;
    try {
      const res = await postAPI(tokenActions.test(idx), {});
      const ok = res?.ok !== false;
      pushToast({
        tone: ok ? "success" : "error",
        title:
          res?.message ||
          (ok
            ? $tr("Account #{idx} probed", { idx: idx + 1 })
            : $tr("Account #{idx} probe failed", { idx: idx + 1 })),
      });
      refreshTokens();
    } catch (e) {
      pushToast({
        tone: "error",
        title: e?.message || $tr("Network error probing token"),
      });
    } finally {
      probing = false;
    }
  }

  // The first available absolute reset anchors the compact countdown strip;
  const resetSource = $derived(
    (data?.tokens ?? []).find((t) =>
      isAbsoluteResetTimestamp(
        t.freebucks?.daily?.reset_at_utc ?? t.freebucks?.daily?.resetAtUtc,
      ),
    ),
  );
  const resetSourceNumber = $derived(
    resetSource
      ? (resetSource.index ?? (data?.tokens ?? []).indexOf(resetSource)) + 1
      : 0,
  );
  const resetAt = $derived(
    resetSource?.freebucks?.daily?.reset_at_utc ??
      resetSource?.freebucks?.daily?.resetAtUtc ??
      "",
  );
  const viewerZone = viewerTimeZone();
  const viewerLocaleRegion = localeRegionCode(
    typeof navigator === "undefined" ? "" : navigator.language,
  );
  const resetCountdown = $derived(
    resetAt ? freebucksResetCountdown(resetAt, now) : "",
  );

  function monthlyWin(token) {
    const m = token.freebucks?.monthly;
    if (m == null) return null;
    const limit = Number(m.limit ?? 0);
    const remaining = Number(
      m.remaining ?? Math.max(0, limit - Number(m.spent ?? 0)),
    );
    const spent = Number(m.spent ?? Math.max(0, limit - remaining));
    // The wire's own remainder decides whether the $ figure is stated at
    // all; the arithmetic fallback above is a meter input, not a balance.
    return { limit, remaining, spent, hasRemaining: m.remaining != null };
  }

  // First-tab offer line (vendor 6cd8970 firstTabDiscountCopy, condensed
  // for the account card): the server already folds an available offer
  // into prices, so this is display state only.
  function discountLine(d) {
    if (d == null) return "";
    if (d.available) {
      return `${$tr("First-tab discount")}: ${$tr("up to {amount} off one session", { amount: `${formatFreebucks(d.amount)} Freebucks` })} · ${$tr("prices shown include it")}`;
    }
    return `${$tr("First-tab discount in use")} · ${$tr("parallel sessions pay the regular price")}`;
  }

  function quotaExempt(token) {
    return Boolean(
      token.freebucks?.quota_exempt ?? token.freebucks?.quotaExempt,
    );
  }
  // Off-peak per-model lines for one account card: the active/upcoming
  // detail for every priced model carrying a server off-peak offer.
  function offPeakLines(token) {
    const fb = token.freebucks;
    if (!fb) return [];
    const offers = fb.off_peak ?? fb.offPeak ?? {};
    const out = [];
    for (const id of Object.keys(offers)) {
      if (fb.prices?.[id] === undefined) continue;
      const detail = offPeakCopy(fb, id, { now })?.detail;
      if (detail) out.push(detail);
    }
    return out;
  }
  // Fleet-wide off-peak lines for the header: ordered-dedupe of the
  // per-account offPeakLines detail strings across every token.
  function fleetOffPeakLines() {
    const seen = new SvelteSet();
    const out = [];
    for (const token of data?.tokens ?? []) {
      for (const line of offPeakLines(token)) {
        if (!seen.has(line)) {
          seen.add(line);
          out.push(line);
        }
      }
    }
    return out;
  }

  let unsubStore = null;
  let unsubErr = null;
  let tick = null;
  onMount(() => {
    recordPageVisit("quota");
    const release = ensureTokensStore();
    unsubStore = tokensData.subscribe((v) => {
      if (v) {
        data = v;
        loading = false;
        error = "";
        notifyError("");
      }
    });
    unsubErr = tokensError.subscribe((err) => {
      if (err) {
        error = err;
        loading = false;
        notifyError(err);
      }
    });
    tick = setInterval(() => {
      now = Date.now();
    }, 1000);
    return () => {
      release();
      unsubStore?.();
      unsubErr?.();
      clearInterval(tick);
      if (errorToast) dismissToast(errorToast);
    };
  });
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
      <Button variant="secondary" onclick={refreshTokens}>{$tr("Retry")}</Button
      >
    </div>
  </div>
{:else if !data?.has_tokens || !data?.tokens?.length}
  <EmptyState
    title={$tr("No tokens in pool")}
    description={$tr(
      "Add a token to the pool to see Freebucks allowances and model pricing.",
    )}
  />
{:else}
  <div class="mb-2 flex flex-wrap items-center justify-end gap-2">
    <Button
      variant="secondary"
      size="sm"
      disabled={probing}
      loading={probing}
      onclick={probeAll}
      title={$tr("Zero-cost probe of every account: no session claimed")}
    >
      {probing ? $tr("Probing…") : $tr("Probe all")}
    </Button>
  </div>
  {#if resetAt}
    {#if Date.parse(resetAt) <= now}
      <div class="flex flex-wrap items-center gap-2" data-testid="reset-strip">
        <p class="text-xs text-[var(--fp-muted)] font-mono">
          {$tr("Updating balance…")}
        </p>
        <Button
          variant="secondary"
          size="sm"
          disabled={probing}
          loading={probing}
          onclick={probeAll}
          title={$tr("Zero-cost probe of every account: no session claimed")}
        >
          {probing ? $tr("Probing…") : $tr("Probe all")}
        </Button>
      </div>
    {:else}
      <p
        class="text-xs text-[var(--fp-muted)] font-mono"
        data-testid="reset-strip"
      >
        {$tr("Account #{account} resets in {countdown}", {
          account: resetSourceNumber,
          countdown: resetCountdown,
        })}
      </p>
    {/if}
  {/if}
  <p
    class="text-[11px] text-[var(--fp-dim)] font-mono"
    data-testid="tz-probe"
    title={$tr(
      "Browser locale and timezone only control this display; upstream country is separate",
    )}
  >
    {$tr("Browser locale region: {region} · browser timezone: {zone}", {
      region: viewerLocaleRegion || $tr("not reported"),
      zone: viewerZone || $tr("not reported"),
    })}
  </p>
  <p class="text-[11px] text-[var(--fp-dim)] font-mono">
    {$tr(
      "Browser locale and timezone only control this local display; upstream-reported country is separate.",
    )}
  </p>
  {#each fleetOffPeakLines() as line, i (i)}
    <p
      class="fp-num text-[11px] text-[var(--fp-muted)] tabular-nums"
      data-testid="off-peak-line"
    >
      {line}
    </p>
  {/each}
  <ul
    class="grid grid-cols-1 lg:grid-cols-2 gap-2.5"
    aria-label={$tr("Accounts")}
  >
    {#each data.tokens as token, ti ((token.account_id || token.email || token.index) ?? ti)}
      {@const idx = token.index ?? ti}
      {@const fb = freebucksDisplayModel(token, now)}
      {@const monthly = monthlyWin(token)}
      {@const streak = streakBadgeFor(token)}
      <li
        class="rounded border border-[var(--fp-border)] bg-[var(--fp-surface-2)]/30 px-3 py-2.5 flex flex-col gap-1.5 min-w-0"
        data-testid="account-row"
      >
        <div class="flex flex-wrap items-baseline gap-x-2 gap-y-0.5 min-w-0">
          <h3 class="text-sm font-semibold text-[var(--fp-text)]">
            {$tr("Account #{index}", { index: idx + 1 })}
          </h3>
          {#if token.email}
            <span class="text-xs text-[var(--fp-dim)] font-mono truncate"
              >{token.email}</span
            >
          {/if}
          {#if token.access_tier}
            <span
              class="text-[10px] font-mono px-1.5 py-0.5 rounded border border-[var(--fp-border)] bg-[var(--fp-surface)] text-[var(--fp-dim)]"
              >{String(token.access_tier).toUpperCase()}</span
            >
          {/if}
          {#if quotaExempt(token)}
            <span
              class="text-[10px] font-mono px-1.5 py-0.5 rounded border text-emerald-400 bg-emerald-500/10 border-emerald-500/20"
              title={$tr(
                "Server-authorized: new sessions stay usable at zero balance",
              )}>{$tr("quota exempt")}</span
            >
          {/if}
          <!-- Streak day count — the same Lucide-chipped label the account
               cards draw (never an emoji); it is what funds the perk note on
               the wallet line below. -->
          <span
            class="fp-num inline-flex items-center gap-1 text-[11px] tabular-nums whitespace-nowrap shrink-0 {streak.active
              ? 'text-[var(--fp-accent)]'
              : 'text-[var(--fp-dim)]'}"
            aria-label={streak.aria}
          >
            <Flame size={11} aria-hidden="true" />
            {streak.label}
          </span>
        </div>
        <p
          class="fp-num text-[11px] text-[var(--fp-dim)] tabular-nums break-words"
          data-testid="account-country-line"
        >
          {$tr("Last upstream-reported country: {country}", {
            country: token.country_code || $tr("not reported"),
          })}
        </p>
        {#if fb?.resetLine}
          <p
            class="fp-num text-[11px] text-[var(--fp-dim)] tabular-nums break-words"
            data-testid="account-reset-line"
          >
            {#if fb.resetLine.resetZone}
              {$tr("{clock} ({zone})", {
                clock: fb.resetLine.clock || $tr("unavailable"),
                zone: fb.resetLine.resetZone,
              })}
              {#if fb.resetLine.browserClock}
                ·
                {$tr("browser time ({zone}): {clock}", {
                  zone: viewerZone || $tr("not reported"),
                  clock: fb.resetLine.browserClock,
                })}
              {/if}
            {:else if fb.resetLine.browserClock}
              {$tr("Browser-local reset time: {clock}", {
                clock: fb.resetLine.browserClock,
              })}
            {:else}
              {$tr("Reset time (source timezone not reported): {clock}", {
                clock: fb.resetLine.clock || $tr("unavailable"),
              })}
            {/if}
          </p>
        {/if}
        {#if fb}
          {#if fb.spendable != null}
            <!-- Spendable right now (vendor: daily.remaining + wallet.balance).
                 The decomposition states that identity inline and is dropped
                 when the served figures do not add up, because the difference
                 would be a bucket the wire never names. -->
            <p
              class="fp-num text-xs text-[var(--fp-text)] tabular-nums"
              data-testid="freebucks-header"
            >
              <span class="text-[var(--fp-accent)] font-semibold"
                >{formatFreebucks(fb.spendable)}</span
              >
              {$tr("Freebucks spendable")}
              {#if fb.decomposition}<span class="text-[var(--fp-muted)]">
                  · = {fb.decomposition}</span
                >{/if}
            </p>
          {/if}
          {#if fb.dailyLimit != null || fb.dailyLeft != null}
            <!-- The daily pool and reset clock are shown per account below. -->
            <div class="flex flex-col gap-1">
              <div
                class="h-[5px] w-full rounded-full bg-[var(--fp-inset)] overflow-hidden"
                role="progressbar"
                aria-valuenow={Math.round(fb.dailyPct)}
                aria-valuemin="0"
                aria-valuemax="100"
                aria-label={$tr("Daily usage {pct}%", {
                  pct: Math.round(fb.dailyPct),
                })}
              >
                <div
                  class="h-full rounded-full transition-all duration-300 bg-emerald-500"
                  style="width: {fb.dailyPct}%"
                ></div>
              </div>
              <p class="fp-num text-[11px] text-[var(--fp-dim)] tabular-nums">
                {$tr("Daily")}
                {$tr("Used")}
                <span class="text-[var(--fp-text)] font-medium"
                  >{formatFreebucks(fb.dailySpent)}</span
                >
                / {formatFreebucks(fb.dailyLimit)}
                ·
                <span class="text-[var(--fp-text)] font-medium"
                  >{formatFreebucks(fb.dailyLeft)}</span
                >
                {$tr("left")}
              </p>
            </div>
          {/if}
          {#if fb.wallet != null || fb.perkNote}
            <!-- The wallet line, with the streak perk attached to it: the
                 server credits the bonus HERE every Pacific day (vendor
                 freebuff-streak.ts), which is why the daily limit above stays
                 put while the wallet grows. -->
            <p
              class="fp-num text-[11px] text-[var(--fp-muted)] tabular-nums"
              data-testid="streak-perk"
            >
              {$tr("Wallet")}
              {#if fb.wallet != null}<span
                  class="text-[var(--fp-text)] font-medium"
                  >{formatFreebucks(fb.wallet)}</span
                >{/if}
              {#if fb.perkNote}<span class="text-[var(--fp-accent)]"
                  >— {fb.perkNote}</span
                >{/if}
            </p>
          {/if}
          {#if token.freebucks?.first_tab_discount}
            <p
              class="fp-num text-[11px] text-[var(--fp-muted)] tabular-nums"
              data-testid="first-tab-discount"
            >
              {discountLine(token.freebucks.first_tab_discount)}
            </p>
          {/if}
          {#if monthly}
            <p class="fp-num text-[11px] text-[var(--fp-dim)] tabular-nums">
              {$tr("Monthly")}
              {$tr("Used")}
              <span class="text-[var(--fp-text)] font-medium"
                >{formatFreebucks(monthly.spent)}</span
              >
              / {formatFreebucks(monthly.limit)}
              {#if monthly.hasRemaining}· {formatAllowanceUsd(
                  monthly.remaining,
                )}
                {$tr("monthly usage left")}{/if}
            </p>
          {/if}
        {:else if token.quota_probed === false}
          <div class="flex flex-wrap items-center gap-2">
            <p class="text-xs text-[var(--fp-dim)] italic">
              {$tr("No Freebucks data yet — probe this account to populate.")}
            </p>
            <Button
              variant="secondary"
              size="sm"
              disabled={probing}
              onclick={() => probeOne(idx)}
              title={$tr("Zero-cost probe of this account: no session claimed")}
            >
              {$tr("Probe")}
            </Button>
          </div>
        {:else}
          <p class="text-xs text-[var(--fp-dim)] italic">
            {$tr(
              token.quota_probed
                ? "No Freebucks data — the last probe returned none."
                : "No Freebucks data — run a request or Probe all to populate.",
            )}
          </p>
        {/if}
        <RefundLines
          {token}
          pendingClass="text-xs text-[var(--fp-warning)]"
          settledClass="text-xs text-[var(--fp-success)]"
        />
      </li>
    {/each}
  </ul>
{/if}
