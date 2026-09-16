<script>
  import SettingsCard from "../../components/SettingsCard.svelte";
  import SettingsRow from "../../components/SettingsRow.svelte";
  import DbOverrideSave from "../../components/DbOverrideSave.svelte";
  import StatusBadge from "../../components/StatusBadge.svelte";
  import ToggleSwitch from "../../components/ToggleSwitch.svelte";
  import NumberStepper from "../../components/NumberStepper.svelte";
  import DurationPicker from "../../components/DurationPicker.svelte";
  import { Gauge, Scale } from "@lucide/svelte";
  import { tr } from "../../i18n.js";
  import { parseEnv } from "../../utils/env.js";
  import {
    STRATEGY_DRAIN,
    STRATEGY_BALANCE,
    BALANCE_THRESHOLD_MIN_SECS,
    BALANCE_THRESHOLD_MAX_SECS,
    detectStrategy,
    thresholdSecs,
  } from "../../utils/poolStrategy.js";

  /**
   * Pool Strategy preset card (top of Pool → Controls) — the single owner of
   * the five strategy keys.
   *
   * A preset tap writes ONLY the keys whose value actually differs from the
   * preset (applyPreset below) through onField; each key's row below then
   * instant-saves that one value to the DB overlay (DbOverrideSave), so a
   * tap posts one request per changed key — never a fixed batch. The card
   * renders those rows itself, so every owned key has exactly one editor on
   * the Controls tab (QUEUE_WAIT/QUEUE_DEPTH used to live in Pool Controls
   * and the rotation/failover/smart block in Custom advanced).
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
    "How long one acquire parks on a full token's queue before failing over. Persisted as QUEUE_WAIT — the same row as the Queue Wait editor below.";
  const KEYS_TITLE = "Strategy keys";

  // --- Rotation policy (the sole editor for the modes the presets never
  // write: both presets only ever set drain) ---
  const ROT_POLICY_LABEL = "Token Rotation Policy";
  const ROT_DRAIN_BTN = "Drain (Safest)";
  const ROT_RR_BTN = "Round Robin (1:1)";
  const ROT_LU_BTN = "Least Used (Max Quota)";
  const ROT_RANDOM_BTN = "Random (Stochastic)";
  const ROT_DRAIN_TITLE = "Drain Mode (Default & Recommended):";
  const ROT_DRAIN_BODY =
    "Sticks to one account until it is unfit (cooldown, quota, or ban) before rotating to the next token. Mimics authentic single-user behavior and provides the strongest anti-ban protection.";
  const ROT_RR_TITLE = "Round-Robin Mode:";
  const ROT_RR_BODY =
    "Rotates to the next token on every request (1:1). Note: rapid alternating requests across healthy accounts may raise upstream anomaly-detection signals.";
  const ROT_LU_TITLE = "Least-Used Mode:";
  const ROT_LU_BODY =
    "Routes requests to the token with the lowest daily usage or active run count. Maximizes concurrency and distributes quota consumption evenly.";
  const ROT_RANDOM_TITLE = "Random Mode:";
  const ROT_RANDOM_BODY =
    "Selects an available healthy token at random per request. Provides stochastic load balancing.";
  const FAILOVER_LABEL = "Auto Failover on Rate Limit (429)";
  const FAILOVER_DESC =
    "When enabled, an in-flight request encountering a 429 rate limit or account throttle immediately leases another healthy pool token and retries seamlessly without failing the request.";
  const FAILOVER_OFF_WARN =
    "Failover off = no safety net (failover mati = tanpa jaring): an in-flight 429 fails the request instead of retrying on another healthy account. Turn off only here, deliberately.";
  const SMART_LABEL = "Smart Routing";
  const SMART_DESC =
    "Route each request through per-token live-turn slots with a FIFO waiter queue plus the unified scorer. Off restores the legacy acquire path.";
  const QWAIT_LABEL = "Queue Wait";
  const QWAIT_DESC =
    "How long one acquire parks on a full token's FIFO live-turn queue before failing over (Go duration, e.g. 5s, 30s). Empty or non-positive falls back to 30s.";
  const QDEPTH_LABEL = "Queue Depth";
  const QDEPTH_DESC =
    "Cap on parked FIFO waiters per token. A full queue fails over at once with the existing 429 shape.";
  const QDEPTH_HINT = "0 = no queueing (fail over at once)";

  const ROT_MODES = ["drain", "round_robin", "least_used", "random"];

  let env = $derived(parseEnv(rawText));
  let tokenRotation = $derived.by(() => {
    const raw = String(
      formValues.TOKEN_ROTATION ?? env.TOKEN_ROTATION ?? "drain",
    ).toLowerCase();
    return ROT_MODES.includes(raw) ? raw : "drain";
  });
  let rateLimitFailover = $derived(
    String(
      formValues.RATE_LIMIT_FAILOVER ?? env.RATE_LIMIT_FAILOVER ?? "true",
    ).toLowerCase() !== "false",
  );
  let routingSmart = $derived(
    String(formValues.ROUTING_SMART ?? "true").toLowerCase() !== "false",
  );
  let queueWait = $derived(formValues.QUEUE_WAIT ?? "30s");
  let queueDepth = $derived(formValues.QUEUE_DEPTH ?? "16");
  let maxConcurrent = $derived(formValues.TOKEN_MAX_CONCURRENT ?? "2");

  function setTokenRotation(mode) {
    if (tokenRotation === mode) return;
    onField("TOKEN_ROTATION", mode);
  }
  function toggleRateLimitFailover(next) {
    const v = typeof next === "boolean" ? next : !rateLimitFailover;
    onField("RATE_LIMIT_FAILOVER", v ? "true" : "false");
  }
  function toggleRoutingSmart(next) {
    const v = typeof next === "boolean" ? next : !routingSmart;
    onField("ROUTING_SMART", v ? "true" : "false");
  }

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

  // Queue rows dim (never disable) while smart routing is off or the
  // per-token cap is unlimited — nothing can park then, so the rows would
  // do nothing. The switch is the one below; the cap lives in Pool Controls.
  let queueParked = $derived(!routingSmart || Number(maxConcurrent) === 0);

  // Honest pool ceiling: the per-account cap the gateway runs with × the
  // pooled accounts from the tokens snapshot. 0 = no slot gating at all.
  let ceiling = $derived.by(() => {
    const cap = Number(String(maxConcurrent).trim());
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
      "ROUTING_SMART",
      "TOKEN_ROTATION",
      "RATE_LIMIT_FAILOVER",
      "QUEUE_WAIT",
      "QUEUE_DEPTH",
      "Pool Strategy",
      DRAIN_LABEL,
      DRAIN_DESC,
      BALANCE_LABEL,
      BALANCE_DESC,
      CUSTOM_LABEL,
      CUSTOM_DESC,
      THRESHOLD_LABEL,
      THRESHOLD_DESC,
      KEYS_TITLE,
      ROT_POLICY_LABEL,
      ROT_DRAIN_BTN,
      ROT_RR_BTN,
      ROT_LU_BTN,
      ROT_RANDOM_BTN,
      ROT_DRAIN_TITLE,
      ROT_DRAIN_BODY,
      ROT_RR_TITLE,
      ROT_RR_BODY,
      ROT_LU_TITLE,
      ROT_LU_BODY,
      ROT_RANDOM_TITLE,
      ROT_RANDOM_BODY,
      FAILOVER_LABEL,
      FAILOVER_DESC,
      FAILOVER_OFF_WARN,
      SMART_LABEL,
      SMART_DESC,
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
        <div class="space-y-0.5">
          <span class="text-xs font-semibold text-[var(--fp-text)]">
            {$tr(ROT_POLICY_LABEL)}
          </span>
          <p class="text-[11px] text-[var(--fp-muted)] leading-relaxed">
            {$tr(
              "Which account each request picks. The presets above only ever select Drain; the other modes are hand-picked here and read as Custom until reset.",
            )}
          </p>
        </div>
        <div
          class="flex flex-wrap items-center gap-2"
          role="radiogroup"
          aria-label={$tr(ROT_POLICY_LABEL)}
        >
          <button
            type="button"
            role="radio"
            aria-checked={tokenRotation === "drain"}
            onclick={() => setTokenRotation("drain")}
            class="fp-btn {tokenRotation === 'drain'
              ? 'fp-btn-primary'
              : 'fp-btn-ghost'} fp-btn-sm text-xs"
          >
            {$tr(ROT_DRAIN_BTN)}
          </button>
          <button
            type="button"
            role="radio"
            aria-checked={tokenRotation === "round_robin"}
            onclick={() => setTokenRotation("round_robin")}
            class="fp-btn {tokenRotation === 'round_robin'
              ? 'fp-btn-primary'
              : 'fp-btn-ghost'} fp-btn-sm text-xs"
          >
            {$tr(ROT_RR_BTN)}
          </button>
          <button
            type="button"
            role="radio"
            aria-checked={tokenRotation === "least_used"}
            onclick={() => setTokenRotation("least_used")}
            class="fp-btn {tokenRotation === 'least_used'
              ? 'fp-btn-primary'
              : 'fp-btn-ghost'} fp-btn-sm text-xs"
          >
            {$tr(ROT_LU_BTN)}
          </button>
          <button
            type="button"
            role="radio"
            aria-checked={tokenRotation === "random"}
            onclick={() => setTokenRotation("random")}
            class="fp-btn {tokenRotation === 'random'
              ? 'fp-btn-primary'
              : 'fp-btn-ghost'} fp-btn-sm text-xs"
          >
            {$tr(ROT_RANDOM_BTN)}
          </button>
        </div>
        <div class="flex justify-end">
          <DbOverrideSave
            settingKey="TOKEN_ROTATION"
            value={tokenRotation}
            source={sources.TOKEN_ROTATION}
            {onReset}
            {onSaved}
            {degraded}
          />
        </div>

        <div
          class="fp-inset p-3 rounded text-xs text-[var(--fp-muted)] flex items-start gap-2"
        >
          {#if tokenRotation === "drain"}
            <p class="leading-relaxed">
              <strong class="text-[var(--fp-text)]"
                >{$tr(ROT_DRAIN_TITLE)}</strong
              >
              {$tr(ROT_DRAIN_BODY)}
            </p>
          {:else if tokenRotation === "round_robin"}
            <p class="leading-relaxed">
              <strong class="text-[var(--fp-text)]">{$tr(ROT_RR_TITLE)}</strong>
              {$tr(ROT_RR_BODY)}
            </p>
          {:else if tokenRotation === "least_used"}
            <p class="leading-relaxed">
              <strong class="text-[var(--fp-text)]">{$tr(ROT_LU_TITLE)}</strong>
              {$tr(ROT_LU_BODY)}
            </p>
          {:else if tokenRotation === "random"}
            <p class="leading-relaxed">
              <strong class="text-[var(--fp-text)]"
                >{$tr(ROT_RANDOM_TITLE)}</strong
              >
              {$tr(ROT_RANDOM_BODY)}
            </p>
          {/if}
        </div>

        <div
          class="pt-3 border-t border-[var(--fp-border)] flex flex-col sm:flex-row sm:items-center justify-between gap-3"
        >
          <div class="space-y-0.5">
            <div class="flex items-center gap-2">
              <span class="text-xs font-semibold text-[var(--fp-text)]">
                {$tr(FAILOVER_LABEL)}
              </span>
              <span class="led {rateLimitFailover ? 'led-good' : 'led-dim'}"
              ></span>
            </div>
            <p class="text-[11px] text-[var(--fp-muted)] leading-relaxed">
              {$tr(FAILOVER_DESC)}
            </p>
            {#if !rateLimitFailover}
              <p
                class="text-[11px] text-[var(--fp-warning)] leading-relaxed font-medium"
              >
                {$tr(FAILOVER_OFF_WARN)}
              </p>
            {/if}
          </div>
          <div class="flex flex-col items-end gap-1.5 shrink-0">
            <ToggleSwitch
              checked={rateLimitFailover}
              ariaLabel="Auto Failover on Rate Limit (429)"
              onchange={(v) => toggleRateLimitFailover(v)}
            />
            <DbOverrideSave
              settingKey="RATE_LIMIT_FAILOVER"
              value={formValues.RATE_LIMIT_FAILOVER ?? "true"}
              source={sources.RATE_LIMIT_FAILOVER}
              {onReset}
              {onSaved}
              {degraded}
            />
          </div>
        </div>

        <div
          class="pt-3 border-t border-[var(--fp-border)] flex flex-col sm:flex-row sm:items-center justify-between gap-3"
        >
          <div class="space-y-0.5">
            <div class="flex items-center gap-2">
              <span class="text-xs font-semibold text-[var(--fp-text)]">
                {$tr(SMART_LABEL)}
              </span>
              <code
                class="text-[10px] px-1.5 py-0.5 rounded bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-mono"
                >ROUTING_SMART</code
              >
              {#if !env.ROUTING_SMART}
                <span
                  class="text-[10px] px-1.5 py-0.5 rounded-[var(--fp-radius-sm)] border border-[var(--fp-border)] bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-semibold uppercase tracking-wider shrink-0"
                  >{$tr("default")}</span
                >
              {/if}
            </div>
            <p class="text-[11px] text-[var(--fp-muted)] leading-relaxed">
              {$tr(SMART_DESC)}
            </p>
          </div>
          <div class="flex flex-col items-end gap-1.5 shrink-0">
            <ToggleSwitch
              checked={routingSmart}
              ariaLabel="ROUTING_SMART"
              onchange={(v) => toggleRoutingSmart(v)}
            />
            <DbOverrideSave
              settingKey="ROUTING_SMART"
              value={formValues.ROUTING_SMART ?? "true"}
              source={sources.ROUTING_SMART}
              {onReset}
              {onSaved}
              {degraded}
            />
          </div>
        </div>
      </div>

      {#if queueParked}
        <p class="text-[11px] text-[var(--fp-dim)] leading-relaxed pb-1">
          {$tr(
            "Parked: smart routing is off or the per-token cap is unlimited (0) — nothing parks until both change (ROUTING_SMART above, TOKEN_MAX_CONCURRENT in Pool Controls).",
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
