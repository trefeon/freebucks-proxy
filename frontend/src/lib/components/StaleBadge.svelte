<script>
  /**
   * StaleBadge — Phase 4 R3 cold-render marker. Rendered next to live status
   * while the page shows a seeded snapshot that no poll has confirmed yet;
   * the query store drops `stale` (and the page unmounts this) on the first
   * confirmed pass.
   *
   * @prop {number} [savedAt=0] - cache timestamp for the title tooltip
   */
  let { savedAt = 0 } = $props();

  let title = $derived(
    savedAt > 0
      ? `Cached snapshot from ${new Date(savedAt).toLocaleString()} — refreshing…`
      : "Cached snapshot — refreshing…",
  );
</script>

<span
  data-testid="stale-badge"
  {title}
  class="inline-flex items-center gap-1 px-1.5 py-0.5 rounded border font-mono text-[11px] uppercase tracking-wider text-[var(--fp-dim)] border-[var(--fp-border)] bg-[var(--fp-surface)]"
>
  <span aria-hidden="true" class="shrink-0">[~]</span>
  <span>stale</span>
</span>
