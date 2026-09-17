<script>
  import Button from "./Button.svelte";
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
      <Button
        variant={value === p ? "primary" : "secondary"}
        size="sm"
        class="shrink-0"
        aria-pressed={value === p}
        onclick={() => oninput(p)}
      >
        {p}
      </Button>
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
