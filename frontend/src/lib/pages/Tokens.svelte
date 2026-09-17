<script>
  import { onMount } from "svelte";
  import { LogIn, Plus, ExternalLink, RefreshCw } from "@lucide/svelte";
  import Button from "../components/Button.svelte";
  import Card from "../components/Card.svelte";
  import Alert from "../components/Alert.svelte";
  import { push as pushToast } from "../stores/toast.js";
  import CopyButton from "../components/CopyButton.svelte";
  import PageShell from "../components/PageShell.svelte";
  import BridgeTokenCard from "../components/BridgeTokenCard.svelte";
  import TokenTable from "./tokens/TokenTable.svelte";
  import SegmentedControl from "../components/SegmentedControl.svelte";
  import MaturityPanel from "../components/MaturityPanel.svelte";
  import TrafficSettings from "./settings/TrafficSettings.svelte";
  import StrategyPresetCard from "./settings/StrategyPresetCard.svelte";
  import PoolCustomAdvanced from "./settings/PoolCustomAdvanced.svelte";
  import AdvancedSettings from "./settings/AdvancedSettings.svelte";
  import { fetchAPI, postAPI, csrfHeader } from "../api/client.js";
  import { adminApi, adminActions, tokenActions } from "../api/paths.js";
  import {
    meta as settingsMeta,
    formValues as settingsFormValues,
    rawText as settingsRawText,
    settingSources as settingsSources,
    loading as settingsLoading,
    settingsDegraded,
    fetchData as fetchSettings,
    resetSetting as resetSettingsKey,
    overlaySaved as settingsOverlaySaved,
    setField as setSettingsField,
  } from "../stores/settings.js";
  import { detectStrategy } from "../utils/poolStrategy.js";
  import { isDevToolsEnabled } from "../utils/devtools.js";
  import {
    tokensData as tokensStore,
    tokensError as tokensErrorStore,
    ensureTokensStore,
    refreshTokens,
  } from "../stores/tokens.js";
  import { tr } from "../i18n.js";
  import { spawnIntent, intentAskLine } from "../utils/freebucks.js";
  import { fallbackModelOptions, cheapestFreeOption } from "../modelOptions.js";
  import { confirmAction } from "../stores/confirm.js";
  import {
    loadPageState,
    savePageState,
    recordPageVisit,
  } from "../stores/pageState.js";
  let data = $state(null);
  let loading = $state(true);
  let error = $state("");
  let unsubStore = null;
  let unsubErr = null;

  // Add-token form
  let newToken = $state("");
  let adding = $state(false);
  // Dev Tools surfaces (per-token session spawn toolbar) are hidden unless
  // the operator enables DEVTOOLS_ENABLED=true in .env (same gate as the
  // sidebar's Dev Tools tab and the server-side DevTools route).
  let devToolsEnabled = $state(false);
  // Queue-posture chip: the same detection the Pool Strategy card badge
  // uses, read from the shared settings store. The store is empty until
  // fetchSettings() resolves and stays empty — or degraded — when the
  // overlay is unreachable, so the chip reports nothing until then rather
  // than guessing a posture from file/loader defaults.
  let posture = $derived(
    $settingsLoading ||
      $settingsDegraded ||
      Object.keys($settingsFormValues).length === 0
      ? null
      : detectStrategy({
          SLOTS_PER_ACCOUNT: $settingsFormValues.SLOTS_PER_ACCOUNT,
          QUEUE_WAIT: $settingsFormValues.QUEUE_WAIT,
          QUEUE_DEPTH: $settingsFormValues.QUEUE_DEPTH,
          PIN_MODEL: $settingsFormValues.PIN_MODEL,
          MAX_SPILL_ACCOUNTS: $settingsFormValues.MAX_SPILL_ACCOUNTS,
        }),
  );
  // Active tab: pool accounts vs pool controls vs account warming. The
  // legacy #maturity hash redirects here one-shot via sessionStorage (see
  // onMount).
  let tab = $state("accounts");
  // Bridge-gated rows (BRIDGE_IDLE_EVICT in Pool Tuning) hide unless bridge
  // mode can serve: BRIDGE_ENABLED on (default true).
  let bridgePossible = $derived(
    String($settingsFormValues.BRIDGE_ENABLED ?? "true").toLowerCase() !==
      "false",
  );

  // Device login flow
  let oauthStarting = $state(false);
  let oauthStatus = $state(null);
  let oauthTimer = null;

  // Token table
  let expandedToken = $state(null);
  let spawnModels = $state({});
  let actionPending = $state(false);
  let now = $state(Date.now());

  const tokenValid = $derived(
    newToken.trim() === ""
      ? null
      : !newToken.trim().toLowerCase().startsWith("bearer ") &&
          !/[,\s]/.test(newToken.trim()) &&
          newToken.trim().length >= 10,
  );

  function applyTokens(v) {
    if (!v) return;
    data = v;
    // Seed the per-token spawn-model map so no TokenCard binding ever sees
    // with a fallback (props_invalid_value) and unmounts the table.
    (v?.tokens ?? []).forEach((t, i) => {
      const idx = t.index ?? i;
      if (!(idx in spawnModels)) spawnModels[idx] = "";
    });
    clampExpandedToken();
    error = "";
    loading = false;
  }

  // A restored expandedToken may point past the live list (the pool shrank
  // while the snapshot sat in pages_state). Drop out-of-range indexes
  // instead of opening the wrong drawer — and never re-persist the stale
  // value back over the snapshot.
  function clampExpandedToken() {
    if (expandedToken == null) return;
    const list = data?.tokens ?? [];
    const ok = list.some((t, i) => (t?.index ?? i) === expandedToken);
    if (!ok) expandedToken = null;
  }

  async function addToken(e) {
    e.preventDefault();
    if (!newToken.trim() || tokenValid === false || adding) return;
    adding = true;
    try {
      const result = await postAPI(adminActions.tokenAdd, {
        token: newToken.trim(),
      });
      const addOK = result.ok !== false;
      pushToast({
        tone: addOK ? "success" : "error",
        title:
          result.message ||
          (addOK
            ? $tr("Token added successfully")
            : $tr("Failed to add token")),
      });
      if (addOK) {
        newToken = "";
        refreshTokens();
      }
    } catch (e) {
      pushToast({
        tone: "error",
        title: e.message || $tr("Network error adding token"),
      });
    } finally {
      adding = false;
    }
  }

  async function triggerAction(
    url,
    body,
    confirmMsg,
    title,
    tone = "warn",
    confirmText = "",
  ) {
    if (confirmMsg) {
      const ok = await confirmAction({
        title: title || $tr("Confirm Action"),
        message: confirmMsg,
        confirmText: confirmText || $tr("Confirm"),
        tone,
      });
      if (!ok) return;
    }
    actionPending = true;
    try {
      const result = await postAPI(url, body || undefined);
      const actOK = result.ok !== false;
      pushToast({
        tone: actOK ? "success" : "error",
        title:
          result.message ||
          (actOK ? $tr("Action completed") : $tr("Action failed")),
      });
      refreshTokens();
    } catch (e) {
      pushToast({
        tone: "error",
        title: e.message || $tr("Network error executing action"),
      });
    } finally {
      actionPending = false;
    }
  }

  function handleTokenAction(token, idx, action) {
    switch (action) {
      case "clear":
        return triggerAction(
          tokenActions.unlock(idx),
          {},
          $tr(
            "Clear cooldown for account {idx}? Only do this if the lock is stale.",
            { idx: idx + 1 },
          ),
          $tr("Clear Cooldown"),
          "warn",
          $tr("Clear"),
        );
      case "unlock":
        return triggerAction(
          tokenActions.unlockLock(idx),
          {},
          $tr("Unlock account {idx}? It will rejoin the active rotation.", {
            idx: idx + 1,
          }),
          $tr("Unlock Token"),
          "neutral",
          $tr("Unlock"),
        );
      case "lock":
        return triggerAction(
          tokenActions.lock(idx),
          {},
          $tr(
            "Lock account {idx}? It will be excluded from rotation until unlocked.",
            { idx: idx + 1 },
          ),
          $tr("Lock Token"),
          "warn",
          $tr("Lock"),
        );
      case "remove":
        return triggerAction(
          adminActions.tokenRemove,
          { token: idx },
          $tr(
            "Remove account {idx} from the pool and .env? The account must be re-added to use it again.",
            { idx: idx + 1 },
          ),
          $tr("Remove Token"),
          "danger",
          $tr("Remove"),
        );
      default:
        return;
    }
  }
  function handleSpawn(idx, model) {
    const m = model || cheapestFreeOption(fallbackModelOptions);
    // Confirm-intent line (issue #350 — mirrors askLineFor): warn when the
    // pick spends wallet Freebucks or ends the live session.
    const token = data?.tokens?.find?.((t) => t.index === idx);
    const intent = spawnIntent(token, m);
    const askLine = intentAskLine(intent, token?.session_model);
    triggerAction(
      tokenActions.session(idx),
      { model: m },
      $tr("Create upstream session for account #{idx} on {model}?", {
        idx: idx + 1,
        model: m,
      }) + (askLine ? " " + askLine : ""),
    );
  }

  function handleRefresh(idx, action) {
    if (action === "probe") {
      return triggerAction(
        tokenActions.test(idx),
        {},
        $tr("Probe account #{idx} against upstream?", { idx: idx + 1 }),
      );
    }
    return triggerAction(
      tokenActions.finish(idx),
      {},
      $tr("Finish active runs on account #{idx}?", { idx: idx + 1 }),
    );
  }

  function handleDropSession(idx) {
    return triggerAction(
      tokenActions.dropSession(idx),
      {},
      $tr(
        "Drop active session on account #{idx}? This ends the current session upstream (e.g. luna) so the next request admits fresh for the model you want. Use this when you need to switch models immediately.",
        { idx: idx + 1 },
      ),
    );
  }

  function handleSwap(from, to) {
    triggerAction(adminActions.tokenSwap, { from, to });
  }

  function handleMove(from, to) {
    if (from === to) return;
    triggerAction(adminActions.tokenSwap, { from, to, action: "move" });
  }

  async function startOAuthLogin() {
    oauthStarting = true;
    pushToast({ tone: "info", title: $tr("Starting headless login flow…") });

    try {
      const res = await fetch(adminActions.loginStart, {
        method: "POST",
        headers: csrfHeader("POST"),
      });
      const result = await res.json();

      if (result.fingerprint && result.login_url) {
        oauthStatus = {
          loginUrl: result.login_url,
          fingerprint: result.fingerprint,
          message: $tr("Open this URL in your browser to sign in:"),
          type: "pending",
        };

        clearInterval(oauthTimer);
        oauthTimer = setInterval(async () => {
          try {
            const pollRes = await fetch(
              `${adminApi.loginStatus}?fingerprint=${encodeURIComponent(result.fingerprint)}`,
            );
            const pollData = await pollRes.json();

            if (pollData.status === "completed") {
              clearInterval(oauthTimer);
              pushToast({
                tone: "success",
                title: $tr("Account #{idx} added to pool and saved to .env.", {
                  idx: pollData.token_index + 1,
                }),
              });
              oauthStatus = null;
              oauthStarting = false;
              refreshTokens();
            } else if (pollData.status === "error") {
              clearInterval(oauthTimer);
              pushToast({
                tone: "error",
                title: $tr("Login failed: {message}", {
                  message: pollData.message || $tr("unknown error"),
                }),
              });
              oauthStatus = null;
              oauthStarting = false;
            }
          } catch {
            // transient poll failure — keep polling
          }
        }, 3000);
      } else {
        pushToast({
          tone: "error",
          title: result.message || $tr("Failed to start login wizard."),
        });
        oauthStatus = null;
        oauthStarting = false;
      }
    } catch (e) {
      pushToast({
        tone: "error",
        title: $tr("Network error: {message}", { message: e.message }),
      });
      oauthStatus = null;
      oauthStarting = false;
    }
  }
  function toggleExpand(idx) {
    expandedToken = expandedToken === idx ? null : idx;
    savePageState("tokens", { expandedToken });
  }

  // Deep page state: the expanded token row survives restarts via
  // pages_state (warn-only; an out-of-range index is dropped). A cross-page
  // account-expand link ("fp-tokens-expand") wins one-shot when present.
  function restoreExpandedToken() {
    try {
      const raw = sessionStorage.getItem("fp-tokens-expand");
      if (raw !== null) {
        sessionStorage.removeItem("fp-tokens-expand");
        const n = Number.parseInt(String(raw).trim(), 10);
        if (Number.isInteger(n)) {
          expandedToken = n;
          clampExpandedToken();
          if (expandedToken !== null) {
            savePageState("tokens", { expandedToken });
            return;
          }
        }
      }
    } catch {
      /* storage blocked: fall through to pages_state */
    }
    loadPageState("tokens").then((d) => {
      if (Number.isInteger(d?.expandedToken) && d.expandedToken >= 0)
        expandedToken = d.expandedToken;
    });
  }
  onMount(() => {
    recordPageVisit("tokens");
    // Legacy #maturity redirects here one-shot: consume the requested tab,
    // then drop the key so a plain visit always lands on Accounts.
    try {
      const want = sessionStorage.getItem("fp-page-tab:tokens");
      if (want !== null) {
        sessionStorage.removeItem("fp-page-tab:tokens");
        if (want === "accounts" || want === "controls" || want === "warming")
          tab = want;
      }
    } catch {
      /* storage blocked: default tab stands */
    }
    restoreExpandedToken();
    // Shared settings draft (same store as Settings): hydrates the inline
    // Pool controls; silent when Settings already loaded it.
    fetchSettings();
    // One shared tokens store owns the /admin/api/tokens poll + SSE (issue
    // #292); this page renders from the cached snapshot and refreshes the
    // store after every mutation.
    const release = ensureTokensStore();
    unsubStore = tokensStore.subscribe(applyTokens);
    unsubErr = tokensErrorStore.subscribe((err) => {
      if (err) {
        error = err;
        loading = false;
      }
    });
    const tick = setInterval(() => {
      now = Date.now();
    }, 1000);
    function onConfigSaved() {
      refreshTokens();
    }
    window.addEventListener("fp-config-saved", onConfigSaved);
    // Only the DevTools gate still reads .env here.
    (async () => {
      try {
        const cfgRes = await fetchAPI(adminApi.config);
        devToolsEnabled = isDevToolsEnabled(cfgRes?.env_content || "");
      } catch {
        devToolsEnabled = false;
      }
    })();
    return () => {
      release();
      unsubStore?.();
      unsubErr?.();
      clearInterval(tick);
      clearInterval(oauthTimer);
      window.removeEventListener("fp-config-saved", onConfigSaved);
    };
  });
</script>

<PageShell
  crumb="freebuff-proxy / Admin / pool.conf"
  title={$tr("Pool")}
  description={$tr(
    "Upstream credentials, device login, and streak enrollment — allowances live on Usage",
  )}
  {loading}
  {error}
  onRetry={() => {
    error = "";
    refreshTokens();
  }}
>
  {#snippet actions()}
    {@const activeLeases = (data?.tokens ?? []).filter(
      (t) => t.session_status === "active",
    ).length}
    <dl
      class="flex flex-wrap items-stretch gap-px bg-[var(--fp-border)] border border-[var(--fp-border)] rounded-[var(--fp-radius-sm)] overflow-hidden font-mono"
      aria-label={$tr("Pool summary")}
    >
      <div class="flex flex-col px-2.5 py-1.5 bg-[var(--fp-surface)]">
        <dt class="text-[10px] uppercase tracking-wider text-[var(--fp-dim)]">
          {$tr("Total")}
        </dt>
        <dd class="text-sm font-semibold text-[var(--fp-text)] tabular-nums">
          {data?.token_count ?? 0}
        </dd>
      </div>
      <div class="flex flex-col px-2.5 py-1.5 bg-[var(--fp-surface)]">
        <dt class="text-[10px] uppercase tracking-wider text-[var(--fp-dim)]">
          {$tr("Active")}
        </dt>
        <dd class="text-sm font-semibold text-[var(--fp-accent)] tabular-nums">
          {activeLeases}
        </dd>
      </div>
      <div
        class="flex flex-col px-2.5 py-1.5 bg-[var(--fp-surface)]"
        data-testid="queue-chip"
        title={posture
          ? $tr(
              "Queue posture from the five strategy keys — the same source as the Pool Strategy card's badge.",
            )
          : $tr(
              "Queue posture needs the settings store: still loading, or the overlay is offline.",
            )}
      >
        <dt class="text-[10px] uppercase tracking-wider text-[var(--fp-dim)]">
          {$tr("Queue")}
        </dt>
        <dd class="text-sm font-semibold text-[var(--fp-text)]">
          {posture === "masq"
            ? $tr("MASQ")
            : posture === "drain"
              ? $tr("Drain")
              : posture === "balance"
                ? $tr("Balance")
                : posture === "custom"
                  ? $tr("Custom")
                  : "—"}
        </dd>
      </div>
    </dl>
  {/snippet}
  {#if oauthStatus?.loginUrl}
    <div
      class="rounded border border-[var(--fp-border)] bg-[var(--fp-surface-2)]/60 px-3 py-2.5"
    >
      <p class="text-[13px] font-semibold text-[var(--fp-text)]">
        {oauthStatus.message}
      </p>
      <div class="flex flex-col gap-2 mt-2">
        <div class="flex flex-wrap items-center gap-2">
          <code class="fp-num text-xs break-all max-w-full"
            >{oauthStatus.loginUrl}</code
          >
          <CopyButton text={oauthStatus.loginUrl} label={$tr("Copy link")} />
          <a
            href={oauthStatus.loginUrl}
            target="_blank"
            rel="noopener noreferrer"
            class="inline-flex items-center gap-1 text-xs text-[var(--fp-accent)] hover:underline font-medium"
          >
            {$tr("Open in New Tab")}
            <ExternalLink size={12} />
          </a>
        </div>
        <p class="text-xs text-[var(--fp-dim)]">
          {$tr(
            "Tip: To add a different FreeBuff account, open this link in an Incognito / Private window so your browser does not reuse an existing GitHub session.",
          )}
        </p>
      </div>
    </div>
  {/if}
  <div class="flex flex-col items-start gap-1">
    <div class="flex flex-wrap items-center gap-2">
      <SegmentedControl
        bind:value={tab}
        options={[
          { id: "accounts", label: $tr("Accounts") },
          { id: "warming", label: $tr("Warming") },
          { id: "controls", label: $tr("Controls") },
        ]}
        ariaLabel={$tr("Tokens sections")}
      />
    </div>
  </div>

  {#if tab === "accounts"}
    <!-- Add token form -->
    <Card
      title={$tr("Add Token to Pool")}
      description={$tr(
        "Paste a FreeBuff auth token (from credentials.json or CLI) to add it to the shared pool and save it to .env. Adding burns no quota.",
      )}
    >
      {#snippet actions()}
        <Button
          variant="secondary"
          onclick={startOAuthLogin}
          disabled={oauthStarting}
        >
          {#if oauthStarting}
            <RefreshCw size={15} class="animate-spin" />
            <span>{$tr("Authorizing…")}</span>
          {:else}
            <LogIn size={15} />
            <span>{$tr("Device Login")}</span>
          {/if}
        </Button>
      {/snippet}
      <form onsubmit={addToken} class="flex flex-col gap-1.5">
        <label
          for="add-token-input"
          class="text-xs font-medium text-[var(--fp-muted)]"
          >{$tr("Token")}</label
        >
        <div
          class="flex flex-col sm:flex-row items-stretch sm:items-center gap-2.5"
        >
          <input
            id="add-token-input"
            type="text"
            bind:value={newToken}
            placeholder="e.g. a94d808e-8a86-455b-80fb-a9df4422bfcb"
            autocomplete="off"
            spellcheck="false"
            class="fp-input fp-num flex-1"
          />
          <Button
            type="submit"
            variant="primary"
            disabled={adding || !newToken.trim() || tokenValid === false}
            loading={adding}
            class="shrink-0"
          >
            <Plus size={15} />
            <span>{$tr("Add Token")}</span>
          </Button>
        </div>
        {#if tokenValid === false}
          <p class="text-[11px] text-[var(--fp-error)]" role="alert">
            {$tr(
              "Token must be at least 10 characters and must not contain spaces, commas, or Bearer prefix",
            )}
          </p>
        {:else}
          <p class="text-[11px] text-[var(--fp-dim)]">
            {tokenValid === true
              ? $tr("Valid format")
              : $tr(
                  "UUID or session token from ~/.config/codebuff/credentials.json",
                )}
          </p>
        {/if}
      </form>
    </Card>
    <TokenTable
      tokens={data?.tokens ?? []}
      tokenCount={data?.token_count ?? 0}
      {loading}
      {error}
      {expandedToken}
      {actionPending}
      {now}
      {devToolsEnabled}
      bind:spawnModels
      onToggle={toggleExpand}
      onAction={handleTokenAction}
      onSpawn={handleSpawn}
      onRefresh={handleRefresh}
      onDropSession={handleDropSession}
      onSwap={handleSwap}
      onMove={handleMove}
      onRetry={() => {
        error = "";
        refreshTokens();
      }}
    />
    {#if data?.show_bridge && data?.bridge_token_cards?.length > 0}
      <Card
        title={$tr("Bridge Clients")}
        description={$tr(
          "{count} active bridge client(s) relaying their own FreeBuff tokens",
          { count: data.bridge_token_cards.length },
        )}
        pad="none"
      >
        <div class="flex flex-col gap-3 p-4">
          {#each data.bridge_token_cards as bc (bc.key)}
            <BridgeTokenCard card={bc} {now} />
          {/each}
        </div>
      </Card>
    {/if}
  {:else if tab === "controls"}
    {#if $settingsDegraded}
      <Alert tone="warning" title={$tr("DB overlay unavailable")}>
        {$tr(
          "The settings store is offline — per-key saves are disabled. Changes cannot be saved right now.",
        )}
      </Alert>
    {/if}
    <StrategyPresetCard
      formValues={$settingsFormValues}
      onField={setSettingsField}
      rawText={$settingsRawText}
      sources={$settingsSources}
      onReset={resetSettingsKey}
      onSaved={settingsOverlaySaved}
      degraded={$settingsDegraded}
      tokenCount={data?.token_count ?? (data?.tokens ?? []).length}
    />
    <TrafficSettings
      cardTitle="Pool Controls"
      formValues={$settingsFormValues}
      rawText={$settingsRawText}
      onField={setSettingsField}
      sources={$settingsSources}
      onReset={resetSettingsKey}
      onSaved={settingsOverlaySaved}
      degraded={$settingsDegraded}
    />
    <PoolCustomAdvanced
      meta={$settingsMeta}
      formValues={$settingsFormValues}
      rawText={$settingsRawText}
      onField={setSettingsField}
      sources={$settingsSources}
      onReset={resetSettingsKey}
      onSaved={settingsOverlaySaved}
      degraded={$settingsDegraded}
      tokenCount={data?.token_count ?? (data?.tokens ?? []).length}
    />
    <AdvancedSettings
      meta={$settingsMeta}
      formValues={$settingsFormValues}
      rawText={$settingsRawText}
      onField={setSettingsField}
      sources={$settingsSources}
      onReset={resetSettingsKey}
      onSaved={settingsOverlaySaved}
      onlyGroups={["pool"]}
      cardTitle="Pool Tuning"
      cardDescription="Pool sizing, sessions, streak maintenance, and quota probing."
      {bridgePossible}
      degraded={$settingsDegraded}
    />
  {:else if tab === "warming"}
    <MaturityPanel
      formValues={$settingsFormValues}
      onField={setSettingsField}
      sources={$settingsSources}
      onReset={resetSettingsKey}
      onSaved={settingsOverlaySaved}
      degraded={$settingsDegraded}
    />
  {/if}
</PageShell>
