<script>
  import Button from "../../components/Button.svelte";
  import SettingsCard from "../../components/SettingsCard.svelte";
  import SettingsRow from "../../components/SettingsRow.svelte";
  import DbOverrideSave from "../../components/DbOverrideSave.svelte";
  import StatusBadge from "../../components/StatusBadge.svelte";
  import NumberStepper from "../../components/NumberStepper.svelte";
  import DurationPicker from "../../components/DurationPicker.svelte";
  import { Gauge, Scale } from "@lucide/svelte";
  import { tr } from "../../i18n.js";
  import { parseEnv } from "../../utils/env.js";
  import {
    STRATEGY_MASQ,
    STRATEGY_DRAIN,
    STRATEGY_BALANCE,
    BALANCE_THRESHOLD_MIN_SECS,
    BALANCE_THRESHOLD_MAX_SECS,
    detectStrategy,
    thresholdSecs,
    slotsPerAccount,
    queueWaitSecs,
    queueDepth,
    maxSpillAccounts,
    parsePinEntries,
    distinctPinModels,
    predictNextAccount,
  } from "../../utils/poolStrategy.js";

  /**
   * Pool Strategy preset card (top of Pool → Controls) — the single owner of
   * the queue-posture keys (SLOTS_PER_ACCOUNT, QUEUE_WAIT, QUEUE_DEPTH,
   * MAX_SPILL_ACCOUNTS). PIN_MODEL is owned but never preset-written: pins
   * are per-account routing edited in the token drawer.
   *
   * A preset tap writes ONLY the keys whose value actually differs from the
   * preset (applyPreset below) through onField; each key's row below then
   * instant-saves that one value to the DB overlay (DbOverrideSave), so a
   * tap posts one request per changed key — never a fixed batch.
   *
   * Custom is an auto-detected badge, never selectable, with one-click reset
   * back to either preset. The Balance threshold slider (5–300s) renders only
   * in Balance; it is a second input onto this card's QUEUE_WAIT row (the
   * DurationPicker is the primary editor) and writes the same field, so no
   * key ever gets two writers.
   *
   * "How this runs" derives the live decision chain from the five owned
   * keys (poolStrategy loader mirrors: slotsPerAccount, queueWaitSecs,
   * queueDepth, maxSpillAccounts, parsePinEntries); "Next account" predicts
   * the head of the spill walk from the tokens snapshot
   * (predictNextAccount) and never guesses — no name without lane data.
   *
   * @prop {Record<string, string>} formValues
   * @prop {(key: string, value: string) => void} onField
   * @prop {Record<string, string>} [sources] - ADR-0019 source tiers
   * @prop {(key: string) => Promise<void>} [onReset] - saved-value reset
   * @prop {(() => Promise<void>) | null} [onSaved] - parent refetch after a
   *   per-key save
   * @prop {boolean} [degraded=false] - settings store offline: per-key
   *   overlay saves render an honest offline note
   * @prop {string} [rawText] - .env document, for the rows' "default" chips
   * @prop {number} [tokenCount=0] - pooled accounts (pool-ceiling line)
   * @prop {Array<object>} [tokens=[]] - dashboard tokenCard rows for the
   *   next-account prediction (read-only; absent rows render rule text)
   * @prop {string} [query] - settings key-search text; hides the card on mismatch
   * @prop {(n: number) => void} [onMatchCount] - reports the visible-row count to the parent
   */
  let {
    formValues,
    onField,
    sources = {},
    onReset = null,
    onSaved = null,
    degraded = false,
    rawText = "",
    tokenCount = 0,
    tokens = [],
    query = "",
    onMatchCount = null,
  } = $props();
  const MASQ_LABEL = "MASQ";
  const MASQ_DESC =
    "Ordered Sticky Slot-Packing: 3 slots per account with 60s deferred scale-out and sticky session retention. Maximizes account session reuse.";
  const DRAIN_LABEL = "Drain";
  const DRAIN_DESC =
    "Deep queues: each account serves up to 5 minutes / 1024 parked waiters before the request spills to the next account. Safest for a few accounts.";
  const BALANCE_LABEL = "Balance";
  const BALANCE_DESC =
    "Shallow queues: 16 parked waiters, then spill to the next account after the threshold below. Best for busy pools.";
  const CUSTOM_LABEL = "Custom";
  const CUSTOM_DESC =
    "Hand-edited: at least one of the strategy keys left its preset value. Reset to a preset below to return to one tap.";
  const THRESHOLD_LABEL = "Balance threshold";
  const THRESHOLD_DESC =
    "How long one acquire parks on a full account lane's queue before spilling to the next account (Balance default 15s; unset installs run 30s). Persisted as QUEUE_WAIT — the same row as the Queue Wait editor below.";
  const KEYS_TITLE = "Strategy keys";

  const SLOTS_LABEL = "Slots per Account";
  const SLOTS_DESC =
    "Cap on concurrent live turns per account-model lane (default 3; 2 is the conservative posture and 1 the strictest). Excess waiters park FIFO until Queue Wait elapses.";
  const SLOTS_HINT = "0 = unlimited (no slot gating at all)";
  const SPILL_LABEL = "Max Spill Accounts";
  const SPILL_DESC =
    "How many continuation accounts one request may spill to after its head lane's Queue Wait elapses. A 429 quota requeue never consumes spill budget.";
  const SPILL_HINT = "0 = unbounded (the full index chain)";
  const QWAIT_LABEL = "Queue Wait";
  const QWAIT_DESC =
    "How long one acquire parks on a full lane's FIFO queue before spilling to the next account (Go duration, e.g. 5s, 30s). Empty or non-positive falls back to 30s.";
  const QDEPTH_LABEL = "Queue Depth";
  const QDEPTH_DESC =
    "Cap on parked FIFO waiters per account-model lane. A full queue spills at once with the existing 429 shape.";
  const QDEPTH_HINT = "0 = no queueing (spill at once)";

  let env = $derived(parseEnv(rawText));
  let slotsRaw = $derived(formValues.SLOTS_PER_ACCOUNT ?? "3");
  let spillRaw = $derived(formValues.MAX_SPILL_ACCOUNTS ?? "0");
  let waitRaw = $derived(formValues.QUEUE_WAIT ?? "30s");
  let depthRaw = $derived(formValues.QUEUE_DEPTH ?? "16");

  let strategy = $derived(
    detectStrategy({
      SLOTS_PER_ACCOUNT: formValues.SLOTS_PER_ACCOUNT,
      QUEUE_WAIT: formValues.QUEUE_WAIT,
      QUEUE_DEPTH: formValues.QUEUE_DEPTH,
      PIN_MODEL: formValues.PIN_MODEL,
      MAX_SPILL_ACCOUNTS: formValues.MAX_SPILL_ACCOUNTS,
    }),
  );
  let sliderSecs = $derived(thresholdSecs(formValues.QUEUE_WAIT));

  // Queue rows dim (never disable) while the per-account cap is unlimited
  // (0 = no slot gating) — nothing can park then, so the rows would do
  // nothing. The cap is the SLOTS_PER_ACCOUNT row below.
  let queueParked = $derived(Number(slotsRaw) === 0);

  // Honest pool ceiling: the per-account cap the gateway runs with × the
  // pooled accounts from the tokens snapshot. 0 = no slot gating at all.
  let ceiling = $derived.by(() => {
    const cap = Number(String(slotsRaw).trim());
    const accounts = Math.max(0, Number(tokenCount) || 0);
    if (!Number.isFinite(cap) || cap <= 0) {
      return $tr("unlimited per account ({accounts} accounts)", { accounts });
    }
    return $tr(
      "{cap} per account × {accounts} accounts = {total} concurrent turns",
      { cap, accounts, total: cap * accounts },
    );
  });
  const HOW_TITLE = "How this runs";
  const NEXT_TITLE = "Next account";

  // Live decision chain ("How this runs") + head-of-walk prediction ("Next
  // account"), both derived from the five owned keys through the same
  // loader mirrors the badge classifies with — the block always names the
  // numbers the gateway actually runs with, never the preset file text.
  let slotsCap = $derived(slotsPerAccount(formValues.SLOTS_PER_ACCOUNT));
  let waitSecs = $derived(queueWaitSecs(formValues.QUEUE_WAIT));
  let depthCap = $derived(queueDepth(formValues.QUEUE_DEPTH));
  let spillCap = $derived(maxSpillAccounts(formValues.MAX_SPILL_ACCOUNTS));
  let pinEntries = $derived(parsePinEntries(formValues.PIN_MODEL ?? ""));
  // The model the prediction runs for: the single pinned model when the
  // map is unanimous, "" for any-model when nothing is pinned, null when
  // pins disagree — ambiguous, so the prediction names no lane (never
  // guess which model the next request carries).
  let selectedModel = $derived.by(() => {
    if (Object.keys(pinEntries).length === 0) return "";
    const distinct = distinctPinModels(formValues.PIN_MODEL ?? "");
    return distinct.length === 1 ? distinct[0] : null;
  });
  let nextPick = $derived(predictNextAccount(tokens, selectedModel));
  let hasLaneRows = $derived(
    Array.isArray(tokens) &&
      tokens.some(
        (t) => t !== null && typeof t === "object" && Number.isFinite(t.index),
      ),
  );
  let presetName = $derived(
    strategy === "masq"
      ? "MASQ"
      : strategy === "drain"
        ? "Drain"
        : strategy === "balance"
          ? "Balance"
          : "Custom",
  );
  // One-line posture with the ACTIVE preset's numbers, e.g. Balance:
  // "parks 15s on a full lane (16 waiters), then spills".
  let postureLine = $derived.by(() => {
    const slots =
      slotsCap === 0
        ? $tr("unlimited slots per lane")
        : $tr("{n} slots per lane", { n: slotsCap });
    const park =
      depthCap === 0
        ? $tr("spills at once on a full lane (no queueing)")
        : $tr("parks {wait}s on a full lane ({depth} waiters), then spills", {
            wait: waitSecs,
            depth: depthCap,
          });
    const tail =
      spillCap === 0
        ? $tr("down the full index chain")
        : $tr("across up to {n} more account(s)", { n: spillCap });
    return $tr("{label} runs {slots} — {park} {tail}.", {
      label: presetName,
      slots,
      park,
      tail,
    });
  });
  // The five walk steps: index order → slot take → park → spill → tail,
  // plus the pin rule. Each names live numbers.
  let howSteps = $derived.by(() => {
    const accounts = Math.max(0, Number(tokenCount) || 0);
    const walk =
      accounts > 0
        ? $tr("Walk accounts #1…#{n} in roster index order.", { n: accounts })
        : $tr("Walk accounts in roster index order.");
    const take =
      slotsCap === 0
        ? $tr("No slot cap — every lane takes the turn at once.")
        : $tr(
            "Take a free slot — up to {n} live turn(s) per account-model lane.",
            {
              n: slotsCap,
            },
          );
    const park =
      depthCap === 0
        ? $tr("A full lane spills at once (no queueing).")
        : $tr(
            "A full lane parks up to {wait}s ({depth} waiters), then spills.",
            {
              wait: waitSecs,
              depth: depthCap,
            },
          );
    const spill =
      spillCap === 0
        ? $tr("Spill down the full index chain (unbounded).")
        : $tr("Spill across up to {n} more account(s).", { n: spillCap });
    const tail = $tr(
      "Tail: the end of the chain surfaces the existing 429 shape — never a new error code. A 429 quota requeue never consumes spill budget.",
    );
    const slots = Object.keys(pinEntries)
      .map(Number)
      .sort((a, b) => a - b);
    const pins =
      slots.length === 0
        ? $tr("No pins — every lane serves any model.")
        : $tr("Pins skip non-matching lanes: {list}.", {
            list: slots.map((s) => `#${s + 1} → ${pinEntries[s]}`).join(", "),
          });
    return [walk, take, park, spill, tail, pins];
  });
  // The next-account line: a named lane when exactly one can be named,
  // else the honest rule text without a name.
  let nextLine = $derived.by(() => {
    if (nextPick) {
      const warmth = nextPick.warm
        ? $tr("warm, live session")
        : $tr("cold, needs admission");
      const pin = nextPick.pinnedModel
        ? $tr(" · pinned to {model}", { model: nextPick.pinnedModel })
        : "";
      const scope =
        selectedModel !== ""
          ? $tr(" — head of the lane for {model}", { model: selectedModel })
          : "";
      const who =
        nextPick.email !== ""
          ? $tr("Account #{n} ({email})", {
              n: nextPick.index + 1,
              email: nextPick.email,
            })
          : $tr("Account #{n}", { n: nextPick.index + 1 });
      return `${who} — ${warmth}${pin}${scope}.`;
    }
    if (selectedModel === null) {
      return $tr(
        "Pins route per model — the next lane depends on the requested model, so no single lane is named.",
      );
    }
    if (!hasLaneRows) {
      return $tr(
        "No lane data yet — the prediction needs the tokens snapshot.",
      );
    }
    return $tr(
      "Every lane is parked (locked, banned, cooling or pinned away) — no lane can take the next turn.",
    );
  });

  function applyPreset(preset) {
    // A preset switch writes ONLY its five owned keys — every other knob
    // keeps its value. When the values already match, skip so no write
    // fires without a change.
    const same = Object.entries(preset).every(
      ([k, v]) => String(formValues[k] ?? "") === v,
    );
    if (same) return;
    for (const [k, v] of Object.entries(preset)) onField(k, v);
  }

  let q = $derived(query.trim().toLowerCase());
  function hit(...parts) {
    if (!q) return true;
    return parts.join("\n").toLowerCase().includes(q);
  }
  let visible = $derived(
    hit(
      "SLOTS_PER_ACCOUNT",
      "QUEUE_WAIT",
      "QUEUE_DEPTH",
      "MAX_SPILL_ACCOUNTS",
      "Pool Strategy",
      MASQ_LABEL,
      MASQ_DESC,
      DRAIN_LABEL,
      DRAIN_DESC,
      BALANCE_LABEL,
      BALANCE_DESC,
      THRESHOLD_LABEL,
      THRESHOLD_DESC,
      KEYS_TITLE,
      SLOTS_LABEL,
      SLOTS_DESC,
      SLOTS_HINT,
      SPILL_LABEL,
      SPILL_DESC,
      SPILL_HINT,
      QWAIT_LABEL,
      QWAIT_DESC,
      QDEPTH_LABEL,
      QDEPTH_DESC,
      QDEPTH_HINT,
      HOW_TITLE,
      NEXT_TITLE,
      "decision chain",
      "next lane",
      "spill",
      "parks",
    )
      ? 1
      : 0,
  );
  $effect(() => {
    onMatchCount?.(visible);
  });
</script>

{#if !q || visible > 0}
  <SettingsCard
    title={$tr("Pool Strategy")}
    description={$tr(
      "One tap picks the pool's queue posture. Anything hand-edited reads as Custom with a one-click reset.",
    )}
  >
    {#snippet icon()}
      <Scale size={20} />
    {/snippet}
    {#snippet actions()}
      {#if strategy === "custom"}
        <StatusBadge tone="warn" status={$tr("Custom")} />
      {:else if strategy === "masq"}
        <StatusBadge tone="good" status={$tr("MASQ")} />
      {:else if strategy === "drain"}
        <StatusBadge tone="info" status={$tr("Drain")} />
      {:else}
        <StatusBadge tone="info" status={$tr("Balance")} />
      {/if}
      {#if q}
        <span
          role="status"
          class="text-[11px] font-mono text-[var(--fp-dim)] shrink-0"
          >{$tr("{visible} of {total}", { visible, total: 1 })}</span
        >
      {/if}
    {/snippet}

    <div
      class="flex flex-wrap items-center gap-2 py-4"
      role="radiogroup"
      aria-label={$tr("Pool strategy")}
    >
      <Button
        variant={strategy === "masq" ? "primary" : "ghost"}
        size="sm"
        role="radio"
        aria-checked={strategy === "masq"}
        onclick={() => applyPreset(STRATEGY_MASQ)}
      >
        {$tr(MASQ_LABEL)}
      </Button>
      <Button
        variant={strategy === "drain" ? "primary" : "ghost"}
        size="sm"
        role="radio"
        aria-checked={strategy === "drain"}
        onclick={() => applyPreset(STRATEGY_DRAIN)}
      >
        {$tr(DRAIN_LABEL)}
      </Button>
      <Button
        variant={strategy === "balance" ? "primary" : "ghost"}
        size="sm"
        role="radio"
        aria-checked={strategy === "balance"}
        onclick={() => applyPreset(STRATEGY_BALANCE)}
      >
        {$tr(BALANCE_LABEL)}
      </Button>
      {#if strategy === "custom"}
        <span class="text-[11px] text-[var(--fp-dim)]">{$tr(CUSTOM_LABEL)}</span
        >
      {/if}
    </div>

    <div
      class="fp-inset p-3 rounded text-xs text-[var(--fp-muted)] flex items-start gap-2"
    >
      {#if strategy === "masq"}
        <p class="leading-relaxed">
          <strong class="text-[var(--fp-text)]">{$tr("MASQ:")}</strong>
          {$tr(MASQ_DESC)}
        </p>
      {:else if strategy === "drain"}
        <p class="leading-relaxed">
          <strong class="text-[var(--fp-text)]">{$tr("Drain:")}</strong>
          {$tr(DRAIN_DESC)}
        </p>
      {:else if strategy === "balance"}
        <p class="leading-relaxed">
          <strong class="text-[var(--fp-text)]">{$tr("Balance:")}</strong>
          {$tr(BALANCE_DESC)}
        </p>
      {:else}
        <p class="leading-relaxed">
          <strong class="text-[var(--fp-text)]">{$tr("Custom:")}</strong>
          {$tr(CUSTOM_DESC)}
        </p>
      {/if}
    </div>
    <div class="pt-3 mt-1" data-testid="strategy-how-runs">
      <p
        class="text-xs font-semibold uppercase tracking-wider text-[var(--fp-muted)] pb-1 px-1"
      >
        {$tr(HOW_TITLE)}
      </p>
      <div
        class="fp-inset p-3 rounded text-xs text-[var(--fp-muted)] space-y-2"
      >
        <p class="leading-relaxed text-[var(--fp-text)]">{postureLine}</p>
        <ol class="list-decimal ml-4 space-y-1 leading-relaxed">
          {#each howSteps as step, i (i)}
            <li>{step}</li>
          {/each}
        </ol>
      </div>
    </div>

    <div
      class="flex items-center gap-2 pt-3 px-1 text-[11px] text-[var(--fp-muted)]"
      data-testid="strategy-next-account"
    >
      <span class="uppercase tracking-wider text-[var(--fp-dim)]"
        >{$tr(NEXT_TITLE)}</span
      >
      <span class="text-[var(--fp-text)]">{nextLine}</span>
    </div>

    <div
      class="flex items-center gap-2 pt-3 px-1 text-[11px] text-[var(--fp-muted)]"
      data-testid="pool-ceiling"
    >
      <Gauge size={13} class="shrink-0 text-[var(--fp-dim)]" />
      <span class="uppercase tracking-wider text-[var(--fp-dim)]"
        >{$tr("Pool ceiling")}</span
      >
      <span class="fp-num text-[var(--fp-text)]">{ceiling}</span>
    </div>

    {#if strategy === "balance"}
      <div class="pt-3 mt-1 border-t border-[var(--fp-border)]">
        <div
          class="flex flex-col sm:flex-row sm:items-center justify-between gap-3 py-2"
        >
          <div class="space-y-0.5">
            <span class="text-xs font-semibold text-[var(--fp-text)]">
              {$tr(THRESHOLD_LABEL)}
            </span>
            <p class="text-[11px] text-[var(--fp-muted)] leading-relaxed">
              {$tr(THRESHOLD_DESC)}
            </p>
          </div>
          <div class="flex items-center gap-2.5 w-full sm:w-64">
            <input
              type="range"
              min={BALANCE_THRESHOLD_MIN_SECS}
              max={BALANCE_THRESHOLD_MAX_SECS}
              step={5}
              value={sliderSecs}
              aria-label={$tr("Balance threshold (QUEUE_WAIT)")}
              class="flex-1 accent-[var(--fp-accent)]"
              oninput={(e) =>
                onField("QUEUE_WAIT", `${e.currentTarget.value}s`)}
            />
            <span
              class="fp-num text-xs text-[var(--fp-text)] w-12 text-right tabular-nums"
              >{sliderSecs}s</span
            >
          </div>
        </div>
      </div>
    {/if}

    {#if strategy === "custom"}
      <div class="flex flex-wrap items-center gap-2 pt-3">
        <Button
          variant="secondary"
          size="sm"
          onclick={() => applyPreset(STRATEGY_MASQ)}
        >
          {$tr("Reset to MASQ")}
        </Button>
        <Button
          variant="secondary"
          size="sm"
          onclick={() => applyPreset(STRATEGY_DRAIN)}
        >
          {$tr("Reset to Drain")}
        </Button>
        <Button
          variant="secondary"
          size="sm"
          onclick={() => applyPreset(STRATEGY_BALANCE)}
        >
          {$tr("Reset to Balance")}
        </Button>
      </div>
    {/if}

    <!-- The five owned keys, each with its single editor + instant save. -->
    <div
      class="pt-4 mt-1 border-t border-[var(--fp-border)]"
      data-testid="strategy-rows"
    >
      <p
        class="text-xs font-semibold uppercase tracking-wider text-[var(--fp-muted)] pb-1"
      >
        {$tr(KEYS_TITLE)}
      </p>

      <div class="space-y-3 py-4">
        <SettingsRow label={$tr(SLOTS_LABEL)} description={$tr(SLOTS_DESC)}>
          {#snippet badge()}
            <code
              class="text-[10px] px-1.5 py-0.5 rounded bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-mono"
              >SLOTS_PER_ACCOUNT</code
            >
            {#if !env.SLOTS_PER_ACCOUNT}
              <span
                class="text-[10px] px-1.5 py-0.5 rounded-[var(--fp-radius-sm)] border border-[var(--fp-border)] bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-semibold uppercase tracking-wider shrink-0"
                >{$tr("default")}</span
              >
            {/if}
          {/snippet}
          {#snippet extra()}
            <DbOverrideSave
              settingKey="SLOTS_PER_ACCOUNT"
              value={slotsRaw}
              source={sources.SLOTS_PER_ACCOUNT}
              {onReset}
              {onSaved}
              {degraded}
            />
          {/snippet}

          <div class="w-full sm:w-56">
            <NumberStepper
              value={slotsRaw}
              min={0}
              step={1}
              ariaLabel="SLOTS_PER_ACCOUNT"
              placeholder="3"
              oninput={(v) => {
                const val = v.trim();
                onField("SLOTS_PER_ACCOUNT", val === "" ? "3" : val);
              }}
            />
            <p class="text-[10px] text-[var(--fp-dim)] mt-1">
              {$tr(SLOTS_HINT)}
            </p>
          </div>
        </SettingsRow>

        <SettingsRow label={$tr(SPILL_LABEL)} description={$tr(SPILL_DESC)}>
          {#snippet badge()}
            <code
              class="text-[10px] px-1.5 py-0.5 rounded bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-mono"
              >MAX_SPILL_ACCOUNTS</code
            >
            {#if !env.MAX_SPILL_ACCOUNTS}
              <span
                class="text-[10px] px-1.5 py-0.5 rounded-[var(--fp-radius-sm)] border border-[var(--fp-border)] bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-semibold uppercase tracking-wider shrink-0"
                >{$tr("default")}</span
              >
            {/if}
          {/snippet}
          {#snippet extra()}
            <DbOverrideSave
              settingKey="MAX_SPILL_ACCOUNTS"
              value={spillRaw}
              source={sources.MAX_SPILL_ACCOUNTS}
              {onReset}
              {onSaved}
              {degraded}
            />
          {/snippet}

          <div class="w-full sm:w-56">
            <NumberStepper
              value={spillRaw}
              min={0}
              step={1}
              ariaLabel="MAX_SPILL_ACCOUNTS"
              placeholder="0"
              oninput={(v) => {
                const val = v.trim();
                onField("MAX_SPILL_ACCOUNTS", val === "" ? "0" : val);
              }}
            />
            <p class="text-[10px] text-[var(--fp-dim)] mt-1">
              {$tr(SPILL_HINT)}
            </p>
          </div>
        </SettingsRow>
      </div>

      {#if queueParked}
        <p class="text-[11px] text-[var(--fp-dim)] leading-relaxed pb-1">
          {$tr(
            "Parked: the per-account cap is unlimited (0) — nothing parks until it changes (SLOTS_PER_ACCOUNT above).",
          )}
        </p>
      {/if}

      <SettingsRow
        class={queueParked ? "opacity-60" : ""}
        label={$tr(QWAIT_LABEL)}
        description={$tr(QWAIT_DESC)}
      >
        {#snippet badge()}
          <code
            class="text-[10px] px-1.5 py-0.5 rounded bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-mono"
            >QUEUE_WAIT</code
          >
          {#if !env.QUEUE_WAIT}
            <span
              class="text-[10px] px-1.5 py-0.5 rounded-[var(--fp-radius-sm)] border border-[var(--fp-border)] bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-semibold uppercase tracking-wider shrink-0"
              >{$tr("default")}</span
            >
          {/if}
        {/snippet}
        {#snippet extra()}
          <DbOverrideSave
            settingKey="QUEUE_WAIT"
            value={waitRaw}
            source={sources.QUEUE_WAIT}
            {onReset}
            {onSaved}
            {degraded}
          />
        {/snippet}

        <div class="w-full sm:w-56">
          <DurationPicker
            value={waitRaw}
            presets={["5s", "15s", "30s", "1m", "5m"]}
            ariaLabel="QUEUE_WAIT"
            placeholder="30s"
            oninput={(v) => onField("QUEUE_WAIT", v)}
          />
        </div>
      </SettingsRow>

      <SettingsRow
        class={queueParked ? "opacity-60" : ""}
        label={$tr(QDEPTH_LABEL)}
        description={$tr(QDEPTH_DESC)}
      >
        {#snippet badge()}
          <code
            class="text-[10px] px-1.5 py-0.5 rounded bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-mono"
            >QUEUE_DEPTH</code
          >
          {#if !env.QUEUE_DEPTH}
            <span
              class="text-[10px] px-1.5 py-0.5 rounded-[var(--fp-radius-sm)] border border-[var(--fp-border)] bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-semibold uppercase tracking-wider shrink-0"
              >{$tr("default")}</span
            >
          {/if}
        {/snippet}
        {#snippet extra()}
          <DbOverrideSave
            settingKey="QUEUE_DEPTH"
            value={depthRaw}
            source={sources.QUEUE_DEPTH}
            {onReset}
            {onSaved}
            {degraded}
          />
        {/snippet}

        <div class="w-full sm:w-56">
          <NumberStepper
            value={depthRaw}
            min={0}
            step={1}
            ariaLabel="QUEUE_DEPTH"
            placeholder="16"
            oninput={(v) => {
              const val = v.trim();
              onField("QUEUE_DEPTH", val === "" ? "16" : val);
            }}
          />
          <p class="text-[10px] text-[var(--fp-dim)] mt-1">
            {$tr(QDEPTH_HINT)}
          </p>
        </div>
      </SettingsRow>
    </div>
  </SettingsCard>
{/if}
