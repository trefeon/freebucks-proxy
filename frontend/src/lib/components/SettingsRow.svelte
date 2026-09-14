<script>
  /**
   * SettingsRow — Standard row template inside a settings card section.
   *
   * Formats a single setting row with:
   * - Title / label with optional status or restart-only badges
   * - Descriptive explanation
   * - Responsive alignment: mobile stacked, desktop inline (center or top aligned)
   * - Right-hand action cluster: interactive control (switch, select, input)
   *   on top with the extra slot (per-key Save) right-aligned beneath it
   *
   * @prop {string} [label=""]
   * @prop {string} [description=""]
   * @prop {'center'|'start'} [align='center'] — desktop vertical alignment of label vs control
   * @prop {boolean} [first=false] — removes top padding on first row
   * @prop {boolean} [last=false] — removes bottom padding on last row
   * @prop {string} [class=""]
   * @slot badge — badges or chips rendered next to the label
   * @slot default — interactive control rendered on top of the action cluster
   * @slot extra — per-key save control rendered right-aligned below the control
   */
  let {
    label = "",
    description = "",
    align = "center",
    first = false,
    last = false,
    class: className = "",
    badge,
    children,
    extra,
  } = $props();
</script>

<div
  class="py-4 {first ? 'first:pt-0' : ''} {last
    ? 'last:pb-0'
    : ''} flex flex-col {align === 'start'
    ? 'md:flex-row md:items-start'
    : 'sm:flex-row sm:items-center'} justify-between gap-4 {className}"
>
  <div class="flex-1 min-w-0">
    <div class="flex flex-wrap items-center gap-2">
      {#if label}
        <span class="font-medium text-sm sm:text-base text-[var(--fp-text)]">
          {label}
        </span>
      {/if}
      {#if badge}
        {@render badge()}
      {/if}
    </div>
    {#if description}
      <p class="text-xs sm:text-sm text-text-muted mt-1 leading-relaxed">
        {description}
      </p>
    {/if}
  </div>
  <div
    class="w-full sm:w-auto sm:shrink-0 min-w-0 flex flex-col items-end gap-2"
  >
    {#if children}
      <div class="w-full sm:w-auto flex items-center justify-end gap-3">
        {@render children()}
      </div>
    {/if}
    {#if extra}
      <div class="flex justify-end">
        {@render extra()}
      </div>
    {/if}
  </div>
</div>
