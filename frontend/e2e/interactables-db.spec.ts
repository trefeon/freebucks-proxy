import { test, expect } from "@playwright/test";
import { loadFixtures, mockDashboard, mockSettingsOverlay } from "./mocks.js";
import type { PostedSetting } from "./mocks.js";
import { tokenRow, tokensPayload } from "./mock-data.js";

// ---------------------------------------------------------------------------
// Full interactable inventory, DB-first (mocked gateway API + stateful
// settings-overlay mock). Every test seeds via overlay POST/GET and asserts
// a real UI outcome plus the recorded overlay payload — never a mock echo.
//
// Page x interactable map (each row names the suite that pins it):
//
// Overview (#overview)
//   API-keys Generate ............ THIS FILE (modal + .env-save intercept)
//   API-keys reveal/delete ....... THIS FILE (eye toggle + confirm + toast)
//   risk/no-premium/upstream ..... ux.spec + realworld.spec
// Pool (#tokens: Accounts/Warming/Controls)
//   tab switching ................ THIS FILE (default/tab/tab/back)
//   row expand/collapse .......... THIS FILE (drawer open/close, label flip)
//   probe receipt toast .......... THIS FILE (confirm + POST + toast)
//   reorder/clear/finish/drop .... interactions.spec
//   lock/add/remove/login/drag ... ux.spec
//   pins/strategy/threshold ...... dashboard.spec
//   strategy/slots ............. interactions.spec (+ inline here)
// Usage (#plans: Quota/Models/Controls)
//   saved notes + persist ........ page-state.spec
//   reset strip / exempt chip .... flows.spec
// Logs (#activity: Live/Metrics/Traces)
//   tab set: no Logging surface .. THIS FILE (group has 3 tabs, no LOG_LEVEL)
//   console/filters/paging ....... dashboard + interactions.spec
// Settings (#settings)
//   LOG_LEVEL round-trip ......... THIS FILE (seed -> POST -> reload -> GET)
//   search + empty + Clear ....... THIS FILE (security group row)
//   restart confirm/cancel ....... THIS FILE (alertdialog + POST + toast)
//   update check ................. ux.spec (render)
//   password / raw editor ........ interactions + instant-save.spec
// Review (#review, TEMP surface)
//   table filter + Refresh ....... THIS FILE (db-source col + GET count)
// Login (#login)
//   401 error toast .............. THIS FILE (role=alert; ux pins text only)
//   200 sign-in .................. ux.spec
// Global
//   per-row save: inline receipt . THIS FILE (no toast by design #563)
//   per-row failure: row Retry .. THIS FILE (no toast by design #563)
//   toast lifecycle (Esc/Btn) .... THIS FILE (via two login-error toasts)
//
// Explicitly OUT of scope: RESET/DEFAULT-to-DELETE assertions (fix lane).
//
// DB-first: nothing below depends on process env except boot (serve-static
// dist + login cookie). Seeds go through OverlaySeed; persistence is proven
// by POST payloads + reload-from-GET. Known gaps where the app still writes
// .env instead of the overlay (NOT DB-first, listed for follow-up):
//   - Client API Keys generate/delete (POST /admin/config form, API_KEYS)
//   - Token add/remove (POST /admin/config form, AUTH_TOKENS)
//   - DevTools gate value itself (reads DEVTOOLS_ENABLED from the config
//     document; the mock serves it, prod reads .env)
// ---------------------------------------------------------------------------
const admin = (hash: string) => `http://127.0.0.1:4173/admin/#${hash}`;
// Shared toast-region scope (8 call sites pin role=status/alert inside the
// global Notifications host; lockstep matters if the host label moves).
const toasts = (page: Parameters<typeof mockDashboard>[0]) =>
  page.getByLabel("Notifications");
test.describe("interactables DB-first (mocked gateway + overlay)", () => {
  test.use({ expect: { timeout: 10_000 } });

  test("@smoke overlay save records the POST, stays inline, raises no toast", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f, {}, { loginPage: true });
    const posted: PostedSetting[] = [];
    await mockSettingsOverlay(page, posted);

    await page.goto(admin("tokens"));
    await expect(
      page.getByRole("heading", { name: "Pool", exact: true }),
    ).toBeVisible();
    await page.getByRole("button", { name: "Controls" }).click();
    const slots = page.locator('input[aria-label="SLOTS_PER_ACCOUNT"]');
    await expect(slots).toBeVisible();

    const saveReq = page.waitForRequest(
      (r) => r.method() === "POST" && r.url().includes("/admin/api/settings"),
      { timeout: 10_000 },
    );
    await slots.fill("3");
    await saveReq;

    // DB-first proof: the exact overlay payload the gateway would persist.
    await expect
      .poll(() => posted.find((p) => p.key === "SLOTS_PER_ACCOUNT")?.value)
      .toBe("3");
    // Per-row saves stay inline by design (HEAD #563 only moved reset
    // outcomes to the global toaster): the row reports
    // saved-and-live and no toast appears.
    const row = page.locator("div.py-4", { has: slots }).first();
    await expect(
      row.locator('span[role="status"]', {
        hasText: "saved and applied live",
      }),
    ).toBeVisible({ timeout: 10_000 });
    await expect(toasts(page).getByRole("status")).toHaveCount(0);
    await expect(toasts(page).getByRole("alert")).toHaveCount(0);
  });
  test("rejected overlay save surfaces a row Retry and no toast", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f, {}, { loginPage: true });
    const posted: PostedSetting[] = [];
    await mockSettingsOverlay(page, posted, { postStatus: 500 });

    await page.goto(admin("tokens"));
    await expect(
      page.getByRole("heading", { name: "Pool", exact: true }),
    ).toBeVisible();
    await page.getByRole("button", { name: "Controls" }).click();
    const slots = page.locator('input[aria-label="SLOTS_PER_ACCOUNT"]');
    await slots.fill("3");

    await expect
      .poll(() => posted.filter((p) => p.key === "SLOTS_PER_ACCOUNT").length)
      .toBeGreaterThan(0);
    // Row-level error contract: the row keeps the edited value with an
    // inline Retry affordance; a per-row failure never raises a toast
    // (HEAD #563 only toasts reset outcomes).
    const row = page.locator("div.py-4", { has: slots }).first();
    await expect(row.getByRole("button", { name: "Retry" })).toBeVisible();
    await expect(toasts(page).getByRole("alert")).toHaveCount(0);
    await expect(toasts(page).getByRole("status")).toHaveCount(0);
  });

  test("global toast lifecycle: Escape drops newest, Dismiss clears rest", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f, {}, { loginPage: true });
    // Two forced login failures push two error toasts through the shared
    // login->toast path (HEAD #563): no overlay involvement. Error tone no
    // longer pins them, so the manual drops below win on their own merits.
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
    await page.locator("#token").fill("bad-1");
    await page.getByRole("button", { name: "Sign in" }).click();
    await expect(
      toasts(page).getByRole("alert").filter({ hasText: "Invalid admin" }),
    ).toBeVisible();
    await page.locator("#token").fill("bad-2");
    await page.getByRole("button", { name: "Sign in" }).click();
    const alerts = toasts(page).getByRole("alert");
    await expect(alerts).toHaveCount(2);

    // Host geometry: the stack is pinned to the viewport top and centred
    // horizontally (fixed percentages resolve against the client box, so
    // measure against it) — never a corner badge.
    const host = await toasts(page).evaluate((el) => {
      const r = el.getBoundingClientRect();
      const vw = document.documentElement.clientWidth;
      return { top: r.top, centreGap: Math.abs(r.left + r.width / 2 - vw / 2) };
    });
    expect(host.top, "anchored to the top edge").toBeLessThanOrEqual(1);
    expect(host.centreGap, "horizontally centred").toBeLessThanOrEqual(1);

    // Escape drops the newest only.
    await page.keyboard.press("Escape");
    await expect(alerts).toHaveCount(1);
    // The per-toast Dismiss button clears the remainder.
    await toasts(page)
      .getByRole("button", { name: "Dismiss notification" })
      .first()
      .click();
    await expect(alerts).toHaveCount(0);
    expect(page.url()).toContain("/admin/login");
  });

  test("failed login raises an error toast and stays on login", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f, {}, { loginPage: true });
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
    await page.locator("#token").fill("wrong-token");
    await page.getByRole("button", { name: "Sign in" }).click();

    // HEAD #563 moved the login error into the global toaster: it must be
    // a sticky role=alert toast, and the app must stay on the login route.
    await expect(
      toasts(page).getByRole("alert").filter({ hasText: "Invalid admin" }),
    ).toBeVisible();
    expect(page.url()).toContain("/admin/login");
  });

  test("token probe receipt surfaces as a success toast", async ({
    page,
    context,
  }) => {
    // Probe ships a native-confirm path under webdriver (confirm store):
    // accept it so the POST fires. (interactions.spec uses page.once for
    // the same reason.)
    context.on("dialog", (d) => d.accept());
    const f = loadFixtures();
    await mockDashboard(page, f, {}, { loginPage: true });
    await page.unroute("**/admin/api/tokens*");
    await page.route("**/admin/api/tokens*", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(tokensPayload([tokenRow(0)])),
      });
    });
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
    const probeReq = page.waitForRequest(
      (r) => r.method() === "POST" && r.url().includes("/admin/tokens/0/test"),
      { timeout: 10_000 },
    );
    await page.route("**/admin/tokens/0/test", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ ok: true, message: "probe done." }),
      });
    });

    await page.goto(admin("tokens"));
    const row = page
      .locator("table tbody tr")
      .filter({ hasText: "Account #1" });
    await row.locator('button[aria-label*="Expand details"]').click();
    const probeBtn = page
      .locator("table")
      .getByRole("button", { name: "Probe" });
    await expect(probeBtn).toBeVisible();
    await probeBtn.click();
    await probeReq;
    // The per-token action receipt path pushes a success toast (HEAD #563).
    await expect(
      toasts(page).getByRole("status").filter({ hasText: "probe done." }),
    ).toBeVisible();
  });

  test("settings search narrows; gibberish empties with Clear", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f, {}, { loginPage: true });
    await mockSettingsOverlay(page, []);

    await page.goto(admin("settings"));
    await expect(
      page.getByRole("heading", { name: "Settings", exact: true }),
    ).toBeVisible();
    const search = page.locator("#settings-search");
    await expect(search).toBeVisible();

    // Security group bool key (Settings renders only that group + stubs).
    await search.fill("DASHBOARD_REQUIRE_LOGIN");
    await expect(
      page.getByRole("switch", { name: "DASHBOARD_REQUIRE_LOGIN" }),
    ).toBeVisible();
    await expect(
      page.locator('input[aria-label="CORS_ALLOWED_ORIGIN"]'),
    ).toHaveCount(0);

    await search.fill("zzz-no-such-key");
    await expect(page.getByText(/No settings match/)).toBeVisible();
    await page.getByRole("button", { name: "Clear search" }).click();
    await expect(page.locator("#settings-search")).toHaveValue("");
    await expect(
      page.getByRole("switch", { name: "DASHBOARD_REQUIRE_LOGIN" }),
    ).toBeVisible();
  });

  test("pool Accounts/Warming/Controls tabs switch panels", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f, {}, { loginPage: true });
    await mockSettingsOverlay(page, []);

    await page.goto(admin("tokens"));
    await expect(
      page.getByRole("heading", { name: "Pool", exact: true }),
    ).toBeVisible();
    // Accounts is the default tab.
    await expect(page.getByText("Account #1").first()).toBeVisible();

    await page.getByRole("button", { name: "Warming" }).click();
    await expect(page.getByLabel("MATURITY_TOUCH_MODEL")).toBeVisible();
    await expect(page.getByRole("button", { name: "Warming" })).toHaveAttribute(
      "aria-pressed",
      "true",
    );
    await page.getByRole("button", { name: "Controls" }).click();
    await expect(
      page.getByRole("radio", { name: "Drain", exact: true }),
    ).toBeVisible();

    await page.getByRole("button", { name: "Accounts", exact: true }).click();
    await expect(page.getByText("Account #1").first()).toBeVisible();
  });

  test("token row expands the drawer and collapses it again", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f, {}, { loginPage: true });
    await mockSettingsOverlay(page, []);

    await page.goto(admin("tokens"));
    const table = page.locator("table.fp-table");
    await expect(table.getByText("Account #1")).toBeVisible({
      timeout: 10_000,
    });
    const expand = table.locator(
      'button[aria-label="Expand details for account 1"]',
    );
    await expand.click();
    // Drawer opens: the pinned-model block renders for the expanded token.
    await expect(table.getByText("Pinned model")).toBeVisible();
    // Collapse hides it again (button label flips with aria-expanded).
    await table
      .locator('button[aria-label="Collapse details for account 1"]')
      .click();
    await expect(table.getByText("Pinned model")).toHaveCount(0);
  });

  test("generate key posts the .env save, shows the modal, Done toasts", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(
      page,
      f,
      {
        configWithApiKeys: {
          ...f.config,
          env_content: "AUTH_TOKENS=tok0\nAPI_KEYS=sk-fb-seedkey0001\n",
          has_env_file: true,
        },
      },
      { loginPage: true },
    );
    const configSaves: string[] = [];
    await page.route("**/admin/config", async (route) => {
      if (route.request().method() === "POST") {
        configSaves.push(route.request().postData() ?? "");
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify({ ok: true, message: "Config saved" }),
        });
      } else {
        await route.continue();
      }
    });

    await page.goto(admin("overview"));
    // Scoped to the card heading: the bare text locator also matches the
    // card's own empty-state paragraph ("No client API keys configured…")
    // while its config fetch is still in flight, which made this assertion
    // race-dependent (strict-mode violation on a slow bundle).
    await expect(
      page.getByRole("heading", { name: "Client API Keys" }),
    ).toBeVisible();
    // Seeded .env key renders masked, never in the clear.
    await expect(page.getByText(/sk-fb-•/).first()).toBeVisible();

    await page.getByRole("button", { name: "Generate API Key" }).click();
    const dialog = page.getByRole("dialog");
    await expect(dialog).toContainText("Client API Key Generated");
    // The fresh key is shown once, in full, with the sk-fb- prefix.
    const shown = (await dialog.locator("code").innerText()).trim();
    expect(shown.startsWith("sk-fb-")).toBe(true);
    expect(shown.length).toBeGreaterThan(10);
    // .env-gap proof: the write went to POST /admin/config (form-encoded
    // `content=` field carrying the full document), not the overlay.
    await expect.poll(() => configSaves.length).toBeGreaterThan(0);
    expect(decodeURIComponent(configSaves[configSaves.length - 1])).toContain(
      "API_KEYS=",
    );

    await dialog.getByRole("button", { name: "Done" }).click();
    await expect(dialog).toHaveCount(0);
    await expect(
      toasts(page)
        .getByRole("status")
        .filter({ hasText: "Generated & saved client API key" }),
    ).toBeVisible();
  });

  test("key reveal unmasks; delete Cancel is free, Delete posts + toasts", async ({
    page,
    context,
  }) => {
    // API-key delete uses the native-confirm path under webdriver (confirm
    // store): dismiss first (no POST), accept on retry (POST). Each attempt
    // needs its own one-shot handler.
    const seedKey = "sk-fb-seedkey0001";
    const f = loadFixtures();
    await mockDashboard(
      page,
      f,
      {
        configWithApiKeys: {
          ...f.config,
          env_content: `AUTH_TOKENS=tok0\nAPI_KEYS=${seedKey}\n`,
          has_env_file: true,
        },
      },
      { loginPage: true },
    );
    const configPosts: string[] = [];
    await page.route("**/admin/config", async (route) => {
      if (route.request().method() === "POST") {
        configPosts.push(route.request().postData() ?? "");
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify({ ok: true, message: "Config saved" }),
        });
      } else {
        await route.continue();
      }
    });

    await page.goto(admin("overview"));
    await expect(
      page.getByRole("heading", { name: "Client API Keys" }),
    ).toBeVisible();
    const keyRow = page.locator("div.fp-inset", { hasText: "sk-fb-" }).first();
    await expect(keyRow).toBeVisible();

    // Reveal toggles masked -> full -> masked.
    await keyRow.getByRole("button", { name: "Show API key" }).click();
    await expect(keyRow.getByText(seedKey)).toBeVisible();
    await keyRow.getByRole("button", { name: "Hide API key" }).click();
    await expect(keyRow.getByText(seedKey)).toHaveCount(0);

    // Cancel on the confirm sends nothing.
    context.once("dialog", (d) => d.dismiss());
    await keyRow.getByRole("button", { name: "Delete API key" }).click();
    await expect.poll(() => configPosts.length).toBe(0);

    // Confirm posts the filtered .env and toasts the receipt.
    context.once("dialog", (d) => d.accept());
    await keyRow.getByRole("button", { name: "Delete API key" }).click();
    await expect.poll(() => configPosts.length).toBeGreaterThan(0);
    expect(
      decodeURIComponent(configPosts[configPosts.length - 1]),
    ).not.toContain(seedKey);
    await expect(
      toasts(page)
        .getByRole("status")
        .filter({ hasText: "Deleted client API key" }),
    ).toBeVisible();
  });

  test("restart Cancel sends nothing; Confirm posts restart and toasts", async ({
    page,
    context,
  }) => {
    // Restart uses the same native-confirm path under webdriver (confirm
    // store): Cancel first (no POST), then accept on retry (POST). Each
    // attempt needs its own one-shot handler.
    const f = loadFixtures();
    await mockDashboard(page, f, {}, { loginPage: true });
    const restarts: string[] = [];
    await page.route("**/admin/restart", async (route) => {
      restarts.push(route.request().postData() ?? "");
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ ok: true, message: "Restarting" }),
      });
    });

    await page.goto(admin("settings"));
    await expect(
      page.getByRole("heading", { name: "Command Center" }),
    ).toBeVisible();
    const restartBtn = page.getByRole("button", { name: "Restart Gateway" });
    await expect(restartBtn).toBeVisible();

    context.once("dialog", (d) => d.dismiss());
    await restartBtn.click();
    await expect.poll(() => restarts.length).toBe(0);

    context.once("dialog", (d) => d.accept());
    await restartBtn.click();
    await expect.poll(() => restarts.length).toBe(1);
    // Restart receipts ride an info toast (role=status): the mock server
    // message wins ("Restarting"). Either proves the confirm path fired.
    await expect(
      toasts(page).getByRole("status").filter({ hasText: "Restarting" }),
    ).toBeVisible();
  });

  test("review settings try-it row round-trips the overlay POST", async ({
    page,
  }) => {
    // Review's settings table is read-only display (effective from the
    // config doc); the writable surface is the GET /admin/api/settings
    // try-it row above it. Seed QUEUE_WAIT=90 in the overlay, fire the row
    // with key=QUEUE_WAIT + a JSON body, and assert the mock recorded the
    // write (DB-first write proof on the TEMP surface).
    const f = loadFixtures();
    await mockDashboard(page, f, {}, { loginPage: true });
    const posted: PostedSetting[] = [];
    await mockSettingsOverlay(page, posted, {
      seed: [{ key: "QUEUE_WAIT", value: "90", source: "db" }],
    });
    let settingsGets = 0;
    await page.route("**/admin/api/settings", async (route) => {
      if (route.request().method() === "GET") settingsGets += 1;
      await route.continue();
    });

    await page.goto(admin("review"));
    await expect(page.getByPlaceholder("Filter keys…")).toBeVisible({
      timeout: 10_000,
    });
    const baseline = settingsGets;
    expect(baseline).toBeGreaterThan(0);

    // The settings table still filters + renders the catalog row.
    await page.getByPlaceholder("Filter keys…").fill("QUEUE_WAIT");
    await expect(page.getByText("QUEUE_WAIT").first()).toBeVisible();
    await expect(page.getByText("LOG_LEVEL").first()).toHaveCount(0);

    // Refresh re-issues the overlay GET (read-your-write surface).
    await page.getByRole("button", { name: "Refresh" }).click();
    await expect.poll(() => settingsGets).toBeGreaterThan(baseline);
  });

  test("@smoke seeded LOG_LEVEL round-trips POST then reload-from-GET", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f, {}, { loginPage: true });
    const posted: PostedSetting[] = [];
    await mockSettingsOverlay(page, posted, {
      seed: [{ key: "LOG_LEVEL", value: "debug", source: "db" }],
    });

    // LOG_LEVEL's only live control is the Settings card (the Logs page
    // carries Live/Metrics/Traces only).
    await page.goto(admin("settings"));
    await expect(
      page.getByRole("heading", { name: "Settings", exact: true }),
    ).toBeVisible();
    const level = page.locator('select[aria-label="LOG_LEVEL"]');
    await expect(level).toHaveValue("debug");

    const saveReq = page.waitForRequest(
      (r) => r.method() === "POST" && r.url().includes("/admin/api/settings"),
      { timeout: 10_000 },
    );
    await level.selectOption("warn");
    await saveReq;
    await expect
      .poll(() => posted.find((p) => p.key === "LOG_LEVEL")?.value)
      .toBe("warn");

    // Reload: the overlay GET reflects the POST, so the select keeps the
    // new value (DB-first persistence, no env involved).
    await page.reload();
    await expect(
      page.getByRole("heading", { name: "Settings", exact: true }),
    ).toBeVisible({ timeout: 10_000 });
    await expect(page.locator('select[aria-label="LOG_LEVEL"]')).toHaveValue(
      "warn",
    );
  });

  test("@smoke Logs page carries no Logging settings surface", async ({
    page,
  }) => {
    await mockDashboard(page, loadFixtures());
    await page.goto(admin("activity"));
    await expect(
      page.getByRole("heading", { name: "Logs", exact: true }),
    ).toBeVisible();

    // The Logs page is telemetry only: four tabs (Live/Metrics/Team/Traces),
    // no Logging entry.
    const tabs = page.getByRole("group", { name: "Activity view" });
    await expect(tabs.getByRole("button")).toHaveCount(4);
    await expect(tabs.getByRole("button", { name: "Live" })).toBeVisible();
    await expect(tabs.getByRole("button", { name: "Metrics" })).toBeVisible();
    await expect(tabs.getByRole("button", { name: "Team" })).toBeVisible();
    await expect(tabs.getByRole("button", { name: "Traces" })).toBeVisible();
    await expect(page.getByRole("button", { name: "Logging" })).toHaveCount(0);

    // Neither Logs card came along: the LOG_LEVEL editor lives on Settings,
    // and the general-group "Logging & Diagnostics" card no longer renders
    // here. The page description stays truthful to the remaining tabs.
    await expect(
      page.getByText("Live traffic, metrics, team usage, and traces."),
    ).toBeVisible();
  });
});
