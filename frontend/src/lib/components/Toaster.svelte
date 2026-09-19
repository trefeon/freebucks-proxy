<script>
  import {
    Info,
    CheckCircle2,
    AlertTriangle,
    AlertCircle,
    X,
  } from "@lucide/svelte";
  import { fade } from "svelte/transition";
  import { toasts, dismiss } from "../stores/toast.js";
  import { tr } from "../i18n.js";

  /**
   * Toaster — the single global toast host, mounted once at the App root.
   * Fixed top-center above Modal, stacking downward from the viewport top.
   * Every toast auto-fades after 10s (stores/toast.js); never steals focus;
   * Escape dismisses the newest toast. error/warning use role="alert",
   * info/success use role="status".
   */
  const icons = {
    info: Info,
    success: CheckCircle2,
    warning: AlertTriangle,
    error: AlertCircle,
  };
  const borders = {
    info: "border-l-[var(--fp-info)]",
    success: "border-l-[var(--fp-success)]",
    warning: "border-l-[var(--fp-warning)]",
    error: "border-l-[var(--fp-error)]",
  };
  const iconTones = {
    info: "text-[var(--fp-info)]",
    success: "text-[var(--fp-success)]",
    warning: "text-[var(--fp-warning)]",
    error: "text-[var(--fp-error)]",
  };

  function onKeydown(e) {
    if (e.key === "Escape" && $toasts.length > 0) {
      dismiss($toasts[$toasts.length - 1].id);
    }
  }
</script>

<svelte:window onkeydown={onKeydown} />

{#if $toasts.length > 0}
  <div
    class="pointer-events-none fixed top-0 left-1/2 z-[100] flex w-[calc(100vw-2rem)] max-w-sm -translate-x-1/2 flex-col gap-2 p-4"
    aria-label={$tr("Notifications")}
  >
    {#each $toasts as t (t.id)}
      {@const Icon = icons[t.tone] ?? Info}
      {@const role =
        t.tone === "error" || t.tone === "warning" ? "alert" : "status"}
      <div
        {role}
        transition:fade={{ duration: 200 }}
        class="pointer-events-auto flex items-start gap-2 rounded-[var(--fp-radius)] border border-[var(--fp-border)] border-l-2 bg-[var(--fp-surface)] px-4 py-3 shadow-lg {borders[
          t.tone
        ] || borders.info}"
      >
        <Icon
          size={18}
          class="mt-0.5 shrink-0 {iconTones[t.tone] || iconTones.info}"
          aria-hidden="true"
        />
        <div class="min-w-0 flex-1">
          {#if t.title}
            <p class="text-[13px] font-semibold text-[var(--fp-text)]">
              {t.title}
            </p>
          {/if}
          {#if t.body}
            <p class="mt-0.5 text-xs break-words text-[var(--fp-muted)]">
              {t.body}
            </p>
          {/if}
        </div>
        <button
          type="button"
          onclick={() => dismiss(t.id)}
          aria-label={$tr("Dismiss notification")}
          class="flex min-h-8 min-w-8 shrink-0 items-center justify-center rounded text-[var(--fp-dim)] transition-colors hover:text-[var(--fp-text)]"
        >
          <X size={14} />
        </button>
      </div>
    {/each}
  </div>
{/if}
