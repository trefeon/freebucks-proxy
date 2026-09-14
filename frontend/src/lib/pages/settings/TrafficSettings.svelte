<script>
  import SettingsCard from "../../components/SettingsCard.svelte";
  import SettingsRow from "../../components/SettingsRow.svelte";
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
   * Holds the curated Smart routing queue rows (TOKEN_MAX_CONCURRENT,
   * QUEUE_WAIT, QUEUE_DEPTH) — dimmed, never disabled, while smart
   * routing is off or turns are unlimited — followed by the Client IP
   * Rate Limit row and the Bridge Mode row (moved from GatewaySettings
   * for single Pool ownership). Rotation policy, the ROUTING_SMART
   * master switch, and 429 failover live in the Pool Strategy card and
   * Custom advanced. All keys apply live on reload (none is restart-only).
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
   * @prop {boolean} [stub=false] - link-out stub for the Settings page
   *   (same search matching + count, body links to #tokens)
   * @prop {boolean} [degraded=false] - settings store offline: per-key
   *   overlay saves render an honest offline note, .env flow stays usable
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
    stub = false,
    degraded = false,
    cardTitle = "Pool",
  } = $props();
  let env = $derived(parseEnv(rawText));
  let rateLimitPerIp = $derived(formValues.RATE_LIMIT_PER_IP ?? "0");
  let routingSmart = $derived(
    String(formValues.ROUTING_SMART ?? "true").toLowerCase() !== "false",
  );
  let tokenMaxConcurrent = $derived(formValues.TOKEN_MAX_CONCURRENT ?? "2");
  let queueWait = $derived(formValues.QUEUE_WAIT ?? "30s");
  let queueDepth = $derived(formValues.QUEUE_DEPTH ?? "16");
  let bridgeEnabled = $derived(formValues.BRIDGE_ENABLED !== "false");

  // ---------------------------------------------------------------------------
  // Key search: row copy lives in consts so rendering + matching share one
  // source (case-insensitive key + label/description substring).
  // ---------------------------------------------------------------------------
  const RL_IP_LABEL = "Rate Limit per Client IP";
  const RL_IP_DESC =
    "Maximum requests per second allowed from any single client IP address. Prevents rapid agent loops from depleting the pool. Set to 0 for no cap.";
  const RL_IP_HINT = "0 = no cap (recommended for a single-user gateway)";

  // Queue posture copy (curated Pool rows: TOKEN_MAX_CONCURRENT et al. —
  // all live-apply, none restart-only). Rotation policy, the smart-routing
  // master switch, and 429 failover moved to the Pool Strategy card and
  // Custom advanced; this card keeps the queue rows, which dim (never
  // disable) while smart routing is off or turns are unlimited.
  const SMART_SECTION = "Smart routing";
  const MAXC_LABEL = "Max Concurrent Turns per Token";
  const MAXC_DESC =
    "Cap on concurrent live turns per pooled token (default 2, the approved anti-ban pacing). Excess waiters park FIFO until Queue Wait elapses.";
  const MAXC_HINT = "0 = unlimited (no slot gating at all)";
  const QWAIT_LABEL = "Queue Wait";
  const QWAIT_DESC =
    "How long one acquire parks on a full token's FIFO live-turn queue before failing over (Go duration, e.g. 5s, 30s). Empty or non-positive falls back to 30s.";
  const QDEPTH_LABEL = "Queue Depth";
  const QDEPTH_DESC =
    "Cap on parked FIFO waiters per token. A full queue fails over at once with the existing 429 shape.";
  const QDEPTH_HINT = "0 = no queueing (fail over at once)";
  const BRIDGE_LABEL = "Allow Client-Provided Tokens (Bridge Mode)";
  const BRIDGE_DESC =
    "Enforces hybrid access: client apps can pass their personal FreeBuff account tokens via the Authorization header, saving your server's shared pool quota.";

  let q = $derived(query.trim().toLowerCase());
  function hit(...parts) {
    if (!q) return true;
    return parts.join("\n").toLowerCase().includes(q);
  }

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
  let showBridge = $derived(hit("BRIDGE_ENABLED", BRIDGE_LABEL, BRIDGE_DESC));
  let visibleKeys = $derived(
    [
      showMaxConc ? "TOKEN_MAX_CONCURRENT" : null,
      showQueueWait ? "QUEUE_WAIT" : null,
      showQueueDepth ? "QUEUE_DEPTH" : null,
      showIp ? "RATE_LIMIT_PER_IP" : null,
      showBridge ? "BRIDGE_ENABLED" : null,
    ].filter((k) => k !== null),
  );
  let visible = $derived(visibleKeys.length);
  $effect(() => {
    onMatchCount?.(visible);
  });

  // Queue rows dim (never disable) while smart routing is off — the
  // master switch moved to Custom advanced — or while turns are
  // unlimited (0 = no slot gating, so nothing ever parks).
  let queueParked = $derived(!routingSmart || Number(tokenMaxConcurrent) === 0);

  // Whole-file flow: every edit batches through onField into the shared
  // Save/Discard flow (file document → configSave).
  function toggleBridge(next) {
    const v = typeof next === "boolean" ? next : !bridgeEnabled;
    onField("BRIDGE_ENABLED", v ? "true" : "false");
  }
</script>

{#if !q || visible > 0}
  <SettingsCard
    title={$tr(cardTitle)}
    description={$tr(
      "Queue posture, client IP limits, and bridge mode for the token pool. Strategy presets sit in the card above; hand-tuning lives in Custom advanced below. Changes apply live without restart.",
    )}
  >
    {#snippet icon()}
      <Activity size={20} />
    {/snippet}
    {#snippet actions()}
      {#if q}
        <span
          role="status"
          class="text-[11px] font-mono text-[var(--fp-dim)] shrink-0"
          >{$tr("{visible} of {total}", { visible, total: 5 })}</span
        >
      {/if}
    {/snippet}

    {#if stub}
      <div class="py-4 flex flex-col items-start gap-2">
        <p class="text-xs text-[var(--fp-muted)] leading-relaxed">
          {$tr(
            "Pool rotation, smart routing, and limits now live under the Pool page's Controls tab.",
          )}
        </p>
        <a
          href="#tokens"
          onclick={() => {
            try {
              sessionStorage.setItem("fp-page-tab:tokens", "controls");
            } catch {
              // Storage unavailable — the Pool page opens on its default tab.
            }
          }}
          class="text-xs text-[var(--fp-accent)] hover:underline font-medium"
        >
          {$tr("Manage Pool controls (Pool → Controls tab)")}
        </a>
      </div>
    {:else}
      {#if showMaxConc || showQueueWait || showQueueDepth}
        <!-- Smart routing queue rows (master switch lives in Custom advanced) -->
        <div class="pt-4">
          <p
            class="text-xs font-semibold uppercase tracking-wider text-[var(--fp-muted)] pb-1"
          >
            {$tr(SMART_SECTION)}
          </p>
          {#if queueParked}
            <p class="text-[11px] text-[var(--fp-dim)] leading-relaxed pb-1">
              {$tr(
                "Parked: smart routing is off or turns are unlimited (0) — these rows do nothing until re-enabled in Custom advanced.",
              )}
            </p>
          {/if}
          {#if showMaxConc}
            <SettingsRow
              class={queueParked ? "opacity-60" : ""}
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
              {/snippet}
              {#snippet extra()}
                {#if degraded}
                  <span class="text-[10px] text-[var(--fp-dim)]"
                    >{$tr("Overlay offline — use .env save")}</span
                  >
                {:else}
                  <DbOverrideSave
                    settingKey="TOKEN_MAX_CONCURRENT"
                    value={tokenMaxConcurrent}
                    source={sources.TOKEN_MAX_CONCURRENT}
                    {onReset}
                    {onSaved}
                  />
                {/if}
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
              class={queueParked ? "opacity-60" : ""}
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
              {/snippet}
              {#snippet extra()}
                {#if degraded}
                  <span class="text-[10px] text-[var(--fp-dim)]"
                    >{$tr("Overlay offline — use .env save")}</span
                  >
                {:else}
                  <DbOverrideSave
                    settingKey="QUEUE_WAIT"
                    value={queueWait}
                    source={sources.QUEUE_WAIT}
                    {onReset}
                    {onSaved}
                  />
                {/if}
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
              class={queueParked ? "opacity-60" : ""}
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
              {/snippet}
              {#snippet extra()}
                {#if degraded}
                  <span class="text-[10px] text-[var(--fp-dim)]"
                    >{$tr("Overlay offline — use .env save")}</span
                  >
                {:else}
                  <DbOverrideSave
                    settingKey="QUEUE_DEPTH"
                    value={queueDepth}
                    source={sources.QUEUE_DEPTH}
                    {onReset}
                    {onSaved}
                  />
                {/if}
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
          {/snippet}
          {#snippet extra()}
            {#if degraded}
              <span class="text-[10px] text-[var(--fp-dim)]"
                >{$tr("Overlay offline — use .env save")}</span
              >
            {:else}
              <DbOverrideSave
                settingKey="RATE_LIMIT_PER_IP"
                value={rateLimitPerIp}
                source={sources.RATE_LIMIT_PER_IP}
                {onReset}
                {onSaved}
              />
            {/if}
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

      {#if showBridge}
        <SettingsRow
          first={visibleKeys[0] === "BRIDGE_ENABLED"}
          last={visibleKeys[visibleKeys.length - 1] === "BRIDGE_ENABLED"}
          label={$tr(BRIDGE_LABEL)}
          description={$tr(BRIDGE_DESC)}
        >
          {#snippet badge()}
            <code
              class="text-[10px] px-1.5 py-0.5 rounded bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-mono"
              >BRIDGE_ENABLED</code
            >
            {#if !env.BRIDGE_ENABLED}
              <span
                class="text-[10px] px-1.5 py-0.5 rounded-[var(--fp-radius-sm)] border border-[var(--fp-border)] bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-semibold uppercase tracking-wider shrink-0"
                >{$tr("default")}</span
              >
            {/if}
          {/snippet}
          {#snippet extra()}
            {#if degraded}
              <span class="text-[10px] text-[var(--fp-dim)]"
                >{$tr("Overlay offline — use .env save")}</span
              >
            {:else}
              <DbOverrideSave
                settingKey="BRIDGE_ENABLED"
                value={formValues.BRIDGE_ENABLED ?? "true"}
                source={sources.BRIDGE_ENABLED}
                {onReset}
                {onSaved}
              />
            {/if}
          {/snippet}

          <div class="flex items-center gap-2.5">
            <ToggleSwitch
              checked={bridgeEnabled}
              ariaLabel="BRIDGE_ENABLED"
              onchange={(v) => toggleBridge(v)}
            />
          </div>
        </SettingsRow>
      {/if}
    {/if}
  </SettingsCard>
{/if}
