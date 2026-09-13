<script>
  import { Minus, Plus } from "@lucide/svelte";

  /**
   * NumberStepper — string-valued numeric stepper for settings rows.
   * Minus/plus buttons around a number input; composes fp-btn/fp-input
   * like Stepper.svelte. The value stays a string so callers can keep
   * passing it straight into the existing onField(key, value) flow.
   * Clearing the input emits "" (the caller applies its own fallback);
   * minus clamps at min.
   *
   * @prop {string} value
   * @prop {number} [min=0]
   * @prop {number} [step=1]
   * @prop {string} ariaLabel - the setting KEY, kept on the input element
   * @prop {string} [placeholder=""]
   * @prop {(v: string) => void} oninput
   */
  let {
    value,
    min = 0,
    step = 1,
    ariaLabel,
    placeholder = "",
    oninput,
  } = $props();

  function nudge(delta) {
    const cur = value.trim() === "" ? 0 : Number(value);
    const next = (Number.isFinite(cur) ? cur : 0) + delta * step;
    oninput(String(Math.max(min, next)));
  }
</script>

<div class="flex items-center gap-1.5 w-full">
  <button
    type="button"
    class="fp-btn fp-btn-secondary fp-btn-sm !px-2 shrink-0"
    aria-label="Decrease {ariaLabel}"
    onclick={() => nudge(-1)}
  >
    <Minus size={13} />
  </button>
  <input
    type="number"
    {min}
    {step}
    {placeholder}
    class="fp-input flex-1 min-w-12 !text-xs !py-1.5 text-center"
    aria-label={ariaLabel}
    {value}
    oninput={(e) => oninput(e.currentTarget.value)}
  />
  <button
    type="button"
    class="fp-btn fp-btn-secondary fp-btn-sm !px-2 shrink-0"
    aria-label="Increase {ariaLabel}"
    onclick={() => nudge(1)}
  >
    <Plus size={13} />
  </button>
</div>
