<script>
  import {
    Zap,
    RefreshCw,
    Check,
    ExternalLink,
    Plus,
    Lock,
  } from "@lucide/svelte";
  import Button from "./Button.svelte";
  import RefundLines from "./RefundLines.svelte";
  import {
    fallbackModelOptions,
    fetchModelOptions,
    cheapestFreeOption,
  } from "../modelOptions.js";
  import { fetchAPI, postAPI } from "../api/client.js";
  import { adminApi } from "../api/paths.js";
  import { refreshTokens } from "../stores/tokens.js";
  import { tr } from "../i18n.js";
  import { isExhausted } from "../utils/tokenStatus.js";
  import {
    spawnIntent,
    firstTabListPriceFor,
    formatFreebucks,
  } from "../utils/freebucks.js";
  import { onMount } from "svelte";

  /**
   * TokenDetailsDrawer — expanded details for one pooled token: the
   * account-standing block and the empty-state message. The live session
   * countdown and its Drop Session kill switch render in the status cell
   * (TokenCard + TokenCardMobile), not here — the drawer stays for
   * pins/spawn/refresh, not session kill.
   *
   * @prop {object} token — dashboard tokenCard payload
   * @prop {string} [spawnModel] — bindable dev-spawn model selection
   * @prop {boolean} [actionPending]
   * @prop {boolean} [devToolsEnabled=false]
   * @prop {(model: string) => void} [onSpawn]
   * @prop {(action: string) => void} [onRefresh]
   */
  let {
    token,
    spawnModel = $bindable(""),
    actionPending,
    devToolsEnabled = false,
    onSpawn,
    onRefresh,
  } = $props();
  let modelOptions = $state(fallbackModelOptions);
  onMount(() => {
    fetchModelOptions().then((rows) => (modelOptions = rows));
  });
  // Meter pre-check (issue #350 — mirrors freebucksRowIntent): paywalled
  // options disable in place; the Make Session button refuses a paywalled
  // pick where the balance is already on screen.
  let selectedIntent = $derived(
    spawnIntent(token, spawnModel || cheapestFreeOption(modelOptions)),
  );
  // Remembered upstream-429 park, read off the live dashboard payload
  // (backend/internal/dashboard/dashboard_cards.go tokenCard +
  // dashboard_helpers.go cardFromSnapshot/liveCardFromSnapshot, both full
  // and hot-poll paths): cooldown_active + cooldown_until, with the
  // additive cooldown_kind / cooldown_resets_at / cooldown_window_hours
  // naming the window refusal. The payload carries no per-model detail, so
  // this names the pool-level reset — the next request spills to the next
  // account. "" when the account is not parked.
  function fmtParkedTime(raw) {
    const ms = Date.parse(String(raw ?? ""));
    if (!Number.isFinite(ms)) return "";
    const d = new Date(ms);
    const hh = String(d.getUTCHours()).padStart(2, "0");
    const mm = String(d.getUTCMinutes()).padStart(2, "0");
    return `${hh}:${mm}Z`;
  }
  let parkedNote = $derived.by(() => {
    if (!token.cooldown_active && !isExhausted(token)) return "";
    const until = fmtParkedTime(token.cooldown_until);
    const kind = token.cooldown_kind ? ` (${token.cooldown_kind})` : "";
    const resets = token.cooldown_resets_at
      ? ` · resets ${fmtParkedTime(token.cooldown_resets_at)}`
      : token.freebucks?.daily?.reset_at
        ? ` · resets ${fmtParkedTime(token.freebucks.daily.reset_at)}`
        : "";
    if (isExhausted(token)) {
      return `Exhausted — upstream 429${kind} until ${until || "—"} — spills to next account${resets}`;
    }
    return `Parked — upstream 429${kind} until ${until || "—"} — spills to next account${resets}`;
  });
  // Crossed-out list price for one model option (<option> carries text
  // only, so the strike renders as a ~N~ prefix): "" unless the first-tab
  // offer actually moved the row (available + list > price).
  function strikeFor(id) {
    const list = firstTabListPriceFor(token?.freebucks, id);
    return list === undefined ? "" : formatFreebucks(list);
  }
  // --- Single-model pin (PIN_MODEL slot syntax) ---
  // Instant-saves through the settings overlay (POST /admin/api/settings),
  // the same path every dashboard row uses — no whole-file .env write,
  // hot-applied via pool.SetConfig, no restart. The current map is read
  // from the settings endpoint so other slots' pins survive the write.
  // One model per account at most: picking another model moves the pin,
  // Clear removes it.
  let pinSaving = $state(false);
  let pinError = $state("");
  let pinNotice = $state("");
  let pinSelect = $state("");

  function parsePins(serialized) {
    const pins = {};
    for (const part of String(serialized || "").split(";")) {
      const i = part.indexOf(":");
      if (i < 0) continue;
      const slot = Number(part.slice(0, i).trim());
      const model = part.slice(i + 1).trim();
      if (Number.isInteger(slot) && model) pins[slot] = model;
    }
    return pins;
  }

  function serializePins(pins) {
    return Object.keys(pins)
      .map(Number)
      .sort((a, b) => a - b)
      .map((slot) => `${slot}:${pins[slot]}`)
      .join(";");
  }

  function patchSlotPin(serialized, slot, model) {
    const pins = parsePins(serialized);
    if (model) pins[slot] = model;
    else delete pins[slot];
    return serializePins(pins);
  }

  async function savePin(model) {
    const slot = token.index;
    if (slot == null || pinSaving) return;
    pinSaving = true;
    pinError = "";
    pinNotice = "";
    try {
      const setRes = await fetchAPI(adminApi.settings);
      const row = (setRes?.settings ?? []).find((e) => e.key === "PIN_MODEL");
      const value = patchSlotPin(row?.value ?? "", slot, model);
      const res = await postAPI(adminApi.settingsSave, {
        key: "PIN_MODEL",
        value,
      });
      if (res && res.ok === false)
        throw new Error(res.message || "Save rejected");
      // Caveat-bearing saves (restart-only, env-shadowed) surface the
      // server message as a warning; plain live saves stay quiet.
      if (
        (res && res.code && res.code !== "setting_saved") ||
        /process env/i.test(res?.message ?? "")
      ) {
        pinNotice = res.message;
      }
      pinSelect = "";
      await refreshTokens();
    } catch (e) {
      pinError = e?.message || String(e);
    } finally {
      pinSaving = false;
    }
  }
</script>

<div class="fp-inset rounded p-3">
  <!-- Dev Tools: Session Generator & Diagnostics Toolbar (hidden unless DEVTOOLS_ENABLED) -->
  {#if devToolsEnabled}
    <div
      class="mb-3 p-2.5 rounded bg-[var(--fp-surface)] border border-[var(--fp-border)] flex flex-wrap items-center justify-between gap-2.5"
    >
      <div class="flex flex-wrap items-center gap-2">
        <span
          class="text-xs font-semibold text-[var(--fp-muted)] uppercase tracking-wider"
          >{$tr("Dev Session:")}</span
        >
        <select
          bind:value={spawnModel}
          class="fp-input !text-xs !py-1 !pl-2.5 !h-7 !w-44 !inline-block"
        >
          {#each modelOptions as m (m.id)}
            {@const intent = spawnIntent(token, m.id)}
            {@const strike = strikeFor(m.id)}
            <option
              value={m.id}
              disabled={intent.kind === "paywall"}
              title={intent.kind === "paywall"
                ? $tr(
                    "Not enough Freebucks (price {price}, balance {balance})",
                    {
                      price: intent.price,
                      balance: token?.freebucks?.balance ?? 0,
                    },
                  )
                : m.label}
              >{strike ? `~${strike}~ ` : ""}{m.label}{intent.kind === "paywall"
                ? " — paywalled"
                : ""}</option
            >
          {/each}
        </select>
        <Button
          variant="secondary"
          size="sm"
          disabled={actionPending || selectedIntent.kind === "paywall"}
          title={selectedIntent.kind === "paywall"
            ? $tr("Not enough Freebucks (price {price}, balance {balance})", {
                price: selectedIntent.price,
                balance: token?.freebucks?.balance ?? 0,
              })
            : $tr("Make Session")}
          onclick={() =>
            onSpawn?.(spawnModel || cheapestFreeOption(modelOptions))}
        >
          <Zap size={12} />
          <span>{$tr("Make Session")}</span>
        </Button>
      </div>
      {#if selectedIntent.kind === "paywall"}
        <p class="text-[11px] text-[var(--fp-warning)] w-full">
          {$tr("Not enough Freebucks (price {price}, balance {balance})", {
            price: selectedIntent.price,
            balance: token?.freebucks?.balance ?? 0,
          })}
        </p>
      {/if}

      <div class="flex items-center gap-1.5">
        <Button
          variant="ghost"
          size="sm"
          disabled={actionPending}
          onclick={() => onRefresh?.("probe")}
        >
          <RefreshCw size={12} />
          <span>{$tr("Probe")}</span>
        </Button>
        <Button
          variant="ghost"
          size="sm"
          disabled={actionPending}
          onclick={() => onRefresh?.("finish")}
        >
          <Check size={12} />
          <span>{$tr("Finish Runs")}</span>
        </Button>
      </div>
    </div>
  {/if}
  <RefundLines
    {token}
    pendingClass="mb-2 px-2 py-1 rounded bg-[var(--fp-warning)]/10 text-xs text-[var(--fp-warning)]"
    settledClass="mb-2 px-2 py-1 rounded bg-[var(--fp-success)]/10 text-xs text-[var(--fp-success)]"
  />
  {#if parkedNote}
    <p
      class="mb-2 px-2 py-1 rounded bg-[var(--fp-warning)]/10 text-xs text-[var(--fp-warning)]"
      data-testid="parked-note"
    >
      {parkedNote}
    </p>
  {/if}
  {#if token.has_standing}
    <!-- Standing / trust block (issue #140): level,
         score progress toward the next level, the cap
         holding the account (capped_by), and upstream's
         suggested earn-back actions. -->
    <div class="mb-2 px-2 py-1.5 rounded bg-[var(--fp-bg)]/40">
      <div class="flex items-center justify-between gap-2 mb-1">
        <p
          class="text-xs text-[var(--fp-muted)] uppercase tracking-wider font-semibold"
        >
          {$tr("Account standing")}
        </p>
        <span class="text-xs text-[var(--fp-text)] font-semibold"
          >{token.standing_label || token.standing_level}</span
        >
      </div>
      {#if token.standing_score != null && token.standing_next_level}
        <div class="flex items-center gap-2">
          <div class="h-1.5 flex-1 rounded bg-[var(--fp-bg)] overflow-hidden">
            <div
              class="h-full rounded bg-[var(--fp-accent)]"
              style={`width: ${Math.min(100, Math.max(0, token.standing_score))}%`}
            ></div>
          </div>
          <span class="fp-num text-xs text-[var(--fp-muted)] shrink-0">
            {$tr("score {score} → next: {level}", {
              score: token.standing_score,
              level: token.standing_next_level,
            })}
          </span>
        </div>
      {:else if token.standing_score != null}
        <span class="fp-num text-xs text-[var(--fp-muted)]"
          >{$tr("score {score}", { score: token.standing_score })}</span
        >
      {/if}
      {#if token.standing_blurb}
        <p class="text-xs text-[var(--fp-dim)] mt-1">{token.standing_blurb}</p>
      {/if}
      {#if token.standing_capped_by}
        <p class="text-xs mt-1 text-[var(--fp-warning)]">
          {$tr("Capped by")}
          <code class="fp-num">{token.standing_capped_by}</code
          >{#if token.standing_capped_reason}: {token.standing_capped_reason}{/if}
        </p>
      {/if}
      {#if token.standing_next_steps?.length > 0}
        <ul class="mt-1.5 flex flex-col gap-1">
          {#each token.standing_next_steps as step (step.label)}
            <li class="text-xs text-[var(--fp-text)] flex items-start gap-1.5">
              <span class="fp-num text-[var(--fp-accent)] shrink-0"
                >+{step.points}</span
              >
              <span>
                {step.label}{#if step.detail}
                  — <span class="text-[var(--fp-dim)]">{step.detail}</span>{/if}
                {#if step.href}
                  <a
                    href={step.href}
                    target="_blank"
                    rel="noopener noreferrer"
                    class="ml-1 text-[var(--fp-accent)] hover:underline inline-flex items-center gap-0.5"
                  >
                    <ExternalLink size={10} />
                  </a>
                {/if}
              </span>
            </li>
          {/each}
        </ul>
      {/if}
    </div>
  {/if}
  <div class="mb-2 px-2 py-1.5 rounded bg-[var(--fp-bg)]/40">
    <div
      class="flex items-center justify-between gap-2 text-xs font-semibold text-[var(--fp-muted)] uppercase tracking-wider mb-1"
    >
      <span class="inline-flex items-center gap-1.5"
        ><Lock size={12} />{$tr("Pinned model")}</span
      >
      {#if token.pinned_model}
        <button
          type="button"
          class="normal-case font-medium text-[var(--fp-accent)] hover:underline disabled:opacity-50"
          disabled={pinSaving}
          onclick={() => savePin("")}
        >
          {$tr("Clear pin")}
        </button>
      {/if}
    </div>
    {#if token.pinned_model}
      <div class="flex flex-wrap gap-1.5">
        <span
          class="inline-flex items-center gap-1 rounded bg-[var(--fp-surface)] border border-[var(--fp-border)] px-1.5 py-0.5"
        >
          <code class="fp-num text-xs text-[var(--fp-text)]"
            >{token.pinned_model}</code
          >
        </span>
      </div>
      {#if token.pin_skips > 0}
        <p class="mt-1 text-xs text-[var(--fp-dim)]">
          {$tr("{count} request(s) routed elsewhere by this pin", {
            count: token.pin_skips,
          })}
        </p>
      {/if}
    {:else}
      <p class="text-xs text-[var(--fp-dim)]">
        {$tr(
          "Unlocked — serves any model. Pin one model to dedicate this account.",
        )}
      </p>
    {/if}
    <div class="mt-1.5 flex items-center gap-1.5">
      <select
        bind:value={pinSelect}
        class="fp-input !text-xs !py-1 !pl-2 !h-7 flex-1 min-w-0"
        aria-label={$tr("Pin a model to this token")}
        disabled={pinSaving}
      >
        <option value="">{$tr("Pin a model…")}</option>
        {#each modelOptions.filter((o) => o.id !== token.pinned_model) as o (o.id)}
          <option value={o.id}>{o.label}</option>
        {/each}
      </select>
      <Button
        variant="secondary"
        size="sm"
        disabled={pinSaving || !pinSelect}
        onclick={() => savePin(pinSelect)}
      >
        <Plus size={12} />
        <span>{pinSaving ? $tr("Saving…") : $tr("Pin")}</span>
      </Button>
    </div>
    {#if pinError}
      <p class="mt-1 text-xs text-red-400">{pinError}</p>
    {/if}
    {#if pinNotice}
      <p class="mt-1 text-xs text-amber-300">{pinNotice}</p>
    {/if}
  </div>
  {#if !devToolsEnabled && !(token.session_remaining_seconds > 0 && token.session_model) && !token.has_standing && !parkedNote}
    <p class="text-xs text-[var(--fp-dim)] italic">
      {$tr("No active session or run for this auth token.")}
    </p>
  {/if}
</div>
