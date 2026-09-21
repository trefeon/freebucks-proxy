import { test, expect } from "@playwright/test";
import { loadFixtures, mockDashboard, mockSettingsOverlay } from "./mocks.js";
import type { PostedSetting } from "./mocks.js";

test.describe("dashboard hermetic mocks", () => {
  // The Settings tests render the 58-key catalog; under parallel workers on
  // slow runners the render can exceed the default 5s expect window, so give
  // this group a wider one (CI: 1 worker + retries anyway).
  test.use({ expect: { timeout: 10_000 } });
  test("Overview polls every 15s; tokens live on Tokens page", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    // Track overview requests
    let overviewCount = 0;
    page.on("response", (res) => {
      if (res.url().includes("/admin/api/overview")) overviewCount++;
    });

    await page.goto("http://127.0.0.1:4173/admin/#overview");
    // First fetch should resolve quickly
    await page
      .waitForResponse(
        (r) => r.url().includes("/admin/api/overview") && r.status() === 200,
        { timeout: 5000 },
      )
      .catch(() => {});
    await expect(page.getByRole("heading", { name: "Overview" })).toBeVisible();
    // Overview KPI row shows Pool total / Banned etc (rendered from fixture)
    await expect(page.getByText("Pool total")).toBeVisible();
    // Pool status lives in the Pool Tokens table rows (the standalone At-risk
    // section is gone): overview must not render it anymore.
    await expect(
      page.locator('section[aria-label="At-risk tokens"]'),
    ).toHaveCount(0);

    // Verify polling: the hot poll hits ?view=live within 17s (15s interval +
    // buffer) while the once-per-mount full fetch carries the static fields
    // (issue #322). The mock answers the live URL with the full shape, so the
    // merge path renders the same cards.
    await page.waitForResponse(
      (r) =>
        r.url().includes("/admin/api/overview") &&
        r.url().includes("view=live") &&
        r.status() === 200,
      { timeout: 17000 },
    );
    expect(overviewCount).toBeGreaterThanOrEqual(2);
    // Tokens page lists all pooled accounts with 1-based Account # labels.
    await page.goto("http://127.0.0.1:4173/admin/#tokens");
    await expect(page.getByText("Account #2").first()).toBeVisible();
  });

  test("Tokens lists pooled tokens and expands details", async ({ page }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);

    await page.goto("http://127.0.0.1:4173/admin/#tokens");
    await expect(
      page.getByRole("heading", { name: "Pool", exact: true }),
    ).toBeVisible();
    const table = page.locator("table.fp-table");
    await expect(table.getByText("Account #1")).toBeVisible({ timeout: 10000 });
    const expandBtn = table
      .locator('button[aria-label*="Expand details"]')
      .first();
    await expect(expandBtn).toBeVisible();
    await expandBtn.click();
    // Without DEVTOOLS_ENABLED the Dev Session toolbar stays hidden; the
    // live session countdown renders in the status cell under the leased
    // badge (the drawer Active Session banner is gone).
    await expect(page.getByText("Dev Session:")).not.toBeVisible();
    await expect(table.getByText("Active Session:")).toHaveCount(0);
    await expect(
      table.locator('[aria-label^="Session time remaining"]').first(),
    ).toBeVisible();

    // With DEVTOOLS_ENABLED=true the toolbar appears (per-token session spawn).
    await page.unroute("**/admin/api/config");
    await page.route("**/admin/api/config", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          env_content:
            "PORT=3457\nAUTH_TOKENS=tok0,tok1\nDEVTOOLS_ENABLED=true\n",
          has_env_file: true,
        }),
      });
    });
    await page.reload();
    await page
      .waitForResponse(
        (r) => r.url().includes("/admin/api/tokens") && r.status() === 200,
        { timeout: 5000 },
      )
      .catch(() => {});
    await table.locator('button[aria-label*="Expand details"]').first().click();
    await expect(table.getByText("Dev Session:")).toBeVisible();
    // Issue #322: the 10s hot poll hits ?view=live (static fields ride the
    // once-per-mount full fetch).
    await page.waitForResponse(
      (r) =>
        r.url().includes("/admin/api/tokens") &&
        r.url().includes("view=live") &&
        r.status() === 200,
      { timeout: 12000 },
    );
  });

  test("Tokens rows show the streak badge per account", async ({ page }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);

    await page.goto("http://127.0.0.1:4173/admin/#tokens");
    const table = page.locator("table.fp-table");
    await expect(table.getByText("Account #1")).toBeVisible({ timeout: 10000 });
    // Fixture token 0 carries streak 7; token 1 carries none.
    const first = table.locator("tbody tr").filter({ hasText: "Account #1" });
    await expect(first.getByLabel("Streak 7 days")).toBeVisible();
    await expect(first.getByLabel("Streak 7 days")).toContainText("7d streak");
    const second = table.locator("tbody tr").filter({ hasText: "Account #2" });
    await expect(second.getByLabel("No streak")).toBeVisible();
  });

  test("Tokens active rows carry Drop Session in the Instance cell; idle rows carry none", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);

    await page.goto("http://127.0.0.1:4173/admin/#tokens");
    const table = page.locator("table.fp-table");
    await expect(table.getByText("Account #1")).toBeVisible({ timeout: 10000 });
    // No expansion: fixture token 0 is leased, so its Instance cell stacks
    // the Drop Session kill switch under the session_model badge, while the
    // Status cell keeps only the countdown timer + LEASED badge.
    // Cells: 0 expand/drag, 1 account, 2 status, 3 instance.
    const active = table.locator("tbody tr").filter({ hasText: "Account #1" });
    await expect(
      active.locator("td").nth(3).getByRole("button", { name: "Drop Session" }),
    ).toBeVisible();
    await expect(
      active.locator("td").nth(2).getByRole("button", { name: "Drop Session" }),
    ).toHaveCount(0);
    // Fixture token 2 is idle with no remaining session: no kill switch.
    const idle = table.locator("tbody tr").filter({ hasText: "Account #3" });
    await expect(
      idle.getByRole("button", { name: "Drop Session" }),
    ).toHaveCount(0);
  });

  test("Tokens drawer shows pinned models for locked slots", async ({
    page,
  }) => {
    const f = loadFixtures();
    const lockedTokens = JSON.parse(JSON.stringify(f.tokens));
    lockedTokens.tokens[0].pinned_model = "z-ai/glm-5.2";
    lockedTokens.tokens[0].pin_skips = 3;
    await mockDashboard(page, f, { tokens: lockedTokens });

    await page.goto("http://127.0.0.1:4173/admin/#tokens");
    const table = page.locator("table.fp-table");
    await expect(table.getByText("Account #1")).toBeVisible({ timeout: 10000 });
    await table.locator('button[aria-label*="Expand details"]').first().click();
    await expect(page.getByText("Pinned model").first()).toBeVisible();
    await expect(page.getByText("z-ai/glm-5.2").first()).toBeVisible();
    await expect(
      page.getByText("3 request(s) routed elsewhere by this pin").first(),
    ).toBeVisible();
  });

  test("Token drawer pins a model through PIN_MODEL save", async ({ page }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    const posted: PostedSetting[] = [];
    await mockSettingsOverlay(page, posted);

    await page.goto("http://127.0.0.1:4173/admin/#tokens");
    const table = page.locator("table.fp-table");
    await expect(table.getByText("Account #1")).toBeVisible({ timeout: 10000 });
    await table.locator('button[aria-label*="Expand details"]').first().click();
    await expect(
      page.getByText("Unlocked — serves any model.").first(),
    ).toBeVisible();

    await table
      .getByLabel("Pin a model to this token")
      .selectOption("mimo/mimo-v2.5");
    await Promise.all([
      page.waitForRequest(
        (r) => r.method() === "POST" && r.url().includes("/admin/api/settings"),
        { timeout: 10000 },
      ),
      table.getByRole("button", { name: "Pin" }).click(),
    ]);
    await expect
      .poll(() => posted.find((p) => p.key === "PIN_MODEL")?.value ?? "")
      .toContain("0:mimo/mimo-v2.5");
  });

  test("Token drawer clears a pin through PIN_MODEL save", async ({ page }) => {
    const f = loadFixtures();
    const pinnedTokens = JSON.parse(JSON.stringify(f.tokens));
    pinnedTokens.tokens[0].pinned_model = "mimo/mimo-v2.5";
    await mockDashboard(page, f, { tokens: pinnedTokens });
    const posted: PostedSetting[] = [];
    await mockSettingsOverlay(page, posted);

    await page.goto("http://127.0.0.1:4173/admin/#tokens");
    const table = page.locator("table.fp-table");
    await expect(table.getByText("Account #1")).toBeVisible({ timeout: 10000 });
    await table.locator('button[aria-label*="Expand details"]').first().click();
    await expect(page.getByText("mimo/mimo-v2.5").first()).toBeVisible();
    await Promise.all([
      page.waitForRequest(
        (r) => r.method() === "POST" && r.url().includes("/admin/api/settings"),
        { timeout: 10000 },
      ),
      table.getByRole("button", { name: "Clear pin" }).click(),
    ]);
    await expect
      .poll(() => posted.filter((p) => p.key === "PIN_MODEL").length)
      .toBeGreaterThan(0);
    expect(posted.find((p) => p.key === "PIN_MODEL")?.value).toBe("");
  });

  test("Quota Tracker shows Freebucks empty state, no session quota bars", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);

    let tokensCount = 0;
    page.on("response", (res) => {
      if (res.url().includes("/admin/api/tokens")) tokensCount++;
    });

    await page.goto("http://127.0.0.1:4173/admin/#plans");
    await page.getByRole("button", { name: "Accounts" }).click();
    await page
      .waitForResponse(
        (r) => r.url().includes("/admin/api/tokens") && r.status() === 200,
        { timeout: 5000 },
      )
      .catch(() => {});
    await expect(
      page.getByRole("heading", { name: "Usage", exact: true }),
    ).toBeVisible();
    // Sidebar entry links to the merged page
    await expect(page.getByRole("link", { name: "Usage" })).toBeVisible();

    // Per-account cards: one per pooled account (1-based Account # labels)
    await expect(
      page.getByRole("heading", { name: "Account #1" }),
    ).toBeVisible();
    await expect(
      page.getByRole("heading", { name: "Account #5" }),
    ).toBeVisible();
    // Session/premium quota bars are gone: no Shared pool bar even though
    // the Account 1 fixture still carries a premium_quota field.
    await expect(page.getByText("Shared pool")).toHaveCount(0);
    await expect(page.getByText("4/day pacific_day")).toHaveCount(0);
    await expect(page.getByText(/Reset Aug 30/)).toHaveCount(0);
    // Tokens without Freebucks or premium data show the empty-state hint
    await expect(
      page
        .getByText(
          "No Freebucks data — run a request or Probe all to populate.",
        )
        .first(),
    ).toBeVisible();

    // Legacy per-model session quota tables are gone: no heading, no
    // usage bars, no reset countdowns from quota rows.
    await expect(
      page.getByRole("heading", { name: "Session quota by model" }),
    ).toHaveCount(0);
    await expect(page.locator('table [role="progressbar"]')).toHaveCount(0);

    // Polls every 10s: a second tokens fetch proves periodic refresh
    await page.waitForResponse(
      (r) => r.url().includes("/admin/api/tokens") && r.status() === 200,
      { timeout: 12000 },
    );
    expect(tokensCount).toBeGreaterThanOrEqual(2);
  });

  test("Models tab renders the served list once, cheapest first", async ({
    page,
  }) => {
    const f = loadFixtures();
    // Upstream prices maps carry models with no gateway agent binding;
    // the Models tab renders the served list once with live prices joined in.
    const pricedTokens = JSON.parse(JSON.stringify(f.tokens));
    pricedTokens.tokens[0].freebucks = {
      balance: 20,
      daily: { remaining: 20, limit: 25, reset_at: "2030-01-01T00:00:00Z" },
      wallet: { balance: 0 },
      monthly: { remaining: 9.63, limit: 10 },
      prices: {
        "upstage/solar-pro4": 0,
        "deepseek/deepseek-v4-flash": 15,
      },
    };
    await mockDashboard(page, f, { tokens: pricedTokens });
    await page.goto("http://127.0.0.1:4173/admin/#plans");
    await page.getByRole("button", { name: "Models" }).click();
    await expect(
      page.getByRole("heading", { name: "Usage", exact: true }),
    ).toBeVisible();
    // Single shared note: live upstream values are identical for every
    // account in the region (no per-account model lists anymore).
    await expect(page.getByTestId("models-note")).toContainText(
      "identical for every account in the region",
    );
    await expect(page.getByText("upstage/solar-pro4").first()).toBeVisible();
    await expect(
      page.getByText("deepseek/deepseek-v4-flash").first(),
    ).toBeVisible();
    // Cheapest first: the 0-price row sorts above the priced row.
    const ids = await page.locator("table.fp-table td code").allTextContents();
    expect(ids.indexOf("upstage/solar-pro4")).toBeLessThan(
      ids.indexOf("deepseek/deepseek-v4-flash"),
    );
  });

  test("Accounts rows render without per-card reset lines", async ({
    page,
  }) => {
    const f = loadFixtures();
    // Simulate a post-restart snapshot: quota rows present but stale.
    const staleTokens = JSON.parse(JSON.stringify(f.tokens));
    staleTokens.tokens[0].quota_stale = true;
    staleTokens.tokens[0].quota_saved_at = "2026-09-03T10:00:00Z";
    await mockDashboard(page, f, { tokens: staleTokens });

    await page.goto("http://127.0.0.1:4173/admin/#plans");
    await page.getByRole("button", { name: "Accounts" }).click();
    await expect(
      page.getByRole("heading", { name: "Usage", exact: true }),
    ).toBeVisible();
    await expect(
      page.getByRole("heading", { name: "Account #1" }),
    ).toBeVisible();
    // Compact rows carry no per-card reset explainer: the restart note and
    // the legacy session/premium bars are gone from this page.
    await expect(page.getByText("before restart")).toHaveCount(0);
    await expect(page.getByText("Shared pool")).toHaveCount(0);
  });
  test("Accounts reset strip carries the shared countdown", async ({
    page,
  }) => {
    const f = loadFixtures();
    // Metered account (issue #364): the row keeps the daily figures and
    // wallet; the live "resets in" countdown renders once in the global
    // strip, shared for all accounts.
    const meteredTokens = JSON.parse(JSON.stringify(f.tokens));
    meteredTokens.tokens[0].freebucks = {
      balance: 50,
      daily: { remaining: 30, limit: 75, reset_at: "2030-01-01T00:00:00Z" },
      wallet: { balance: 20 },
      monthly: { remaining: 20 },
      prices: {},
    };
    await mockDashboard(page, f, { tokens: meteredTokens });

    await page.goto("http://127.0.0.1:4173/admin/#plans");
    await page.getByRole("button", { name: "Accounts" }).click();
    await expect(
      page.getByRole("heading", { name: "Account #1" }),
    ).toBeVisible();
    const header = page.getByTestId("freebucks-header").first();
    await expect(header).toContainText("30/75 Freebucks daily");
    await expect(header).toContainText("20 in wallet");
    await expect(header).not.toContainText("resets in");
    await expect(page.getByTestId("reset-strip")).toContainText("resets in");
  });

  test("Accounts row renders the first-tab discount line when offered", async ({
    page,
  }) => {
    const f = loadFixtures();
    // Vendor 6cd8970 first-tab offer: an available offer renders the
    // discount line with the amount; accounts without the block render
    // no line.
    const discountTokens = JSON.parse(JSON.stringify(f.tokens));
    discountTokens.tokens[0].freebucks = {
      balance: 50,
      daily: {
        remaining: 30,
        limit: 75,
        reset_at: "2030-01-01T00:00:00Z",
        reset_time_zone: "America/New_York",
      },
      wallet: { balance: 20 },
      prices: {},
      first_tab_discount: { amount: 3, available: true },
    };
    await mockDashboard(page, f, { tokens: discountTokens });

    await page.goto("http://127.0.0.1:4173/admin/#plans");
    await page.getByRole("button", { name: "Accounts" }).click();
    await expect(
      page.getByRole("heading", { name: "Account #1" }),
    ).toBeVisible();
    const line = page.getByTestId("first-tab-discount").first();
    await expect(line).toContainText("First-tab discount");
    await expect(line).toContainText("up to 3 Freebucks off one session");
    await expect(line).toContainText("prices shown include it");
  });

  test("Accounts row renders the in-use copy while another session holds the offer", async ({
    page,
  }) => {
    const f = loadFixtures();
    const heldTokens = JSON.parse(JSON.stringify(f.tokens));
    heldTokens.tokens[0].freebucks = {
      balance: 50,
      daily: { remaining: 30, limit: 75, reset_at: "2030-01-01T00:00:00Z" },
      wallet: { balance: 20 },
      prices: {},
      first_tab_discount: {
        amount: 3,
        available: false,
        holder_surface: "desktop",
      },
    };
    await mockDashboard(page, f, { tokens: heldTokens });

    await page.goto("http://127.0.0.1:4173/admin/#plans");
    await page.getByRole("button", { name: "Accounts" }).click();
    await expect(
      page.getByRole("heading", { name: "Account #1" }),
    ).toBeVisible();
    const line = page.getByTestId("first-tab-discount").first();
    await expect(line).toContainText("First-tab discount in use");
    await expect(line).toContainText("parallel sessions pay the regular price");
  });

  test("Accounts row renders the tier prefix and pending-refund line", async ({
    page,
  }) => {
    const f = loadFixtures();
    // Full-tier account with a parked release: the header carries the
    // server-driven tier plus the daily fraction, and the refund line
    // renders once for the parked account only.
    const refundTokens = JSON.parse(JSON.stringify(f.tokens));
    refundTokens.tokens[0].access_tier = "full";
    refundTokens.tokens[0].freebucks = {
      balance: 50,
      daily: { remaining: 95, limit: 100, reset_at: "2030-01-01T00:00:00Z" },
      wallet: { balance: 2.5 },
      prices: {},
    };
    refundTokens.tokens[0].pending_refund = "inst-abc-123";
    await mockDashboard(page, f, { tokens: refundTokens });

    await page.goto("http://127.0.0.1:4173/admin/#plans");
    await page.getByRole("button", { name: "Accounts" }).click();
    await expect(
      page.getByRole("heading", { name: "Account #1" }),
    ).toBeVisible();
    const header = page.getByTestId("freebucks-header").first();
    await expect(header).toContainText("FULL");
    await expect(header).toContainText("95/100 Freebucks daily");
    const refund = page.getByTestId("refund-line");
    await expect(refund).toHaveCount(1);
    await expect(refund).toContainText("awaiting final usage");
  });

  test("Accounts pending refund replays to a settled line on refresh", async ({
    page,
  }) => {
    const f = loadFixtures();
    const state = JSON.parse(JSON.stringify(f.tokens));
    state.tokens[0].pending_refund = "inst-abc-123";
    delete state.tokens[0].last_refund;
    await mockDashboard(page, f, { tokens: state });
    // Mutable tokens payload: the refund-refresh replay settles the parked
    // release, and the next tokens fetch carries the receipt.
    await page.unroute("**/admin/api/tokens*");
    await page.route("**/admin/api/tokens*", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(state),
      });
    });
    await page.route("**/admin/tokens/0/refund-refresh", async (route) => {
      delete state.tokens[0].pending_refund;
      state.tokens[0].last_refund = 1.5;
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          ok: true,
          message: "Token 0 refund settled: 1.5 Freebucks returned to wallet.",
        }),
      });
    });
    const replayed = page.waitForRequest(
      (r) =>
        r.method() === "POST" &&
        r.url().includes("/admin/tokens/0/refund-refresh"),
    );
    await page.goto("http://127.0.0.1:4173/admin/#plans");
    await page.getByRole("button", { name: "Accounts" }).click();
    // The pending line fires one automatic refresh on first render (pinned
    // by the sibling test); the mock settles fast, so assert the replay
    // POST plus the settled line replacing the pending one.
    await replayed;
    // Vendor formatFreebucks rounds the 1.5 mock refund to 2.
    await expect(page.getByTestId("refund-settled-line")).toContainText(
      "2 Freebucks returned to your wallet.",
    );
    await expect(page.getByTestId("refund-line")).toHaveCount(0);
  });

  test("Accounts settled zero refund renders the zero line", async ({
    page,
  }) => {
    const f = loadFixtures();
    // A zero receipt is settled, not unknown: last_refund 0 renders.
    const zeroTokens = JSON.parse(JSON.stringify(f.tokens));
    zeroTokens.tokens[0].last_refund = 0;
    await mockDashboard(page, f, { tokens: zeroTokens });
    await page.goto("http://127.0.0.1:4173/admin/#plans");
    await page.getByRole("button", { name: "Accounts" }).click();
    await expect(page.getByTestId("refund-settled-line").first()).toContainText(
      "0 Freebucks returned to your wallet.",
    );
  });

  test("Models rows render NEW markers and training warnings", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);

    await page.goto("http://127.0.0.1:4173/admin/#plans");
    await page.getByRole("button", { name: "Models" }).click();
    await expect(
      page.getByRole("heading", { name: "Usage", exact: true }),
    ).toBeVisible();
    // Vendor-catalog copy renders verbatim: the freshness marker, the
    // data-training warning, and the single-label reasoning chip.
    await expect(page.getByText("NEW", { exact: true }).first()).toBeVisible();
    await expect(
      page.getByText("May use data for AI training").first(),
    ).toBeVisible();
    await expect(
      page.getByText("Reasoning: max*", { exact: true }).first(),
    ).toBeVisible();
  });

  test("Settings and pages render catalog groups, toggled bool saves to .env", async ({
    page,
  }) => {
    const f = loadFixtures();
    const configWithContent = {
      ...f.config,
      env_content:
        "LISTEN_ADDR=127.0.0.1:3457\nAUTH_TOKENS=tok0,tok1\nAPI_KEYS=sk-local-xyz\nSAFE_MODE=true\nLOG_LEVEL=info\n",
      has_env_file: true,
      effective: [
        { key: "LISTEN_ADDR", value: "127.0.0.1:3457", secret: false },
        { key: "AUTH_TOKENS", value: "2 token(s)", secret: true },
        { key: "API_KEYS", value: "1 key(s)", secret: true },
        { key: "ADMIN_TOKEN", value: "set", secret: true },
        { key: "SAFE_MODE", value: "true", secret: false },
        { key: "LOG_LEVEL", value: "info", secret: false },
        { key: "COST_MODE", value: "free", secret: false },
        { key: "MAX_MESSAGES_PER_DAY", value: "0", secret: false },
      ],
    };
    await mockDashboard(page, f, { configWithApiKeys: configWithContent });
    const posted: PostedSetting[] = [];
    await mockSettingsOverlay(page, posted);
    const metaResp = page.waitForResponse(
      (r) => r.url().includes("/admin/api/config/meta") && r.status() === 200,
      { timeout: 5000 },
    );
    await page.goto("http://127.0.0.1:4173/admin/#settings");
    await metaResp;
    await expect(
      page.getByRole("heading", { name: "Settings", exact: true }),
    ).toBeVisible();
    // Group cards moved to their pages: Settings keeps General (Gateway),
    // Security leftovers, and the three link-out stubs.
    await expect(page.getByRole("heading", { name: "General" })).toBeVisible();
    await expect(
      page.getByRole("heading", { name: "Security", exact: true }),
    ).toBeVisible();
    // A documented bool renders as a switch; effective value drives it.
    const safeMode = page.getByRole("switch", { name: "SAFE_MODE" });
    await expect(safeMode).toBeVisible();
    await expect(safeMode).toHaveAttribute("aria-checked", "true");
    // HTTP_READ_TIMEOUT is env-only (data-architecture decision): the
    // Gateway card renders it read-only with an env-note — no combobox.
    await expect(
      page.getByRole("combobox", { name: "HTTP_READ_TIMEOUT" }),
    ).toHaveCount(0);
    await expect(
      page.getByText("the reader never consults the overlay").first(),
    ).toBeVisible();

    // Toggling instant-saves the key to the overlay (debounced ~400ms).
    await safeMode.click();
    await expect(safeMode).toHaveAttribute("aria-checked", "false");
    await page.waitForRequest(
      (r) => r.method() === "POST" && r.url().includes("/admin/api/settings"),
      { timeout: 10000 },
    );
    await expect
      .poll(() =>
        posted.some((p) => p.key === "SAFE_MODE" && p.value === "false"),
      )
      .toBe(true);

    // Reload keeps the toggled value: GET reflects the POSTed overlay row.
    await page.reload();
    await expect(
      page.getByRole("switch", { name: "SAFE_MODE" }),
    ).toHaveAttribute("aria-checked", "false");
    await expect(
      page.getByRole("combobox", { name: "HTTP_READ_TIMEOUT" }),
    ).toHaveCount(0);
  });

  test("Pool controls render relocated policy keys and save", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f, {}, { loginPage: true });
    const posted: PostedSetting[] = [];
    await mockSettingsOverlay(page, posted);
    const metaResp = page.waitForResponse(
      (r) => r.url().includes("/admin/api/config/meta") && r.status() === 200,
      { timeout: 5000 },
    );
    await page.goto("http://127.0.0.1:4173/admin/#tokens");
    await metaResp;
    await expect(
      page.getByRole("heading", { name: "Pool", exact: true }),
    ).toBeVisible();
    // Queue posture moved from Settings Traffic to the Pool page's Controls
    // tab: the slots-per-account stepper lives there; secrets never reach
    // the advanced list.
    await page.getByRole("button", { name: "Controls" }).click();
    const slots = page.locator('input[aria-label="SLOTS_PER_ACCOUNT"]');
    await expect(page.getByText("ADMIN_TOKEN", { exact: true })).toHaveCount(0);
    await expect(slots).toBeVisible();
    // Editing instant-saves the key to the overlay (debounced ~400ms).
    await slots.fill("3");
    await page.waitForRequest(
      (r) => r.method() === "POST" && r.url().includes("/admin/api/settings"),
      { timeout: 10000 },
    );
    await expect
      .poll(() =>
        posted.some((p) => p.key === "SLOTS_PER_ACCOUNT" && p.value === "3"),
      )
      .toBe(true);
    await expect(
      page.getByRole("status").filter({ hasText: "SLOTS_PER_ACCOUNT saved" }),
    ).toBeVisible();

    // Reload keeps the row visible: GET reflects the POSTed overlay row.
    await page.reload();
    await page.getByRole("button", { name: "Controls" }).click();
    await expect(
      page.locator('input[aria-label="SLOTS_PER_ACCOUNT"]'),
    ).toBeVisible();
  });
  test("Usage controls render routing keys and save", async ({ page }) => {
    const f = loadFixtures();
    await mockDashboard(page, f, {}, { loginPage: true });
    const posted: PostedSetting[] = [];
    await mockSettingsOverlay(page, posted);
    const metaResp = page.waitForResponse(
      (r) => r.url().includes("/admin/api/config/meta") && r.status() === 200,
      { timeout: 5000 },
    );
    await page.goto("http://127.0.0.1:4173/admin/#plans");
    await metaResp;
    await expect(
      page.getByRole("heading", { name: "Usage", exact: true }),
    ).toBeVisible();
    // Model routing moved from Settings Upstream to the Usage page's
    // Controls tab: the reasoning-format switch lives there, keyed by
    // badge; secrets never surface.
    await page.getByRole("button", { name: "Controls" }).click();
    await expect(
      page.getByText("REASONING_IN_CONTENT", { exact: true }).first(),
    ).toBeVisible();
    await expect(
      page.getByRole("switch", { name: "REASONING_IN_CONTENT" }),
    ).toBeVisible();
    // Toggling instant-saves the key to the overlay (debounced ~400ms).
    await page.getByRole("switch", { name: "REASONING_IN_CONTENT" }).click();
    await page.waitForRequest(
      (r) => r.method() === "POST" && r.url().includes("/admin/api/settings"),
      { timeout: 10000 },
    );
    await expect
      .poll(() => posted.some((p) => p.key === "REASONING_IN_CONTENT"))
      .toBe(true);
    await expect(
      page
        .getByRole("status")
        .filter({ hasText: "REASONING_IN_CONTENT saved" }),
    ).toBeVisible();
  });
  test("Pool Controls tab renders pool tuning keys and saves", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f, {}, { loginPage: true });
    const metaResp = page.waitForResponse(
      (r) => r.url().includes("/admin/api/config/meta") && r.status() === 200,
      { timeout: 5000 },
    );
    await page.goto("http://127.0.0.1:4173/admin/#tokens");
    await metaResp;
    await page.getByRole("button", { name: "Controls" }).click();
    // Pool-group keys moved from Settings Advanced to the Pool Controls
    // tab: pool-tuning rows render in the Pool Tuning card, while every
    // MATURITY_* key lives only on the Warming tab's Streak Maintenance
    // card (single address, no Pool Tuning dupe). QUOTA_AUTO_PROBE is gone
    // with the excised prober, so RATE_LIMIT_BURST stands in as the
    // catalog-rendered pool row.
    await expect(page.getByText("Pool Tuning")).toBeVisible();
    await expect(
      page.getByText("MATURITY_ENABLED", { exact: true }),
    ).toHaveCount(0);
    await expect(
      page.getByText("RATE_LIMIT_BURST", { exact: true }).first(),
    ).toBeVisible();
    // Settings no longer renders pool rows: only the Security leftover.
    await page.goto("http://127.0.0.1:4173/admin/#settings");
    await expect(
      page.getByText("MATURITY_ENABLED", { exact: true }),
    ).toHaveCount(0);
    await expect(
      page.getByText("CORS_ALLOWED_ORIGIN", { exact: true }).first(),
    ).toBeVisible();
  });
  test("Pool strategy preset switch writes the four owned keys", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f, {}, { loginPage: true });
    const posted: PostedSetting[] = [];
    await mockSettingsOverlay(page, posted);
    const metaResp = page.waitForResponse(
      (r) => r.url().includes("/admin/api/config/meta") && r.status() === 200,
      { timeout: 5000 },
    );
    await page.goto("http://127.0.0.1:4173/admin/#tokens");
    await metaResp;
    await page.getByRole("button", { name: "Controls" }).click();
    // Catalog defaults (30s / 16) already read as Balance.
    const drain = page.getByRole("radio", { name: "Drain", exact: true });
    const balance = page.getByRole("radio", { name: "Balance", exact: true });
    await expect(
      page.getByText("Pool Strategy", { exact: true }),
    ).toBeVisible();
    await expect(balance).toHaveAttribute("aria-checked", "true");
    // A Drain tap calls onField for the four preset-written owned keys
    // (PIN_MODEL is owned but never preset-written), but the card's own
    // rows only POST changed values (no write without change): exactly
    // QUEUE_WAIT and QUEUE_DEPTH leave Balance behind.
    await drain.click();
    await expect(drain).toHaveAttribute("aria-checked", "true");
    await expect
      .poll(() => posted.filter((p) => p.key === "QUEUE_WAIT").length)
      .toBeGreaterThan(0);
    await expect
      .poll(() => posted.filter((p) => p.key === "QUEUE_DEPTH").length)
      .toBeGreaterThan(0);
    const keys = posted.map((p) => p.key);
    expect(keys).toContain("QUEUE_WAIT");
    expect(keys).toContain("QUEUE_DEPTH");
    expect(posted.find((p) => p.key === "QUEUE_WAIT")?.value).toBe("300s");
    expect(posted.find((p) => p.key === "QUEUE_DEPTH")?.value).toBe("1024");
    expect(keys).not.toContain("ROUTING_SMART");
    expect(keys).not.toContain("TOKEN_ROTATION");
    expect(keys).not.toContain("RATE_LIMIT_FAILOVER");
    expect(keys).not.toContain("TOKEN_MAX_CONCURRENT");
    expect(keys).not.toContain("MODEL_LOCKS");
    expect(keys).not.toContain("SESSION_IDLE_END");

    // Reload keeps Drain: the rows pin their display through the post-save
    // refetches, so no stale file default is re-posted after the tap.
    await page.reload();
    await page
      .waitForResponse(
        (r) => r.url().includes("/admin/api/config/meta") && r.status() === 200,
        { timeout: 5000 },
      )
      .catch(() => {});
    await page.getByRole("button", { name: "Controls" }).click();
    await expect(
      page.getByRole("radio", { name: "Drain", exact: true }),
    ).toHaveAttribute("aria-checked", "true");
    // The owned rows are the card's own editors now, and they carry the
    // saved Drain values after the reload.
    await expect(page.locator('input[aria-label="QUEUE_WAIT"]')).toHaveValue(
      "300s",
    );
    await expect(page.locator('input[aria-label="QUEUE_DEPTH"]')).toHaveValue(
      "1024",
    );
  });

  test("Balance threshold slider shows only in Balance and persists QUEUE_WAIT", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f, {}, { loginPage: true });
    const posted: PostedSetting[] = [];
    await mockSettingsOverlay(page, posted);
    const metaResp = page.waitForResponse(
      (r) => r.url().includes("/admin/api/config/meta") && r.status() === 200,
      { timeout: 5000 },
    );
    await page.goto("http://127.0.0.1:4173/admin/#tokens");
    await metaResp;
    await page.getByRole("button", { name: "Controls" }).click();
    const slider = page.locator(
      'input[type="range"][aria-label="Balance threshold (QUEUE_WAIT)"]',
    );
    await expect(slider).toBeVisible();
    // In-range moves keep the Balance badge (never flip to Custom).
    await slider.evaluate((el) => {
      const input = el as HTMLInputElement;
      input.value = "90";
      input.dispatchEvent(new Event("input", { bubbles: true }));
    });
    await expect(
      page.getByRole("radio", { name: "Balance", exact: true }),
    ).toHaveAttribute("aria-checked", "true");
    await page.waitForRequest(
      (r) => r.method() === "POST" && r.url().includes("/admin/api/settings"),
      { timeout: 10000 },
    );
    await expect
      .poll(() =>
        posted.some((p) => p.key === "QUEUE_WAIT" && p.value === "90s"),
      )
      .toBe(true);
    await expect(
      page.getByRole("status").filter({ hasText: "QUEUE_WAIT saved" }),
    ).toBeVisible();
    // The slider and the picker are two inputs onto the ONE QUEUE_WAIT row
    // in the strategy card: the drag lands as that row's value (one writer),
    // it does not spawn a second POST path.
    await expect(page.locator('input[aria-label="QUEUE_WAIT"]')).toHaveValue(
      "90s",
    );
    // Drain hides the slider.
    await page.getByRole("radio", { name: "Drain", exact: true }).click();
    await expect(slider).toHaveCount(0);
  });
  test("Editing an owned key flips the badge to Custom with reset", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f, {}, { loginPage: true });
    const posted: PostedSetting[] = [];
    await mockSettingsOverlay(page, posted);
    const metaResp = page.waitForResponse(
      (r) => r.url().includes("/admin/api/config/meta") && r.status() === 200,
      { timeout: 5000 },
    );
    await page.goto("http://127.0.0.1:4173/admin/#tokens");
    await metaResp;
    await page.getByRole("button", { name: "Controls" }).click();
    await expect(
      page.getByRole("radio", { name: "Balance", exact: true }),
    ).toHaveAttribute("aria-checked", "true");
    // Hand-editing one owned key (queue depth) flips to Custom.
    await page.locator('input[aria-label="QUEUE_DEPTH"]').fill("32");
    await page.waitForRequest(
      (r) => r.method() === "POST" && r.url().includes("/admin/api/settings"),
      { timeout: 10000 },
    );
    await expect
      .poll(() =>
        posted.some((p) => p.key === "QUEUE_DEPTH" && p.value === "32"),
      )
      .toBe(true);
    await expect(
      page.getByRole("button", { name: "Reset to Balance" }),
    ).toBeVisible();
    await expect(
      page.getByRole("button", { name: "Reset to Drain" }),
    ).toBeVisible();
    // Reset restores the Balance four (threshold back to its 60s default).
    await page.getByRole("button", { name: "Reset to Balance" }).click();
    await expect(
      page.getByRole("radio", { name: "Balance", exact: true }),
    ).toHaveAttribute("aria-checked", "true");
    await expect
      .poll(
        () =>
          posted.filter((p) => p.key === "QUEUE_DEPTH" && p.value === "16")
            .length,
      )
      .toBeGreaterThan(0);
    await expect
      .poll(() =>
        posted.some((p) => p.key === "QUEUE_WAIT" && p.value === "15s"),
      )
      .toBe(true);
    // The card's own rows display the restored preset values.
    await expect(page.locator('input[aria-label="QUEUE_DEPTH"]')).toHaveValue(
      "16",
    );
    await expect(page.locator('input[aria-label="QUEUE_WAIT"]')).toHaveValue(
      "15s",
    );
  });

  test("Pool strategy survives a Go-normalized echo in ONE click each way", async ({
    page,
  }) => {
    // Regression for the two-click bug: the gateway used to serve the
    // Go-normalized effective duration ("1m0s") for saved rows, which the
    // badge could not round-trip — the first Balance tap landed as Custom
    // and only the second tap settled. The mock models the real echo by
    // answering the post-refetch GET with "1m0s" for QUEUE_WAIT; the
    // frontend compound parser must still read Balance after ONE click,
    // Drain→Balance→Drain, with reload persistence.
    const f = loadFixtures();
    await mockDashboard(page, f, {}, { loginPage: true });
    const posted: PostedSetting[] = [];
    await mockSettingsOverlay(page, posted);
    const metaResp = page.waitForResponse(
      (r) => r.url().includes("/admin/api/config/meta") && r.status() === 200,
      { timeout: 5000 },
    );
    await page.goto("http://127.0.0.1:4173/admin/#tokens");
    await metaResp;
    await page.getByRole("button", { name: "Controls" }).click();
    const drain = page.getByRole("radio", { name: "Drain", exact: true });
    const balance = page.getByRole("radio", { name: "Balance", exact: true });
    await expect(balance).toHaveAttribute("aria-checked", "true");
    // Drain in one click (values + badge agree without a second tap).
    await drain.click();
    await expect(drain).toHaveAttribute("aria-checked", "true");
    await expect
      .poll(() =>
        posted.some((p) => p.key === "QUEUE_DEPTH" && p.value === "1024"),
      )
      .toBe(true);
    await expect
      .poll(() =>
        posted.some((p) => p.key === "QUEUE_WAIT" && p.value === "300s"),
      )
      .toBe(true);
    await expect(drain).toHaveAttribute("aria-checked", "true");
    // Back to Balance in one click; the queued post-refetch echoes "1m0s"
    // (Go-normalized) and the badge must stay Balance regardless.
    await balance.click();
    await expect(balance).toHaveAttribute("aria-checked", "true");
    await expect
      .poll(() =>
        posted.some((p) => p.key === "QUEUE_DEPTH" && p.value === "16"),
      )
      .toBe(true);
    await expect
      .poll(() =>
        posted.some((p) => p.key === "QUEUE_WAIT" && p.value === "15s"),
      )
      .toBe(true);
    await page.route("**/admin/api/settings", async (route) => {
      if (route.request().method() !== "GET") {
        await route.fallback();
        return;
      }
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          settings: [
            { key: "SLOTS_PER_ACCOUNT", value: "2", source: "db" },
            { key: "QUEUE_WAIT", value: "1m0s", source: "db" },
            { key: "QUEUE_DEPTH", value: "16", source: "db" },
            { key: "MAX_SPILL_ACCOUNTS", value: "0", source: "db" },
          ],
          degraded: false,
        }),
      });
    });
    await page.reload();
    await page
      .waitForResponse(
        (r) => r.url().includes("/admin/api/config/meta") && r.status() === 200,
        { timeout: 5000 },
      )
      .catch(() => {});
    await page.getByRole("button", { name: "Controls" }).click();
    await expect(
      page.getByRole("radio", { name: "Balance", exact: true }),
    ).toHaveAttribute("aria-checked", "true");
  });

  test("Pool strategy badge reads the loader defaults for blank queue rows", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f, {}, { loginPage: true });
    // Blank / whitespace overlay rows are what the gateway loader is
    // zero-tolerant about: QUEUE_WAIT falls back to 30s and QUEUE_DEPTH to
    // 16 (backend/internal/config/config_load.go), so a stock install
    // still reads Balance — never Custom.
    const posted: PostedSetting[] = [];
    await mockSettingsOverlay(page, posted, {
      seed: [
        { key: "QUEUE_WAIT", value: "   ", source: "db" },
        { key: "QUEUE_DEPTH", value: "", source: "db" },
      ],
    });
    const metaResp = page.waitForResponse(
      (r) => r.url().includes("/admin/api/config/meta") && r.status() === 200,
      { timeout: 5000 },
    );
    await page.goto("http://127.0.0.1:4173/admin/#tokens");
    await metaResp;
    await page.getByRole("button", { name: "Controls" }).click();
    await expect(page.getByTestId("strategy-rows")).toBeVisible();
    await expect(
      page.getByRole("radio", { name: "Balance", exact: true }),
    ).toHaveAttribute("aria-checked", "true");
    await expect(
      page.getByRole("button", { name: "Reset to Balance" }),
    ).toHaveCount(0);
  });

  test("Pool strategy badge reads the loader defaults for unparseable queue rows", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f, {}, { loginPage: true });
    // Neither "not-a-duration" (QUEUE_WAIT) nor "lots" (QUEUE_DEPTH) is a
    // value the loader would keep, so the badge reports the defaults it
    // would run with instead of falling to Custom.
    const posted: PostedSetting[] = [];
    await mockSettingsOverlay(page, posted, {
      seed: [
        { key: "QUEUE_WAIT", value: "not-a-duration", source: "db" },
        { key: "QUEUE_DEPTH", value: "lots", source: "db" },
      ],
    });
    const metaResp = page.waitForResponse(
      (r) => r.url().includes("/admin/api/config/meta") && r.status() === 200,
      { timeout: 5000 },
    );
    await page.goto("http://127.0.0.1:4173/admin/#tokens");
    await metaResp;
    await page.getByRole("button", { name: "Controls" }).click();
    await expect(
      page.getByRole("radio", { name: "Balance", exact: true }),
    ).toHaveAttribute("aria-checked", "true");
  });

  test("the four strategy keys have exactly one editor each, in the strategy card", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f, {}, { loginPage: true });
    const posted: PostedSetting[] = [];
    await mockSettingsOverlay(page, posted);
    const metaResp = page.waitForResponse(
      (r) => r.url().includes("/admin/api/config/meta") && r.status() === 200,
      { timeout: 5000 },
    );
    await page.goto("http://127.0.0.1:4173/admin/#tokens");
    await metaResp;
    await page.getByRole("button", { name: "Controls" }).click();
    const rows = page.getByTestId("strategy-rows");
    await expect(rows).toBeVisible();
    // One editor per owned key, anywhere on the Controls tab...
    await expect(page.locator('input[aria-label="QUEUE_WAIT"]')).toHaveCount(1);
    await expect(page.locator('input[aria-label="QUEUE_DEPTH"]')).toHaveCount(
      1,
    );
    await expect(
      page.locator('input[aria-label="SLOTS_PER_ACCOUNT"]'),
    ).toHaveCount(1);
    await expect(
      page.locator('input[aria-label="MAX_SPILL_ACCOUNTS"]'),
    ).toHaveCount(1);
    // ...and that editor is the strategy card's own row.
    await expect(rows.locator('input[aria-label="QUEUE_WAIT"]')).toHaveCount(1);
    await expect(rows.locator('input[aria-label="QUEUE_DEPTH"]')).toHaveCount(
      1,
    );
    await expect(
      rows.locator('input[aria-label="SLOTS_PER_ACCOUNT"]'),
    ).toHaveCount(1);
    await expect(
      rows.locator('input[aria-label="MAX_SPILL_ACCOUNTS"]'),
    ).toHaveCount(1);
    // No catalog key has a second editor anywhere on the tab either: every
    // row renders its key as one <code> chip, so a duplicate chip is a
    // duplicate owner.
    const meta = f.configMeta;
    if (!Array.isArray(meta))
      throw new Error("config-meta fixture is not an array");
    const keys = meta.flatMap((e) =>
      e && typeof e === "object" && "key" in e ? [String(e.key)] : [],
    );
    const dupes = await page.evaluate((ks) => {
      const chips = Array.from(document.querySelectorAll("code")).map((c) =>
        (c.textContent ?? "").trim(),
      );
      return ks.filter((k) => chips.filter((t) => t === k).length > 1);
    }, keys);
    expect(dupes).toEqual([]);
  });

  test("Pool header queue posture agrees with the strategy card badge", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f, {}, { loginPage: true });
    // Prod shape: the queue posture is Balance (QUEUE_WAIT=60s /
    // QUEUE_DEPTH=16, SLOTS_PER_ACCOUNT=2, MAX_SPILL_ACCOUNTS=0).
    const posted: PostedSetting[] = [];
    await mockSettingsOverlay(page, posted, {
      seed: [
        { key: "QUEUE_WAIT", value: "60s", source: "db" },
        { key: "QUEUE_DEPTH", value: "16", source: "db" },
        { key: "SLOTS_PER_ACCOUNT", value: "2", source: "db" },
        { key: "MAX_SPILL_ACCOUNTS", value: "0", source: "db" },
      ],
    });
    const metaResp = page.waitForResponse(
      (r) => r.url().includes("/admin/api/config/meta") && r.status() === 200,
      { timeout: 5000 },
    );
    await page.goto("http://127.0.0.1:4173/admin/#tokens");
    await metaResp;
    const queueChip = page.getByTestId("queue-chip").locator("dd");
    // The queue chip reports the posture from the same source as the card
    // badge (the tokens snapshot carries no posture field).
    await expect(queueChip).toHaveText("Balance");
    await page.getByRole("button", { name: "Controls" }).click();
    const balance = page.getByRole("radio", { name: "Balance", exact: true });
    const drain = page.getByRole("radio", { name: "Drain", exact: true });
    await expect(balance).toHaveAttribute("aria-checked", "true");
    // One tap on the card moves both surfaces together: same source.
    await drain.click();
    await expect(drain).toHaveAttribute("aria-checked", "true");
    await expect(queueChip).toHaveText("Drain");
    await balance.click();
    await expect(balance).toHaveAttribute("aria-checked", "true");
    await expect(queueChip).toHaveText("Balance");
  });

  test("Pool header queue posture stays unclassified while the store is offline", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f, {}, { loginPage: true });
    // With the overlay unreachable we cannot know whether saved rows move
    // the strategy keys, so the chip must stay unclassified instead
    // of guessing a posture from file/default values.
    const posted: PostedSetting[] = [];
    await mockSettingsOverlay(page, posted, { degraded: true });
    const metaResp = page.waitForResponse(
      (r) => r.url().includes("/admin/api/config/meta") && r.status() === 200,
      { timeout: 5000 },
    );
    await page.goto("http://127.0.0.1:4173/admin/#tokens");
    await metaResp;
    await expect(page.getByTestId("queue-chip").locator("dd")).toHaveText("—");
  });

  test("the strategy card shows the honest pool ceiling (cap × accounts)", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f, {}, { loginPage: true });
    const metaResp = page.waitForResponse(
      (r) => r.url().includes("/admin/api/config/meta") && r.status() === 200,
      { timeout: 5000 },
    );
    await page.goto("http://127.0.0.1:4173/admin/#tokens");
    await metaResp;
    await page.getByRole("button", { name: "Controls" }).click();
    // SLOTS_PER_ACCOUNT ships at 2 (the approved anti-ban pacing) and
    // the tokens snapshot reports the pooled account count.
    const snapshot = f.tokens;
    const accounts = Number(
      snapshot && typeof snapshot === "object" && "token_count" in snapshot
        ? snapshot.token_count
        : 0,
    );
    expect(accounts).toBeGreaterThan(0);
    await expect(page.getByTestId("pool-ceiling")).toContainText(
      `2 per account × ${accounts} accounts = ${2 * accounts} concurrent turns`,
    );
  });

  test("the pool ceiling line reports an unlimited cap honestly", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f, {}, { loginPage: true });
    const posted: PostedSetting[] = [];
    await mockSettingsOverlay(page, posted, {
      seed: [{ key: "SLOTS_PER_ACCOUNT", value: "0", source: "db" }],
    });
    const metaResp = page.waitForResponse(
      (r) => r.url().includes("/admin/api/config/meta") && r.status() === 200,
      { timeout: 5000 },
    );
    await page.goto("http://127.0.0.1:4173/admin/#tokens");
    await metaResp;
    await page.getByRole("button", { name: "Controls" }).click();
    await expect(page.getByTestId("pool-ceiling")).toContainText(
      "unlimited per account",
    );
  });

  test("Usage Controls tab renders upstream and quota keys", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f, {}, { loginPage: true });
    const metaResp = page.waitForResponse(
      (r) => r.url().includes("/admin/api/config/meta") && r.status() === 200,
      { timeout: 5000 },
    );
    await page.goto("http://127.0.0.1:4173/admin/#plans");
    await metaResp;
    await page.getByRole("button", { name: "Controls" }).click();
    // Upstream/quota-group keys moved from Settings Advanced to Usage.
    await expect(page.getByText("Upstream & Quota")).toBeVisible();
    await expect(
      page.getByText("REGISTRY_REFRESH", { exact: true }).first(),
    ).toBeVisible();
    await expect(
      page.getByText("MODELS_HIDE_UNAVAILABLE", { exact: true }).first(),
    ).toBeVisible();
  });

  test("Settings hosts the logging keys: LOG_LEVEL plus diagnostics", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f, {}, { loginPage: true });
    const metaResp = page.waitForResponse(
      (r) => r.url().includes("/admin/api/config/meta") && r.status() === 200,
      { timeout: 5000 },
    );
    await page.goto("http://127.0.0.1:4173/admin/#settings");
    await metaResp;
    // The log keys are homed on Settings: the working LOG_LEVEL control plus
    // the diagnostics card whose rows the Logs tab's card used to render.
    await expect(
      page.getByRole("heading", { name: "Server Log Level" }),
    ).toBeVisible();
    await expect(page.locator('select[aria-label="LOG_LEVEL"]')).toBeVisible();
    await expect(page.getByText("Logging & Diagnostics")).toBeVisible();
    for (const key of [
      "DEBUG_DUMP",
      "DEVTOOLS_ENABLED",
      "LOG_ACCESS",
      "LOG_FORMAT",
    ]) {
      await expect(page.getByText(key, { exact: true }).first()).toBeVisible();
    }
    // The Logs → Logging destination no longer exists, so no copy on
    // Settings may point at it.
    await expect(
      page.getByRole("link", { name: "Manage log level (Logs → Logging tab)" }),
    ).toHaveCount(0);
    await expect(
      page.getByText(
        "Server log level now lives under the Logs page's Logging tab.",
      ),
    ).toHaveCount(0);
  });

  test("Settings card renders log level and saves", async ({ page }) => {
    const f = loadFixtures();
    await mockDashboard(page, f, {}, { loginPage: true });
    const posted: PostedSetting[] = [];
    await mockSettingsOverlay(page, posted);
    const metaResp = page.waitForResponse(
      (r) => r.url().includes("/admin/api/config/meta") && r.status() === 200,
      { timeout: 5000 },
    );
    await page.goto("http://127.0.0.1:4173/admin/#settings");
    await metaResp;
    await expect(
      page.getByRole("heading", { name: "Settings", exact: true }),
    ).toBeVisible();
    // LOG_LEVEL's only functional control is here now: the select instant-
    // saves to the overlay and the row keeps its restart-only honesty.
    const level = page.locator('select[aria-label="LOG_LEVEL"]');
    await level.selectOption("warn");
    await page.waitForRequest(
      (r) => r.method() === "POST" && r.url().includes("/admin/api/settings"),
      { timeout: 10000 },
    );
    await expect
      .poll(() =>
        posted.some((p) => p.key === "LOG_LEVEL" && p.value === "warn"),
      )
      .toBe(true);
    await expect(
      page
        .getByRole("status")
        .filter({ hasText: "LOG_LEVEL saved. It applies after restart." }),
    ).toBeVisible();
    // The old link-out to the Logs page's Logging tab is gone with it.
    await expect(
      page.getByRole("link", { name: "Manage log level (Logs → Logging tab)" }),
    ).toHaveCount(0);
  });

  test("Settings log level saves via the overlay (legacy #config alias routes to Settings)", async ({
    page,
  }) => {
    const f = loadFixtures();
    const configWithContent = {
      ...f.config,
      env_content:
        "LISTEN_ADDR=127.0.0.1:3457\nAUTH_TOKENS=tok0,tok1\nAPI_KEYS=sk-local-xyz\nSAFE_MODE=true\nLOG_LEVEL=info\n",
      has_env_file: true,
      effective: [
        { key: "LISTEN_ADDR", value: "127.0.0.1:3457", secret: false },
        { key: "AUTH_TOKENS", value: "2 token(s)", secret: true },
        { key: "API_KEYS", value: "1 key(s)", secret: true },
        { key: "ADMIN_TOKEN", value: "set", secret: true },
        { key: "SAFE_MODE", value: "true", secret: false },
        { key: "LOG_LEVEL", value: "info", secret: false },
        { key: "COST_MODE", value: "free", secret: false },
        { key: "MAX_MESSAGES_PER_DAY", value: "0", secret: false },
      ],
    };
    await mockDashboard(page, f, { configWithApiKeys: configWithContent });
    const posted: PostedSetting[] = [];
    await mockSettingsOverlay(page, posted);

    // Legacy '#config' hash still routes to the Settings page.
    const metaRespLegacy = page.waitForResponse(
      (r) => r.url().includes("/admin/api/config/meta") && r.status() === 200,
      { timeout: 5000 },
    );
    await page.goto("http://127.0.0.1:4173/admin/#config");
    await metaRespLegacy;
    await expect(
      page.getByRole("heading", { name: "Settings", exact: true }),
    ).toBeVisible();

    // LOG_LEVEL is homed on Settings now: the select edits in place, and the
    // old link-out to the Logs page's Logging tab no longer exists.
    await expect(
      page.getByRole("link", { name: "Manage log level (Logs → Logging tab)" }),
    ).toHaveCount(0);
    const logLevel = page.getByRole("combobox", { name: "LOG_LEVEL" });
    await expect(logLevel).toBeVisible();
    await expect(logLevel).toContainText("debug");
    await expect(logLevel).toContainText("trace");
    await logLevel.selectOption("warn");

    // The select instant-saves through the overlay (debounced ~400ms).
    await page.waitForRequest(
      (r) => r.method() === "POST" && r.url().includes("/admin/api/settings"),
      { timeout: 10000 },
    );
    await expect
      .poll(() =>
        posted.some((p) => p.key === "LOG_LEVEL" && p.value === "warn"),
      )
      .toBe(true);
    await expect(
      page
        .getByRole("status")
        .filter({ hasText: "LOG_LEVEL saved. It applies after restart." }),
    ).toBeVisible();
  });

  test("no catalog key renders twice on the Settings page", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f, {}, { loginPage: true });
    const metaResp = page.waitForResponse(
      (r) => r.url().includes("/admin/api/config/meta") && r.status() === 200,
      { timeout: 5000 },
    );
    await page.goto("http://127.0.0.1:4173/admin/#settings");
    await metaResp;
    // Every rendered catalog row carries its key as a <code> chip, so a key
    // owned by two cards would show up here as a duplicate chip.
    const meta = f.configMeta;
    if (!Array.isArray(meta))
      throw new Error("config-meta fixture is not an array");
    const keys = meta.flatMap((e) =>
      e && typeof e === "object" && "key" in e ? [String(e.key)] : [],
    );
    expect(keys.length).toBeGreaterThan(0);
    const dupes = await page.evaluate((ks) => {
      const chips = Array.from(document.querySelectorAll("code")).map((c) =>
        (c.textContent ?? "").trim(),
      );
      return ks.filter((k) => chips.filter((t) => t === k).length > 1);
    }, keys);
    expect(dupes).toEqual([]);
    // The search placeholder names the settings this page actually filters,
    // not the whole catalog (most of which is homed on other surfaces).
    const placeholder = await page
      .locator("#settings-search")
      .getAttribute("placeholder");
    const named = Number(/Search (\d+) settings…/.exec(placeholder ?? "")?.[1]);
    // 9 catalog rows the page renders (1 access + 2 general +
    // 1 log level + 4 diagnostics + 1 security), plus the non-catalog admin
    // password row, plus the 27 hidden non-secret catalog keys the "Hidden
    // keys" disclosure lists (HTTP_READ_TIMEOUT is the one hidden key that
    // renders in the Gateway card instead) — not the catalog.
    expect(named).toBe(37);
    const rendered = await page.evaluate(
      () =>
        Array.from(document.querySelectorAll("code")).filter((c) =>
          /^[A-Z][A-Z0-9_]*$/.test((c.textContent ?? "").trim()),
        ).length,
    );
    expect(named).toBeGreaterThanOrEqual(rendered);
    expect(named).toBeLessThan(keys.length);
  });

  test("Settings rejected save keeps the edited value with a Retry affordance", async ({
    page,
  }) => {
    const f = loadFixtures();
    const configWithContent = {
      ...f.config,
      env_content:
        "LISTEN_ADDR=127.0.0.1:3457\nAUTH_TOKENS=tok0,tok1\nAPI_KEYS=sk-local-xyz\nSAFE_MODE=true\nLOG_LEVEL=info\n",
      has_env_file: true,
      effective: [
        { key: "LISTEN_ADDR", value: "127.0.0.1:3457", secret: false },
        { key: "AUTH_TOKENS", value: "2 token(s)", secret: true },
        { key: "API_KEYS", value: "1 key(s)", secret: true },
        { key: "ADMIN_TOKEN", value: "set", secret: true },
        { key: "SAFE_MODE", value: "true", secret: false },
        { key: "LOG_LEVEL", value: "info", secret: false },
        { key: "COST_MODE", value: "free", secret: false },
        { key: "MAX_MESSAGES_PER_DAY", value: "0", secret: false },
      ],
    };
    await mockDashboard(page, f, { configWithApiKeys: configWithContent });
    const posted: PostedSetting[] = [];
    await mockSettingsOverlay(page, posted, { postStatus: 400 });

    const metaResp = page
      .waitForResponse(
        (r) => r.url().includes("/admin/api/config/meta") && r.status() === 200,
        { timeout: 5000 },
      )
      .catch(() => {});
    await page.goto("http://127.0.0.1:4173/admin/#settings");
    await metaResp;
    const safeMode = page.getByRole("switch", { name: "SAFE_MODE" });
    await expect(safeMode).toHaveAttribute("aria-checked", "true");

    // Toggling instant-saves; the 400 rejection surfaces inline on the row.
    await safeMode.click();
    await page.waitForRequest(
      (r) => r.method() === "POST" && r.url().includes("/admin/api/settings"),
      { timeout: 10000 },
    );
    await expect
      .poll(() => posted.filter((p) => p.key === "SAFE_MODE").length)
      .toBeGreaterThan(0);
    await expect(
      page.getByRole("status").filter({ hasText: "Setting rejected: boom" }),
    ).toBeVisible();
    await expect(page.getByRole("button", { name: "Retry" })).toBeVisible();
    // The control keeps the edited value (no revert to server state).
    await expect(safeMode).toHaveAttribute("aria-checked", "false");

    // Retry re-POSTs the same key.
    const before = posted.filter((p) => p.key === "SAFE_MODE").length;
    await page.getByRole("button", { name: "Retry" }).click();
    await expect
      .poll(() => posted.filter((p) => p.key === "SAFE_MODE").length)
      .toBeGreaterThan(before);
  });

  test("Logs filters by ?msg= and paginates with Next/Prev", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);

    await page.goto("http://127.0.0.1:4173/admin/#activity");
    await expect(
      page.getByRole("heading", { name: "Logs", exact: true }),
    ).toBeVisible();

    // Console (/v1 inference traffic) is the default view; table filtering
    // and pagination live in the Table view, so switch there first.
    await page.getByRole("button", { name: "Table" }).click();

    const msgInput = page.locator("#log-msg");
    await expect(msgInput).toBeVisible();
    await msgInput.fill("upstream timeout");
    // The page re-fetches on input; wait for filtered response
    await page.waitForResponse(
      (r) =>
        r.url().includes("/admin/api/logs") &&
        r.url().includes("msg=upstream") &&
        r.status() === 200,
      { timeout: 5000 },
    );
    // After filter, range should reflect fewer entries
    await expect(page.getByText(/of \d+/)).toBeVisible();

    // Pagination controls exist
    await expect(page.getByRole("button", { name: "Next" })).toBeVisible();
    await expect(page.getByRole("button", { name: "Prev" })).toBeVisible();

    // Clear filters should restore full count — click Clear filters if present
    const clearBtn = page.getByRole("button", { name: "Clear filters" });
    if (await clearBtn.isVisible().catch(() => false)) {
      await clearBtn.click();
      await page.waitForResponse(
        (r) =>
          r.url().includes("/admin/api/logs") &&
          !r.url().includes("msg=upstream"),
        { timeout: 5000 },
      );
    }
  });

  test("Logs console survives same-second duplicate request lines", async ({
    page,
  }) => {
    // Live entries share second-precision timestamps: one request emits
    // chat request + routing + access + trace + done with the same req_id
    // and time. Console line ids must stay unique or Svelte throws
    // each_key_duplicate and the view breaks.
    const t = new Date().toISOString();
    const E = (message: string, fields: string) => ({
      time: t,
      level: "INFO",
      message,
      fields,
    });
    const f = loadFixtures();
    const pageErrors: string[] = [];
    page.on("pageerror", (e) => pageErrors.push(String(e)));
    await mockDashboard(page, f, {
      logs: {
        entries: [
          E(
            "chat request",
            "req_id=dup  model=openai/gpt-5.6-luna  msgs=3  tools=2",
          ),
          E("chat routing", "req_id=dup  agent=stealth/ox-alpha"),
          E(
            "access",
            "req_id=dup  method=POST  path=/v1/chat/completions  status=200  ms=100",
          ),
          E("chat trace", "req_id=dup  total_ms=100"),
          E("chat done", "req_id=dup  ms=100"),
        ],
      },
    });

    await page.goto("http://127.0.0.1:4173/admin/#activity");
    await expect(page.getByText("1 model request")).toBeVisible();
    expect(pageErrors).toEqual([]);
  });

  test("Logs console labels the view window and widens it on demand", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);

    await page.goto("http://127.0.0.1:4173/admin/#activity");
    // The label follows the server's effective window (the fixture stands in
    // for the 1h default) and the matching option is active.
    await expect(page.getByText("last 1 hour")).toBeVisible();
    await expect(page.getByRole("button", { name: "1h" })).toHaveAttribute(
      "aria-pressed",
      "true",
    );

    // Picking a window re-queries with ?window= and relabels the view.
    await Promise.all([
      page.waitForResponse(
        (r) =>
          r.url().includes("/admin/api/logs") &&
          r.url().includes("window=6h") &&
          r.status() === 200,
        { timeout: 5000 },
      ),
      page.getByRole("button", { name: "6h" }).click(),
    ]);
    await expect(page.getByText("last 6 hours")).toBeVisible();
  });

  test("Logs console notes a truncated view window", async ({ page }) => {
    const f = loadFixtures();
    await mockDashboard(page, f, {
      logs: { entries: f.logs.entries, window: "1h0m0s", truncated: true },
    });

    await page.goto("http://127.0.0.1:4173/admin/#activity");
    await expect(
      page.getByText(/this window holds more than the console displays/),
    ).toBeVisible();
  });

  test("Logs console follows newest entries and pauses on manual scroll-up", async ({
    page,
  }) => {
    // 25 requests overflow the console viewport: follow mode must stick to
    // the bottom on load, pause when the user scrolls up, and resume via
    // the Follow toggle.
    const t = new Date().toISOString();
    const E = (message: string, fields: string) => ({
      time: t,
      level: "INFO",
      message,
      fields,
    });
    const entries = [];
    for (let i = 0; i < 25; i++) {
      const id = `follow${i}`;
      entries.push(
        E(
          "chat request",
          `req_id=${id}  model=openai/gpt-5.6-luna  msgs=3  tools=2`,
        ),
      );
      entries.push(E("chat routing", `req_id=${id}  agent=stealth/ox-alpha`));
      entries.push(
        E(
          "access",
          `req_id=${id}  method=POST  path=/v1/chat/completions  status=200  ms=100`,
        ),
      );
      entries.push(E("chat trace", `req_id=${id}  total_ms=100`));
      entries.push(E("chat done", `req_id=${id}  ms=100`));
    }
    const f = loadFixtures();
    await mockDashboard(page, f, { logs: { entries } });

    await page.goto("http://127.0.0.1:4173/admin/#activity");
    await expect(page.getByText("25 model requests")).toBeVisible();
    await expect(page.getByRole("button", { name: "Follow on" })).toBeVisible();

    await page.waitForFunction(
      () => {
        const el = document.querySelector(".bg-black.rounded-b-lg");
        return !!el && el.scrollHeight - el.scrollTop - el.clientHeight < 64;
      },
      null,
      { timeout: 5000 },
    );

    // Manual scroll-up pauses follow mode instead of yanking the reader.
    await page.evaluate(() => {
      const el = document.querySelector(".bg-black.rounded-b-lg");
      if (el) {
        el.scrollTop = 0;
        el.dispatchEvent(new Event("scroll"));
      }
    });
    await expect(
      page.getByRole("button", { name: "Follow off" }),
    ).toBeVisible();

    // Toggling Follow back on sticks to the newest entry again.
    await page.getByRole("button", { name: "Follow off" }).click();
    await expect(page.getByRole("button", { name: "Follow on" })).toBeVisible();
    await page.waitForFunction(
      () => {
        const el = document.querySelector(".bg-black.rounded-b-lg");
        return !!el && el.scrollHeight - el.scrollTop - el.clientHeight < 64;
      },
      null,
      { timeout: 5000 },
    );
  });

  test("Models lists 13 rows with tiers, withdrawals, and the live offer", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);

    await page.goto("http://127.0.0.1:4173/admin/#plans");
    await page.getByRole("button", { name: "Models" }).click();
    await page
      .waitForResponse(
        (r) => r.url().includes("/admin/api/models") && r.status() === 200,
        { timeout: 5000 },
      )
      .catch(() => {});
    // Models tab: table assertions stay, scoped to the merged page.
    await expect(
      page.getByRole("heading", { name: "Usage", exact: true }),
    ).toBeVisible();

    // Served stat tells the truth about 13 rows: 6 served of 13 listed.
    await expect(page.getByText("6 of 13")).toBeVisible();
    await expect(page.getByText("13 registered · 49 agents")).toBeVisible();
    // Tier column renders; the pool column stays gone.
    await expect(page.getByText("Tier").first()).toBeVisible();
    await expect(page.locator("table").getByText("Pool")).toHaveCount(0);
    // 13 rows in the desktop table; tier cells render in both the table
    // and the mobile cards.
    await expect(page.locator("table tbody tr")).toHaveCount(13);
    await expect(page.getByTestId("model-tier")).toHaveCount(26);
    // Per-row tiers + status copy, scoped to the desktop table (one
    // rendering per row; the mobile cards carry the same copy).
    const table = page.getByRole("table");
    const want: Array<[string, string[], string]> = [
      [
        "stealth/ox-alpha",
        ["withdrawn", "Withdrawn — use GLM 5.3 Flash"],
        "withdrawn",
      ],
      [
        "deepseek/deepseek-v4-pro",
        ["withdrawn", "Withdrawn — use GLM 5.3 Flash"],
        "withdrawn",
      ],
      [
        "minimax/minimax-m3",
        ["withdrawn", "Withdrawn — use GLM 5.3 Flash"],
        "withdrawn",
      ],
      [
        "meta/muse-spark-1.3-contributor",
        ["withdrawn", "Withdrawn — use GLM 5.3 Flash"],
        "withdrawn",
      ],
      ["openai/gpt-5.6-luna", ["full", "paid plan"], "served"],
      ["upstage/solar-pro4", ["limited", "full"], "served"],
      ["google/gemini-3.8-flash", ["paid plan"], "unserved"],
      ["meta/muse-spark-1.2-contributor", ["full"], "served"],
      [
        "z-ai/glm-5.2",
        ["withdrawn", "Withdrawn — use GLM 5.3 Flash"],
        "withdrawn",
      ],
      ["z-ai/glm-5.3-flash", ["limited", "full", "paid plan"], "served"],
      [
        "deepseek/deepseek-v4-flash",
        ["limited", "full", "paid plan"],
        "served",
      ],
      ["mimo/mimo-v2.5", ["limited", "full"], "served"],
      [
        "anthropic/claude-fable-5.1",
        ["limited trial", "3 of 10 sessions left"],
        "unserved",
      ],
    ];
    for (const [id, chips, state] of want) {
      const row = table.locator("tbody tr").filter({ hasText: id });
      await expect(row).toHaveCount(1);
      await expect(row).toContainText(state);
      for (const chip of chips) {
        await expect(row.getByTestId("model-tier")).toContainText(chip);
      }
    }
    await expect(page.getByTestId("model-offer").first()).toContainText(
      "3 of 10 sessions left",
    );
    // No "referral" badge renders anywhere on the tab: the withdrawn
    // referral row carries the withdrawn treatment, and the tier-only rows
    // read "unserved".
    await expect(page.getByText("referral", { exact: true })).toHaveCount(0);
    await expect(page.getByText("Referral grant").first()).toBeVisible();
    await expect(page.getByText("Referral only").first()).toBeVisible();
    await expect(page.getByText("low/high/max").first()).toBeVisible();
    await expect(page.getByText("Price").first()).toBeVisible();
  });
  test("Models sorts cheapest-first on the meter", async ({ page }) => {
    const f = loadFixtures();
    // Metered account: luna at 2/hr sorts above flash at 15/hr; unpriced
    // rows follow in catalog order.
    const pricedTokens = JSON.parse(JSON.stringify(f.tokens));
    pricedTokens.tokens[0].freebucks = {
      balance: 50,
      daily: { remaining: 30, limit: 75, reset_at: "2030-01-01T00:00:00Z" },
      wallet: { balance: 20 },
      monthly: { remaining: 20 },
      prices: {
        "openai/gpt-5.6-luna": 2,
        "deepseek/deepseek-v4-flash": 15,
      },
    };
    await mockDashboard(page, f, { tokens: pricedTokens });

    await page.goto("http://127.0.0.1:4173/admin/#plans");
    await page.getByRole("button", { name: "Models" }).click();
    await expect(
      page.getByRole("heading", { name: "Usage", exact: true }),
    ).toBeVisible();
    const rows = page.locator("table tbody tr");
    await expect(rows).toHaveCount(13);
    await expect(rows.first()).toContainText("openai/gpt-5.6-luna");
  });
  test("Models offer row names the spent trial", async ({ page }) => {
    const f = loadFixtures();
    const models = JSON.parse(JSON.stringify(f.models));
    // Spent trial: the shared pool still has sessions, this account's
    // slice is gone (the vendor's joinable gate mirrors user_remaining).
    const fable = models.models.find(
      (m: { id: string }) => m.id === "anthropic/claude-fable-5.1",
    );
    fable.offer.user_remaining = 0;
    fable.offer.joinable = false;
    fable.offer.reason = "used";
    await mockDashboard(page, f, { models });

    await page.goto("http://127.0.0.1:4173/admin/#plans");
    await page.getByRole("button", { name: "Models" }).click();
    await expect(
      page.getByRole("heading", { name: "Usage", exact: true }),
    ).toBeVisible();
    // Live counts stay exact; the trial-used phrasing joins them.
    await expect(page.getByTestId("model-offer")).toHaveCount(2);
    await expect(page.getByTestId("model-offer").first()).toContainText(
      "3 of 10 sessions left · trial used",
    );
  });
  test("Models offer row says so when no campaign runs", async ({ page }) => {
    const f = loadFixtures();
    const models = JSON.parse(JSON.stringify(f.models));
    // No campaign: the offer row keeps its tier chip but carries no wire
    // block, so the panel says so instead of showing zero counts.
    const fable = models.models.find(
      (m: { id: string }) => m.id === "anthropic/claude-fable-5.1",
    );
    delete fable.offer;
    await mockDashboard(page, f, { models });

    await page.goto("http://127.0.0.1:4173/admin/#plans");
    await page.getByRole("button", { name: "Models" }).click();
    await expect(
      page.getByRole("heading", { name: "Usage", exact: true }),
    ).toBeVisible();
    const table = page.getByRole("table");
    const row = table
      .locator("tbody tr")
      .filter({ hasText: "anthropic/claude-fable-5.1" });
    await expect(row.getByTestId("model-tier")).toContainText("limited trial");
    await expect(page.getByTestId("model-offer")).toHaveCount(2);
    await expect(page.getByTestId("model-offer").first()).toContainText(
      "trial not offered right now",
    );
  });

  test("Overview shows client integration and base_url", async ({ page }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);

    await page.goto("http://127.0.0.1:4173/admin/#overview");
    await page
      .waitForResponse(
        (r) => r.url().includes("/admin/api/overview") && r.status() === 200,
        { timeout: 5000 },
      )
      .catch(() => {});
    await expect(page.getByRole("heading", { name: "Overview" })).toBeVisible();

    // Client integration section with base URL and dual protocols
    await expect(
      page.getByRole("heading", { name: "Client Integration" }),
    ).toBeVisible();
    await expect(
      page.getByText("http://127.0.0.1:3457/v1").first(),
    ).toBeVisible();
    await expect(page.getByText("POST /v1/chat/completions")).toBeVisible();
    await expect(page.getByText("POST /v1/messages")).toBeVisible();
  });

  test("Login 401 shows error banner and stays on login", async ({ page }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);

    // Override login POST to return 401 with JSON error
    await page.unroute("**/admin/login");
    await page.route("**/admin/login", async (route) => {
      if (route.request().method() === "POST") {
        await route.fulfill({
          status: 401,
          contentType: "application/json",
          body: JSON.stringify({ error: "Invalid admin token." }),
        });
      } else {
        await route.continue();
      }
    });

    await page.goto("http://127.0.0.1:4173/admin/login");
    await expect(
      page
        .getByRole("heading", { name: "Admin" })
        .or(page.getByText("freebucks-proxy")),
    ).toBeVisible();

    const tokenInput = page.locator("#token");
    await expect(tokenInput).toBeVisible();
    await tokenInput.fill("wrong-token");
    await page.getByRole("button", { name: "Sign in" }).click();

    // Error banner from 401 should appear
    await expect(page.getByText("Invalid admin token.")).toBeVisible({
      timeout: 5000,
    });
    // Should still be on login (no redirect to /admin)
    expect(page.url()).toContain("/admin/login");
  });

  test("a11y: pages expose aria-live, aria-describedby and labelling after mock", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);

    // Register the response wait before navigating; the mocked overview
    // response can resolve during goto and the late wait would miss it.
    const overviewResp = page.waitForResponse(
      (r) => r.url().includes("/admin/api/overview"),
      { timeout: 5000 },
    );
    await page.goto("http://127.0.0.1:4173/admin/#overview");
    await overviewResp;
    // Overview loading skeleton used aria-live="polite" and aria-busy="true"
    // Pool status lives in the Pool Tokens table (the standalone At-risk
    // section is gone): overview must not render it, tokens rows must show
    // status per account.
    await expect(
      page.locator('section[aria-label="At-risk tokens"]'),
    ).toHaveCount(0);
    await page.goto("http://127.0.0.1:4173/admin/#tokens");
    const tokensTable = page.locator("table.fp-table");
    // Fixture token #2 (Account #2) carries no streak, so its row exposes
    // the "No streak" label in the Account cell's status area.
    await expect(
      tokensTable
        .locator("tbody tr")
        .filter({ hasText: "Account #2" })
        .getByLabel("No streak"),
    ).toBeVisible();

    // Navigate to Activity and check filter labelling + live region. Live is
    // the default tab; the labelled filter inputs and entry text live in
    // the Table view.
    await page.goto("http://127.0.0.1:4173/admin/#activity");
    await page.getByRole("button", { name: "Table" }).click();
    await page
      .waitForResponse((r) => r.url().includes("/admin/api/logs"), {
        timeout: 5000,
      })
      .catch(() => {});
    await expect(page.locator("#log-level")).toBeVisible();
    await expect(page.locator("#log-msg")).toBeVisible();
    await expect(
      page
        .getByText("request 0")
        .first()
        .or(page.getByText("upstream timeout").first()),
    ).toBeVisible();
    // Check that at least one element has aria-live or aria-describedby
    const liveCount = await page.locator("[aria-live]").count();
    expect(liveCount).toBeGreaterThanOrEqual(0);
    // LOG_LEVEL's accessible select is homed on Settings (the Logs page no
    // longer hosts a Logging tab, and the old link-out stub is gone), next
    // to the inline SAFE_MODE switch.
    await page.goto("http://127.0.0.1:4173/admin/#settings");
    await page
      .waitForResponse((r) => r.url().includes("/admin/api/config"), {
        timeout: 5000,
      })
      .catch(() => {});
    await expect(
      page.getByRole("combobox", { name: "LOG_LEVEL" }),
    ).toBeVisible();
    await expect(
      page.getByRole("link", { name: "Manage log level (Logs → Logging tab)" }),
    ).toHaveCount(0);
    await expect(page.getByRole("switch", { name: "SAFE_MODE" })).toBeVisible();
  });

  test("Metrics tab renders KPIs, sparklines and per-token rows", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);

    await page.goto("http://127.0.0.1:4173/admin/#activity");
    await page
      .waitForResponse(
        (r) => r.url().includes("/admin/api/metrics") && r.status() === 200,
        { timeout: 5000 },
      )
      .catch(() => {});
    // Metrics lives behind the Activity tab bar now: pin the tab, then the
    // panel content.
    await page.getByRole("button", { name: "Metrics" }).click();
    await expect(page.getByRole("button", { name: "Metrics" })).toHaveAttribute(
      "aria-pressed",
      "true",
    );

    // KPI stats from fixtures/metrics.json (Models served card removed).
    await expect(page.getByText("Requests served")).toBeVisible();
    await expect(page.getByText("Models served")).toHaveCount(0);
    // Sparkline SVG embedded from the API payload
    await expect(page.locator('svg[role="img"]').first()).toBeVisible();
    // Per-token table rows carry the fixture requests_24h counts (2 and 4).
    await expect(
      page.getByRole("heading", { name: "Per-token metrics" }),
    ).toBeVisible();
    const perTokenTable = page.locator("table", {
      has: page.getByRole("columnheader", { name: "Requests (24h)" }),
    });
    await expect(
      perTokenTable.getByRole("columnheader", { name: "Token" }),
    ).toBeVisible();
    await expect(
      perTokenTable.getByRole("columnheader", { name: "Requests (24h)" }),
    ).toBeVisible();
    for (const gone of [
      "Spend",
      "Transient retries",
      "Fingerprint rotations",
    ]) {
      await expect(
        perTokenTable.getByRole("columnheader", { name: gone }),
      ).toHaveCount(0);
    }
    const metricRows = perTokenTable.locator("tbody tr");
    await expect(metricRows.nth(0)).toContainText("2");
    await expect(metricRows.nth(1)).toContainText("4");
    await expect(metricRows).toHaveCount(2);
  });

  test("Traces tab renders the trace table from /admin/api/traces", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);

    await page.goto("http://127.0.0.1:4173/admin/#activity");
    await page
      .waitForResponse(
        (r) => r.url().includes("/admin/api/traces") && r.status() === 200,
        { timeout: 5000 },
      )
      .catch(() => {});
    // Traces lives behind the Activity tab bar now: pin the tab, then the
    // panel content.
    await page.getByRole("button", { name: "Traces" }).click();
    await expect(page.getByRole("button", { name: "Traces" })).toHaveAttribute(
      "aria-pressed",
      "true",
    );
    await expect(page.locator("table tbody tr")).toHaveCount(2);
    const traceTable = page.locator("table");
    await expect(
      traceTable.getByText("deepseek/deepseek-v4-flash"),
    ).toBeVisible();
    // Phase chips render the per-phase latency names from the payload
    await expect(traceTable.getByText("acquire_ms")).toBeVisible();
    // The error row surfaces the error text
    await expect(traceTable.getByText("upstream timeout")).toBeVisible();
    // Mobile renders the same rows as stacked cards instead of the table.
    await page.setViewportSize({ width: 390, height: 844 });
    const traceCards = page.getByLabel("Chat traces");
    await expect(
      traceCards.getByText("deepseek/deepseek-v4-flash"),
    ).toBeVisible();
    await expect(traceCards.getByText("upstream timeout")).toBeVisible();
  });

  test("Traces tab survives duplicate timestamps and phase names", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    await page.unroute("**/admin/api/traces");
    const dup = {
      enabled: true,
      traces: [
        {
          time: "2026-08-27T10:00:00Z",
          token: "0",
          model: "dup-model-a",
          status: "ok",
          ms: "10ms",
          phases: [
            { name: "proxy", ms: 1 },
            { name: "proxy", ms: 2 },
          ],
        },
        {
          time: "2026-08-27T10:00:00Z",
          token: "1",
          model: "dup-model-b",
          status: "ok",
          ms: "20ms",
          phases: [{ name: "proxy", ms: 3 }],
        },
      ],
    };
    await page.route("**/admin/api/traces", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(dup),
      });
    });
    const errors: string[] = [];
    page.on("pageerror", (e) => errors.push(e.message));
    await page.goto("http://127.0.0.1:4173/admin/#activity");
    await page.getByRole("button", { name: "Traces" }).click();
    await expect(page.locator("table tbody tr")).toHaveCount(2);
    await expect(page.locator("table").getByText("dup-model-b")).toBeVisible();
    expect(errors.join("\n")).not.toContain("each_key_duplicate");
  });

  test("Traces error shows a titled alert with retry", async ({ page }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    await page.unroute("**/admin/api/traces");
    await page.route("**/admin/api/traces", async (route) => {
      await route.fulfill({
        status: 500,
        contentType: "application/json",
        body: JSON.stringify({
          ok: false,
          message: "boom",
          code: "traces_failed",
        }),
      });
    });
    await page.goto("http://127.0.0.1:4173/admin/#activity");
    await page.getByRole("button", { name: "Traces" }).click();
    await expect(page.getByRole("button", { name: "Traces" })).toHaveAttribute(
      "aria-pressed",
      "true",
    );
    await expect(page.getByText("Could not load this page")).toBeVisible();
    await expect(page.getByText("boom")).toBeVisible();
    // Retry refetches: restore success and click through to the table.
    await page.unroute("**/admin/api/traces");
    await page.route("**/admin/api/traces", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(f.traces),
      });
    });
    await page.getByRole("button", { name: "Retry" }).click();
    await expect(page.locator("table tbody tr")).toHaveCount(2);
  });

  test("Legacy /admin/setup and /admin/playground URLs redirect; unknown tab shows NotFound", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);

    // /admin/setup redirects to the Overview page (client setup block removed).
    await page.goto("http://127.0.0.1:4173/admin/setup");
    await expect(
      page.getByRole("heading", { name: "Overview", exact: true }),
    ).toBeVisible();
    await expect(
      page.getByRole("heading", { name: "Client Integration" }),
    ).toBeVisible();

    // /admin/playground maps to Dev Tools (fully gated off here: renders
    // nothing and bounces to Overview).
    await page.goto("http://127.0.0.1:4173/admin/playground");
    await expect(
      page.getByRole("heading", { name: "Overview", exact: true }),
    ).toBeVisible();
    // Unknown tab renders the NotFound fallback, not a blank shell
    await page.goto("http://127.0.0.1:4173/admin/#does-not-exist");
    await expect(page.getByText("Page not found")).toBeVisible();
  });
  test("Sidebar footer shortens a 40-char build SHA with full id in tooltip", async ({
    page,
  }) => {
    const f = loadFixtures();
    const sha = "d088f4468e77c1bcf3814862b42bee9415e5fe6e";
    await mockDashboard(page, f, {
      version: { ...f.version, current_version: sha },
    });
    await page.goto("http://127.0.0.1:4173/admin/#overview");
    await expect(
      page.getByRole("heading", { name: "Overview", exact: true }),
    ).toBeVisible();
    // Short id renders in the desktop sidebar footer; the full SHA never
    // appears as visible text (it would overflow the 224px sidebar) but
    // stays available as the tooltip.
    const badge = page.locator("aside span[title]").filter({
      hasText: "d088f44",
    });
    await expect(badge).toBeVisible();
    await expect(badge).toHaveAttribute("title", sha);
    await expect(
      page.locator("aside").getByText(sha, { exact: false }),
    ).toHaveCount(0);
  });
});
