<script>
  import { onMount } from "svelte";
  import { RefreshCw, Key, Eye, EyeOff, Trash2 } from "@lucide/svelte";
  import Card from "./Card.svelte";
  import Button from "./Button.svelte";
  import CopyButton from "./CopyButton.svelte";
  import { push as pushToast } from "../stores/toast.js";
  import GeneratedKeyModal from "./GeneratedKeyModal.svelte";
  import { deleteAPI, fetchAPI, postAPI } from "../api/client.js";
  import { adminApi } from "../api/paths.js";
  import { generateRandomApiKey } from "../utils/format.js";
  import { getEnvValue } from "../utils/env.js";
  import { tr } from "../i18n.js";
  import { confirmAction } from "../stores/confirm.js";
  /**
   * ApiKeysEditor - the Client API Keys card (sk-fb- credentials in the
   * API_KEYS overlay row). Split out of Overview.svelte (issue #287) so the
   * overview page owns KPIs/risk cards while this component owns the API-key
   * generate/delete/reveal flow and the generated-key modal.
   *
   * Unified store: writes go through the DB-overlay instant-save path (POST
   * /admin/api/settings, applied live like every other knob) — the .env
   * file is never written. The key list seeds from the live config export
   * (GET /admin/api/config env_content, rendered from the running snapshot,
   * read-only here); every mutation re-reads it first so the read-modify-
   * write merges against live truth instead of a stale mount-time list.
   */
  let apiKeys = $state([]);
  let generatingKey = $state(false);
  let generatedKey = $state("");
  let deletingKey = $state("");
  let showGeneratedModal = $state(false);
  let visibleKeys = $state({});

  function toggleKeyVisibility(key) {
    visibleKeys = { ...visibleKeys, [key]: !visibleKeys[key] };
  }
  function maskKey(key) {
    if (visibleKeys[key]) return key;
    if (!key) return "";
    if (key.length <= 10) return "••••••••";
    const prefix = key.startsWith("sk-fb-") ? "sk-fb-" : key.slice(0, 6);
    const suffix = key.slice(-4);
    const padding = "•".repeat(
      Math.max(0, key.length - prefix.length - suffix.length),
    );
    return `${prefix}${padding}${suffix}`;
  }

  function openGeneratedKeyModal(key) {
    generatedKey = key;
    showGeneratedModal = true;
  }

  function closeGeneratedKeyModal() {
    showGeneratedModal = false;
  }

  async function generateClientKey() {
    if (generatingKey) return;
    generatingKey = true;
    generatedKey = "";
    try {
      const newKey = generateRandomApiKey();
      const cfgRes = await fetchAPI(adminApi.config);
      const existing = getEnvValue(cfgRes?.env_content || "", "API_KEYS") || "";
      const updated = existing ? `${existing},${newKey}` : newKey;
      const result = await postAPI(adminApi.settingsSave, {
        key: "API_KEYS",
        value: updated,
      });
      apiKeys = updated
        .split(",")
        .map((s) => s.trim())
        .filter(Boolean);
      openGeneratedKeyModal(newKey);
      pushToast({
        tone: "success",
        title: result?.message || $tr("Generated & saved client API key"),
      });
    } catch (e) {
      pushToast({
        tone: "error",
        title: e.message || $tr("Failed to save client API key"),
      });
    } finally {
      generatingKey = false;
    }
  }

  async function deleteApiKey(target) {
    if (deletingKey) return;
    const confirmed = await confirmAction({
      title: $tr("Delete API Key"),
      message: $tr(
        "Are you sure you want to delete this client API key? Clients using this key will immediately lose access to the gateway.",
      ),
      confirmText: $tr("Delete"),
      tone: "danger",
    });
    if (!confirmed) return;
    deletingKey = target;
    try {
      const cfgRes = await fetchAPI(adminApi.config);
      const val = getEnvValue(cfgRes?.env_content || "", "API_KEYS") || "";
      const keys = val
        ? val
            .split(",")
            .map((s) => s.trim())
            .filter(Boolean)
        : [];
      const filtered = keys.filter((k) => k !== target);
      // An empty value 400s on the overlay (the gateway requires DELETE to
      // reset a key), so dropping the last key deletes the row: the
      // effective value falls back to the boot seed, and the server receipt
      // says so honestly.
      const result =
        filtered.length === 0
          ? await deleteAPI(adminApi.settingsDelete("API_KEYS"))
          : await postAPI(adminApi.settingsSave, {
              key: "API_KEYS",
              value: filtered.join(","),
            });
      apiKeys = filtered;
      pushToast({
        tone: "success",
        title: result?.message || $tr("Deleted client API key"),
      });
    } catch (e) {
      pushToast({
        tone: "error",
        title: e.message || $tr("Failed to delete client API key"),
      });
    } finally {
      deletingKey = "";
    }
  }

  // Config-derived display field (apiKeys) changes only on save, so it is
  // fetched once on mount instead of on every 15s overview poll. The export
  // is read-only here: saves write the overlay and update apiKeys locally.
  async function fetchConfig() {
    try {
      const cfgRes = await fetchAPI(adminApi.config);
      const envContent = cfgRes?.env_content || "";
      const m = envContent.match(/^\s*API_KEYS=(.*)$/m);
      const val = m ? m[1].trim() : "";
      apiKeys = val
        ? val
            .split(",")
            .map((s) => s.trim())
            .filter(Boolean)
        : [];
    } catch {
      apiKeys = [];
    }
  }

  onMount(() => {
    fetchConfig();
  });
</script>

<Card
  title={$tr("Client API Keys")}
  description={$tr(
    "sk-fb-… credentials for clients (omp, Cursor, Claude Code, curl) to authenticate against this gateway. Stored in API_KEYS; changes apply immediately.",
  )}
>
  {#snippet actions()}
    <Button
      variant="primary"
      size="sm"
      onclick={generateClientKey}
      disabled={generatingKey}
    >
      {#if generatingKey}
        <RefreshCw size={14} class="animate-spin" />
        <span>{$tr("Generating…")}</span>
      {:else}
        <Key size={14} />
        <span>{$tr("Generate API Key")}</span>
      {/if}
    </Button>
  {/snippet}

  {#if apiKeys.length > 0}
    <div class="flex flex-col gap-2 mb-3">
      {#each apiKeys as key (key)}
        <div
          class="fp-inset rounded flex items-center justify-between gap-2 px-3 py-2"
        >
          <code class="fp-num text-xs truncate flex-1 select-all font-mono"
            >{maskKey(key)}</code
          >
          <div class="flex items-center gap-1 shrink-0">
            <Button
              variant="ghost"
              size="sm"
              onclick={() => toggleKeyVisibility(key)}
              aria-label={visibleKeys[key]
                ? $tr("Hide API key")
                : $tr("Show API key")}
              title={visibleKeys[key]
                ? $tr("Hide API key")
                : $tr("Show API key")}
            >
              {#if visibleKeys[key]}
                <EyeOff size={14} />
              {:else}
                <Eye size={14} />
              {/if}
            </Button>
            <CopyButton text={key} label="Copy" />
            <Button
              variant="ghost"
              size="sm"
              onclick={() => deleteApiKey(key)}
              disabled={deletingKey === key}
              aria-label={$tr("Delete API key")}
              title={$tr("Delete API key")}
            >
              <Trash2 size={14} />
              <span>{$tr("Delete")}</span>
            </Button>
          </div>
        </div>
      {/each}
    </div>
  {:else}
    <p class="text-xs text-[var(--fp-dim)] mb-3">
      {$tr(
        "No client API keys configured. In open mode, clients can authenticate with any key or leave it unset.",
      )}
    </p>
  {/if}
</Card>

<GeneratedKeyModal
  bind:open={showGeneratedModal}
  key={generatedKey}
  onClose={closeGeneratedKeyModal}
/>
