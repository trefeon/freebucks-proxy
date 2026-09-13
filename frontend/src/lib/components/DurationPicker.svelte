<script>
  /**
   * DurationPicker — preset buttons plus a free-text input for Go
   * duration settings. Each preset button writes its value straight
   * through oninput; the text input keeps aria-label={ariaLabel} so
   * e2e selectors survive. The active preset (exact match) highlights.
   *
   * @prop {string} value
   * @prop {string[]} presets
   * @prop {string} ariaLabel - the setting KEY, kept on the input element
   * @prop {string} [placeholder=""]
   * @prop {(v: string) => void} oninput
   */
  let { value, presets, ariaLabel, placeholder = "", oninput } = $props();
</script>

<div class="space-y-1.5 w-full">
  <div class="flex flex-wrap gap-1.5">
    {#each presets as p (p)}
      <button
        type="button"
        class="fp-btn fp-btn-sm shrink-0 {value === p
          ? 'fp-btn-primary'
          : 'fp-btn-secondary'}"
        aria-pressed={value === p}
        onclick={() => oninput(p)}
      >
        {p}
      </button>
    {/each}
  </div>
  <input
    type="text"
    {placeholder}
    class="fp-input w-full !text-xs !py-1.5 font-mono"
    aria-label={ariaLabel}
    {value}
    oninput={(e) => oninput(e.currentTarget.value)}
  />
</div>
