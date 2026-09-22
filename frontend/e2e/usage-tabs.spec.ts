import { test, expect } from "@playwright/test";
import { loadFixtures, mockDashboard, mockSettingsOverlay } from "./mocks.js";
import { adminUrl } from "./mock-data.js";

// Usage + activity tab behavior against inline route mocks (interactables
// inventory gap). mocks.ts predates the /admin/api/usage route, so every
// test serves it inline here (later route wins; fixtures/ untouched):
// group_by=key feeds the Team panel, ?range= feeds the Metrics usage block.
//
// Web-first assertions only: role/label locators, no sleeps, route-hit
// counters instead of timers.

test.describe("usage tabs (mock backend)", () => {
  test.use({ expect: { timeout: 10_000 } });

  type TeamKey = Record<string, unknown>;

  // Serves GET /admin/api/usage* inline: per-client keys for group_by=key,
  // range totals for the Metrics usage block.
  async function mockUsageRoutes(
    page: Parameters<typeof mockDashboard>[0],
    keys: TeamKey[],
  ) {
    await page.route("**/admin/api/usage*", async (route) => {
      const url = new URL(route.request().url());
      const body =
        url.searchParams.get("group_by") === "key"
          ? { range: "7d", keys }
          : {
              range: "7d",
              totals: {
                requests: 42,
                input: 1200,
                cached: 300,
                output: 600,
                cost: 7,
              },
              entries: [],
            };
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(body),
      });
    });
  }

  // Plans one-shot deep-link restore: the shell plants fp-page-tab:plans via
  // sessionStorage on legacy-hash redirects; the page consumes it on mount.
  test("plans one-shot restore lands on the Models tab", async ({ page }) => {
    await mockDashboard(page, loadFixtures());
    await mockSettingsOverlay(page, []);
    await mockUsageRoutes(page, []);
    await page.addInitScript(() => {
      sessionStorage.setItem("fp-page-tab:plans", "models");
    });
    await page.goto(adminUrl("plans"));
    // Models tab active (pressed) with a served model row on screen.
    await expect(
      page.getByRole("button", { name: "Catalog", exact: true }),
    ).toHaveAttribute("aria-pressed", "true");
    await expect(
      page.getByText("deepseek/deepseek-v4-flash").first(),
    ).toBeVisible();
    // One-shot: the key is consumed, so a reload falls back to Accounts.
    await expect
      .poll(
        () => page.evaluate(() => sessionStorage.getItem("fp-page-tab:plans")),
        { timeout: 10_000 },
      )
      .toBe(null);
  });

  test("plans one-shot restore lands on Controls and consumes the key", async ({
    page,
  }) => {
    await mockDashboard(page, loadFixtures());
    await mockSettingsOverlay(page, []);
    await mockUsageRoutes(page, []);
    await page.addInitScript(() => {
      sessionStorage.setItem("fp-page-tab:plans", "controls");
    });
    await page.goto(adminUrl("plans"));
    await expect(
      page.getByRole("button", { name: "Routing", exact: true }),
    ).toHaveAttribute("aria-pressed", "true");
    await expect(
      page.getByRole("heading", { name: "Usage Controls" }),
    ).toBeVisible();
    await expect
      .poll(
        () => page.evaluate(() => sessionStorage.getItem("fp-page-tab:plans")),
        { timeout: 10_000 },
      )
      .toBe(null);
  });

  test("refresh-all refetches the mounted panel once, never the parked ones", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    await mockSettingsOverlay(page, []);
    await mockUsageRoutes(page, []);
    // Counting re-routes over the shared layer: each Activity panel gets
    // its own counter so one Refresh-all tap is observable per endpoint.
    const hits = { logs: 0, metrics: 0, usage: 0, team: 0, traces: 0 };
    const teamUrls: string[] = [];
    await page.unroute("**/admin/api/logs**");
    await page.route("**/admin/api/logs**", async (route) => {
      hits.logs += 1;
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(f.logs),
      });
    });
    await page.unroute("**/admin/api/metrics");
    await page.route("**/admin/api/metrics", async (route) => {
      hits.metrics += 1;
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(f.metrics),
      });
    });
    await page.unroute("**/admin/api/usage*");
    await page.route("**/admin/api/usage*", async (route) => {
      const url = new URL(route.request().url());
      if (url.searchParams.get("group_by") === "key") {
        hits.team += 1;
        teamUrls.push(url.toString());
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify({ range: "7d", keys: [] }),
        });
      } else {
        hits.usage += 1;
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify({
            range: "7d",
            totals: {
              requests: 42,
              input: 1200,
              cached: 300,
              output: 600,
              cost: 7,
            },
            entries: [],
          }),
        });
      }
    });
    await page.unroute("**/admin/api/traces");
    await page.route("**/admin/api/traces", async (route) => {
      hits.traces += 1;
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(f.traces),
      });
    });

    // Tour all four tabs: each mount fetches its own endpoint exactly for
    // its visible panel.
    await page.goto(adminUrl("activity"));
    await expect(page.getByText("1 model request").first()).toBeVisible();
    await page.getByRole("button", { name: "Metrics", exact: true }).click();
    await expect(page.getByText("Requests served")).toBeVisible();
    await page
      .getByRole("button", { name: "Client Keys", exact: true })
      .click();
    await expect(
      page.getByRole("heading", { name: "Team usage" }),
    ).toBeVisible();
    await page.getByRole("button", { name: "Traces", exact: true }).click();
    await expect(
      page.getByRole("heading", { name: "Trace log" }),
    ).toBeVisible();
    await expect(page.getByText("acquire_ms").first()).toBeVisible();

    // All four endpoints served their panel (Live logs, Metrics metrics +
    // usage, Team group_by=key, Traces).
    expect(hits.logs).toBeGreaterThanOrEqual(1);
    expect(hits.metrics).toBeGreaterThanOrEqual(1);
    expect(hits.usage).toBeGreaterThanOrEqual(1);
    expect(hits.team).toBeGreaterThanOrEqual(1);
    expect(hits.traces).toBeGreaterThanOrEqual(1);

    // One Refresh-all tap: only the mounted Traces panel refetches, once.
    // (Live/Metrics/Team are unmounted; their counters freeze — no
    // cross-panel storm, no retry burst.)
    const before = { ...hits };
    await page.getByRole("button", { name: "Refresh all" }).click();
    await expect
      .poll(() => hits.traces, { timeout: 10_000 })
      .toBe(before.traces + 1);
    expect(hits.logs).toBe(before.logs);
    expect(hits.metrics).toBe(before.metrics);
    expect(hits.usage).toBe(before.usage);
    expect(hits.team).toBe(before.team);
  });

  test("team empty state names the cause and the next step", async ({
    page,
  }) => {
    await mockDashboard(page, loadFixtures());
    await mockSettingsOverlay(page, []);
    await mockUsageRoutes(page, []);
    const teamReq = page.waitForResponse(
      (r) => r.url().includes("/admin/api/usage") && r.status() === 200,
    );
    await page.goto(adminUrl("activity"));
    await page
      .getByRole("button", { name: "Client Keys", exact: true })
      .click();
    const resp = await teamReq;
    // The panel asks for the per-client aggregation, not the range view.
    expect(resp.url()).toContain("group_by=key");
    await expect(
      page.getByRole("heading", { name: "Team usage" }),
    ).toBeVisible();
    await expect(page.getByText("No per-client usage yet.")).toBeVisible();
    await expect(
      page.getByText(/once clients send requests with per-client API keys/),
    ).toBeVisible();
  });

  test("team rows render short key hashes, never raw keys", async ({
    page,
  }) => {
    await mockDashboard(page, loadFixtures());
    await mockSettingsOverlay(page, []);
    await mockUsageRoutes(page, [
      {
        key_id: "b2c3d4e5f6a7b8c9",
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
        last_seen: "2026-09-16T11:00:00Z",
        freebucks: 3,
      },
    ]);
    const teamReq = page.waitForResponse(
      (r) =>
        r.url().includes("/admin/api/usage") &&
        r.url().includes("group_by=key") &&
        r.status() === 200,
    );
    await page.goto(adminUrl("activity"));
    await page
      .getByRole("button", { name: "Client Keys", exact: true })
      .click();
    await teamReq;
    const table = page.locator("table.fp-table");
    await expect(table.getByText("b2c3d4e5f6a7b8c9")).toBeVisible();
    await expect(table.getByText("deepseek/deepseek-v4-flash")).toBeVisible();
    await expect(table.getByText("3,456", { exact: true })).toBeVisible();
    // Raw client keys never reach the DOM, even as substrings.
    await expect(table.getByText(/sk-fb-/)).toHaveCount(0);
  });

  test("sparkline renders after the next poll appends samples, without remount", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    await mockSettingsOverlay(page, []);
    await mockUsageRoutes(page, []);
    // Route sequence: the first metrics poll has no samples yet, the second
    // (after Refresh all) appends them. Same mount throughout — no reload,
    // no tab switch between the two paints.
    let calls = 0;
    await page.unroute("**/admin/api/metrics");
    await page.route("**/admin/api/metrics", async (route) => {
      calls += 1;
      const body =
        calls <= 1
          ? { ...f.metrics, requests_spark: null, retries_spark: null }
          : f.metrics;
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(body),
      });
    });
    await page.goto(adminUrl("activity"));
    await page.getByRole("button", { name: "Metrics", exact: true }).click();
    await expect(
      page.getByRole("heading", { name: "Token usage" }),
    ).toBeVisible();
    await expect(
      page.getByText("Not enough samples yet.").first(),
    ).toBeVisible();
    await expect(
      page.getByRole("img", { name: "requests served over time" }),
    ).toHaveCount(0);
    // Refresh all advances the shared cursor; the mounted panel refetches
    // in place and the sparkline SVG paints without a remount.
    await page.getByRole("button", { name: "Refresh all" }).click();
    await expect(
      page.getByRole("img", { name: "requests served over time" }),
    ).toBeVisible();
    await expect(page.getByText("Not enough samples yet.")).toHaveCount(0);
    expect(calls).toBe(2);
  });
});
