<script>
  import SettingsCard from "../../components/SettingsCard.svelte";
  import SettingsRow from "../../components/SettingsRow.svelte";
  import DbBadge from "../../components/DbOverrideBadge.svelte";
  import DbOverrideSave from "../../components/DbOverrideSave.svelte";
  import ToggleSwitch from "../../components/ToggleSwitch.svelte";
  import NumberStepper from "../../components/NumberStepper.svelte";
  import DurationPicker from "../../components/DurationPicker.svelte";
  import { Activity } from "@lucide/svelte";
  import { tr } from "../../i18n.js";
  import { parseEnv } from "../../utils/env.js";

  /**
   * Traffic & Rate Limiting settings card (Pool group).
   * Built using the SettingsCard and SettingsRow template components.
   * Leads with a bespoke Rotation section (relocated from the
   * Tokens page): the TOKEN_ROTATION radiogroup + RATE_LIMIT_FAILOVER
   * toggle persist through the whole-file flow (onField, batched
   * into the page Save). Followed by the curated Smart routing group
   * (ROUTING_SMART, TOKEN_MAX_CONCURRENT, QUEUE_WAIT, QUEUE_DEPTH)
   * and the Client IP Rate Limit row.
   * All keys apply live on reload (none is restart-only).
   *
   * @prop {Record<string, string>} formValues
   * @prop {string} rawText
   * @prop {(key: string, value: string) => void} onField
   * @prop {Record<string, string>} [sources] - ADR-0019 source tiers
   * @prop {(key: string) => Promise<void>} [onReset] - saved-value reset
   * @prop {(() => Promise<void>) | null} [onSaved] - parent refetch after a
   *   per-key save
   * @prop {string} [query] - settings key-search text; hides non-matching rows
   * @prop {(n: number) => void} [onMatchCount] - reports the visible-row count to the parent
   *   global empty state
   */
  let {
    formValues,
    rawText = "",
    onField,
    sources = {},
    onReset = null,
    onSaved = null,
    query = "",
    onMatchCount = null,
  } = $props();

  let env = $derived(parseEnv(rawText));
  let rateLimitPerIp = $derived(formValues.RATE_LIMIT_PER_IP ?? "0");
  let routingSmart = $derived(
    String(formValues.ROUTING_SMART ?? "true").toLowerCase() !== "false",
  );
  let tokenMaxConcurrent = $derived(formValues.TOKEN_MAX_CONCURRENT ?? "2");
  let queueWait = $derived(formValues.QUEUE_WAIT ?? "30s");
  let queueDepth = $derived(formValues.QUEUE_DEPTH ?? "16");

  // ---------------------------------------------------------------------------
  // Key search: row copy lives in consts so rendering + matching share one
  // source (case-insensitive key + label/description substring).
  // ---------------------------------------------------------------------------
  const RL_IP_LABEL = "Rate Limit per Client IP";
  const RL_IP_DESC =
    "Maximum requests per second allowed from any single client IP address. Prevents rapid agent loops from depleting the pool. Set to 0 for no cap.";
  const RL_IP_HINT = "0 = no cap (recommended for a single-user gateway)";

  // Rotation copy (relocated verbatim from Tokens.svelte).
  const ROT_SECTION = "Rotation";
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
  // Smart routing copy (curated Pool rows: ROUTING_SMART et al. — all
  // live-apply, none restart-only).
  const SMART_SECTION = "Smart routing";
  const SMART_LABEL = "Smart Routing";
  const SMART_DESC =
    "Route each request through per-token live-turn slots with a FIFO waiter queue plus the unified scorer. Off restores the legacy acquire path.";
  const MAXC_LABEL = "Max Concurrent Turns per Token";
  const MAXC_DESC =
    "Cap on concurrent live turns per pooled token (default 2, the approved anti-ban pacing). Excess waiters park FIFO until Queue Wait elapses.";
  const MAXC_HINT = "0 = unlimited (no slot gating at all)";
  const QWAIT_LABEL = "Queue Wait";
  const QWAIT_DESC =
    "How long one acquire parks on a full token's FIFO queue before failing over (Go duration, e.g. 5s, 30s). Empty or non-positive falls back to 30s.";
  const QDEPTH_LABEL = "Queue Depth";
  const QDEPTH_DESC =
    "Cap on parked FIFO waiters per token. A full queue fails over at once with the existing 429 shape.";
  const QDEPTH_HINT = "0 = no queueing (fail over at once)";

  let q = $derived(query.trim().toLowerCase());
  function hit(...parts) {
    if (!q) return true;
    return parts.join("\n").toLowerCase().includes(q);
  }

  let showRotation = $derived(
    hit(
      "TOKEN_ROTATION",
      "RATE_LIMIT_FAILOVER",
      ROT_SECTION,
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
      "Token Rotation & Handling Policy",
      "Strategy used by the gateway to select upstream accounts for model requests.",
    ),
  );
  let showSmart = $derived(
    hit("ROUTING_SMART", SMART_SECTION, SMART_LABEL, SMART_DESC),
  );
  let showMaxConc = $derived(
    hit(
      "TOKEN_MAX_CONCURRENT",
      SMART_SECTION,
      MAXC_LABEL,
      MAXC_DESC,
      MAXC_HINT,
    ),
  );
  let showQueueWait = $derived(
    hit("QUEUE_WAIT", SMART_SECTION, QWAIT_LABEL, QWAIT_DESC),
  );
  let showQueueDepth = $derived(
    hit("QUEUE_DEPTH", SMART_SECTION, QDEPTH_LABEL, QDEPTH_DESC, QDEPTH_HINT),
  );
  let showIp = $derived(
    hit("RATE_LIMIT_PER_IP", RL_IP_LABEL, RL_IP_DESC, RL_IP_HINT),
  );
  let visibleKeys = $derived(
    [
      showSmart ? "ROUTING_SMART" : null,
      showMaxConc ? "TOKEN_MAX_CONCURRENT" : null,
      showQueueWait ? "QUEUE_WAIT" : null,
      showQueueDepth ? "QUEUE_DEPTH" : null,
      showIp ? "RATE_LIMIT_PER_IP" : null,
    ].filter((k) => k !== null),
  );
  // The bespoke section counts as one row for the "N of M" search count.
  let visible = $derived((showRotation ? 1 : 0) + visibleKeys.length);
  $effect(() => {
    onMatchCount?.(visible);
  });

  // ---------------------------------------------------------------------------
  // Rotation & failover: whole-file flow. Unlike Tokens.svelte (which
  // saved immediately via its own fetch + postForm), this page batches every
  // edit through onField into the shared Save/Discard flow — the
  // persistence target (file document → configSave) is identical.
  // ---------------------------------------------------------------------------
  const ROT_MODES = ["drain", "round_robin", "least_used", "random"];
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
</script>

{#if !q || visible > 0}
  <SettingsCard
    title={$tr("Pool")}
    description={$tr(
      "Traffic limits, rotation policy, and smart routing for the token pool. Changes apply live without restart.",
    )}
  >
    {#snippet icon()}
      <Activity size={20} />
    {/snippet}
    {#snippet actions()}
      <span
        class="inline-flex items-center gap-1.5 font-mono text-xs text-[var(--fp-muted)]"
      >
        <span class="led {tokenRotation === 'drain' ? 'led-good' : 'led-idle'}"
        ></span>
        <span
          class="uppercase tracking-wider font-semibold text-[var(--fp-accent)]"
          >{tokenRotation}</span
        >
      </span>
      {#if q}
        <span
          role="status"
          class="text-[11px] font-mono text-[var(--fp-dim)] shrink-0"
          >{$tr("{visible} of {total}", { visible, total: 6 })}</span
        >
      {/if}
    {/snippet}

    {#if showRotation}
      <!-- Rotation (relocated from Tokens.svelte) -->
      <div class="space-y-3 py-4">
        <p
          class="text-xs font-semibold uppercase tracking-wider text-[var(--fp-muted)]"
        >
          {$tr(ROT_SECTION)}
        </p>
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
        <!-- Rate Limit Auto-Failover Toggle -->
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
          </div>
          <ToggleSwitch
            checked={rateLimitFailover}
            ariaLabel="Auto Failover on Rate Limit (429)"
            onchange={(v) => toggleRateLimitFailover(v)}
          />
        </div>
      </div>
    {/if}

    {#if showSmart || showMaxConc || showQueueWait || showQueueDepth}
      <!-- Smart routing (curated Pool rows) -->
      <div class="pt-4">
        <p
          class="text-xs font-semibold uppercase tracking-wider text-[var(--fp-muted)] pb-1"
        >
          {$tr(SMART_SECTION)}
        </p>
        {#if showSmart}
          <SettingsRow
            first={visibleKeys[0] === "ROUTING_SMART"}
            last={visibleKeys[visibleKeys.length - 1] === "ROUTING_SMART"}
            label={$tr(SMART_LABEL)}
            description={$tr(SMART_DESC)}
          >
            {#snippet badge()}
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
              {#if sources.ROUTING_SMART === "db" || sources.ROUTING_SMART === "env"}
                <DbBadge
                  settingKey="ROUTING_SMART"
                  source={sources.ROUTING_SMART}
                  {onReset}
                />
              {/if}
            {/snippet}
            {#snippet extra()}
              <DbOverrideSave
                settingKey="ROUTING_SMART"
                value={formValues.ROUTING_SMART ?? "true"}
                {onSaved}
              />
            {/snippet}

            <div class="flex items-center gap-2.5">
              <ToggleSwitch
                checked={routingSmart}
                ariaLabel="ROUTING_SMART"
                onchange={(v) => toggleRoutingSmart(v)}
              />
            </div>
          </SettingsRow>
        {/if}
        {#if showMaxConc}
          <SettingsRow
            first={visibleKeys[0] === "TOKEN_MAX_CONCURRENT"}
            last={visibleKeys[visibleKeys.length - 1] ===
              "TOKEN_MAX_CONCURRENT"}
            label={$tr(MAXC_LABEL)}
            description={$tr(MAXC_DESC)}
          >
            {#snippet badge()}
              <code
                class="text-[10px] px-1.5 py-0.5 rounded bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-mono"
                >TOKEN_MAX_CONCURRENT</code
              >
              {#if !env.TOKEN_MAX_CONCURRENT}
                <span
                  class="text-[10px] px-1.5 py-0.5 rounded-[var(--fp-radius-sm)] border border-[var(--fp-border)] bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-semibold uppercase tracking-wider shrink-0"
                  >{$tr("default")}</span
                >
              {/if}
              {#if sources.TOKEN_MAX_CONCURRENT === "db" || sources.TOKEN_MAX_CONCURRENT === "env"}
                <DbBadge
                  settingKey="TOKEN_MAX_CONCURRENT"
                  source={sources.TOKEN_MAX_CONCURRENT}
                  {onReset}
                />
              {/if}
            {/snippet}
            {#snippet extra()}
              <DbOverrideSave
                settingKey="TOKEN_MAX_CONCURRENT"
                value={tokenMaxConcurrent}
                {onSaved}
              />
            {/snippet}

            <div class="w-full sm:w-56">
              <NumberStepper
                value={tokenMaxConcurrent}
                min={0}
                step={1}
                ariaLabel="TOKEN_MAX_CONCURRENT"
                placeholder="2"
                oninput={(v) => {
                  const val = v.trim();
                  onField("TOKEN_MAX_CONCURRENT", val === "" ? "2" : val);
                }}
              />
              <p class="text-[10px] text-[var(--fp-dim)] mt-1">
                {$tr(MAXC_HINT)}
              </p>
            </div>
          </SettingsRow>
        {/if}
        {#if showQueueWait}
          <SettingsRow
            first={visibleKeys[0] === "QUEUE_WAIT"}
            last={visibleKeys[visibleKeys.length - 1] === "QUEUE_WAIT"}
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
              {#if sources.QUEUE_WAIT === "db" || sources.QUEUE_WAIT === "env"}
                <DbBadge
                  settingKey="QUEUE_WAIT"
                  source={sources.QUEUE_WAIT}
                  {onReset}
                />
              {/if}
            {/snippet}
            {#snippet extra()}
              <DbOverrideSave
                settingKey="QUEUE_WAIT"
                value={queueWait}
                {onSaved}
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
        {/if}
        {#if showQueueDepth}
          <SettingsRow
            first={visibleKeys[0] === "QUEUE_DEPTH"}
            last={visibleKeys[visibleKeys.length - 1] === "QUEUE_DEPTH"}
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
              {#if sources.QUEUE_DEPTH === "db" || sources.QUEUE_DEPTH === "env"}
                <DbBadge
                  settingKey="QUEUE_DEPTH"
                  source={sources.QUEUE_DEPTH}
                  {onReset}
                />
              {/if}
            {/snippet}
            {#snippet extra()}
              <DbOverrideSave
                settingKey="QUEUE_DEPTH"
                value={queueDepth}
                {onSaved}
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
        {/if}
      </div>
    {/if}

    {#if showIp}
      <!-- Client IP Rate Limit -->
      <SettingsRow
        first={visibleKeys[0] === "RATE_LIMIT_PER_IP"}
        last={visibleKeys[visibleKeys.length - 1] === "RATE_LIMIT_PER_IP"}
        label={$tr(RL_IP_LABEL)}
        description={$tr(RL_IP_DESC)}
      >
        {#snippet badge()}
          <code
            class="text-[10px] px-1.5 py-0.5 rounded bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-mono"
            >RATE_LIMIT_PER_IP</code
          >
          {#if !env.RATE_LIMIT_PER_IP}
            <span
              class="text-[10px] px-1.5 py-0.5 rounded-[var(--fp-radius-sm)] border border-[var(--fp-border)] bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-semibold uppercase tracking-wider shrink-0"
              >{$tr("default")}</span
            >
          {/if}
          {#if sources.RATE_LIMIT_PER_IP === "db" || sources.RATE_LIMIT_PER_IP === "env"}
            <DbBadge
              settingKey="RATE_LIMIT_PER_IP"
              source={sources.RATE_LIMIT_PER_IP}
              {onReset}
            />
          {/if}
        {/snippet}
        {#snippet extra()}
          <DbOverrideSave
            settingKey="RATE_LIMIT_PER_IP"
            value={rateLimitPerIp}
            {onSaved}
          />
        {/snippet}

        <div class="w-full sm:w-56">
          <NumberStepper
            value={rateLimitPerIp}
            min={0}
            step={1}
            ariaLabel="RATE_LIMIT_PER_IP"
            placeholder="0"
            oninput={(v) => {
              const val = v.trim();
              onField("RATE_LIMIT_PER_IP", val === "" ? "0" : val);
            }}
          />
          <p class="text-[10px] text-[var(--fp-dim)] mt-1">
            {$tr(RL_IP_HINT)}
          </p>
        </div>
      </SettingsRow>
    {/if}
  </SettingsCard>
{/if}
