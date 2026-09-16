<script>
  import { onMount } from "svelte";
  import SettingsCard from "../../components/SettingsCard.svelte";
  import SettingsRow from "../../components/SettingsRow.svelte";
  import ToggleSwitch from "../../components/ToggleSwitch.svelte";
  import DbOverrideSave from "../../components/DbOverrideSave.svelte";
  import NumberStepper from "../../components/NumberStepper.svelte";
  import DurationPicker from "../../components/DurationPicker.svelte";
  import { SlidersHorizontal } from "@lucide/svelte";
  import { tr } from "../../i18n.js";
  import { parseEnv } from "../../utils/env.js";

  /**
   * Advanced settings: every catalog key the curated sections do not own.
   * Rows render generically from /admin/api/config/meta (bool → switch,
   * select → dropdown, int → number, text/list → input) with the catalog
   * default and restart-only badges. Secrets never reach this list (the
   * catalog flags them; Tokens/Security own their surfaces).
   *
   * @prop {Array} meta - config catalog entries
   * @prop {Record<string, string>} formValues
   * @prop {string} rawText
   * @prop {Record<string, string>} [sources] - ADR-0019 source tiers
   * @prop {(key: string) => Promise<void>} [onReset] - saved-value reset
   * @prop {(() => Promise<void>) | null} [onSaved] - parent refetch after a
   *   per-key save
   * @prop {string} [query] - settings key-search text; hides non-matching rows
   * @prop {(n: number) => void} [onMatchCount] - reports the visible-row count to the parent
   *   global empty state
   * @prop {Array<string> | null} [onlyGroups] - null renders every group,
   *   otherwise only the listed catalog group ids (e.g. ["pool"])
   * @prop {string} [cardTitle] - card heading, translated at render
   * @prop {string} [cardDescription] - card subheading, translated at render
   * @prop {boolean} [bridgePossible=true] - BRIDGE_IDLE_EVICT hides unless set
   * @prop {boolean} [degraded=false] - settings store offline: rows render
   *   an honest offline note and stay read-only for saves
   */
  let {
    meta = [],
    formValues,
    rawText = "",
    onField,
    sources = {},
    onReset = null,
    onSaved = null,
    query = "",
    onMatchCount = null,
    onlyGroups = null,
    cardTitle = "Advanced",
    cardDescription = "Every remaining tunable with its decided default. Restart-only keys need a container restart; the rest apply on save.",
    bridgePossible = true,
    degraded = false,
  } = $props();
  // Keys owned by the curated section components above (Gateway, Traffic,
  // ModelRouting, Dashboard access, the Pool Strategy card, Pool Custom
  // advanced, and the Warming tab's Streak Maintenance card which owns every
  // MATURITY_* key); Advanced shows everything else the catalog exposes.
  const COVERED = new Set([
    "ADOPT_CLI_SESSION",
    "BRIDGE_ENABLED",
    "DASHBOARD_REQUIRE_LOGIN",
    "HTTP_READ_TIMEOUT",
    "LOG_LEVEL",
    "MATURITY_ENABLED",
    "MATURITY_TOUCH_MODEL",
    "MODEL_LOCKS",
    "MODEL_UNAVAILABLE_CACHE_TTL",
    "MODELS_ALLOW",
    "QUEUE_DEPTH",
    "QUEUE_WAIT",
    "QUOTA_PROBE_ACTIVE_INTERVAL",
    "QUOTA_PROBE_IDLE_HEARTBEAT",
    "RATE_LIMIT_FAILOVER",
    "RATE_LIMIT_PER_IP",
    "REASONING_IN_CONTENT",
    "ROUTING_SMART",
    "SAFE_MODE",
    "SESSION_PERSIST",
    "SESSION_PROBE_CACHE_TTL",
    "TOKEN_MAX_CONCURRENT",
    "TOKEN_ROTATION",
  ]);

  const GROUP_TITLES = {
    general: "General",
    pool: "Pool",
    quota: "Quota",
    upstream: "Upstream",
    security: "Security",
  };
  let env = $derived(parseEnv(rawText));
  // BRIDGE_IDLE_EVICT only makes sense while bridge mode can serve: hidden
  // unless the parent reports bridge as possible (BRIDGE_ENABLED on).
  let rows = $derived(
    (meta ?? []).filter(
      (e) =>
        e &&
        e.key &&
        !e.hidden &&
        !e.secret &&
        !COVERED.has(e.key) &&
        (e.key !== "BRIDGE_IDLE_EVICT" || bridgePossible) &&
        (!onlyGroups || onlyGroups.includes(e.group)),
    ),
  );
  let groups = $derived.by(() => {
    const seen = [];
    for (const e of filtered) {
      if (!seen.includes(e.group)) seen.push(e.group);
    }
    return seen;
  });

  // Key search across every remaining catalog key (key + label + description,
  // case-insensitive substring). Groups with no matches vanish with their
  // rows; an empty query renders exactly as before.
  let q = $derived(query.trim().toLowerCase());
  let filtered = $derived(
    !q
      ? rows
      : rows.filter((e) =>
          `${e.key} ${labelFor(e.key)} ${e.description ?? ""}`
            .toLowerCase()
            .includes(q),
        ),
  );
  $effect(() => {
    onMatchCount?.(filtered.length);
  });

  function labelFor(key) {
    return key
      .toLowerCase()
      .split("_")
      .map((w) => w.charAt(0).toUpperCase() + w.slice(1))
      .join(" ");
  }
  function val(key, entry) {
    const v = formValues[key];
    if (v !== undefined && v !== null && v !== "") return String(v);
    return entry?.default ?? "";
  }
  function boolVal(key, entry) {
    const v = val(key, entry);
    if (v === "") return (entry?.default ?? "true") !== "false";
    return v !== "false";
  }
  // Duration text-kind catalog keys: a key is a Go duration when its
  // documented default parses as one (e.g. 30s, 5m, 168h), its description
  // says so, or its name is temporal (TIMEOUT/TTL/IDLE/...). Catches the
  // "0 = disabled" idle knobs whose default alone is not a duration;
  // non-temporal text (CORS origin, file paths, URLs) stays a plain input.
  const GO_DURATION_RE = /^(\d+(\.\d+)?(ns|us|µs|ms|s|m|h))+$/;
  const TEMPORAL_KEY_RE =
    /(TIMEOUT|TTL|INTERVAL|IDLE|EVICT|RETENTION|WINDOW|HEARTBEAT|EXPIR|WAIT|DELAY|DRAIN|PERIOD|JITTER)/;
  const DURATION_PRESETS = ["5s", "15s", "30s", "1m", "5m", "15m", "1h"];
  function isDurationKey(entry) {
    if (!entry || entry.kind !== "text") return false;
    if (GO_DURATION_RE.test((entry.default ?? "").trim())) return true;
    if (/go duration|duration/i.test(entry.description ?? "")) return true;
    return TEMPORAL_KEY_RE.test(entry.key ?? "");
  }
  // Deep-link focus from cross-page jump links: a link stashes a catalog
  // key in sessionStorage, then routes here.
  let pendingFocusKey = $state("");
  onMount(() => {
    try {
      pendingFocusKey = sessionStorage.getItem("fp-settings-focus") ?? "";
      sessionStorage.removeItem("fp-settings-focus");
    } catch {
      /* storage blocked: no deep focus, page still renders */
    }
  });
  $effect(() => {
    const rowCount = rows.length;
    if (!pendingFocusKey || rowCount === 0) return;
    const key = pendingFocusKey;
    pendingFocusKey = "";
    requestAnimationFrame(() => {
      // Scope to the row's own labeled control: the row also hosts the
      // per-key DbOverrideSave cluster (Reset + status), so a bare
      // "input, button" selector would focus Reset instead of the control.
      const el = document.getElementById(`setting-${key}`);
      const control = el?.querySelector(`[aria-label="${CSS.escape(key)}"]`);
      if (!el || !(control instanceof HTMLElement)) return;
      el.scrollIntoView({ block: "center" });
      control.focus({ preventScroll: true });
    });
  });
</script>

{#if !q || filtered.length > 0}
  <SettingsCard title={$tr(cardTitle)} description={$tr(cardDescription)}>
    {#snippet icon()}
      <SlidersHorizontal size={20} />
    {/snippet}
    {#snippet actions()}
      {#if q}
        <span
          role="status"
          class="text-[11px] font-mono text-[var(--fp-dim)] shrink-0"
          >{$tr("{visible} of {total}", {
            visible: filtered.length,
            total: rows.length,
          })}</span
        >
      {/if}
    {/snippet}

    {#if rows.length === 0}
      <p class="text-xs text-[var(--fp-dim)]">
        {$tr("No advanced keys exposed by the catalog.")}
      </p>
    {:else}
      {#each groups as group, gi (group)}
        {#if gi > 0}
          <p
            class="text-xs font-semibold uppercase tracking-wider text-[var(--fp-muted)] pt-4"
          >
            {GROUP_TITLES[group] ?? group}
          </p>
        {/if}
        {#each filtered.filter((e) => e.group === group) as entry, ei (entry.key)}
          {@const isFirst = gi === 0 && ei === 0}
          <!-- Stable anchor for cross-page jump links (fp-settings-focus). -->
          <div id="setting-{entry.key}" class="scroll-mt-24">
            <SettingsRow
              first={isFirst}
              label={labelFor(entry.key)}
              description={entry.description ?? ""}
            >
              {#snippet badge()}
                <code
                  class="text-[10px] px-1.5 py-0.5 rounded bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-mono"
                  >{entry.key}</code
                >
                {#if !env[entry.key]}
                  <span
                    class="text-[10px] px-1.5 py-0.5 rounded-[var(--fp-radius-sm)] border border-[var(--fp-border)] bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-semibold uppercase tracking-wider shrink-0"
                    >{$tr("default")}</span
                  >
                {/if}
                {#if entry.restart_only}
                  <span
                    class="text-[10px] text-[var(--fp-dim)] lowercase shrink-0"
                    >{$tr("(needs restart)")}</span
                  >
                {/if}
              {/snippet}
              {#snippet extra()}
                <DbOverrideSave
                  settingKey={entry.key}
                  value={val(entry.key, entry)}
                  restartOnly={entry.restart_only}
                  source={sources[entry.key]}
                  {onReset}
                  {onSaved}
                  {degraded}
                />
              {/snippet}
              {#if entry.kind === "bool"}
                <ToggleSwitch
                  checked={boolVal(entry.key, entry)}
                  ariaLabel={entry.key}
                  onchange={(v) => onField(entry.key, v ? "true" : "false")}
                />
              {:else if entry.kind === "select"}
                <select
                  class="fp-select"
                  value={val(entry.key, entry)}
                  aria-label={entry.key}
                  onchange={(e) => onField(entry.key, e.currentTarget.value)}
                >
                  {#each entry.enum ?? [] as opt (opt)}
                    <option value={opt}>{opt}</option>
                  {/each}
                </select>
              {:else if entry.kind === "int"}
                <NumberStepper
                  value={val(entry.key, entry)}
                  ariaLabel={entry.key}
                  placeholder={entry.default ?? ""}
                  oninput={(v) => onField(entry.key, v)}
                />
              {:else if isDurationKey(entry)}
                <DurationPicker
                  value={val(entry.key, entry)}
                  presets={DURATION_PRESETS}
                  ariaLabel={entry.key}
                  placeholder={entry.default ?? ""}
                  oninput={(v) => onField(entry.key, v)}
                />
              {:else}
                <input
                  type="text"
                  class="fp-input fp-mono"
                  value={val(entry.key, entry)}
                  title={val(entry.key, entry)}
                  aria-label={entry.key}
                  placeholder={entry.default ?? ""}
                  oninput={(e) => onField(entry.key, e.currentTarget.value)}
                />
              {/if}
            </SettingsRow>
          </div>
        {/each}
      {/each}
    {/if}
  </SettingsCard>
{/if}
