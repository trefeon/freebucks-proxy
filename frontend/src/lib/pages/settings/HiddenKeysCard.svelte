<script>
  import { ChevronRight, KeyRound } from "@lucide/svelte";
  import SettingsRow from "../../components/SettingsRow.svelte";
  import DbOverrideSave from "../../components/DbOverrideSave.svelte";
  import { tr } from "../../i18n.js";

  /**
   * Hidden keys — a collapsed, read-only listing of the catalog keys no card
   * on the page owns.
   *
   * The catalog marks some keys `hidden`: environment-only, restart-only, or
   * deprecated knobs with no honest dashboard control (LISTEN_ADDR, LOG_FILE,
   * COST_MODE, TLS_FINGERPRINT, the drain/run-finish bounds...). They are
   * still operator surface, so they are listed here with their effective
   * value and ADR-0019 source tier instead of being unreachable: a hidden key
   * must never become a stranded one. Secret keys are excluded outright — no
   * credential value reaches this card.
   *
   * Read-only by construction: there is no control to edit, so the only write
   * path is DbOverrideSave's Reset on a saved (`db`) row — the same
   * instant-save cluster every other row uses, never a second writer. Source
   * tiers `db`/`env` carry their own note from that cluster ("saved value" /
   * "overridden by process env"); this card adds the `file`/`default` chips
   * it does not render.
   *
   * @prop {Array} meta - config catalog entries (/admin/api/config/meta)
   * @prop {Record<string, string>} formValues
   * @prop {Record<string, string>} [sources] - ADR-0019 source tiers
   * @prop {(key: string) => Promise<void>} [onReset] - saved-value reset
   * @prop {(() => Promise<void>) | null} [onSaved] - parent refetch
   * @prop {string} [query] - settings key-search text; filters the rows and
   *   force-opens the disclosure so a hit is never hidden
   * @prop {(n: number) => void} [onMatchCount] - reports the visible-row
   *   count to the parent global empty state
   * @prop {boolean} [degraded=false] - settings store offline: Reset is
   *   unavailable and the cluster says so
   */
  let {
    meta = [],
    formValues = {},
    sources = {},
    onReset = null,
    onSaved = null,
    query = "",
    onMatchCount = null,
    degraded = false,
  } = $props();

  // Keys that DO have a dedicated editor elsewhere in the dashboard: the
  // Gateway card owns HTTP_READ_TIMEOUT and the Pool Custom advanced card
  // owns the two session keys. Listing them here would duplicate a control
  // that already works.
  const OWNED_ELSEWHERE = new Set([
    "HTTP_READ_TIMEOUT",
    "SESSION_RE_ADMIT_LEAD",
    "WAITING_ROOM_CHAIN",
  ]);

  const GROUP_TITLES = {
    general: "General",
    pool: "Pool",
    quota: "Quota",
    upstream: "Upstream",
    security: "Security",
  };

  // Default = surface it. A new hidden catalog key lands here unless someone
  // explicitly records that another card owns it, so a key cannot silently
  // go missing again.
  let rows = $derived(
    (meta ?? []).filter(
      (e) => e && e.key && e.hidden && !e.secret && !OWNED_ELSEWHERE.has(e.key),
    ),
  );

  function labelFor(key) {
    return key
      .toLowerCase()
      .split("_")
      .map((w) => w.charAt(0).toUpperCase() + w.slice(1))
      .join(" ");
  }

  // The effective value, like every other card: the derived display value
  // first (file, then overlay, then remote effective), the catalog default
  // last. These keys are usually absent from both, so the default is the
  // common answer.
  function val(entry) {
    const v = formValues[entry.key];
    if (v !== undefined && v !== null && v !== "") return String(v);
    return entry?.default ?? "";
  }

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
  let groups = $derived.by(() => {
    const seen = [];
    for (const e of filtered) {
      if (!seen.includes(e.group)) seen.push(e.group);
    }
    return seen;
  });
  $effect(() => {
    onMatchCount?.(filtered.length);
  });
</script>

{#if !q || filtered.length > 0}
  <details
    class="group bg-surface border border-border-subtle rounded-[var(--fp-radius)] page-enter"
    data-testid="hidden-keys"
    open={q ? true : undefined}
  >
    <summary
      data-testid="hidden-keys-toggle"
      class="flex items-center gap-3 p-6 cursor-pointer select-none rounded-[var(--fp-radius)] focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[var(--fp-accent)]"
    >
      <div class="p-2 rounded bg-primary/10 text-primary shrink-0">
        <KeyRound size={20} />
      </div>
      <div class="min-w-0 flex-1">
        <h2 class="text-base sm:text-lg font-semibold text-[var(--fp-text)]">
          {$tr("Hidden keys")}
        </h2>
        <p class="text-xs sm:text-sm text-text-muted mt-0.5">
          {$tr(
            "Keys no card above owns: environment-only, restart-only, or deprecated. Set them in the host .env file or the compose environment — the process environment always wins over the file, so a change applies after a restart. A saved value can be reset here.",
          )}
        </p>
      </div>
      <span
        data-testid="hidden-keys-count"
        class="text-[11px] font-mono text-[var(--fp-dim)] shrink-0"
        >{q
          ? $tr("{visible} of {total}", {
              visible: filtered.length,
              total: rows.length,
            })
          : rows.length}</span
      >
      <ChevronRight
        size={16}
        class="shrink-0 text-[var(--fp-dim)] transition-transform group-open:rotate-90"
      />
    </summary>

    <div class="px-6 pb-6 flex flex-col divide-y divide-border-subtle/50">
      {#each groups as group, gi (group)}
        {#if gi > 0}
          <p
            class="text-xs font-semibold uppercase tracking-wider text-[var(--fp-muted)] pt-4"
          >
            {GROUP_TITLES[group] ?? group}
          </p>
        {/if}
        {#each filtered.filter((e) => e.group === group) as entry (entry.key)}
          {@const tier = sources[entry.key] ?? "default"}
          <div data-setting-key={entry.key}>
            <SettingsRow
              label={labelFor(entry.key)}
              description={entry.description ?? ""}
            >
              {#snippet badge()}
                <code
                  class="text-[10px] px-1.5 py-0.5 rounded bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-mono"
                  >{entry.key}</code
                >
                {#if tier === "file"}
                  <span
                    class="text-[10px] px-1.5 py-0.5 rounded-[var(--fp-radius-sm)] border border-[var(--fp-border)] bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-semibold uppercase tracking-wider shrink-0"
                    title={$tr(
                      "Set in the .env (or -config JSON) file. The process environment still wins while it holds the key.",
                    )}>{$tr("from file")}</span
                  >
                {:else if tier !== "db" && tier !== "env"}
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
                  value={val(entry)}
                  restartOnly={entry.restart_only}
                  source={sources[entry.key]}
                  {onReset}
                  {onSaved}
                  {degraded}
                />
              {/snippet}
              {#if val(entry) === ""}
                <span class="text-xs text-[var(--fp-dim)]"
                  >{$tr("(empty)")}</span
                >
              {:else}
                <code
                  class="fp-mono text-xs text-[var(--fp-text)] break-all text-right select-all"
                  title={val(entry)}>{val(entry)}</code
                >
              {/if}
            </SettingsRow>
          </div>
        {/each}
      {/each}
    </div>
  </details>
{/if}
