import { test, expect } from "@playwright/test";
import { loadFixtures, mockDashboard, mockSettingsOverlay } from "./mocks.js";
import type { PostedSetting } from "./mocks.js";
import {
  adminUrl,
  tokenRow,
  tokensPayload,
  MASQ_STRATEGY_SEED,
} from "./mock-data.js";

// Edge + state + a11y-behavioral coverage for every dashboard data view.
// One factory builds the fake backend (mock-data.ts), one route layer serves
// it (mocks.ts mockDashboard/mockSettingsOverlay). Web-first assertions only:
// role/label locators, no sleeps, no brittle selectors.
// Interactive targets are measured in whole CSS px: boundingBox reports
// sub-pixel float dust (observed 43.99999237 for a 44px floor), so round.
async function boxHeight(locator) {
  return Math.round((await locator.boundingBox())?.height ?? 0);
}

test.describe("dashboard edge states (mock backend)", () => {
  test.use({ expect: { timeout: 10_000 } });

  // -- Tokens (#tokens accounts tab: TokenTable inside PageShell) ---------

  test("tokens empty names the cause and the next step", async ({ page }) => {
    const f = loadFixtures();
    await mockDashboard(page, f, {
      tokens: tokensPayload([], { has_tokens: false, token_count: 0 }),
    });
    await mockSettingsOverlay(page, []);
    await page.goto(adminUrl("tokens"));
    await expect(
      page.getByRole("heading", { name: "No tokens in pool" }),
    ).toBeVisible();
    await expect(
      page.getByText(
        "Add one above or use Device Login to generate credentials via browser.",
      ),
    ).toBeVisible();
    // The next action is on the page, not just in the copy.
    await expect(
      page.getByRole("button", { name: "Device Login" }),
    ).toBeVisible();
  });

  test("tokens loading announces itself instead of staying silent", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    await mockSettingsOverlay(page, []);
    // Slow backend: the first tokens fetch answers after the page mounts.
    await page.unroute("**/admin/api/tokens*");
    await page.route("**/admin/api/tokens*", async (route) => {
      const { promise, resolve } = Promise.withResolvers<void>();
      setTimeout(resolve, 2000);
      await promise;
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(f.tokens),
      });
    });
    await page.goto(adminUrl("tokens"));
    await expect(page.getByRole("status", { name: "Loading" })).toBeVisible();
    await expect(page.getByText("Account #1").first()).toBeVisible();
  });
  test("tokens failure says what broke, offers retry, and recovers", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    await mockSettingsOverlay(page, []);
    let calls = 0;
    await page.unroute("**/admin/api/tokens*");
    await page.route("**/admin/api/tokens*", async (route) => {
      calls += 1;
      if (calls === 1) {
        await route.fulfill({ status: 500, body: "boom" });
      } else {
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify(f.tokens),
        });
      }
    });
    await page.goto(adminUrl("tokens"));
    // What failed (page heading + backend message) and what next (Retry).
    await expect(
      page.getByRole("heading", { name: "Could not load this page" }),
    ).toBeVisible();
    await expect(page.getByText("boom").first()).toBeVisible();
    await expect(page.getByRole("button", { name: "Retry" })).toBeVisible();
    await page.getByRole("button", { name: "Retry" }).click();
    await expect(page.getByText("Account #1").first()).toBeVisible();
  });

  test("expired rows carry no live facts", async ({ page }) => {
    // Post-#608 shape: an expired snapshot row ships no instance or model,
    // so the row must not show a stale session, model badge, or kill switch.
    const f = loadFixtures();
    await mockDashboard(page, f, {
      tokens: tokensPayload([
        tokenRow(0, {
          session_status: "expired",
          session_instance: "",
          session_model: "",
          session_remaining_seconds: 0,
        }),
        tokenRow(1),
      ]),
    });
    await mockSettingsOverlay(page, []);
    await page.goto(adminUrl("tokens"));
    const row = page
      .locator("table.fp-table tbody tr")
      .filter({ hasText: "Account #1" });
    await expect(row.getByText("expired")).toBeVisible();
    await expect(row.locator("code")).toHaveCount(0);
    await expect(row.getByRole("button", { name: "Drop Session" })).toHaveCount(
      0,
    );
  });
  // -- Accounts (#plans accounts tab: AllowancesPanel) ---------------------

  test("accounts empty names the cause and the next step", async ({ page }) => {
    const f = loadFixtures();
    await mockDashboard(page, f, {
      tokens: tokensPayload([], { has_tokens: false, token_count: 0 }),
    });
    await mockSettingsOverlay(page, []);
    await page.goto(adminUrl("plans"));
    await expect(
      page.getByRole("heading", { name: "No tokens in pool" }),
    ).toBeVisible();
    await expect(
      page.getByText(
        "Add a token to the pool to see Freebucks allowances and model pricing.",
      ),
    ).toBeVisible();
  });

  test("accounts loading shows a loading marker", async ({ page }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    await mockSettingsOverlay(page, []);
    await page.unroute("**/admin/api/tokens*");
    await page.route("**/admin/api/tokens*", async (route) => {
      const { promise, resolve } = Promise.withResolvers<void>();
      setTimeout(resolve, 2000);
      await promise;
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(f.tokens),
      });
    });
    await page.goto(adminUrl("plans"));
    await expect(page.getByRole("status", { name: "Loading" })).toBeVisible();
    await expect(page.getByText("Loading…").first()).toBeVisible();
    await expect(page.getByText("Account #1").first()).toBeVisible();
  });

  test("accounts failure offers retry and toasts what broke", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    await mockSettingsOverlay(page, []);
    let calls = 0;
    await page.unroute("**/admin/api/tokens*");
    await page.route("**/admin/api/tokens*", async (route) => {
      calls += 1;
      if (calls === 1) {
        await route.fulfill({ status: 500, body: "boom" });
      } else {
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify(f.tokens),
        });
      }
    });
    await page.goto(adminUrl("plans"));
    await expect(page.getByRole("button", { name: "Retry" })).toBeVisible();
    // The backend message renders inline in the panel, not toast-only.
    await expect(page.getByTestId("inline-error")).toContainText("boom");
    await expect(
      page.getByRole("alert").filter({ hasText: "Could not load this page" }),
    ).toBeVisible();
    await page.getByRole("button", { name: "Retry" }).click();
    await expect(page.getByText("Account #1").first()).toBeVisible();
  });

  // -- Models (#plans models tab: ModelsPanel) ------------------------------

  test("models loading announces itself instead of staying silent", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    await mockSettingsOverlay(page, []);
    await page.unroute("**/admin/api/models*");
    await page.route("**/admin/api/models*", async (route) => {
      const { promise, resolve } = Promise.withResolvers<void>();
      setTimeout(resolve, 2000);
      await promise;
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(f.models),
      });
    });
    await page.goto(adminUrl("plans"));
    await expect(page.getByText("Account #1").first()).toBeVisible();
    await page.getByRole("button", { name: "Models" }).click();
    await expect(page.getByRole("status", { name: "Loading" })).toBeVisible();
    await expect(
      page.getByText("deepseek/deepseek-v4-flash").first(),
    ).toBeVisible();
  });

  test("models failure names the cause inline and recovers", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    await mockSettingsOverlay(page, []);
    let calls = 0;
    await page.unroute("**/admin/api/models*");
    await page.route("**/admin/api/models*", async (route) => {
      calls += 1;
      if (calls === 1) {
        await route.fulfill({ status: 500, body: "boom" });
      } else {
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify(f.models),
        });
      }
    });
    await page.goto(adminUrl("plans"));
    await page.getByRole("button", { name: "Models" }).click();
    // The backend message renders inline in the panel, not toast-only.
    await expect(page.getByTestId("inline-error")).toContainText("boom");
    const retry = page.getByRole("button", { name: "Retry" });
    await expect(retry).toBeVisible();
    await expect(retry).toBeEnabled();
    // DESIGN.md button scale: md controls are 40px on desktop (44 on coarse pointers).
    expect(await boxHeight(retry)).toBeGreaterThanOrEqual(40);
    await retry.click();
    await expect(
      page.getByText("deepseek/deepseek-v4-flash").first(),
    ).toBeVisible();
  });

  // -- Usage (#activity metrics tab: MetricsPanel) --------------------------

  test("usage with no entries in range says so in details view", async ({
    page,
  }) => {
    // The committed metrics fixture carries no `usage` key, so the usage
    // block falls back to an empty range, exactly like a quiet gateway.
    await mockDashboard(page, loadFixtures());
    await mockSettingsOverlay(page, []);
    await page.goto(adminUrl("activity"));
    await page.getByRole("button", { name: "Metrics" }).click();
    await expect(
      page.getByRole("heading", { name: "Token usage" }),
    ).toBeVisible();
    await page.getByRole("button", { name: "Details" }).click();
    await expect(page.getByText("No usage in this range yet.")).toBeVisible();
    await expect(page.getByText("widen the range")).toBeVisible();
  });

  test("usage loading is announced for assistive tech", async ({ page }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    await mockSettingsOverlay(page, []);
    await page.unroute("**/admin/api/metrics");
    await page.route("**/admin/api/metrics", async (route) => {
      const { promise, resolve } = Promise.withResolvers<void>();
      setTimeout(resolve, 2000);
      await promise;
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(f.metrics),
      });
    });
    await page.goto(adminUrl("activity"));
    await page.getByRole("button", { name: "Metrics" }).click();
    // Screen-reader announcement (paired with aria-busy on the skeleton).
    await expect(page.getByText("Loading metrics")).toBeAttached();
    await expect(
      page.getByRole("heading", { name: "Token usage" }),
    ).toBeVisible();
  });

  test("usage failure offers retry and toasts what broke", async ({ page }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    await mockSettingsOverlay(page, []);
    let calls = 0;
    await page.unroute("**/admin/api/metrics");
    await page.route("**/admin/api/metrics", async (route) => {
      calls += 1;
      if (calls === 1) {
        await route.fulfill({ status: 500, body: "boom" });
      } else {
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify(f.metrics),
        });
      }
    });
    await page.goto(adminUrl("activity"));
    await page.getByRole("button", { name: "Metrics" }).click();
    await expect(page.getByRole("button", { name: "Retry" })).toBeVisible();
    await expect(page.getByTestId("inline-error")).toContainText("boom");
    await expect(
      page.getByRole("alert").filter({ hasText: "boom" }),
    ).toBeVisible();
    await page.getByRole("button", { name: "Retry" }).click();
    await expect(
      page.getByRole("heading", { name: "Token usage" }),
    ).toBeVisible();
  });

  // -- Logs (#activity live tab: LiveConsole console view) ------------------

  test("logs empty console names the cause and what streams here", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f, { logs: { ...f.logs, entries: [] } });
    await mockSettingsOverlay(page, []);
    await page.goto(adminUrl("activity"));
    await expect(
      page.getByText("No request activity recorded yet."),
    ).toBeVisible();
    await expect(
      page.getByText(
        "Live incoming chat and messages requests will stream here.",
      ),
    ).toBeVisible();
  });

  test("logs loading announces itself instead of staying silent", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    await mockSettingsOverlay(page, []);
    await page.unroute("**/admin/api/logs**");
    await page.route("**/admin/api/logs**", async (route) => {
      const { promise, resolve } = Promise.withResolvers<void>();
      setTimeout(resolve, 2000);
      await promise;
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(f.logs),
      });
    });
    await page.goto(adminUrl("activity"));
    await expect(
      page.getByRole("status", { name: "Loading logs" }),
    ).toBeVisible();
    await expect(page.getByText(/model requests?/).first()).toBeVisible();
  });

  test("logs failure names the fetch and offers retry", async ({ page }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    await mockSettingsOverlay(page, []);
    let calls = 0;
    await page.unroute("**/admin/api/logs**");
    await page.route("**/admin/api/logs**", async (route) => {
      calls += 1;
      if (calls === 1) {
        await route.fulfill({ status: 500, body: "boom" });
      } else {
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify(f.logs),
        });
      }
    });
    await page.goto(adminUrl("activity"));
    await expect(page.getByRole("button", { name: "Retry" })).toBeVisible();
    await expect(
      page.getByRole("alert").filter({ hasText: "Could not load log entries" }),
    ).toBeVisible();
    await page.getByRole("button", { name: "Retry" }).click();
    await expect(page.getByText(/model requests?/).first()).toBeVisible();
  });

  // -- Team (#activity team tab: TeamUsagePanel, #609) ---------------------------

  test("activity carries the four-tab set including Team", async ({ page }) => {
    await mockDashboard(page, loadFixtures());
    await mockSettingsOverlay(page, []);
    await page.goto(adminUrl("activity"));
    for (const name of ["Live", "Metrics", "Team", "Traces"]) {
      await expect(
        page.getByRole("button", { name, exact: true }),
      ).toBeVisible();
    }
  });

  test("team empty says no per-client usage yet", async ({ page }) => {
    await mockDashboard(page, loadFixtures());
    await mockSettingsOverlay(page, []);
    await page.route("**/admin/api/usage*key*", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ keys: [] }),
      });
    });
    await page.goto(adminUrl("activity"));
    await page.getByRole("button", { name: "Team", exact: true }).click();
    await expect(
      page.getByRole("heading", { name: "Team usage" }),
    ).toBeVisible();
    await expect(page.getByText("No per-client usage yet.")).toBeVisible();
    // Empty names the cause and the next action, not just the absence.
    await expect(
      page.getByText(/once clients send requests with per-client API keys/),
    ).toBeVisible();
  });

  test("team rows render short key hashes, never raw keys", async ({
    page,
  }) => {
    await mockDashboard(page, loadFixtures());
    await mockSettingsOverlay(page, []);
    await page.route("**/admin/api/usage*key*", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          keys: [
            {
              key_id: "a1b2c3d4",
              requests: 12,
              success_rate: 0.916,
              total_tokens: 3456,
              by_model: {
                "deepseek/deepseek-v4-flash": {
                  requests: 12,
                  tokens: 3456,
                  prompt: 1000,
                  completion: 2000,
                  reasoning: 456,
                },
              },
              first_seen: "2026-09-16T10:00:00Z",
              freebucks: 3,
            },
          ],
        }),
      });
    });
    await page.goto(adminUrl("activity"));
    await page.getByRole("button", { name: "Team", exact: true }).click();
    const table = page.locator("table.fp-table");
    await expect(table.getByText("a1b2c3d4")).toBeVisible();
    await expect(table.getByText("deepseek/deepseek-v4-flash")).toBeVisible();
    await expect(table.getByText("3,456", { exact: true })).toBeVisible();
  });

  test("team loading is announced for assistive tech", async ({ page }) => {
    await mockDashboard(page, loadFixtures());
    await mockSettingsOverlay(page, []);
    await page.route("**/admin/api/usage*key*", async (route) => {
      const { promise, resolve } = Promise.withResolvers<void>();
      setTimeout(resolve, 2000);
      await promise;
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ keys: [] }),
      });
    });
    await page.goto(adminUrl("activity"));
    await page.getByRole("button", { name: "Team", exact: true }).click();
    await expect(page.getByText("Loading team usage")).toBeAttached();
    // Announced via role=status (pattern LiveConsole), not a bare live div.
    await expect(
      page.getByRole("status", { name: "Loading team usage" }),
    ).toBeAttached();
  });

  test("team failure offers retry and toasts what broke", async ({ page }) => {
    await mockDashboard(page, loadFixtures());
    await mockSettingsOverlay(page, []);
    // One fetch per mount: the first request fails and the manual Retry
    // takes the success path.
    let calls = 0;
    await page.route("**/admin/api/usage*key*", async (route) => {
      calls += 1;
      if (calls <= 1) {
        await route.fulfill({ status: 500, body: "boom" });
      } else {
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify({ keys: [] }),
        });
      }
    });
    await page.goto(adminUrl("activity"));
    await page.getByRole("button", { name: "Team", exact: true }).click();
    await expect(page.getByRole("button", { name: "Retry" })).toBeVisible();
    // One failure renders exactly one toast (deduped by message): a second
    // fetch or a duplicate push would show up as a second live alert.
    await expect(page.locator('[role="alert"]:not([inert])')).toHaveCount(1);
    await expect(
      page.locator('[role="alert"]:not([inert])', { hasText: "boom" }),
    ).toBeVisible();
    await page.getByRole("button", { name: "Retry" }).click();
    await expect(
      page.getByRole("heading", { name: "Team usage" }),
    ).toBeVisible();
  });

  test("team issues one upstream request per mount", async ({ page }) => {
    await mockDashboard(page, loadFixtures());
    await mockSettingsOverlay(page, []);
    let calls = 0;
    await page.route("**/admin/api/usage*key*", async (route) => {
      calls += 1;
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ keys: [] }),
      });
    });
    await page.goto(adminUrl("activity"));
    await page.getByRole("button", { name: "Team", exact: true }).click();
    await expect(
      page.getByRole("heading", { name: "Team usage" }),
    ).toBeVisible();
    // Mount fetches once (onMount); the shared-cursor effect only refetches
    // when the cursor advances, so no second request follows.
    expect(calls).toBe(1);
  });

  // -- Filtered-to-nothing vs unavailable -----------------------------------

  test("logs filtered to nothing offers to clear the filter", async ({
    page,
  }) => {
    await mockDashboard(page, loadFixtures());
    await mockSettingsOverlay(page, []);
    await page.goto(adminUrl("activity"));
    await page.getByRole("button", { name: "Table" }).click();
    await expect(page.locator("#log-msg")).toBeVisible();
    const filtered = page.waitForResponse(
      (r) => r.url().includes("/admin/api/logs") && r.url().includes("msg="),
    );
    await page.locator("#log-msg").fill("zzz-no-such-message");
    await filtered;
    await expect(
      page.getByRole("heading", { name: "No matching log entries" }),
    ).toBeVisible();
    await expect(
      page.getByText("No log entries matched your level or message filter."),
    ).toBeVisible();
    await page.getByRole("button", { name: "Clear filters" }).click();
    await expect(page.locator("#log-msg")).toHaveValue("");
    await expect(
      page.getByText("error handling request 0: upstream timeout").first(),
    ).toBeVisible();
  });

  test("disabled log ring reads differently from filtered-to-nothing", async ({
    page,
  }) => {
    await mockDashboard(page, loadFixtures());
    await mockSettingsOverlay(page, []);
    // The shared logs mock hardcodes enabled:true, so the disabled ring is
    // served by a test-local override (same endpoint, different state).
    await page.unroute("**/admin/api/logs**");
    await page.route("**/admin/api/logs**", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          enabled: false,
          level: "",
          msg: "",
          has_filter: false,
          window: "1h0m0s",
          truncated: false,
          entries: [],
        }),
      });
    });
    await page.goto(adminUrl("activity"));
    await expect(
      page.getByRole("heading", { name: "Log ring disabled" }),
    ).toBeVisible();
    await expect(
      page.getByText(
        "The server was started without an active logring handler, so no log entries are available.",
      ),
    ).toBeVisible();
    // No filter affordance: there is nothing a filter could match.
    await expect(
      page.getByRole("heading", { name: "No matching log entries" }),
    ).toHaveCount(0);
    await expect(
      page.getByRole("button", { name: "Clear filters" }),
    ).toHaveCount(0);
  });

  test("session-expired locks the dashboard with a sign-in next step", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f, {
      tokens: tokensPayload([tokenRow(0), tokenRow(1)]),
    });
    await mockSettingsOverlay(page, []);
    // The App shell version check observes the 401 and latches the session
    // dead without navigating away.
    await page.unroute("**/admin/api/version");
    await page.route("**/admin/api/version", async (route) => {
      await route.fulfill({ status: 401, body: "Unauthorized" });
    });
    await page.goto(adminUrl("tokens"));
    await expect(
      page.getByRole("heading", { name: "Dashboard Locked" }),
    ).toBeVisible();
    await expect(
      page.getByText(
        "Your session has ended. All dashboard operations are locked until you sign in again.",
      ),
    ).toBeVisible();
    await expect(
      page.getByRole("button", { name: "Sign in again" }),
    ).toBeVisible();
  });

  // -- Offline posture -------------------------------------------------------

  test("offline overlay leaves the queue posture unclassified", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f, {
      tokens: tokensPayload([tokenRow(0), tokenRow(1)]),
    });
    // The seed would classify a posture when reachable; degraded keeps the
    // chip honest instead of guessing from file defaults.
    await mockSettingsOverlay(page, [], {
      degraded: true,
      seed: MASQ_STRATEGY_SEED,
    });
    await page.goto(adminUrl("tokens"));
    await expect(page.getByText("Account #1").first()).toBeVisible();
    const chip = page.getByTestId("queue-chip");
    await expect(chip).toContainText("—");
    await expect(chip).toHaveAttribute("title", /offline/);
  });

  test("reachable overlay classifies the queue posture", async ({ page }) => {
    const f = loadFixtures();
    await mockDashboard(page, f, {
      tokens: tokensPayload([tokenRow(0), tokenRow(1)]),
    });
    // Balance-exact values per detectStrategy: the bounded MASQ seed reads
    // Custom, so the control seeds the preset it names.
    await mockSettingsOverlay(page, [], {
      seed: [
        { key: "SLOTS_PER_ACCOUNT", value: "2", source: "db" },
        { key: "QUEUE_WAIT", value: "60s", source: "db" },
        { key: "QUEUE_DEPTH", value: "16", source: "db" },
        { key: "MAX_SPILL_ACCOUNTS", value: "0", source: "db" },
      ],
    });
    await page.goto(adminUrl("tokens"));
    await expect(page.getByText("Account #1").first()).toBeVisible();
    await expect(page.getByTestId("queue-chip")).toContainText("Balance");
  });

  // -- Rejected save ----------------------------------------------------------

  test("a rejected settings save keeps the value and offers retry", async ({
    page,
  }) => {
    await mockDashboard(page, loadFixtures());
    const posted: PostedSetting[] = [];
    await mockSettingsOverlay(page, posted, { postStatus: 400 });
    await page.goto(adminUrl("plans"));
    await page.getByRole("button", { name: "Controls" }).click();
    const input = page.locator('input[aria-label="REASONING_IN_CONTENT"]');
    await expect(input).toBeVisible();
    const row = page.locator("div.py-4", { has: input });
    const rowSwitch = row.getByRole("switch", {
      name: "REASONING_IN_CONTENT",
    });
    const firstPost = page.waitForRequest(
      (r) => r.method() === "POST" && r.url().includes("/admin/api/settings"),
    );
    await rowSwitch.click();
    await firstPost;
    await expect.poll(() => posted.length).toBeGreaterThan(0);
    await expect(
      row.locator('span[role="status"]', { hasText: "Setting rejected" }),
    ).toBeVisible();
    await expect(row.getByRole("button", { name: "Retry" })).toBeVisible();
    // The attempted value stays on the row: nothing silently reverted it.
    await expect(rowSwitch).toHaveAttribute("aria-checked", "true");
    const secondPost = page.waitForRequest(
      (r) => r.method() === "POST" && r.url().includes("/admin/api/settings"),
    );
    await row.getByRole("button", { name: "Retry" }).click();
    await secondPost;
    await expect.poll(() => posted.length).toBeGreaterThan(1);
  });

  // -- Touch targets (DESIGN.md scale: md 40 desktop, 44 coarse) ---------------

  test("key controls keep the standard touch target and stay enabled", async ({
    page,
  }) => {
    await mockDashboard(page, loadFixtures(), {
      tokens: tokensPayload([tokenRow(0), tokenRow(1)]),
    });
    await mockSettingsOverlay(page, [], { seed: MASQ_STRATEGY_SEED });
    await page.goto(adminUrl("tokens"));
    await expect(page.getByText("2 pooled token(s)")).toBeVisible();
    // Pool section tabs: sleek desktop height, visible and operable.
    for (const name of ["Accounts", "Warming", "Controls"]) {
      const btn = page.getByRole("button", { name, exact: true });
      await expect(btn).toBeVisible();
      await expect(btn).toBeEnabled();
      expect(await boxHeight(btn)).toBeGreaterThanOrEqual(24);
    }
    // Primary action: enabling it keeps the md box in both dimensions.
    await page.locator("#add-token-input").fill("test-token-1234");
    const addToken = page.getByRole("button", { name: "Add Token" });
    await expect(addToken).toBeEnabled();
    const addBox = await addToken.boundingBox();
    expect(Math.round(addBox?.height ?? 0)).toBeGreaterThanOrEqual(40);
    expect(Math.round(addBox?.width ?? 0)).toBeGreaterThanOrEqual(44);
    const deviceLogin = page.getByRole("button", { name: "Device Login" });
    await expect(deviceLogin).toBeVisible();
    await expect(deviceLogin).toBeEnabled();
    expect(await boxHeight(deviceLogin)).toBeGreaterThanOrEqual(40);
    // Activity view tabs plus the small-button density (Refresh all and the
    // Activity tabs + inner views render sleek desktop controls.
    await page.goto(adminUrl("activity"));
    for (const name of ["Live", "Metrics", "Team", "Traces"]) {
      const btn = page.getByRole("button", { name, exact: true });
      await expect(btn).toBeVisible();
      await expect(btn).toBeEnabled();
      expect(await boxHeight(btn)).toBeGreaterThanOrEqual(24);
    }
    await page.getByRole("button", { name: "Metrics" }).click();
    await expect(
      page.getByRole("heading", { name: "Token usage" }),
    ).toBeVisible();
    for (const name of ["7D", "Details", "Refresh all"]) {
      const btn = page.getByRole("button", { name, exact: true });
      await expect(btn).toBeVisible();
      await expect(btn).toBeEnabled();
      expect(await boxHeight(btn)).toBeGreaterThanOrEqual(24);
    }
  });

  // -- Narrow-viewport behavior (360px phone) ----------------------------------

  for (const hash of ["tokens", "plans", "activity", "settings"]) {
    test(`no horizontal overflow at 360px on #${hash}`, async ({ page }) => {
      await mockDashboard(page, loadFixtures(), {
        tokens: tokensPayload([tokenRow(0), tokenRow(1)]),
      });
      await mockSettingsOverlay(page, [], { seed: MASQ_STRATEGY_SEED });
      await page.setViewportSize({ width: 360, height: 844 });
      await page.goto(adminUrl(hash));
      await expect(page.getByRole("heading").first()).toBeVisible();
      const overflow = await page.evaluate(() => ({
        scroll: document.documentElement.scrollWidth,
        inner: window.innerWidth,
      }));
      expect(
        overflow.scroll,
        `#${hash} scrolls sideways at 360px`,
      ).toBeLessThanOrEqual(overflow.inner + 1);
    });
  }

  test("key actions stay reachable at 360px", async ({ page }) => {
    await mockDashboard(page, loadFixtures(), {
      tokens: tokensPayload([tokenRow(0), tokenRow(1)]),
    });
    await mockSettingsOverlay(page, [], { seed: MASQ_STRATEGY_SEED });
    await page.setViewportSize({ width: 360, height: 844 });
    await page.goto(adminUrl("tokens"));
    // Narrow viewports render stacked cards instead of the desktop table
    // (whose twin stays hidden), so gate on the visible card copy.
    await expect(page.getByText("2 pooled token(s)")).toBeVisible();
    for (const name of ["Accounts", "Warming", "Controls"]) {
      await expect(
        page.getByRole("button", { name, exact: true }),
      ).toBeVisible();
    }
    await expect(
      page.getByRole("button", { name: "Device Login" }),
    ).toBeEnabled();
    await page.goto(adminUrl("activity"));
    for (const name of ["Live", "Metrics", "Team", "Traces"]) {
      await expect(
        page.getByRole("button", { name, exact: true }),
      ).toBeVisible();
    }
  });

  // -- Keyboard + focus + dialogs ----------------------------------------------

  test("skip link drives focus into the main content", async ({ page }) => {
    await mockDashboard(page, loadFixtures());
    await mockSettingsOverlay(page, []);
    await page.goto(adminUrl("overview"));
    await expect(page.getByRole("heading", { name: "Overview" })).toBeVisible();
    await page.keyboard.press("Tab");
    const skip = page.getByRole("link", { name: "Skip to content" });
    await expect(skip).toBeFocused();
    await expect(skip).toBeVisible();
    await page.keyboard.press("Enter");
    expect(await page.evaluate(() => document.activeElement?.id)).toBe(
      "main-content",
    );
  });

  test("keyboard reaches the Pool nav with a visible focus indicator", async ({
    page,
  }) => {
    await mockDashboard(page, loadFixtures());
    await mockSettingsOverlay(page, []);
    await page.goto(adminUrl("overview"));
    await expect(page.getByRole("heading", { name: "Overview" })).toBeVisible();
    // Tab-only, like a keyboard-only operator: walk the tab order until the
    // Pool nav link holds focus.
    let found = false;
    for (let i = 0; i < 25; i++) {
      const atPool = await page.evaluate(() => {
        const el = document.activeElement;
        return (
          el instanceof HTMLAnchorElement &&
          (el.textContent ?? "").includes("Pool")
        );
      });
      if (atPool) {
        found = true;
        break;
      }
      await page.keyboard.press("Tab");
    }
    expect(found).toBe(true);
    // The global focus-visible ring is on: focus is perceivable, not silent.
    const focusStyle = await page.evaluate(() => {
      const el = document.activeElement;
      if (!(el instanceof HTMLElement)) return null;
      const cs = getComputedStyle(el);
      return {
        focusVisible: el.matches(":focus-visible"),
        outlineStyle: cs.outlineStyle,
        outlineWidth: cs.outlineWidth,
      };
    });
    expect(focusStyle?.focusVisible).toBe(true);
    expect(focusStyle?.outlineStyle).not.toBe("none");
    await page.keyboard.press("Enter");
    await expect(
      page.getByRole("heading", { name: "Pool", exact: true }),
    ).toBeVisible();
  });

  test("Escape closes the generated-key dialog", async ({ page }) => {
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
    await page.route("**/admin/config", async (route) => {
      if (route.request().method() === "POST") {
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify({ ok: true, message: "Config saved" }),
        });
      } else {
        await route.continue();
      }
    });
    await page.goto(adminUrl("overview"));
    await expect(
      page.getByRole("heading", { name: "Client API Keys" }),
    ).toBeVisible();
    await page.getByRole("button", { name: "Generate API Key" }).click();
    const dialog = page.getByRole("dialog");
    await expect(dialog).toContainText("Client API Key Generated");
    await page.keyboard.press("Escape");
    await expect(dialog).toHaveCount(0);
    // No focus-return pin here: the async generate disables its trigger
    // mid-flight (which moves focus to the body before the modal opens),
    // so there is no trigger focus for the modal to restore.
  });
});
