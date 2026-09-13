<script>
  import SettingsCard from "../../components/SettingsCard.svelte";
  import SettingsRow from "../../components/SettingsRow.svelte";
  import ToggleSwitch from "../../components/ToggleSwitch.svelte";
  import DbBadge from "../../components/DbOverrideBadge.svelte";
  import DbOverrideSave from "../../components/DbOverrideSave.svelte";
  import { Lock } from "@lucide/svelte";
  import { tr } from "../../i18n.js";
  import { parseEnv } from "../../utils/env.js";

  /**
   * Dashboard access card: the DASHBOARD_REQUIRE_LOGIN knob as a curated
   * bool row (same ToggleSwitch + per-row save + reset pattern as the
   * GatewaySettings BRIDGE_ENABLED row, no dedicated endpoint).
   *
   * @prop {Record<string, string>} formValues
   * @prop {string} rawText
   * @prop {(key: string, value: string) => void} onField
   * @prop {Record<string, string>} [sources] - ADR-0019 source tiers
   * @prop {(key: string) => Promise<void>} [onReset] - saved-value reset
   * @prop {(() => Promise<void>) | null} [onSaved] - parent refetch after a
   *   per-key save
   * @prop {string} [query] - settings key-search text; hides non-matching rows
   * @prop {(n: number) => void} [onMatchCount] - reports the visible-row count to the parent
   *   global empty state
   */
  let {
    formValues,
    rawText = "",
    onField,
    sources = {},
    onReset = null,
    onSaved = null,
    query = "",
    onMatchCount = null,
  } = $props();

  let env = $derived(parseEnv(rawText));
  let requireLogin = $derived(formValues.DASHBOARD_REQUIRE_LOGIN !== "false");

  const LOGIN_LABEL = "Require login for dashboard";
  const LOGIN_DESC =
    "When on, opening the dashboard asks for the admin password first. When off, this machine opens the dashboard directly with no login screen.";

  let q = $derived(query.trim().toLowerCase());
  function hit(...parts) {
    if (!q) return true;
    return parts.join("\n").toLowerCase().includes(q);
  }
  let showLogin = $derived(
    hit("DASHBOARD_REQUIRE_LOGIN", LOGIN_LABEL, LOGIN_DESC),
  );
  let visible = $derived(showLogin ? 1 : 0);
  $effect(() => {
    onMatchCount?.(visible);
  });
</script>

{#if !q || visible > 0}
  <SettingsCard
    title={$tr("Dashboard access")}
    description={$tr(
      "Who can open the dashboard on this machine: with login or directly.",
    )}
  >
    {#snippet icon()}
      <Lock size={20} />
    {/snippet}
    {#snippet actions()}
      {#if q}
        <span
          role="status"
          class="text-[11px] font-mono text-[var(--fp-dim)] shrink-0"
          >{$tr("{visible} of {total}", { visible, total: 1 })}</span
        >
      {/if}
    {/snippet}

    {#if showLogin}
      <SettingsRow
        first
        last
        label={$tr(LOGIN_LABEL)}
        description={$tr(LOGIN_DESC)}
      >
        {#snippet badge()}
          <code
            class="text-[10px] px-1.5 py-0.5 rounded bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-mono"
            >DASHBOARD_REQUIRE_LOGIN</code
          >
          {#if !env.DASHBOARD_REQUIRE_LOGIN}
            <span
              class="text-[10px] px-1.5 py-0.5 rounded-[var(--fp-radius-sm)] border border-[var(--fp-border)] bg-[var(--fp-surface-2)] text-[var(--fp-dim)] font-semibold uppercase tracking-wider shrink-0"
              >{$tr("default")}</span
            >
          {/if}
          {#if sources.DASHBOARD_REQUIRE_LOGIN === "db"}
            <DbBadge settingKey="DASHBOARD_REQUIRE_LOGIN" {onReset} />
          {/if}
        {/snippet}
        {#snippet extra()}
          <DbOverrideSave
            settingKey="DASHBOARD_REQUIRE_LOGIN"
            value={formValues.DASHBOARD_REQUIRE_LOGIN ?? "true"}
            {onSaved}
          />
        {/snippet}

        <div class="flex items-center gap-2.5">
          <ToggleSwitch
            checked={requireLogin}
            ariaLabel="DASHBOARD_REQUIRE_LOGIN"
            onchange={(v) =>
              onField("DASHBOARD_REQUIRE_LOGIN", v ? "true" : "false")}
          />
        </div>
      </SettingsRow>
    {/if}
  </SettingsCard>
{/if}
