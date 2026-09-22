import { test, expect } from "@playwright/test";
import { loadFixtures, mockDashboard } from "./mocks.js";
import {
  adminUrl,
  captureConfigPosts,
  configWithKeysEnv,
  discountToken,
  forceLogin401,
  killTokensApi,
  meteredToken,
  mockRefundReplay,
  mockTeamUsage,
  mockUsage,
  refundPendingToken,
  SEED_KEYS,
  settledRefundToken,
  teamKeyRow,
  tokenRow,
  tokensPayload,
} from "./mock-usage.js";

// Keys + auth + usage + logs flows against fake backend state (mock-usage
// builders over the shared mock-data factory, served by the shared
// mocks.ts route layer). Web-first assertions only: role/label locators,
// no sleeps, each test seeds its own state (DAMP).

const toasts = (page: Parameters<typeof mockDashboard>[0]) =>
  page.getByLabel("Notifications");

test.describe("keys, usage and logs (mock backend)", () => {
  test.use({ expect: { timeout: 10_000 } });

  test("keys: empty env shows the open-mode note and no masked rows", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(
      page,
      f,
      { configWithApiKeys: configWithKeysEnv(f, []) },
      { loginPage: true },
    );

    await page.goto(adminUrl("overview"));
    await expect(
      page.getByRole("heading", { name: "Client API Keys" }),
    ).toBeVisible();
    await expect(
      page.getByText("No client API keys configured."),
    ).toBeVisible();
    await expect(page.getByText(/sk-fb-•/)).toHaveCount(0);
    await expect(
      page.getByRole("button", { name: "Generate API Key" }),
    ).toBeVisible();
  });

  test("keys: seeded keys render masked, never in the clear", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(
      page,
      f,
      { configWithApiKeys: configWithKeysEnv(f, SEED_KEYS) },
      { loginPage: true },
    );

    await page.goto(adminUrl("overview"));
    await expect(
      page.getByRole("heading", { name: "Client API Keys" }),
    ).toBeVisible();
    await expect(page.getByText(/sk-fb-•/).first()).toBeVisible();
    for (const seed of SEED_KEYS) {
      await expect(page.getByText(seed)).toHaveCount(0);
    }
  });

  test("keys: generate posts the .env save, shows the key once, Done toasts", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(
      page,
      f,
      { configWithApiKeys: configWithKeysEnv(f, [SEED_KEYS[0]]) },
      { loginPage: true },
    );
    const saves: string[] = [];
    await captureConfigPosts(page, saves);

    await page.goto(adminUrl("overview"));
    await expect(
      page.getByRole("heading", { name: "Client API Keys" }),
    ).toBeVisible();
    await page.getByRole("button", { name: "Generate API Key" }).click();

    const dialog = page.getByRole("dialog");
    await expect(dialog).toContainText("Client API Key Generated");
    await expect(dialog).toContainText("Saved to .env in API_KEYS");
    const shown = (await dialog.locator("code").innerText()).trim();
    expect(shown.startsWith("sk-fb-")).toBe(true);
    expect(shown.length).toBeGreaterThan(10);
    // The write went to POST /admin/config (form `content=` carrying the
    // full document), not the settings overlay.
    await expect.poll(() => saves.length).toBeGreaterThan(0);
    expect(decodeURIComponent(saves[saves.length - 1])).toContain("API_KEYS=");

    await dialog.getByRole("button", { name: "Done" }).click();
    await expect(dialog).toHaveCount(0);
    await expect(
      toasts(page)
        .getByRole("status")
        .filter({ hasText: "Generated & saved client API key" }),
    ).toBeVisible();
  });

  test("keys: failed save surfaces an error toast and no modal", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(
      page,
      f,
      { configWithApiKeys: configWithKeysEnv(f, [SEED_KEYS[0]]) },
      { loginPage: true },
    );
    const saves: string[] = [];
    await captureConfigPosts(page, saves, 500);

    await page.goto(adminUrl("overview"));
    await expect(
      page.getByRole("heading", { name: "Client API Keys" }),
    ).toBeVisible();
    await page.getByRole("button", { name: "Generate API Key" }).click();

    await expect.poll(() => saves.length).toBeGreaterThan(0);
    await expect(page.getByRole("dialog")).toHaveCount(0);
    await expect(
      toasts(page).getByRole("alert").filter({ hasText: "Config save failed" }),
    ).toBeVisible();
  });

  test("keys: reveal unmasks; delete Cancel is free, Delete posts + toasts", async ({
    page,
    context,
  }) => {
    const f = loadFixtures();
    await mockDashboard(
      page,
      f,
      { configWithApiKeys: configWithKeysEnv(f, [SEED_KEYS[0]]) },
      { loginPage: true },
    );
    const posts: string[] = [];
    await captureConfigPosts(page, posts);

    await page.goto(adminUrl("overview"));
    await expect(
      page.getByRole("heading", { name: "Client API Keys" }),
    ).toBeVisible();
    const keyRow = page.locator("div.fp-inset", { hasText: "sk-fb-" }).first();
    await expect(keyRow).toBeVisible();

    await keyRow.getByRole("button", { name: "Show API key" }).click();
    await expect(keyRow.getByText(SEED_KEYS[0])).toBeVisible();
    await keyRow.getByRole("button", { name: "Hide API key" }).click();
    await expect(keyRow.getByText(SEED_KEYS[0])).toHaveCount(0);

    context.once("dialog", (d) => d.dismiss());
    await keyRow.getByRole("button", { name: "Delete API key" }).click();
    await expect.poll(() => posts.length).toBe(0);

    context.once("dialog", (d) => d.accept());
    await keyRow.getByRole("button", { name: "Delete API key" }).click();
    await expect.poll(() => posts.length).toBeGreaterThan(0);
    expect(decodeURIComponent(posts[posts.length - 1])).not.toContain(
      SEED_KEYS[0],
    );
    await expect(
      toasts(page)
        .getByRole("status")
        .filter({ hasText: "Deleted client API key" }),
    ).toBeVisible();
  });

  test("auth: 401 login stays on the login view with the server error", async ({
    page,
    context,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f, {}, { loginPage: true });
    await forceLogin401(page);

    await page.goto("http://127.0.0.1:4173/admin/login");
    await page.locator("#token").fill("wrong-token");
    await page.getByRole("button", { name: "Sign in" }).click();

    await expect(
      toasts(page).getByRole("alert").filter({ hasText: "Invalid admin" }),
    ).toBeVisible();
    expect(page.url()).toContain("/admin/login");
    await expect(page.getByRole("button", { name: "Sign in" })).toBeVisible();
    const cookies = await context.cookies("http://127.0.0.1:4173");
    expect(cookies.find((c) => c.name === "fb_admin")).toBeUndefined();
  });

  test("auth: dead API session raises the expired banner; Log in lands on login", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f, {}, { loginPage: true });
    await killTokensApi(page);
    const deadResp = page.waitForResponse(
      (r) => r.url().includes("/admin/api/tokens") && r.status() === 401,
      { timeout: 15_000 },
    );

    await page.goto(adminUrl("tokens"));
    await deadResp;
    await expect(page.getByText("Session expired")).toBeVisible();
    await expect(
      page.getByText(
        "Your session has ended. Sign in again to continue using the dashboard.",
      ),
    ).toBeVisible();
    await page.getByRole("button", { name: "Log in" }).click();
    await expect(page.locator("#token")).toBeVisible();
    expect(page.url()).toContain("/admin/#login");
  });

  test("usage: freebucks empty renders the hint with no legacy session bars", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(
      page,
      f,
      { tokens: tokensPayload([tokenRow(0), tokenRow(1)]) },
      { loginPage: true },
    );

    await page.goto(adminUrl("tokens"));
    await page.getByRole("button", { name: "Allowances" }).click();
    await expect(
      page.getByRole("heading", { name: "Accounts", exact: true }),
    ).toBeVisible();
    await expect(
      page.getByRole("heading", { name: "Account #1" }),
    ).toBeVisible();
    await expect(
      page.getByText(
        "No Freebucks data — run a request or Probe all to populate.",
      ),
    ).toHaveCount(2);
    // Legacy session/premium quota bars are gone: no shared-pool bar, no
    // per-model session table, no usage rings, no reset countdowns.
    await expect(page.getByText("Shared pool")).toHaveCount(0);
    await expect(
      page.getByRole("heading", { name: "Session quota by model" }),
    ).toHaveCount(0);
    await expect(page.getByRole("progressbar")).toHaveCount(0);
    await expect(page.getByText("resets in")).toHaveCount(0);
  });

  test("usage: metered account renders the daily ring and the shared reset strip", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(
      page,
      f,
      { tokens: tokensPayload([meteredToken(0), tokenRow(1)]) },
      { loginPage: true },
    );

    await page.goto(adminUrl("tokens"));
    await page.getByRole("button", { name: "Allowances" }).click();
    await expect(
      page.getByRole("heading", { name: "Account #1" }),
    ).toBeVisible();
    const header = page.getByTestId("freebucks-header").first();
    await expect(header).toContainText("30/75 Freebucks daily");
    await expect(header).toContainText("20 in wallet");
    await expect(header).not.toContainText("resets in");
    // Daily usage ring: 45 of 75 spent = 60%.
    const ring = page.getByRole("progressbar", { name: /Daily usage/ });
    await expect(ring).toBeVisible();
    await expect(ring).toHaveAttribute("aria-valuenow", "60");
    // One shared countdown for all accounts, never per row.
    await expect(page.getByTestId("reset-strip")).toContainText("resets in");
    await expect(page.getByTestId("reset-strip")).toContainText(
      "shared for all accounts",
    );
    await expect(page.getByTestId("reset-strip")).toHaveCount(1);
  });

  test("usage: first-tab discount line when offered, absent otherwise", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(
      page,
      f,
      { tokens: tokensPayload([discountToken(0, true), tokenRow(1)]) },
      { loginPage: true },
    );

    await page.goto(adminUrl("tokens"));
    await page.getByRole("button", { name: "Allowances" }).click();
    await expect(
      page.getByRole("heading", { name: "Account #1" }),
    ).toBeVisible();
    const line = page.getByTestId("first-tab-discount").first();
    await expect(line).toContainText("First-tab discount");
    await expect(line).toContainText("up to 3 Freebucks off one session");
    await expect(line).toContainText("prices shown include it");
    // The account without the vendor block renders no line.
    await expect(page.getByTestId("first-tab-discount")).toHaveCount(1);
  });

  test("usage: in-use copy while another session holds the offer", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(
      page,
      f,
      { tokens: tokensPayload([discountToken(0, false)]) },
      { loginPage: true },
    );
    await page.goto(adminUrl("tokens"));
    await page.getByRole("button", { name: "Allowances" }).click();
    await expect(
      page.getByRole("heading", { name: "Account #1" }),
    ).toBeVisible();
    const line = page.getByTestId("first-tab-discount").first();
    await expect(line).toContainText("First-tab discount in use");
    await expect(line).toContainText("parallel sessions pay the regular price");
  });
  test("usage: tier prefix and pending-refund line render once", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(
      page,
      f,
      {
        tokens: tokensPayload([refundPendingToken(0), meteredToken(1)]),
      },
      { loginPage: true },
    );

    await page.goto(adminUrl("tokens"));
    await page.getByRole("button", { name: "Allowances" }).click();
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

  test("usage: pending refund replays to a settled line on refresh", async ({
    page,
  }) => {
    const f = loadFixtures();
    const rows = [refundPendingToken(0)];
    const state = tokensPayload(rows);
    await mockDashboard(page, f, { tokens: state }, { loginPage: true });
    await mockRefundReplay(page, state);
    const replayed = page.waitForRequest(
      (r) =>
        r.method() === "POST" &&
        r.url().includes("/admin/tokens/0/refund-refresh"),
    );

    await page.goto(adminUrl("tokens"));
    await page.getByRole("button", { name: "Allowances" }).click();
    await replayed;
    // Vendor formatFreebucks rounds the 1.5 mock refund to 2.
    await expect(page.getByTestId("refund-settled-line")).toContainText(
      "2 Freebucks returned to your wallet.",
    );
    await expect(page.getByTestId("refund-line")).toHaveCount(0);
  });

  test("usage: settled zero refund renders the zero line", async ({ page }) => {
    const f = loadFixtures();
    await mockDashboard(
      page,
      f,
      { tokens: tokensPayload([settledRefundToken(0, 0)]) },
      { loginPage: true },
    );

    await page.goto(adminUrl("tokens"));
    await page.getByRole("button", { name: "Allowances" }).click();
    await expect(page.getByTestId("refund-settled-line").first()).toContainText(
      "0 Freebucks returned to your wallet.",
    );
  });

  test("activity: four tabs on main (Live/Metrics/Team/Traces, no Logging)", async ({
    page,
  }) => {
    // #609 merged the per-client team-usage tab into Activity: the tab set
    // is Live/Metrics/Team/Traces. A count change here means the IA moved
    // again, not that this test is stale.
    await mockUsage(page, { loginPage: true });

    await page.goto(adminUrl("activity"));
    const tabs = page.getByRole("group", { name: "Activity view" });
    await expect(tabs.getByRole("button")).toHaveCount(4);
    await expect(tabs.getByRole("button", { name: "Requests" })).toBeVisible();
    await expect(tabs.getByRole("button", { name: "Metrics" })).toBeVisible();
    await expect(
      tabs.getByRole("button", { name: "Client Keys" }),
    ).toBeVisible();
    await expect(tabs.getByRole("button", { name: "Traces" })).toBeVisible();
    await expect(page.getByRole("button", { name: "Logging" })).toHaveCount(0);
    await expect(page.locator('select[aria-label="LOG_LEVEL"]')).toHaveCount(0);
    await expect(
      page.getByText("Live traffic, metrics, team usage, and traces."),
    ).toBeVisible();
  });

  test("activity: Team tab renders per-key rows, then the empty state", async ({
    page,
  }) => {
    await mockUsage(page, { loginPage: true });
    await mockTeamUsage(page, [
      teamKeyRow(),
      teamKeyRow({
        key_id: "zzzz000011112222",
        requests: 1,
        success_rate: 1,
        total_tokens: 10,
        by_model: {},
        freebucks: 0,
      }),
    ]);

    await page.goto(adminUrl("activity"));
    await page.getByRole("button", { name: "Client Keys" }).click();
    await expect(
      page.getByRole("heading", { name: "Team usage" }),
    ).toBeVisible();
    const teamTable = page.locator("table", {
      has: page.getByRole("columnheader", { name: "Person / key" }),
    });
    // Short-hash labels only: raw API keys never reach the DOM.
    await expect(teamTable.getByText("aaaabbbbccccdddd")).toBeVisible();
    await expect(teamTable.getByText("zzzz000011112222")).toBeVisible();
    await expect(teamTable.getByText("40 Freebucks")).toBeVisible();
    await expect(teamTable.getByText("50.0%")).toBeVisible();
    await expect(teamTable.getByText("test/priced · 165")).toBeVisible();
    // Raw client keys stay out of the panel even as substrings.
    await expect(teamTable.getByText(/sk-fb-/)).toHaveCount(0);

    await mockTeamUsage(page, []);
    await page.getByRole("button", { name: "Refresh all" }).click();
    await expect(page.getByText("No per-client usage yet.")).toBeVisible();
  });
  test("activity: Live console, Metrics KPIs and Traces rows render from mocks", async ({
    page,
  }) => {
    await mockUsage(page, { loginPage: true });

    await page.goto(adminUrl("activity"));
    await expect(page.getByText("1 model request").first()).toBeVisible();

    await page.getByRole("button", { name: "Table" }).click();
    await expect(page.locator("#log-level")).toBeVisible();
    await expect(page.locator("#log-msg")).toBeVisible();
    const levelResp = page.waitForResponse(
      (r) =>
        r.url().includes("/admin/api/logs") &&
        r.url().includes("level=error") &&
        r.status() === 200,
    );
    await page.locator("#log-level").selectOption("error");
    await levelResp;
    await expect(page.getByText("request 2 completed")).toBeVisible();

    await page.getByRole("button", { name: "Metrics" }).click();
    await expect(page.getByText("Requests served")).toBeVisible();
    await expect(
      page.getByRole("heading", { name: "Account fleet activity" }),
    ).toBeVisible();

    await page.getByRole("button", { name: "Traces" }).click();
    await expect(page.locator("table tbody tr")).toHaveCount(2);
    const traceTable = page.locator("table");
    await expect(
      traceTable.getByText("deepseek/deepseek-v4-flash"),
    ).toBeVisible();
    await expect(traceTable.getByText("acquire_ms")).toBeVisible();
    await expect(traceTable.getByText("upstream timeout")).toBeVisible();
  });

  test("logs: LOG_LEVEL lives on Settings and instant-saves via the overlay", async ({
    page,
  }) => {
    const { posted } = await mockUsage(page, { loginPage: true });
    const metaResp = page.waitForResponse(
      (r) => r.url().includes("/admin/api/config/meta") && r.status() === 200,
      { timeout: 5000 },
    );

    await page.goto(adminUrl("settings"));
    await metaResp;
    await expect(
      page.getByRole("heading", { name: "Server Log Level" }),
    ).toBeVisible();
    const level = page.locator('select[aria-label="LOG_LEVEL"]');
    await expect(level).toBeVisible();
    await level.selectOption("warn");
    await page.waitForRequest(
      (r) => r.method() === "POST" && r.url().includes("/admin/api/settings"),
      { timeout: 10_000 },
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

  test("logs: legacy #config alias routes to Settings and edits the same control", async ({
    page,
  }) => {
    const { posted } = await mockUsage(page, { loginPage: true });
    const metaResp = page.waitForResponse(
      (r) => r.url().includes("/admin/api/config/meta") && r.status() === 200,
      { timeout: 5000 },
    );

    await page.goto(adminUrl("config"));
    await metaResp;
    await expect(
      page.getByRole("heading", { name: "Settings", exact: true }),
    ).toBeVisible();
    await expect(
      page.getByRole("link", { name: "Manage log level (Logs → Logging tab)" }),
    ).toHaveCount(0);
    const logLevel = page.getByRole("combobox", { name: "LOG_LEVEL" });
    await expect(logLevel).toBeVisible();
    await logLevel.selectOption("warn");
    await page.waitForRequest(
      (r) => r.method() === "POST" && r.url().includes("/admin/api/settings"),
      { timeout: 10_000 },
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
});
