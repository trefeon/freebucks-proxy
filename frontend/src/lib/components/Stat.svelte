<script>
  /**
   * Stat — instrument readout: mono value, LED-labeled caption.
   *
   * @prop {string} label
   * @prop {string|number} value
   * @prop {string} [hint]
   * @prop {'default'|'good'|'warn'|'bad'} [tone='default']
   * @prop {boolean} [big=false]
   * @prop {boolean} [showLed] — whether to show the status LED. Defaults to
   *   true when tone conveys state ('good'|'warn'|'bad'), false when 'default'
   *   to obey DESIGN.md Rule 3 (LEDs are indicators, not bullets).
   */
  let {
    label,
    value,
    hint,
    tone = "default",
    big = false,
    showLed = tone !== "default",
  } = $props();
  const ledTones = {
    default: "led-idle",
    good: "led-good",
    warn: "led-warn",
    bad: "led-bad",
  };

  const valueTones = {
    default: "text-[var(--fp-text)]",
    good: "text-[var(--fp-success)]",
    warn: "text-[var(--fp-warning)]",
    bad: "text-[var(--fp-error)]",
  };
</script>

<div class="flex flex-col gap-1">
  <div class="flex items-center gap-1.5">
    {#if showLed}
      <span class="led {ledTones[tone]}" aria-hidden="true"></span>
    {/if}
    <span class="text-xs text-[var(--fp-muted)]">{label}</span>
  </div>
  <span
    class="fp-num font-semibold leading-tight {big
      ? 'text-[26px]'
      : 'text-[20px]'} {valueTones[tone]}"
  >
    {value}
  </span>
  {#if hint}
    <span class="text-[11px] text-[var(--fp-dim)]">{hint}</span>
  {/if}
</div>
