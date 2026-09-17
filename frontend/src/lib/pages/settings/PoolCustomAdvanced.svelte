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
   * Hand-tuning home for the relocated Pool Tuning rows (admission
   * caches, sessions). The routing knobs the strategy presets own —
   * token rotation, the ROUTING_SMART master switch, 429 failover,
   * QUEUE_WAIT, QUEUE_DEPTH — live in the Pool Strategy card, so none
   * of them is rendered twice. Values carried over unchanged; only the
   * address moved. The quota-prober rows are gone with the prober
   * removal (no catalog rows, no section), not relocated.
   * Every row instant-saves to the DB overlay on edit (DbOverrideSave).
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

  // --- Relocated groups (values preserved, address moved) ---
  // No probing group: QUOTA_PROBE_* / QUOTA_AUTO_PROBE were excised from
  // the catalog with the prober removal, so there is nothing to relocate.
  const CACHE_KEYS = ["MODEL_UNAVAILABLE_CACHE_TTL", "SESSION_PROBE_CACHE_TTL"];
  const SESSION_KEYS = [
    "SESSION_RE_ADMIT_LEAD",
    "WAITING_ROOM_CHAIN",
    "SESSION_PERSIST",
    "ADOPT_CLI_SESSION",
  ];
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
  function shownKeys(keys) {
    return keys.filter((k) => entry(k) && hit(rowText(k)));
  }
  let cacheShown = $derived(shownKeys(CACHE_KEYS));
  let sessionShown = $derived(shownKeys(SESSION_KEYS));
  let visible = $derived(cacheShown.length + sessionShown.length);
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
          <DbOverrideSave
            settingKey={key}
            value={val(key)}
            restartOnly={e.restart_only}
            source={sources[key]}
            {onReset}
            {onSaved}
            {degraded}
          />
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
      "Hand-tuned admission-cache and session knobs. Values carried over unchanged — only the address moved.",
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
          >{$tr("{visible} of {total}", { visible, total: 6 })}</span
        >
      {/if}
    {/snippet}

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
  </SettingsCard>
{/if}
