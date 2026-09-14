<script>
  import SettingsCard from "../../components/SettingsCard.svelte";
  import StatusBadge from "../../components/StatusBadge.svelte";
  import { Scale } from "@lucide/svelte";
  import { tr } from "../../i18n.js";
  import {
    STRATEGY_DRAIN,
    STRATEGY_BALANCE,
    BALANCE_THRESHOLD_MIN_SECS,
    BALANCE_THRESHOLD_MAX_SECS,
    detectStrategy,
    thresholdSecs,
  } from "../../utils/poolStrategy.js";

  /**
   * Pool Strategy preset card (top of Pool → Controls).
   * Drain / Balance preset buttons write ONLY the five owned keys through
   * the shared batched flow (onField → page Save); Custom is an
   * auto-detected badge, never selectable, with one-click reset back to
   * either preset. The Balance threshold slider (5–300s, default 60s)
   * renders only in Balance and persists straight to QUEUE_WAIT.
   *
   * @prop {Record<string, string>} formValues
   * @prop {(key: string, value: string) => void} onField
   * @prop {string} [query] - settings key-search text; hides the card on mismatch
   * @prop {(n: number) => void} [onMatchCount] - reports the visible-row count to the parent
   */
  let { formValues, onField, query = "", onMatchCount = null } = $props();

  const DRAIN_LABEL = "Drain";
  const DRAIN_DESC =
    "Deep queues: each account serves up to 5 minutes / 1024 parked waiters before the pool fails over. Safest for a few accounts.";
  const BALANCE_LABEL = "Balance";
  const BALANCE_DESC =
    "Shallow queues: 16 parked waiters, then fail over after the threshold below. Best for busy pools.";
  const CUSTOM_LABEL = "Custom";
  const CUSTOM_DESC =
    "Hand-edited: at least one of the five strategy keys left its preset value. Reset to a preset below to return to one tap.";
  const THRESHOLD_LABEL = "Balance threshold";
  const THRESHOLD_DESC =
    "How long one acquire parks on a full token's queue before failing over. Persisted as QUEUE_WAIT.";

  let strategy = $derived(
    detectStrategy({
      ROUTING_SMART: formValues.ROUTING_SMART,
      TOKEN_ROTATION: formValues.TOKEN_ROTATION,
      RATE_LIMIT_FAILOVER: formValues.RATE_LIMIT_FAILOVER,
      QUEUE_WAIT: formValues.QUEUE_WAIT,
      QUEUE_DEPTH: formValues.QUEUE_DEPTH,
    }),
  );
  let sliderSecs = $derived(thresholdSecs(formValues.QUEUE_WAIT));

  function applyPreset(preset) {
    // A preset switch writes ONLY its five owned keys — every other knob
    // keeps its draft value. When the draft already matches, skip so the
    // form never dirties without a change.
    const same = Object.entries(preset).every(
      ([k, v]) => String(formValues[k] ?? "") === v,
    );
    if (same) return;
    for (const [k, v] of Object.entries(preset)) onField(k, v);
  }

  let q = $derived(query.trim().toLowerCase());
  function hit(...parts) {
    if (!q) return true;
    return parts.join("\n").toLowerCase().includes(q);
  }
  let visible = $derived(
    hit(
      "ROUTING_SMART",
      "TOKEN_ROTATION",
      "RATE_LIMIT_FAILOVER",
      "QUEUE_WAIT",
      "QUEUE_DEPTH",
      DRAIN_LABEL,
      DRAIN_DESC,
      BALANCE_LABEL,
      BALANCE_DESC,
      CUSTOM_LABEL,
      CUSTOM_DESC,
      THRESHOLD_LABEL,
      THRESHOLD_DESC,
      "Pool Strategy",
    )
      ? 1
      : 0,
  );
  $effect(() => {
    onMatchCount?.(visible);
  });
</script>

{#if !q || visible > 0}
  <SettingsCard
    title={$tr("Pool Strategy")}
    description={$tr(
      "One tap picks the pool's queue posture. Anything hand-edited reads as Custom with a one-click reset.",
    )}
  >
    {#snippet icon()}
      <Scale size={20} />
    {/snippet}
    {#snippet actions()}
      {#if strategy === "custom"}
        <StatusBadge tone="warn" status={$tr("Custom")} />
      {:else if strategy === "drain"}
        <StatusBadge tone="good" status={$tr("Drain")} />
      {:else}
        <StatusBadge tone="info" status={$tr("Balance")} />
      {/if}
      {#if q}
        <span
          role="status"
          class="text-[11px] font-mono text-[var(--fp-dim)] shrink-0"
          >{$tr("{visible} of {total}", { visible, total: 1 })}</span
        >
      {/if}
    {/snippet}

    <div
      class="flex flex-wrap items-center gap-2 py-4"
      role="radiogroup"
      aria-label={$tr("Pool strategy")}
    >
      <button
        type="button"
        role="radio"
        aria-checked={strategy === "drain"}
        onclick={() => applyPreset(STRATEGY_DRAIN)}
        class="fp-btn {strategy === 'drain'
          ? 'fp-btn-primary'
          : 'fp-btn-ghost'} fp-btn-sm text-xs"
      >
        {$tr(DRAIN_LABEL)}
      </button>
      <button
        type="button"
        role="radio"
        aria-checked={strategy === "balance"}
        onclick={() => applyPreset(STRATEGY_BALANCE)}
        class="fp-btn {strategy === 'balance'
          ? 'fp-btn-primary'
          : 'fp-btn-ghost'} fp-btn-sm text-xs"
      >
        {$tr(BALANCE_LABEL)}
      </button>
      {#if strategy === "custom"}
        <span class="text-[11px] text-[var(--fp-dim)]">{$tr(CUSTOM_LABEL)}</span
        >
      {/if}
    </div>

    <div
      class="fp-inset p-3 rounded text-xs text-[var(--fp-muted)] flex items-start gap-2"
    >
      {#if strategy === "drain"}
        <p class="leading-relaxed">
          <strong class="text-[var(--fp-text)]">{$tr("Drain:")}</strong>
          {$tr(DRAIN_DESC)}
        </p>
      {:else if strategy === "balance"}
        <p class="leading-relaxed">
          <strong class="text-[var(--fp-text)]">{$tr("Balance:")}</strong>
          {$tr(BALANCE_DESC)}
        </p>
      {:else}
        <p class="leading-relaxed">
          <strong class="text-[var(--fp-text)]">{$tr("Custom:")}</strong>
          {$tr(CUSTOM_DESC)}
        </p>
      {/if}
    </div>

    {#if strategy === "balance"}
      <div class="pt-3 mt-1 border-t border-[var(--fp-border)]">
        <div
          class="flex flex-col sm:flex-row sm:items-center justify-between gap-3 py-2"
        >
          <div class="space-y-0.5">
            <div class="flex items-center gap-2">
              <span class="text-xs font-semibold text-[var(--fp-text)]">
                {$tr(THRESHOLD_LABEL)}
              </span>
              <code
                class="fp-num text-[10px] px-1.5 py-0.5 rounded bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-mono"
                >QUEUE_WAIT</code
              >
            </div>
            <p class="text-[11px] text-[var(--fp-muted)] leading-relaxed">
              {$tr(THRESHOLD_DESC)}
            </p>
          </div>
          <div class="flex items-center gap-2.5 w-full sm:w-64">
            <input
              type="range"
              min={BALANCE_THRESHOLD_MIN_SECS}
              max={BALANCE_THRESHOLD_MAX_SECS}
              step={5}
              value={sliderSecs}
              aria-label={$tr("Balance threshold (QUEUE_WAIT)")}
              class="flex-1 accent-[var(--fp-accent)]"
              oninput={(e) =>
                onField("QUEUE_WAIT", `${e.currentTarget.value}s`)}
            />
            <span
              class="fp-num text-xs text-[var(--fp-text)] w-12 text-right tabular-nums"
              >{sliderSecs}s</span
            >
          </div>
        </div>
      </div>
    {/if}

    {#if strategy === "custom"}
      <div class="flex flex-wrap items-center gap-2 pt-3">
        <button
          type="button"
          onclick={() => applyPreset(STRATEGY_DRAIN)}
          class="fp-btn fp-btn-secondary fp-btn-sm text-xs"
        >
          {$tr("Reset to Drain")}
        </button>
        <button
          type="button"
          onclick={() => applyPreset(STRATEGY_BALANCE)}
          class="fp-btn fp-btn-secondary fp-btn-sm text-xs"
        >
          {$tr("Reset to Balance")}
        </button>
      </div>
    {/if}
  </SettingsCard>
{/if}
