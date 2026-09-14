<script>
  import SettingsCard from "../../components/SettingsCard.svelte";
  import SettingsRow from "../../components/SettingsRow.svelte";
  import DbOverrideSave from "../../components/DbOverrideSave.svelte";
  import ToggleSwitch from "../../components/ToggleSwitch.svelte";
  import NumberStepper from "../../components/NumberStepper.svelte";
  import DurationPicker from "../../components/DurationPicker.svelte";
  import { Wrench } from "@lucide/svelte";
  import { tr } from "../../i18n.js";
  import { parseEnv } from "../../utils/env.js";

  /**
   * Custom advanced card (Pool → Controls, below Pool Controls).
   * Hand-tuning home for the knobs the strategy presets fix: token
   * rotation, smart-routing master switch, and 429 failover — plus the
   * relocated Pool Tuning rows (probe intervals, admission caches,
   * sessions). Values carried over unchanged; only the address moved.
   * Edits batch through onField into the page Save; per-key overlay saves
   * stay available on the generic rows.
   *
   * @prop {Array} meta - config catalog entries (for relocated-row copy)
   * @prop {Record<string, string>} formValues
   * @prop {string} rawText
   * @prop {(key: string, value: string) => void} onField
   * @prop {Record<string, string>} [sources] - ADR-0019 source tiers
   * @prop {(key: string) => Promise<void>} [onReset] - saved-value reset
   * @prop {(() => Promise<void>) | null} [onSaved] - parent refetch after a
   *   per-key save
   * @prop {boolean} [degraded=false] - settings store offline: per-key
   *   overlay saves render an honest offline note, .env flow stays usable
   * @prop {number} [tokenCount=0] - pooled accounts (ADOPT_CLI_SESSION gate note)
   * @prop {string} [query] - settings key-search text; hides non-matching rows
   * @prop {(n: number) => void} [onMatchCount] - reports the visible-row count to the parent
   */
  let {
    meta = [],
    formValues,
    rawText = "",
    onField,
    sources = {},
    onReset = null,
    onSaved = null,
    degraded = false,
    tokenCount = 0,
    query = "",
    onMatchCount = null,
  } = $props();

  let env = $derived(parseEnv(rawText));

  // --- Relocated-row catalog lookup (copy/defaults stay sourced) ---
  function entry(key) {
    return (meta ?? []).find((e) => e?.key === key);
  }
  function labelFor(key) {
    return key
      .toLowerCase()
      .split("_")
      .map((w) => w.charAt(0).toUpperCase() + w.slice(1))
      .join(" ");
  }
  function val(key) {
    const e = entry(key);
    const v = formValues[key];
    if (v !== undefined && v !== null && v !== "") return String(v);
    return e?.default ?? "";
  }
  function boolVal(key) {
    const e = entry(key);
    const v = val(key);
    if (v === "") return (e?.default ?? "true") !== "false";
    return v !== "false";
  }
  const GO_DURATION_RE = /^(\d+(\.\d+)?(ns|us|µs|ms|s|m|h))+$/;
  const TEMPORAL_KEY_RE =
    /(TIMEOUT|TTL|INTERVAL|IDLE|EVICT|RETENTION|WINDOW|HEARTBEAT|EXPIR|WAIT|DELAY|DRAIN|PERIOD|JITTER)/;
  const DURATION_PRESETS = ["5s", "15s", "30s", "1m", "5m", "15m", "1h"];
  function isDurationKey(e) {
    if (!e || e.kind !== "text") return false;
    if (GO_DURATION_RE.test((e.default ?? "").trim())) return true;
    if (/go duration|duration/i.test(e.description ?? "")) return true;
    return TEMPORAL_KEY_RE.test(e.key ?? "");
  }

  // --- Rotation / failover / smart switch (moved from TrafficSettings) ---
  // Rotation copy moved verbatim with the radiogroup.
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
  let routingSmart = $derived(
    String(formValues.ROUTING_SMART ?? "true").toLowerCase() !== "false",
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

  // --- Relocated groups (values preserved, address moved) ---
  const PROBE_KEYS = [
    "QUOTA_PROBE_ACTIVE_INTERVAL",
    "QUOTA_PROBE_IDLE_HEARTBEAT",
  ];
  const CACHE_KEYS = ["MODEL_UNAVAILABLE_CACHE_TTL", "SESSION_PROBE_CACHE_TTL"];
  const SESSION_KEYS = [
    "SESSION_RE_ADMIT_LEAD",
    "WAITING_ROOM_CHAIN",
    "SESSION_PERSIST",
    "ADOPT_CLI_SESSION",
  ];
  const FLOOR_TITLE = "Floor cadangan (Phase 2)";
  const FLOOR_BODY =
    "Below the floor on every account, the gateway still serves and only warns — fail-open, never a refusal.";
  const FLOOR_NOTE =
    "No key yet, nothing is written: enforcement lands in a later phase.";

  // Probing rows dim (never disable) while the prober master switch is
  // off — the switch itself stays in Pool Tuning.
  let autoProbeOff = $derived(
    String(formValues.QUOTA_AUTO_PROBE ?? "true").toLowerCase() === "false",
  );
  let adoptGated = $derived((tokenCount ?? 0) > 1);

  function rowText(key) {
    const e = entry(key);
    return `${key} ${labelFor(key)} ${e?.description ?? ""}`;
  }
  let q = $derived(query.trim().toLowerCase());
  function hit(...parts) {
    if (!q) return true;
    return parts.join("\n").toLowerCase().includes(q);
  }
  let showRouting = $derived(
    hit(
      "TOKEN_ROTATION",
      "RATE_LIMIT_FAILOVER",
      "ROUTING_SMART",
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
    ),
  );
  function shownKeys(keys) {
    return keys.filter((k) => entry(k) && hit(rowText(k)));
  }
  let probeShown = $derived(shownKeys(PROBE_KEYS));
  let cacheShown = $derived(shownKeys(CACHE_KEYS));
  let sessionShown = $derived(shownKeys(SESSION_KEYS));
  let showFloor = $derived(hit(FLOOR_TITLE, FLOOR_BODY, FLOOR_NOTE));
  let visible = $derived(
    (showRouting ? 3 : 0) +
      probeShown.length +
      cacheShown.length +
      sessionShown.length +
      (showFloor ? 1 : 0),
  );
  $effect(() => {
    onMatchCount?.(visible);
  });
</script>

{#snippet genRow(key, dim = false)}
  {@const e = entry(key)}
  {#if e}
    <div id="setting-{key}" class="scroll-mt-24">
      <SettingsRow
        class={dim ? "opacity-60" : ""}
        label={labelFor(key)}
        description={e.description ?? ""}
      >
        {#snippet badge()}
          <code
            class="text-[10px] px-1.5 py-0.5 rounded bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-mono"
            >{key}</code
          >
          {#if !env[key]}
            <span
              class="text-[10px] px-1.5 py-0.5 rounded-[var(--fp-radius-sm)] border border-[var(--fp-border)] bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-semibold uppercase tracking-wider shrink-0"
              >{$tr("default")}</span
            >
          {/if}
          {#if e.restart_only}
            <span class="text-[10px] text-[var(--fp-dim)] lowercase shrink-0"
              >{$tr("(needs restart)")}</span
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
              settingKey={key}
              value={val(key)}
              restartOnly={e.restart_only}
              source={sources[key]}
              {onReset}
              {onSaved}
            />
          {/if}
        {/snippet}
        {#if e.kind === "bool"}
          <ToggleSwitch
            checked={boolVal(key)}
            ariaLabel={key}
            onchange={(v) => onField(key, v ? "true" : "false")}
          />
        {:else if e.kind === "int"}
          <div class="w-full sm:w-56">
            <NumberStepper
              value={val(key)}
              min={0}
              step={1}
              ariaLabel={key}
              placeholder={e.default ?? ""}
              oninput={(v) => onField(key, v)}
            />
          </div>
        {:else if isDurationKey(e)}
          <div class="w-full sm:w-56">
            <DurationPicker
              value={val(key)}
              presets={DURATION_PRESETS}
              ariaLabel={key}
              placeholder={e.default ?? ""}
              oninput={(v) => onField(key, v)}
            />
          </div>
        {:else}
          <div class="w-full sm:w-56">
            <input
              type="text"
              class="fp-input w-full !text-xs !py-1.5 font-mono"
              value={val(key)}
              title={val(key)}
              aria-label={key}
              placeholder={e.default ?? ""}
              oninput={(e2) => onField(key, e2.currentTarget.value)}
            />
          </div>
        {/if}
      </SettingsRow>
      {#if key === "ADOPT_CLI_SESSION"}
        <p class="text-[10px] text-[var(--fp-dim)] -mt-1 pb-3 leading-relaxed">
          {$tr(
            "Adopts the upstream CLI's live session instead of creating one. Single-token pools only{extra}.",
            {
              extra: adoptGated
                ? ` — this pool holds ${tokenCount} accounts`
                : "",
            },
          )}
        </p>
      {/if}
    </div>
  {/if}
{/snippet}

{#if !q || visible > 0}
  <SettingsCard
    title={$tr("Custom advanced")}
    description={$tr(
      "Hand-tuned routing, probing, and session knobs. Values carried over unchanged — only the address moved.",
    )}
  >
    {#snippet icon()}
      <Wrench size={20} />
    {/snippet}
    {#snippet actions()}
      {#if q}
        <span
          role="status"
          class="text-[11px] font-mono text-[var(--fp-dim)] shrink-0"
          >{$tr("{visible} of {total}", { visible, total: 12 })}</span
        >
      {/if}
    {/snippet}

    {#if showRouting}
      <div class="space-y-3 py-4">
        <p
          class="text-xs font-semibold uppercase tracking-wider text-[var(--fp-muted)]"
        >
          {$tr("Routing")}
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
          <ToggleSwitch
            checked={rateLimitFailover}
            ariaLabel="Auto Failover on Rate Limit (429)"
            onchange={(v) => toggleRateLimitFailover(v)}
          />
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
            </div>
            <p class="text-[11px] text-[var(--fp-muted)] leading-relaxed">
              {$tr(SMART_DESC)}
            </p>
          </div>
          <ToggleSwitch
            checked={routingSmart}
            ariaLabel="ROUTING_SMART"
            onchange={(v) => toggleRoutingSmart(v)}
          />
        </div>
      </div>
    {/if}

    {#if probeShown.length > 0}
      <div class="pt-4">
        <p
          class="text-xs font-semibold uppercase tracking-wider text-[var(--fp-muted)] pb-1"
        >
          {$tr("Probing")}
        </p>
        {#if autoProbeOff}
          <p class="text-[11px] text-[var(--fp-dim)] leading-relaxed pb-1">
            {$tr(
              "Parked: the prober master switch (QUOTA_AUTO_PROBE) is off in Pool Tuning.",
            )}
          </p>
        {/if}
        {#each probeShown as key (key)}
          {@render genRow(key, autoProbeOff)}
        {/each}
      </div>
    {/if}

    {#if cacheShown.length > 0}
      <div class="pt-4">
        <p
          class="text-xs font-semibold uppercase tracking-wider text-[var(--fp-muted)] pb-1"
        >
          {$tr("Admission cache")}
        </p>
        {#each cacheShown as key (key)}
          {@render genRow(key)}
        {/each}
      </div>
    {/if}

    {#if sessionShown.length > 0}
      <div class="pt-4">
        <p
          class="text-xs font-semibold uppercase tracking-wider text-[var(--fp-muted)] pb-1"
        >
          {$tr("Sessions")}
        </p>
        {#each sessionShown as key (key)}
          {@render genRow(key, key === "ADOPT_CLI_SESSION" && adoptGated)}
        {/each}
      </div>
    {/if}

    {#if showFloor}
      <div class="pt-4">
        <p
          class="text-xs font-semibold uppercase tracking-wider text-[var(--fp-muted)] pb-1"
        >
          {$tr(FLOOR_TITLE)}
        </p>
        <div
          class="fp-inset p-3 rounded text-xs text-[var(--fp-muted)] flex items-start gap-2"
        >
          <p class="leading-relaxed">
            {$tr(FLOOR_BODY)}
            {$tr(FLOOR_NOTE)}
          </p>
        </div>
      </div>
    {/if}
  </SettingsCard>
{/if}
