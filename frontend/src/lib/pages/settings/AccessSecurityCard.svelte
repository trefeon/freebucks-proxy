<script>
  import { onMount } from "svelte";
  import { Shield, Key, Eye, EyeOff, Lock } from "@lucide/svelte";
  import SettingsCard from "../../components/SettingsCard.svelte";
  import SettingsRow from "../../components/SettingsRow.svelte";
  import ToggleSwitch from "../../components/ToggleSwitch.svelte";
  import DbOverrideSave from "../../components/DbOverrideSave.svelte";
  import Button from "../../components/Button.svelte";
  import Alert from "../../components/Alert.svelte";
  import { postAPI, fetchAPI } from "../../api/client.js";
  import { adminApi } from "../../api/paths.js";
  import { tr } from "../../i18n.js";
  import { updateAuthState } from "../../stores/session.js";
  import { parseEnv } from "../../utils/env.js";

  /**
   * Access and Security card: merges the old SecurityCard password form with
   * the RequireLoginCard DASHBOARD_REQUIRE_LOGIN row into one SettingsCard.
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
   * @prop {boolean} [isDefaultAdminToken]
   * @prop {boolean} [hasPassword]
   * @prop {(() => void) | null} [onPasswordSuccess] - parent refetch after a
   *   password change
   * @prop {boolean} [degraded=false] - settings store offline: the login
   *   row renders an honest offline note and stays read-only for saves
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
    isDefaultAdminToken = $bindable(false),
    hasPassword = $bindable(true),
    onPasswordSuccess = null,
    degraded = false,
  } = $props();

  // --- Password form state (from SecurityCard, unchanged logic) ---
  let currentPassword = $state("");
  let newPassword = $state("");

  let showCurrentPassword = $state(false);
  let showNewPassword = $state(false);

  let submitting = $state(false);
  let errorMsg = $state("");
  let successMsg = $state("");

  onMount(async () => {
    try {
      const data = await fetchAPI(adminApi.authStatus);
      if (data) {
        if (data.is_default_admin_token !== undefined) {
          isDefaultAdminToken = Boolean(data.is_default_admin_token);
        }
        if (data.has_password !== undefined) {
          hasPassword = Boolean(data.has_password);
        }
        updateAuthState({
          isDefaultAdminToken: Boolean(data.is_default_admin_token),
          hasPassword: Boolean(data.has_password),
        });
      }
    } catch {}
  });
  let canSubmit = $derived.by(() => {
    if (hasPassword && !currentPassword.trim()) return false;
    if (!newPassword || newPassword.length < 6) return false;
    if (newPassword === "123456") return false;
    return true;
  });

  async function handleSubmit(e) {
    e.preventDefault();
    errorMsg = "";
    successMsg = "";

    if (hasPassword && !currentPassword.trim()) {
      errorMsg = $tr("Please enter your current password.");
      return;
    }
    if (newPassword.length < 6) {
      errorMsg = $tr("New password must be at least 6 characters.");
      return;
    }
    if (newPassword === "123456") {
      errorMsg = $tr("New password cannot be the default password (123456).");
      return;
    }
    submitting = true;

    try {
      const res = await postAPI(adminApi.changePassword, {
        current_password: hasPassword ? currentPassword.trim() : "",
        new_password: newPassword.trim(),
      });

      if (res.ok) {
        successMsg = res.message || $tr("Admin password updated successfully!");
        currentPassword = "";
        newPassword = "";
        isDefaultAdminToken = false;
        hasPassword = true;
        updateAuthState({
          isDefaultAdminToken: false,
          hasPassword: true,
        });
        onPasswordSuccess?.();
      } else {
        errorMsg = res.message || $tr("Failed to update password.");
      }
    } catch (err) {
      errorMsg =
        err.message || $tr("Could not update password. Check connection.");
    } finally {
      submitting = false;
    }
  }

  // --- Require-login row state (from RequireLoginCard, unchanged logic) ---
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
  // The password form carries no catalog key: it stays visible unless the
  // query clearly targets something else, so searching "password" still
  // finds this card while key search keeps exact row semantics.
  let showPassword = $derived(
    !q || hit("password", "security", "access", "admin password"),
  );
  let cardVisible = $derived(showLogin || showPassword);
  let visible = $derived((showLogin ? 1 : 0) + (showPassword ? 1 : 0));
  $effect(() => {
    onMatchCount?.(visible);
  });
</script>

{#if !q || cardVisible}
  <SettingsCard
    title={$tr("Access and Security")}
    description={$tr(
      "Admin password and who can open the dashboard on this machine.",
    )}
  >
    {#snippet icon()}
      <Shield size={20} />
    {/snippet}
    {#snippet actions()}
      {#if q}
        <span
          role="status"
          class="text-[11px] font-mono text-[var(--fp-dim)] shrink-0"
          >{$tr("{visible} of {total}", { visible, total: 2 })}</span
        >
      {/if}
    {/snippet}

    {#if showPassword}
      <div class="flex flex-col gap-4 {showLogin ? 'pb-5' : ''}">
        <div class="flex items-center gap-2 text-[var(--fp-dim)]">
          <Key size={14} />
          <h3 class="text-xs font-semibold uppercase tracking-wider">
            {$tr("Admin password")}
          </h3>
        </div>
        <div class="flex flex-col gap-4 pt-3">
          <!-- Password Change Form -->
          <form onsubmit={handleSubmit} class="flex flex-col gap-4">
            {#if hasPassword}
              <div class="flex flex-col gap-2">
                <div class="flex items-center justify-between">
                  <label
                    for="sec-current-password"
                    class="text-xs sm:text-sm font-medium text-[var(--fp-text)]"
                  >
                    {$tr("Current Password")}
                  </label>
                  {#if isDefaultAdminToken}
                    <span
                      class="text-[11px] text-[var(--fp-warning)] flex items-center gap-1 font-mono"
                    >
                      {$tr("(Default: 123456)")}
                    </span>
                  {/if}
                </div>
                <div class="relative">
                  <input
                    id="sec-current-password"
                    type={showCurrentPassword ? "text" : "password"}
                    bind:value={currentPassword}
                    placeholder={$tr("Enter current password")}
                    class="fp-input pr-10"
                    autocomplete="current-password"
                    disabled={submitting}
                  />
                  <button
                    type="button"
                    class="absolute right-2.5 top-1/2 -translate-y-1/2 text-[var(--fp-dim)] hover:text-[var(--fp-text)] p-1 rounded transition-colors"
                    onclick={() => (showCurrentPassword = !showCurrentPassword)}
                    aria-label={showCurrentPassword
                      ? $tr("Hide password")
                      : $tr("Show password")}
                  >
                    {#if showCurrentPassword}
                      <EyeOff size={16} />
                    {:else}
                      <Eye size={16} />
                    {/if}
                  </button>
                </div>
              </div>
            {/if}

            <div class="flex flex-col gap-2">
              <label
                for="sec-new-password"
                class="text-xs sm:text-sm font-medium text-[var(--fp-text)]"
              >
                {$tr("New Password")}
              </label>
              <div class="relative">
                <input
                  id="sec-new-password"
                  type={showNewPassword ? "text" : "password"}
                  bind:value={newPassword}
                  placeholder={$tr("Enter new password")}
                  class="fp-input pr-10"
                  autocomplete="new-password"
                  disabled={submitting}
                />
                <button
                  type="button"
                  class="absolute right-2.5 top-1/2 -translate-y-1/2 text-[var(--fp-dim)] hover:text-[var(--fp-text)] p-1 rounded transition-colors"
                  onclick={() => (showNewPassword = !showNewPassword)}
                  aria-label={showNewPassword
                    ? $tr("Hide password")
                    : $tr("Show password")}
                >
                  {#if showNewPassword}
                    <EyeOff size={16} />
                  {:else}
                    <Eye size={16} />
                  {/if}
                </button>
              </div>
              {#if newPassword && newPassword.length < 6}
                <p class="text-[11px] text-[var(--fp-warning)]">
                  {$tr("Minimum 6 characters")}
                </p>
              {:else if newPassword === "123456"}
                <p class="text-[11px] text-[var(--fp-error)]">
                  {$tr("Cannot be factory default (123456)")}
                </p>
              {/if}
            </div>
            {#if errorMsg}
              <Alert tone="error">{errorMsg}</Alert>
            {/if}
            {#if successMsg}
              <Alert tone="success">{successMsg}</Alert>
            {/if}

            <div class="pt-2">
              <Button
                type="submit"
                variant="primary"
                loading={submitting}
                disabled={!canSubmit || submitting}
              >
                <Key size={14} />
                {$tr("Update Password")}
              </Button>
            </div>
          </form>
        </div>
      </div>
    {/if}

    {#if showLogin}
      <div class={showPassword ? "border-t border-border-subtle pt-5" : ""}>
        <div class="flex items-center gap-2 text-[var(--fp-dim)]">
          <Lock size={14} />
          <h3 class="text-xs font-semibold uppercase tracking-wider">
            {$tr("Dashboard access")}
          </h3>
        </div>
        <div class="pt-1">
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
            {/snippet}
            {#snippet extra()}
              <DbOverrideSave
                settingKey="DASHBOARD_REQUIRE_LOGIN"
                value={formValues.DASHBOARD_REQUIRE_LOGIN ?? "true"}
                source={sources.DASHBOARD_REQUIRE_LOGIN}
                {onReset}
                {onSaved}
                {degraded}
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
        </div>
      </div>
    {/if}
  </SettingsCard>
{/if}
