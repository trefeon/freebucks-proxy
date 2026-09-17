import { test, expect } from "@playwright/test";
import { loadFixtures, mockDashboard, mockSettingsOverlay } from "./mocks.js";
import type { Fixtures, PostedSetting } from "./mocks.js";

// ---------------------------------------------------------------------------
// Clickable / interactable coverage (hermetic mocks).
//
// dashboard.spec.ts and ux.spec.ts pin data rendering and the main mutation
// flows; this suite pins every remaining button, toggle, select, radio and
// dialog on the operator path: token reorder/clear/probe/finish/drop-session,
// dialog dismiss, strategy preset radios, slots stepper, log view/filter/paging
// controls, settings discard/bridge/rate-limit/password, setup key buttons,
// sidebar navigation and the overview error-retry path.
// ---------------------------------------------------------------------------

// Minimal token row mirroring the real /admin/api/tokens row shape.
function tokenRow(
  idx: number,
  over: Record<string, unknown> = {},
): Record<string, unknown> {
  return {
    index: idx,
    email: `acct${idx}@example.com`,
    session_status: "idle",
    queue_position: 0,
    queue_depth: 0,
    active_runs: 0,
    requests: 0,
    messages_24h: 0,
    cooldown_active: false,
    cooldown_until: "",
    locked: false,
    transient_retries: 1,
    has_standing: false,
    session_instance: "",
    session_model: "",
    session_remaining_seconds: 0,
    has_quota: false,
    ...over,
  };
}

function tokensPayload(tokens: Array<Record<string, unknown>>) {
  return {
    mode: "pooled",
    in_bridge: false,
    bridge_tokens: 0,
    token_count: tokens.length,
    has_tokens: true,
    tokens,
    bridge_token_cards: [],
  };
}

// Settings fixtures carry a live .env so the form cards render with values.
function settingsConfig(f: Fixtures) {
  return {
    ...f.config,
    env_content:
      "LISTEN_ADDR=127.0.0.1:3457\nAUTH_TOKENS=tok0,tok1\nAPI_KEYS=sk-local-xyz\nSAFE_MODE=true\nLOG_LEVEL=info\n",
    has_env_file: true,
  };
}

test.describe("operator interactions (hermetic mocks)", () => {
  test.use({ expect: { timeout: 10_000 } });

  // -------------------------------------------------------------------------
  // 1. Token reorder: Move Down posts from/to and reorders; Move Up is
  //    disabled on the first account and posts the reverse on the second.
  // -------------------------------------------------------------------------
  test("tokens: move down swaps pool order; move up disabled on first account", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f, {}, { loginPage: true });

    const state = { tokens: [tokenRow(0), tokenRow(1), tokenRow(2)] };
    await page.unroute("**/admin/api/tokens*");
    await page.route("**/admin/api/tokens*", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(tokensPayload(state.tokens)),
      });
    });
    const swaps: Array<Record<string, unknown>> = [];
    await page.route("**/admin/tokens/swap", async (route) => {
      swaps.push(JSON.parse(route.request().postData() || "{}"));
      const { from, to } = swaps[swaps.length - 1] as {
        from: number;
        to: number;
      };
      const moved = state.tokens.splice(from, 1)[0];
      state.tokens.splice(to, 0, moved);
      state.tokens.forEach((t, i) => {
        t.index = i;
      });
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ ok: true, message: "Pool order updated." }),
      });
    });

    await page.goto("http://127.0.0.1:4173/admin/#tokens");
    const rows = page.locator("table tbody tr");
    await expect(rows).toHaveCount(3);
    const first = rows.filter({ hasText: "Account #1" });
    const second = rows.filter({ hasText: "Account #2" });
    const last = rows.filter({ hasText: "Account #3" });

    // Boundary states: nothing above the first account, nothing below the last.
    await expect(first.getByRole("button", { name: "Move Up" })).toBeDisabled();
    await expect(
      first.getByRole("button", { name: "Move Down" }),
    ).toBeEnabled();
    await expect(
      last.getByRole("button", { name: "Move Down" }),
    ).toBeDisabled();
    await expect(last.getByRole("button", { name: "Move Up" })).toBeEnabled();

    // Move Down on Account #1 swaps positions 0 and 1 (no confirm dialog).
    const refetch = page.waitForResponse(
      (r) => r.url().includes("/admin/api/tokens") && r.status() === 200,
    );
    await first.getByRole("button", { name: "Move Down" }).click();
    await refetch;
    expect(swaps).toEqual([{ from: 0, to: 1 }]);
    await expect(page.getByText("Pool order updated.")).toBeVisible();
    // The second account's email now leads the table.
    await expect(rows.first().getByText("acct1@example.com")).toBeVisible();

    // Move Up on the (new) second row posts the reverse swap.
    const refetch2 = page.waitForResponse(
      (r) => r.url().includes("/admin/api/tokens") && r.status() === 200,
    );
    await second.getByRole("button", { name: "Move Up" }).click();
    await refetch2;
    expect(swaps).toEqual([
      { from: 0, to: 1 },
      { from: 1, to: 0 },
    ]);
    await expect(rows.first().getByText("acct0@example.com")).toBeVisible();
  });

  // -------------------------------------------------------------------------
  // 2. Clear cooldown posts the per-token unlock endpoint and clears the row.
  // -------------------------------------------------------------------------
  test("tokens: clear cooldown posts unlock and clears the warning", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f, {}, { loginPage: true });

    const state = {
      tokens: [
        tokenRow(0, {
          cooldown_active: true,
          cooldown_until: new Date(Date.now() + 5 * 60_000).toISOString(),
        }),
        tokenRow(1),
      ],
    };
    await page.unroute("**/admin/api/tokens*");
    await page.route("**/admin/api/tokens*", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(tokensPayload(state.tokens)),
      });
    });
    await page.route("**/admin/tokens/0/unlock", async (route) => {
      state.tokens[0].cooldown_active = false;
      state.tokens[0].cooldown_until = "";
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ ok: true, message: "Cooldown cleared." }),
      });
    });

    await page.goto("http://127.0.0.1:4173/admin/#tokens");
    const row = page
      .locator("table tbody tr")
      .filter({ hasText: "Account #1" });
    await expect(row.getByRole("button", { name: "Clear" })).toBeVisible();

    const unlockReq = page.waitForRequest(
      (r) =>
        r.method() === "POST" && r.url().includes("/admin/tokens/0/unlock"),
    );
    const refetch = page.waitForResponse(
      (r) => r.url().includes("/admin/api/tokens") && r.status() === 200,
    );
    page.once("dialog", (d) => d.accept());
    await row.getByRole("button", { name: "Clear" }).click();
    await unlockReq;
    await refetch;
    await expect(page.getByText("Cooldown cleared.")).toBeVisible();
    await expect(row.getByRole("button", { name: "Clear" })).toHaveCount(0);
  });

  // -------------------------------------------------------------------------
  // 3. Drawer actions: probe, finish and drop-session post per-token endpoints.
  // -------------------------------------------------------------------------
  test("tokens: probe, finish and drop session post per-token endpoints", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f, {}, { loginPage: true });

    const state = {
      tokens: [
        tokenRow(0, {
          session_status: "active",
          session_instance: "inst-live-1",
          session_model: "openai/gpt-5.6-luna",
          session_remaining_seconds: 1800,
        }),
      ],
    };
    await page.unroute("**/admin/api/tokens*");
    await page.route("**/admin/api/tokens*", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(tokensPayload(state.tokens)),
      });
    });
    // DEVTOOLS on so the drawer Probe / Finish Runs toolbar renders.
    await page.unroute("**/admin/api/config");
    await page.route("**/admin/api/config", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          env_content: "AUTH_TOKENS=tok0\nDEVTOOLS_ENABLED=true\n",
          has_env_file: true,
        }),
      });
    });
    const posts: string[] = [];
    for (const action of ["test", "finish", "drop-session"]) {
      await page.route(`**/admin/tokens/0/${action}`, async (route) => {
        posts.push(action);
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify({ ok: true, message: `${action} done.` }),
        });
      });
    }

    await page.goto("http://127.0.0.1:4173/admin/#tokens");
    const row = page
      .locator("table tbody tr")
      .filter({ hasText: "Account #1" });
    await row.locator('button[aria-label*="Expand details"]').click();
    await expect(
      page.locator("table").getByRole("button", { name: "Drop Session" }),
    ).toBeVisible();

    for (const [label, action] of [
      ["Probe", "test"],
      ["Finish Runs", "finish"],
      ["Drop Session", "drop-session"],
    ] as Array<[string, string]>) {
      const req = page.waitForRequest(
        (r) =>
          r.method() === "POST" &&
          r.url().includes(`/admin/tokens/0/${action}`),
      );
      page.once("dialog", (d) => d.accept());
      await page.locator("table").getByRole("button", { name: label }).click();
      await req;
      await expect(page.getByText(`${action} done.`)).toBeVisible();
    }
    expect(posts).toEqual(["test", "finish", "drop-session"]);
  });

  // -------------------------------------------------------------------------
  // 3b. Manual probing excised: no probe buttons anywhere; Dev Tools keeps
  // the playground + spawner panels.
  // -------------------------------------------------------------------------
  test("quota: plans has no manual probe buttons; devtools spawn stays", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f, {}, { loginPage: true });
    await page.goto("http://127.0.0.1:4173/admin/#plans");
    await page.getByRole("button", { name: "Accounts" }).click();
    await expect(
      page.getByRole("heading", { name: "Usage", exact: true }),
    ).toBeVisible();
    await expect(
      page.getByRole("heading", { name: "Account #1" }),
    ).toBeVisible();
    await expect(page.getByRole("button", { name: "Probe all" })).toHaveCount(
      0,
    );
    await expect(page.getByText("Quotas refreshed from upstream.")).toHaveCount(
      0,
    );
    // Manual probing is gone (test-all endpoint excised): Dev Tools keeps
    // only the session spawn + playground panels (DEVTOOLS on for this page).
    await expect(
      page.getByRole("button", { name: "Probe All Tokens" }),
    ).toHaveCount(0);
    await page.unroute("**/admin/api/config");
    await page.route("**/admin/api/config", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          env_content: "AUTH_TOKENS=tok0\nDEVTOOLS_ENABLED=true\n",
          has_env_file: true,
        }),
      });
    });
    await page.goto("http://127.0.0.1:4173/admin/#devtools");
    await expect(page.getByLabel("Model Playground")).toBeVisible();
  });
  // -------------------------------------------------------------------------
  // 3c. Tokens page has no Probe-all header button.
  // -------------------------------------------------------------------------
  test("tokens: no probe-all header button, per-row probe stays", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f, {}, { loginPage: true });
    const state = { tokens: [tokenRow(0), tokenRow(1)] };
    await page.unroute("**/admin/api/tokens*");
    await page.route("**/admin/api/tokens*", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(tokensPayload(state.tokens)),
      });
    });
    await page.goto("http://127.0.0.1:4173/admin/#tokens");
    await expect(
      page.getByRole("heading", { name: "Pool", exact: true }),
    ).toBeVisible();
    await expect(page.getByRole("button", { name: "Probe all" })).toHaveCount(
      0,
    );
  });

  // -------------------------------------------------------------------------
  // 3d. Pool Tokens never scrolls horizontally (desktop table + narrow cards).
  // -------------------------------------------------------------------------
  test("tokens: pool table fits its card without horizontal scroll", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f, {}, { loginPage: true });
    const state = { tokens: [tokenRow(0), tokenRow(1)] };
    await page.unroute("**/admin/api/tokens*");
    await page.route("**/admin/api/tokens*", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(tokensPayload(state.tokens)),
      });
    });
    await page.setViewportSize({ width: 1280, height: 800 });
    await page.goto("http://127.0.0.1:4173/admin/#tokens");
    await expect(
      page.getByRole("heading", { name: "Pool Tokens" }),
    ).toBeVisible();
    const overflow = await page
      .locator("section", { hasText: "Pool Tokens" })
      .locator("div.overflow-x-auto")
      .evaluate((el) => el.scrollWidth - el.clientWidth);
    expect(overflow).toBeLessThanOrEqual(1);
  });

  test("tokens: narrow viewport uses stacked cards without page scroll", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f, {}, { loginPage: true });
    const state = { tokens: [tokenRow(0), tokenRow(1)] };
    await page.unroute("**/admin/api/tokens*");
    await page.route("**/admin/api/tokens*", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(tokensPayload(state.tokens)),
      });
    });
    await page.setViewportSize({ width: 800, height: 800 });
    await page.goto("http://127.0.0.1:4173/admin/#tokens");
    await expect(
      page.getByRole("heading", { name: "Pool Tokens" }),
    ).toBeVisible();
    // Below lg the pool table hides (display:none still matches locators)
    // and stacked cards take over (other page sections may keep tables).
    await expect(
      page.locator("section", { hasText: "Pool Tokens" }).locator("table"),
    ).toBeHidden();
    // The expand chevron lives in the pinned action row above the drawer:
    // tapping it reveals the behind-chevron detail (instance row).
    await page
      .locator("section", { hasText: "Pool Tokens" })
      .locator('button[aria-label*="Expand details"]')
      .filter({ visible: true })
      .first()
      .click();
    await expect(
      page
        .locator("section", { hasText: "Pool Tokens" })
        .getByText("Instance", { exact: true })
        .filter({ visible: true })
        .first(),
    ).toBeVisible();
    // The action row (expand chevron + Lock/Remove) sits above the drawer:
    // expanding grows the card downward without moving the buttons.
    const lockBox = await page
      .locator("section", { hasText: "Pool Tokens" })
      .getByRole("button", { name: "Lock" })
      .filter({ visible: true })
      .first()
      .boundingBox();
    const instanceBox = await page
      .locator("section", { hasText: "Pool Tokens" })
      .getByText("Instance", { exact: true })
      .filter({ visible: true })
      .first()
      .boundingBox();
    expect(lockBox && instanceBox && lockBox.y < instanceBox.y).toBe(true);
    const pageOverflow = await page.evaluate(
      () => document.documentElement.scrollWidth - window.innerWidth,
    );
    expect(pageOverflow).toBeLessThanOrEqual(1);
  });

  // -------------------------------------------------------------------------
  // 3d. Sidebar log out posts logout and lands on the login view.
  // -------------------------------------------------------------------------
  test("sidebar: log out posts logout and lands on login", async ({ page }) => {
    const f = loadFixtures();
    await mockDashboard(page, f, {}, { loginPage: true });
    await page.unroute("**/admin/api/auth/status");
    await page.route("**/admin/api/auth/status", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ require_login: true }),
      });
    });
    const state = { tokens: [tokenRow(0)] };
    await page.unroute("**/admin/api/tokens*");
    await page.route("**/admin/api/tokens*", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(tokensPayload(state.tokens)),
      });
    });
    const logout = page.waitForRequest(
      (r) => r.method() === "POST" && r.url().includes("/admin/logout"),
    );
    await page.route("**/admin/logout", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ ok: true }),
      });
    });
    await page.goto("http://127.0.0.1:4173/admin/#tokens");
    await expect(
      page.getByRole("heading", { name: "Pool", exact: true }),
    ).toBeVisible();
    await page.getByRole("button", { name: "Log out" }).click();
    await logout;
    await expect.poll(() => page.url()).toContain("#login");
    await expect(page.getByRole("button", { name: "Sign in" })).toBeVisible();
  });

  // -------------------------------------------------------------------------
  // 4. Dismissing the confirm dialog sends no request and keeps the row.
  // -------------------------------------------------------------------------
  test("tokens: dismissing the confirm dialog sends no request", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f, {}, { loginPage: true });

    const state = { tokens: [tokenRow(0), tokenRow(1)] };
    await page.unroute("**/admin/api/tokens*");
    await page.route("**/admin/api/tokens*", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(tokensPayload(state.tokens)),
      });
    });
    let removePosted = false;
    await page.route("**/admin/tokens/remove", async (route) => {
      removePosted = true;
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ ok: true, message: "removed" }),
      });
    });

    await page.goto("http://127.0.0.1:4173/admin/#tokens");
    const row = page
      .locator("table tbody tr")
      .filter({ hasText: "Account #1" });
    page.once("dialog", (d) => d.dismiss());
    await row.getByRole("button", { name: "Remove", exact: true }).click();
    await page.waitForTimeout(800);
    expect(removePosted).toBe(false);
    await expect(
      page.locator("table tbody tr").filter({ hasText: "Account #1" }),
    ).toBeVisible();
  });

  // -------------------------------------------------------------------------
  // 5. Strategy preset radios and the slots stepper instant-save per key.
  // -------------------------------------------------------------------------
  test("tokens: strategy preset radios and slots stepper instant-save per key", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f, {}, { loginPage: true });
    const posted: PostedSetting[] = [];
    await mockSettingsOverlay(page, posted);

    await page.goto("http://127.0.0.1:4173/admin/#tokens");
    // Pool controls moved behind the Controls tab.
    await page.getByRole("button", { name: "Controls" }).click();
    const drain = page.getByRole("radio", { name: "Drain" });
    const balance = page.getByRole("radio", { name: "Balance" });
    // Catalog defaults (2 slots, 30s wait, depth 16, unbounded spill)
    // classify as Balance.
    await expect(balance).toHaveAttribute("aria-checked", "true");

    const presetReq = page.waitForRequest(
      (r) => r.method() === "POST" && r.url().includes("/admin/api/settings"),
      { timeout: 10000 },
    );
    await drain.click();
    await expect(drain).toHaveAttribute("aria-checked", "true");
    await presetReq;

    // A Drain tap writes its changed keys (debounced ~400ms each): the
    // queue posture lands in the overlay posts.
    await expect
      .poll(() => posted.find((p) => p.key === "QUEUE_WAIT")?.value)
      .toBe("300s");
    await expect
      .poll(() => posted.find((p) => p.key === "QUEUE_DEPTH")?.value)
      .toBe("1024");

    const slots = page.locator('input[aria-label="SLOTS_PER_ACCOUNT"]');
    const slotsReq = page.waitForRequest(
      (r) => r.method() === "POST" && r.url().includes("/admin/api/settings"),
      { timeout: 10000 },
    );
    await slots.fill("3");
    await slotsReq;
    await expect
      .poll(() => posted.find((p) => p.key === "SLOTS_PER_ACCOUNT")?.value)
      .toBe("3");

    // The deleted rotation/failover keys are never written by this card.
    expect(posted.find((p) => p.key === "TOKEN_ROTATION")).toBeUndefined();
    expect(posted.find((p) => p.key === "RATE_LIMIT_FAILOVER")).toBeUndefined();
  });
  // -------------------------------------------------------------------------
  // 6. Logs: console/table toggle, auto toggle, refresh and clear console.
  // -------------------------------------------------------------------------
  test("logs: view toggle, auto toggle, refresh and clear console", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);

    await page.goto("http://127.0.0.1:4173/admin/#activity");
    // Console is the default view: the seeded chat request/done pair renders
    // one request-group card (singular header + POST line).
    await expect(page.getByText("1 model request").first()).toBeVisible();
    await expect(
      page.getByText("POST openai/gpt-5.6-luna").first(),
    ).toBeVisible();

    // Table view exposes the labelled filter controls.
    await page.getByRole("button", { name: "Table" }).click();
    await expect(page.locator("#log-level")).toBeVisible();
    await expect(page.locator("#log-msg")).toBeVisible();
    await page.getByRole("button", { name: "Console" }).click();
    await expect(page.getByText("1 model request").first()).toBeVisible();

    // Auto toggle flips label and pauses the 1s poll.
    const auto = page.getByRole("button", { name: /^Auto / });
    await expect(auto).toContainText("Auto 1s");
    await auto.click();
    await expect(page.getByRole("button", { name: "Auto off" })).toBeVisible();

    // Manual refresh always fetches.
    const refetch = page.waitForResponse(
      (r) => r.url().includes("/admin/api/logs") && r.status() === 200,
    );
    await page.getByRole("button", { name: "Refresh", exact: true }).click();
    await refetch;

    // Clear wipes the console view behind a confirm dialog.
    page.once("dialog", (d) => d.accept());
    await page.getByRole("button", { name: "Clear" }).click();
    await expect(
      page.getByText("No request activity recorded yet."),
    ).toBeVisible();
  });

  // -------------------------------------------------------------------------
  // 7. Logs table: level select, hide-admin toggle and clear filters.
  // -------------------------------------------------------------------------
  test("logs: level select, hide-admin toggle and clear filters", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);

    await page.goto("http://127.0.0.1:4173/admin/#activity");
    await page.getByRole("button", { name: "Table" }).click();
    await expect(page.locator("#log-level")).toBeVisible();

    // Admin-path rows are hidden by default; an info /v1 row shows.
    await expect(page.getByText("request 0").first()).toBeVisible();
    await expect(page.getByText("request 1 completed")).toHaveCount(0);

    // Reveal admin rows.
    await page.getByRole("button", { name: "Hide admin" }).click();
    await expect(page.getByText("request 1 completed")).toBeVisible();

    // Level select filters server-side (?level=).
    const levelResp = page.waitForResponse(
      (r) =>
        r.url().includes("/admin/api/logs") &&
        r.url().includes("level=error") &&
        r.status() === 200,
    );
    await page.locator("#log-level").selectOption("error");
    await levelResp;
    await expect(page.getByText("request 2 completed")).toBeVisible();
    await expect(page.getByText("request 1 completed")).toHaveCount(0);

    // Clear filters resets to All levels and hides admin rows again (it must
    // not apply ?level=info — that would silently drop warn/error rows).
    await page.getByRole("button", { name: "Clear filters" }).click();
    await expect(page.locator("#log-level")).toHaveValue("");
    await expect(page.getByText("request 1 completed")).toHaveCount(0);
  });

  // -------------------------------------------------------------------------
  // 8. Logs table: rows-per-page select collapses pagination.
  // -------------------------------------------------------------------------
  test("logs: rows-per-page select collapses pagination", async ({ page }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);

    await page.goto("http://127.0.0.1:4173/admin/#activity");
    await page.getByRole("button", { name: "Table" }).click();
    await expect(page.locator("#logs-page-size")).toBeVisible();
    await expect(page.getByText("Page 1 /", { exact: false })).toBeVisible();

    await page.locator("#logs-page-size").selectOption("100");
    await expect(page.getByText("Page 1 / 1")).toBeVisible();
    await expect(page.getByRole("button", { name: "Next" })).toBeDisabled();
    await expect(page.getByRole("button", { name: "Prev" })).toBeDisabled();
  });

  // -------------------------------------------------------------------------
  // 9. Settings: rows expose no Save/Discard affordances (instant-save).
  // -------------------------------------------------------------------------
  test("settings: rows expose no Save/Discard affordances", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f, { configWithApiKeys: settingsConfig(f) });
    const posted: PostedSetting[] = [];
    await mockSettingsOverlay(page, posted);
    const metaResp = page.waitForResponse(
      (r) => r.url().includes("/admin/api/config/meta") && r.status() === 200,
      { timeout: 5000 },
    );
    await page.goto("http://127.0.0.1:4173/admin/#settings");
    await metaResp;

    // No batched-save affordances anywhere on the page.
    await expect(
      page.getByRole("button", { name: "Save Changes", exact: true }),
    ).toHaveCount(0);
    await expect(page.getByRole("button", { name: "Discard" })).toHaveCount(0);
    await expect(page.getByText("Unsaved changes")).toHaveCount(0);

    // Edits save themselves: toggling SAFE_MODE POSTs the overlay directly.
    const safeMode = page.getByRole("switch", { name: "SAFE_MODE" });
    await expect(safeMode).toHaveAttribute("aria-checked", "true");
    const saveReq = page.waitForRequest(
      (r) => r.method() === "POST" && r.url().includes("/admin/api/settings"),
      { timeout: 10000 },
    );
    await safeMode.click();
    await expect(safeMode).toHaveAttribute("aria-checked", "false");
    await saveReq;
    await expect
      .poll(() => posted.find((p) => p.key === "SAFE_MODE")?.value)
      .toBe("false");
  });

  // -------------------------------------------------------------------------
  // 10. Pool: bridge toggle and rate-limit input instant-save per key.
  // -------------------------------------------------------------------------
  test("pool: bridge toggle and rate-limit input instant-save per key", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f, { configWithApiKeys: settingsConfig(f) });
    const posted: PostedSetting[] = [];
    await mockSettingsOverlay(page, posted);
    const metaResp = page.waitForResponse(
      (r) => r.url().includes("/admin/api/config/meta") && r.status() === 200,
      { timeout: 5000 },
    );
    await page.goto("http://127.0.0.1:4173/admin/#tokens");
    await metaResp;
    // Pool controls moved behind the Controls tab.
    await page.getByRole("button", { name: "Controls" }).click();

    // Absent from .env, the bridge switch defaults to on.
    const bridge = page.getByRole("switch", { name: "BRIDGE_ENABLED" });
    const bridgeReq = page.waitForRequest(
      (r) => r.method() === "POST" && r.url().includes("/admin/api/settings"),
      { timeout: 10000 },
    );
    await bridge.click();
    await bridgeReq;

    const ipReq = page.waitForRequest(
      (r) => r.method() === "POST" && r.url().includes("/admin/api/settings"),
      { timeout: 10000 },
    );
    await page.locator('input[aria-label="RATE_LIMIT_PER_IP"]').fill("25");
    await ipReq;

    await expect
      .poll(() => posted.find((p) => p.key === "BRIDGE_ENABLED")?.value)
      .toBe("false");
    await expect
      .poll(() => posted.find((p) => p.key === "RATE_LIMIT_PER_IP")?.value)
      .toBe("25");
    // Each row reports its own save outcome inline.
    await expect(page.getByRole("status").first()).toBeVisible();
  });

  // -------------------------------------------------------------------------
  // 11. Settings: password form validates, then submits successfully.
  test("settings: password form validates, then submits successfully", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f, { configWithApiKeys: settingsConfig(f) });
    const metaResp = page.waitForResponse(
      (r) => r.url().includes("/admin/api/config/meta") && r.status() === 200,
      { timeout: 5000 },
    );
    await page.goto("http://127.0.0.1:4173/admin/#settings");
    await metaResp;
    await expect(
      page.getByRole("heading", { name: "Security", exact: true }),
    ).toBeVisible();

    // Eye toggles reveal the password text.
    const eyes = page.getByRole("button", { name: "Show password" });
    await expect(eyes.first()).toBeVisible();
    await eyes.first().click();
    await expect(
      page.getByRole("button", { name: "Hide password" }).first(),
    ).toBeVisible();

    // A short password surfaces the min-length note and keeps submit
    // disabled.
    const submit = page.getByRole("button", { name: "Update Password" });
    await expect(submit).toBeDisabled();
    await page.locator("#sec-new-password").fill("abc");
    await expect(page.getByText("Minimum 6 characters")).toBeVisible();
    await expect(submit).toBeDisabled();

    // The factory default is rejected with its own note.
    await page.locator("#sec-new-password").fill("123456");
    await expect(
      page.getByText("Cannot be factory default (123456)"),
    ).toBeVisible();
    await expect(submit).toBeDisabled();

    // A valid new password plus the current password enables submit; the
    // mocked endpoint succeeds and the fields clear.
    await page.locator("#sec-current-password").fill("oldpass1");
    await page.locator("#sec-new-password").fill("newpass123");
    await expect(submit).toBeEnabled();
    const changeReq = page.waitForRequest(
      (r) =>
        r.method() === "POST" && r.url().includes("/admin/api/change-password"),
    );
    await submit.click();
    const req = await changeReq;
    expect(JSON.parse(req.postData() || "{}")).toEqual({
      current_password: "oldpass1",
      new_password: "newpass123",
    });
    await expect(page.locator("#sec-current-password")).toHaveValue("");
    await expect(page.locator("#sec-new-password")).toHaveValue("");
    await expect(page.getByText("Password changed")).toBeVisible();
  });

  // -------------------------------------------------------------------------
  // 13. Sidebar reaches every section.
  // -------------------------------------------------------------------------
  test("nav: sidebar links reach every section", async ({ page }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    const nav = page.locator(
      'aside[aria-label="Sidebar"] nav[aria-label="Main navigation"]',
    );

    await page.goto("http://127.0.0.1:4173/admin/#overview");
    for (const [link, heading] of [
      ["Pool", "Pool"],
      ["Usage", "Usage"],
      ["Logs", "Logs"],
      ["Settings", "Settings"],
    ] as Array<[string, string]>) {
      await nav.getByRole("link", { name: link }).click();
      await expect(
        page.getByRole("heading", { name: heading, exact: true }),
      ).toBeVisible();
    }
  });

  // -------------------------------------------------------------------------
  // 14. Overview: a failed load shows Retry and recovers on click.
  // -------------------------------------------------------------------------
  test("overview: failed load shows retry and recovers on click", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    let calls = 0;
    await page.unroute("**/admin/api/overview*");
    await page.route("**/admin/api/overview*", async (route) => {
      calls += 1;
      if (calls === 1) {
        await route.fulfill({ status: 500, body: "boom" });
      } else {
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify(f.overview),
        });
      }
    });

    await page.goto("http://127.0.0.1:4173/admin/#overview");
    await expect(page.getByRole("button", { name: "Retry" })).toBeVisible();
    await page.getByRole("button", { name: "Retry" }).click();
    await expect(page.getByRole("heading", { name: "Overview" })).toBeVisible();
    await expect(page.getByText("Pool total")).toBeVisible();
  });
});
