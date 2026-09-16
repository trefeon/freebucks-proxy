<script>
  import SettingsCard from "./SettingsCard.svelte";
  import Button from "./Button.svelte";
  import { AlertTriangle, Save, X } from "@lucide/svelte";
  import { tr } from "../i18n.js";
  import {
    baseContent,
    rawText,
    saving,
    saveConfig,
  } from "../stores/settings.js";

  /**
   * Emergency raw .env editor (break-glass path).
   *
   * The whole file, exactly as the proxy reads it, in a plain textarea.
   * Prefer the per-row controls above — they validate and instant-save.
   * A raw save rewrites the file and reloads; DB overlay rows still win
   * for their keys until reset. The draft is local: background refetches
   * never clobber typed text; Save stages it and reuses the shared
   * whole-file save (with its confirm + result reporting).
   */
  let text = $state(null); // null = follow the fetched file

  $effect(() => {
    if (text === null && $baseContent) text = $baseContent;
  });

  let edited = $derived(text !== null && text !== $baseContent);

  async function save(e) {
    rawText.set(text ?? "");
    await saveConfig(e, { confirm: true });
  }

  function revert() {
    text = null;
  }
</script>

<SettingsCard
  title={$tr("Emergency raw .env editor")}
  description={$tr(
    "Break-glass only: the whole .env file, exactly as the proxy reads it. Prefer the per-row controls above — they validate and save instantly. A raw save rewrites the file and reloads; saved overlay rows still win for their keys until reset.",
  )}
>
  {#snippet icon()}
    <AlertTriangle size={20} />
  {/snippet}
  {#snippet actions()}
    {#if edited}
      <Button variant="ghost" size="sm" onclick={revert} disabled={$saving}>
        <X size={14} />
        {$tr("Revert")}
      </Button>
    {/if}
    <Button
      variant="primary"
      size="sm"
      onclick={save}
      disabled={$saving || !edited}
      loading={$saving}
    >
      <Save size={14} />
      {$tr("Save raw .env")}
    </Button>
  {/snippet}

  <div class="flex flex-col gap-2 py-2">
    <label
      for="emergency-raw-env"
      class="text-xs font-semibold text-[var(--fp-muted)]"
      >{$tr("Raw .env content")}</label
    >
    <textarea
      id="emergency-raw-env"
      class="fp-input fp-mono w-full min-h-64"
      rows="20"
      spellcheck="false"
      autocomplete="off"
      autocapitalize="off"
      disabled={$saving}
      value={text ?? ""}
      oninput={(e) => (text = e.currentTarget.value)}></textarea>
  </div>
</SettingsCard>
