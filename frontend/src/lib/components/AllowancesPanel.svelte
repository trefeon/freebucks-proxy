<script>
  import { onMount } from "svelte";
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
  import { tr } from "../i18n.js";
  import {
    formatFreebucks,
    formatAllowanceUsd,
    freebucksResetCountdown,
  } from "../utils/freebucks.js";

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

  // Global reset strip: the first account carrying a daily reset time sets
  // the shared Pacific-midnight countdown for every account on the page.
  const resetSource = $derived(
    (data?.tokens ?? []).find((t) => t.freebucks?.daily?.reset_at),
  );
  const resetAt = $derived(resetSource?.freebucks?.daily?.reset_at ?? "");
  const resetCountdown = $derived(
    resetAt ? freebucksResetCountdown(resetAt, now) : "",
  );

  // Daily window math mirrors FreebucksQuotaBar: spent defaults to
  // limit − remaining when the server only sends the remainder.
  function dailyWin(token) {
    const d = token.freebucks?.daily;
    if (d == null) return null;
    const limit = Number(d.limit ?? 0);
    const remaining = Number(
      d.remaining ?? Math.max(0, limit - Number(d.spent ?? 0)),
    );
    const spent = Number(d.spent ?? Math.max(0, limit - remaining));
    let pct = Number(d.percent_used ?? (limit > 0 ? (spent / limit) * 100 : 0));
    if (!Number.isFinite(pct)) pct = 0;
    pct = Math.min(100, Math.max(0, pct));
    return { limit, remaining, spent, pct };
  }

  function monthlyWin(token) {
    const m = token.freebucks?.monthly;
    if (m == null) return null;
    const limit = Number(m.limit ?? 0);
    const remaining = Number(
      m.remaining ?? Math.max(0, limit - Number(m.spent ?? 0)),
    );
    const spent = Number(m.spent ?? Math.max(0, limit - remaining));
    return { limit, remaining, spent };
  }

  // One-line account summary: access tier (server-driven per account —
  // full vs limited pools differ in cap and price), daily fraction,
  // wallet, monthly remainder. The "resets in" countdown is intentionally
  // absent here — it renders once in the global strip above, shared for
  // all accounts.
  function accountHeaderLine(token) {
    const fb = token.freebucks;
    if (!fb?.daily) return "";
    const parts = [];
    if (token.access_tier) parts.push(String(token.access_tier).toUpperCase());
    parts.push(
      `${formatFreebucks(fb.daily.remaining)}/${formatFreebucks(fb.daily.limit)} ${$tr("Freebucks daily")}`,
    );
    const walletBalance = fb.wallet?.balance ?? 0;
    if (walletBalance > 0) {
      parts.push(`${formatFreebucks(walletBalance)} ${$tr("in wallet")}`);
    }
    if (fb.monthly != null && fb.monthly.remaining != null) {
      parts.push(
        `${formatAllowanceUsd(fb.monthly.remaining)} ${$tr("monthly usage left")}`,
      );
    }
    return parts.join(" · ");
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
{:else if data}
  {#if resetAt}
    <p
      class="text-xs text-[var(--fp-muted)] font-mono"
      data-testid="reset-strip"
    >
      {$tr("Daily pools reset at")}
      {resetAt} · {$tr("resets in")}
      {resetCountdown} · {$tr("shared for all accounts")}
    </p>
  {/if}
  <ul
    class="grid grid-cols-1 lg:grid-cols-2 gap-2.5"
    aria-label={$tr("Accounts")}
  >
    {#each data.tokens as token, ti (token.index ?? ti)}
      {@const idx = token.index ?? ti}
      {@const daily = dailyWin(token)}
      {@const monthly = monthlyWin(token)}
      {@const balance = token.freebucks?.balance ?? token.freebucks?.Balance}
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
          {#if quotaExempt(token)}
            <span
              class="text-[10px] font-mono px-1.5 py-0.5 rounded border text-emerald-400 bg-emerald-500/10 border-emerald-500/20"
              title={$tr(
                "Server-authorized: new sessions stay usable at zero balance",
              )}>{$tr("quota exempt")}</span
            >
          {/if}
        </div>
        {#if token.freebucks}
          <p
            class="text-xs text-[var(--fp-muted)] font-mono"
            data-testid="freebucks-header"
          >
            {accountHeaderLine(token)}
          </p>
          {#if balance != null}
            <p class="fp-num text-xs text-[var(--fp-text)] tabular-nums">
              {$tr("Balance")}
              <span class="text-[var(--fp-accent)]"
                >{formatFreebucks(balance)}</span
              >
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
          {#if daily}
            <div class="flex flex-col gap-1">
              <div
                class="h-[5px] w-full rounded-full bg-[var(--fp-inset)] overflow-hidden"
                role="progressbar"
                aria-valuenow={Math.round(daily.pct)}
                aria-valuemin="0"
                aria-valuemax="100"
                aria-label={$tr("Daily usage {pct}%", {
                  pct: Math.round(daily.pct),
                })}
              >
                <div
                  class="h-full rounded-full transition-all duration-300 bg-emerald-500"
                  style="width: {daily.pct}%"
                ></div>
              </div>
              <p class="fp-num text-[11px] text-[var(--fp-dim)] tabular-nums">
                {$tr("Daily")}
                {$tr("Used")}
                <span class="text-[var(--fp-text)] font-medium"
                  >{formatFreebucks(daily.spent)}</span
                >
                / {formatFreebucks(daily.limit)}
                • {$tr("Remaining")}
                <span class="text-[var(--fp-text)] font-medium"
                  >{formatFreebucks(daily.remaining)}</span
                >
              </p>
            </div>
          {/if}
          {#if monthly}
            <p class="fp-num text-[11px] text-[var(--fp-dim)] tabular-nums">
              {$tr("Monthly")}
              {$tr("Used")}
              <span class="text-[var(--fp-text)] font-medium"
                >{formatFreebucks(monthly.spent)}</span
              >
              / {formatFreebucks(monthly.limit)}
            </p>
          {/if}
        {:else}
          <p class="text-xs text-[var(--fp-dim)] italic">
            {$tr("No Freebucks data — run a request or Probe all to populate.")}
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
