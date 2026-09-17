<script>
  import SettingsCard from "../../components/SettingsCard.svelte";
  import SettingsRow from "../../components/SettingsRow.svelte";
  import DbOverrideSave from "../../components/DbOverrideSave.svelte";
  import ToggleSwitch from "../../components/ToggleSwitch.svelte";
  import NumberStepper from "../../components/NumberStepper.svelte";
  import { Activity } from "@lucide/svelte";
  import { tr } from "../../i18n.js";
  import { parseEnv } from "../../utils/env.js";

  /**
   * Pool Controls settings card (Pool group).
   * Built using the SettingsCard and SettingsRow template components.
   * Holds the Client IP Rate Limit row and the Bridge Mode row (moved from
   * GatewaySettings for single Pool ownership). The queue-posture keys —
   * SLOTS_PER_ACCOUNT, QUEUE_WAIT, QUEUE_DEPTH, MAX_SPILL_ACCOUNTS — are
   * owned and rendered by the Pool Strategy card above, and per-account
   * pins live in the token drawer, so no key has a second editor here.
   * Every row instant-saves to the DB overlay on edit (DbOverrideSave);
   * all keys apply live on save (none is restart-only).
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
   * @prop {boolean} [degraded=false] - settings store offline: rows render
   *   an honest offline note and stay read-only for saves
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
  let bridgeEnabled = $derived(formValues.BRIDGE_ENABLED !== "false");

  // ---------------------------------------------------------------------------
  // Key search: row copy lives in consts so rendering + matching share one
  // source (case-insensitive key + label/description substring).
  // ---------------------------------------------------------------------------
  const RL_IP_LABEL = "Rate Limit per Client IP";
  const RL_IP_DESC =
    "Maximum requests per second allowed from any single client IP address. Prevents rapid agent loops from depleting the pool. Set to 0 for no cap.";
  const RL_IP_HINT = "0 = no cap (recommended for a single-user gateway)";
  const BRIDGE_LABEL = "Allow Client-Provided Tokens (Bridge Mode)";
  const BRIDGE_DESC =
    "Enforces hybrid access: client apps can pass their personal FreeBuff account tokens via the Authorization header, saving your server's shared pool quota.";

  let q = $derived(query.trim().toLowerCase());
  function hit(...parts) {
    if (!q) return true;
    return parts.join("\n").toLowerCase().includes(q);
  }

  let showIp = $derived(
    hit("RATE_LIMIT_PER_IP", RL_IP_LABEL, RL_IP_DESC, RL_IP_HINT),
  );
  let showBridge = $derived(hit("BRIDGE_ENABLED", BRIDGE_LABEL, BRIDGE_DESC));
  let visibleKeys = $derived(
    [
      showIp ? "RATE_LIMIT_PER_IP" : null,
      showBridge ? "BRIDGE_ENABLED" : null,
    ].filter((k) => k !== null),
  );
  let visible = $derived(visibleKeys.length);
  $effect(() => {
    onMatchCount?.(visible);
  });

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
      "Client IP limits and bridge mode for the token pool. Queue posture (slots, waits, spill) lives in the Pool Strategy card above; per-account pins live in the token drawer. Changes apply live without restart.",
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
          >{$tr("{visible} of {total}", { visible, total: 2 })}</span
        >
      {/if}
    {/snippet}

    {#if stub}
      <div class="py-4 flex flex-col items-start gap-2">
        <p class="text-xs text-[var(--fp-muted)] leading-relaxed">
          {$tr(
            "Queue posture, spill bounds, and limits now live under the Pool page's Controls tab.",
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
            <DbOverrideSave
              settingKey="RATE_LIMIT_PER_IP"
              value={rateLimitPerIp}
              source={sources.RATE_LIMIT_PER_IP}
              {onReset}
              {onSaved}
              {degraded}
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
            <DbOverrideSave
              settingKey="BRIDGE_ENABLED"
              value={formValues.BRIDGE_ENABLED ?? "true"}
              source={sources.BRIDGE_ENABLED}
              {onReset}
              {onSaved}
              {degraded}
            />
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
