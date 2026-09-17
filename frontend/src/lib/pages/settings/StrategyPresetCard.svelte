<script>
  import SettingsCard from "../../components/SettingsCard.svelte";
  import SettingsRow from "../../components/SettingsRow.svelte";
  import DbOverrideSave from "../../components/DbOverrideSave.svelte";
  import StatusBadge from "../../components/StatusBadge.svelte";
  import NumberStepper from "../../components/NumberStepper.svelte";
  import DurationPicker from "../../components/DurationPicker.svelte";
  import { Gauge, Scale } from "@lucide/svelte";
  import { tr } from "../../i18n.js";
  import { parseEnv } from "../../utils/env.js";
  import {
    STRATEGY_MASQ,
    STRATEGY_DRAIN,
    STRATEGY_BALANCE,
    BALANCE_THRESHOLD_MIN_SECS,
    BALANCE_THRESHOLD_MAX_SECS,
    detectStrategy,
    thresholdSecs,
  } from "../../utils/poolStrategy.js";

  /**
   * Pool Strategy preset card (top of Pool → Controls) — the single owner of
   * the queue-posture keys (SLOTS_PER_ACCOUNT, QUEUE_WAIT, QUEUE_DEPTH,
   * MAX_SPILL_ACCOUNTS). PIN_MODEL is owned but never preset-written: pins
   * are per-account routing edited in the token drawer.
   *
   * A preset tap writes ONLY the keys whose value actually differs from the
   * preset (applyPreset below) through onField; each key's row below then
   * instant-saves that one value to the DB overlay (DbOverrideSave), so a
   * tap posts one request per changed key — never a fixed batch.
   *
   * Custom is an auto-detected badge, never selectable, with one-click reset
   * back to either preset. The Balance threshold slider (5–300s) renders only
   * in Balance; it is a second input onto this card's QUEUE_WAIT row (the
   * DurationPicker is the primary editor) and writes the same field, so no
   * key ever gets two writers.
   *
   * @prop {Record<string, string>} formValues
   * @prop {(key: string, value: string) => void} onField
   * @prop {Record<string, string>} [sources] - ADR-0019 source tiers
   * @prop {(key: string) => Promise<void>} [onReset] - saved-value reset
   * @prop {(() => Promise<void>) | null} [onSaved] - parent refetch after a
   *   per-key save
   * @prop {boolean} [degraded=false] - settings store offline: per-key
   *   overlay saves render an honest offline note
   * @prop {string} [rawText] - .env document, for the rows' "default" chips
   * @prop {number} [tokenCount=0] - pooled accounts (pool-ceiling line)
   * @prop {string} [query] - settings key-search text; hides the card on mismatch
   * @prop {(n: number) => void} [onMatchCount] - reports the visible-row count to the parent
   */
  let {
    formValues,
    onField,
    sources = {},
    onReset = null,
    onSaved = null,
    degraded = false,
    rawText = "",
    tokenCount = 0,
    query = "",
    onMatchCount = null,
  } = $props();

  const MASQ_LABEL = "MASQ";
  const MASQ_DESC =
    "Ordered Sticky Slot-Packing: 2 slots per account with 60s deferred scale-out and sticky session retention. Maximizes account session reuse.";
  const DRAIN_LABEL = "Drain";
  const DRAIN_DESC =
    "Deep queues: each account serves up to 5 minutes / 1024 parked waiters before the request spills to the next account. Safest for a few accounts.";
  const BALANCE_LABEL = "Balance";
  const BALANCE_DESC =
    "Shallow queues: 16 parked waiters, then spill to the next account after the threshold below. Best for busy pools.";
  const CUSTOM_LABEL = "Custom";
  const CUSTOM_DESC =
    "Hand-edited: at least one of the strategy keys left its preset value. Reset to a preset below to return to one tap.";
  const THRESHOLD_LABEL = "Balance threshold";
  const THRESHOLD_DESC =
    "How long one acquire parks on a full account lane's queue before spilling to the next account (Balance default 60s; unset installs run 30s). Persisted as QUEUE_WAIT — the same row as the Queue Wait editor below.";
  const KEYS_TITLE = "Strategy keys";

  const SLOTS_LABEL = "Slots per Account";
  const SLOTS_DESC =
    "Cap on concurrent live turns per account-model lane (default 2, the approved anti-ban pacing). Excess waiters park FIFO until Queue Wait elapses.";
  const SLOTS_HINT = "0 = unlimited (no slot gating at all)";
  const SPILL_LABEL = "Max Spill Accounts";
  const SPILL_DESC =
    "How many continuation accounts one request may spill to after its head lane's Queue Wait elapses. A 429 quota requeue never consumes spill budget.";
  const SPILL_HINT = "0 = unbounded (the full index chain)";
  const QWAIT_LABEL = "Queue Wait";
  const QWAIT_DESC =
    "How long one acquire parks on a full lane's FIFO queue before spilling to the next account (Go duration, e.g. 5s, 30s). Empty or non-positive falls back to 30s.";
  const QDEPTH_LABEL = "Queue Depth";
  const QDEPTH_DESC =
    "Cap on parked FIFO waiters per account-model lane. A full queue spills at once with the existing 429 shape.";
  const QDEPTH_HINT = "0 = no queueing (spill at once)";

  let env = $derived(parseEnv(rawText));
  let slotsPerAccount = $derived(formValues.SLOTS_PER_ACCOUNT ?? "2");
  let maxSpillAccounts = $derived(formValues.MAX_SPILL_ACCOUNTS ?? "0");
  let queueWait = $derived(formValues.QUEUE_WAIT ?? "30s");
  let queueDepth = $derived(formValues.QUEUE_DEPTH ?? "16");

  let strategy = $derived(
    detectStrategy({
      SLOTS_PER_ACCOUNT: formValues.SLOTS_PER_ACCOUNT,
      QUEUE_WAIT: formValues.QUEUE_WAIT,
      QUEUE_DEPTH: formValues.QUEUE_DEPTH,
      PIN_MODEL: formValues.PIN_MODEL,
      MAX_SPILL_ACCOUNTS: formValues.MAX_SPILL_ACCOUNTS,
    }),
  );
  let sliderSecs = $derived(thresholdSecs(formValues.QUEUE_WAIT));

  // Queue rows dim (never disable) while the per-account cap is unlimited
  // (0 = no slot gating) — nothing can park then, so the rows would do
  // nothing. The cap is the SLOTS_PER_ACCOUNT row below.
  let queueParked = $derived(Number(slotsPerAccount) === 0);

  // Honest pool ceiling: the per-account cap the gateway runs with × the
  // pooled accounts from the tokens snapshot. 0 = no slot gating at all.
  let ceiling = $derived.by(() => {
    const cap = Number(String(slotsPerAccount).trim());
    const accounts = Math.max(0, Number(tokenCount) || 0);
    if (!Number.isFinite(cap) || cap <= 0) {
      return $tr("unlimited per account ({accounts} accounts)", { accounts });
    }
    return $tr(
      "{cap} per account × {accounts} accounts = {total} concurrent turns",
      { cap, accounts, total: cap * accounts },
    );
  });

  function applyPreset(preset) {
    // A preset switch writes ONLY its five owned keys — every other knob
    // keeps its value. When the values already match, skip so no write
    // fires without a change.
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
      "SLOTS_PER_ACCOUNT",
      "QUEUE_WAIT",
      "QUEUE_DEPTH",
      "MAX_SPILL_ACCOUNTS",
      "Pool Strategy",
      MASQ_LABEL,
      MASQ_DESC,
      DRAIN_LABEL,
      DRAIN_DESC,
      BALANCE_LABEL,
      BALANCE_DESC,
      CUSTOM_LABEL,
      CUSTOM_DESC,
      THRESHOLD_LABEL,
      THRESHOLD_DESC,
      KEYS_TITLE,
      SLOTS_LABEL,
      SLOTS_DESC,
      SLOTS_HINT,
      SPILL_LABEL,
      SPILL_DESC,
      SPILL_HINT,
      QWAIT_LABEL,
      QWAIT_DESC,
      QDEPTH_LABEL,
      QDEPTH_DESC,
      QDEPTH_HINT,
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
      {:else if strategy === "masq"}
        <StatusBadge tone="good" status={$tr("MASQ")} />
      {:else if strategy === "drain"}
        <StatusBadge tone="info" status={$tr("Drain")} />
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
        aria-checked={strategy === "masq"}
        onclick={() => applyPreset(STRATEGY_MASQ)}
        class="fp-btn {strategy === 'masq'
          ? 'fp-btn-primary'
          : 'fp-btn-ghost'} fp-btn-sm text-xs"
      >
        {$tr(MASQ_LABEL)}
      </button>
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
      {#if strategy === "masq"}
        <p class="leading-relaxed">
          <strong class="text-[var(--fp-text)]">{$tr("MASQ:")}</strong>
          {$tr(MASQ_DESC)}
        </p>
      {:else if strategy === "drain"}
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

    <div
      class="flex items-center gap-2 pt-3 px-1 text-[11px] text-[var(--fp-muted)]"
      data-testid="pool-ceiling"
    >
      <Gauge size={13} class="shrink-0 text-[var(--fp-dim)]" />
      <span class="uppercase tracking-wider text-[var(--fp-dim)]"
        >{$tr("Pool ceiling")}</span
      >
      <span class="fp-num text-[var(--fp-text)]">{ceiling}</span>
    </div>

    {#if strategy === "balance"}
      <div class="pt-3 mt-1 border-t border-[var(--fp-border)]">
        <div
          class="flex flex-col sm:flex-row sm:items-center justify-between gap-3 py-2"
        >
          <div class="space-y-0.5">
            <span class="text-xs font-semibold text-[var(--fp-text)]">
              {$tr(THRESHOLD_LABEL)}
            </span>
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
          onclick={() => applyPreset(STRATEGY_MASQ)}
          class="fp-btn fp-btn-secondary fp-btn-sm text-xs"
        >
          {$tr("Reset to MASQ")}
        </button>
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

    <!-- The five owned keys, each with its single editor + instant save. -->
    <div
      class="pt-4 mt-1 border-t border-[var(--fp-border)]"
      data-testid="strategy-rows"
    >
      <p
        class="text-xs font-semibold uppercase tracking-wider text-[var(--fp-muted)] pb-1"
      >
        {$tr(KEYS_TITLE)}
      </p>

      <div class="space-y-3 py-4">
        <SettingsRow label={$tr(SLOTS_LABEL)} description={$tr(SLOTS_DESC)}>
          {#snippet badge()}
            <code
              class="text-[10px] px-1.5 py-0.5 rounded bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-mono"
              >SLOTS_PER_ACCOUNT</code
            >
            {#if !env.SLOTS_PER_ACCOUNT}
              <span
                class="text-[10px] px-1.5 py-0.5 rounded-[var(--fp-radius-sm)] border border-[var(--fp-border)] bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-semibold uppercase tracking-wider shrink-0"
                >{$tr("default")}</span
              >
            {/if}
          {/snippet}
          {#snippet extra()}
            <DbOverrideSave
              settingKey="SLOTS_PER_ACCOUNT"
              value={slotsPerAccount}
              source={sources.SLOTS_PER_ACCOUNT}
              {onReset}
              {onSaved}
              {degraded}
            />
          {/snippet}

          <div class="w-full sm:w-56">
            <NumberStepper
              value={slotsPerAccount}
              min={0}
              step={1}
              ariaLabel="SLOTS_PER_ACCOUNT"
              placeholder="2"
              oninput={(v) => {
                const val = v.trim();
                onField("SLOTS_PER_ACCOUNT", val === "" ? "2" : val);
              }}
            />
            <p class="text-[10px] text-[var(--fp-dim)] mt-1">
              {$tr(SLOTS_HINT)}
            </p>
          </div>
        </SettingsRow>

        <SettingsRow label={$tr(SPILL_LABEL)} description={$tr(SPILL_DESC)}>
          {#snippet badge()}
            <code
              class="text-[10px] px-1.5 py-0.5 rounded bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-mono"
              >MAX_SPILL_ACCOUNTS</code
            >
            {#if !env.MAX_SPILL_ACCOUNTS}
              <span
                class="text-[10px] px-1.5 py-0.5 rounded-[var(--fp-radius-sm)] border border-[var(--fp-border)] bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-semibold uppercase tracking-wider shrink-0"
                >{$tr("default")}</span
              >
            {/if}
          {/snippet}
          {#snippet extra()}
            <DbOverrideSave
              settingKey="MAX_SPILL_ACCOUNTS"
              value={maxSpillAccounts}
              source={sources.MAX_SPILL_ACCOUNTS}
              {onReset}
              {onSaved}
              {degraded}
            />
          {/snippet}

          <div class="w-full sm:w-56">
            <NumberStepper
              value={maxSpillAccounts}
              min={0}
              step={1}
              ariaLabel="MAX_SPILL_ACCOUNTS"
              placeholder="0"
              oninput={(v) => {
                const val = v.trim();
                onField("MAX_SPILL_ACCOUNTS", val === "" ? "0" : val);
              }}
            />
            <p class="text-[10px] text-[var(--fp-dim)] mt-1">
              {$tr(SPILL_HINT)}
            </p>
          </div>
        </SettingsRow>
      </div>

      {#if queueParked}
        <p class="text-[11px] text-[var(--fp-dim)] leading-relaxed pb-1">
          {$tr(
            "Parked: the per-account cap is unlimited (0) — nothing parks until it changes (SLOTS_PER_ACCOUNT above).",
          )}
        </p>
      {/if}

      <SettingsRow
        class={queueParked ? "opacity-60" : ""}
        label={$tr(QWAIT_LABEL)}
        description={$tr(QWAIT_DESC)}
      >
        {#snippet badge()}
          <code
            class="text-[10px] px-1.5 py-0.5 rounded bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-mono"
            >QUEUE_WAIT</code
          >
          {#if !env.QUEUE_WAIT}
            <span
              class="text-[10px] px-1.5 py-0.5 rounded-[var(--fp-radius-sm)] border border-[var(--fp-border)] bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-semibold uppercase tracking-wider shrink-0"
              >{$tr("default")}</span
            >
          {/if}
        {/snippet}
        {#snippet extra()}
          <DbOverrideSave
            settingKey="QUEUE_WAIT"
            value={queueWait}
            source={sources.QUEUE_WAIT}
            {onReset}
            {onSaved}
            {degraded}
          />
        {/snippet}

        <div class="w-full sm:w-56">
          <DurationPicker
            value={queueWait}
            presets={["5s", "15s", "30s", "1m", "5m"]}
            ariaLabel="QUEUE_WAIT"
            placeholder="30s"
            oninput={(v) => onField("QUEUE_WAIT", v)}
          />
        </div>
      </SettingsRow>

      <SettingsRow
        class={queueParked ? "opacity-60" : ""}
        label={$tr(QDEPTH_LABEL)}
        description={$tr(QDEPTH_DESC)}
      >
        {#snippet badge()}
          <code
            class="text-[10px] px-1.5 py-0.5 rounded bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-mono"
            >QUEUE_DEPTH</code
          >
          {#if !env.QUEUE_DEPTH}
            <span
              class="text-[10px] px-1.5 py-0.5 rounded-[var(--fp-radius-sm)] border border-[var(--fp-border)] bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-semibold uppercase tracking-wider shrink-0"
              >{$tr("default")}</span
            >
          {/if}
        {/snippet}
        {#snippet extra()}
          <DbOverrideSave
            settingKey="QUEUE_DEPTH"
            value={queueDepth}
            source={sources.QUEUE_DEPTH}
            {onReset}
            {onSaved}
            {degraded}
          />
        {/snippet}

        <div class="w-full sm:w-56">
          <NumberStepper
            value={queueDepth}
            min={0}
            step={1}
            ariaLabel="QUEUE_DEPTH"
            placeholder="16"
            oninput={(v) => {
              const val = v.trim();
              onField("QUEUE_DEPTH", val === "" ? "16" : val);
            }}
          />
          <p class="text-[10px] text-[var(--fp-dim)] mt-1">
            {$tr(QDEPTH_HINT)}
          </p>
        </div>
      </SettingsRow>
    </div>
  </SettingsCard>
{/if}
