import { test, expect } from "@playwright/test";
import type { Page } from "@playwright/test";
import { loadFixtures, mockDashboard } from "./mocks.js";
import { adminUrl } from "./mock-data.js";

// Ads telemetry section (mock backend). The ledger contract lives behind
// GET /admin/api/ads/summary + GET /admin/api/ads/legs; mocks.ts predates
// the endpoints, so this slice serves them here (no shared file touched).
// Web-first assertions only: role/testid locators, no sleeps.

const SUMMARY = {
  totals: { auction: 7, impression: 5, streak: 2 },
  creditsGranted: 42,
  byProvider: { default: 9, fallback: 5 },
  bySurface: { waiting_room: 8, cli_chat: 6 },
  lastEventAt: 1785892800000,
  errors: 1,
};

const LEGS = [
  {
    ts: 1785892800000,
    surface: "cli_chat",
    provider: "default",
    leg: "impression",
    title: "Solar Pro 4",
    brand: "Freebuff",
    credits: 15,
  },
  {
    ts: 1785892700000,
    surface: "waiting_room",
    provider: "default",
    leg: "auction",
    title: "Mini 4",
  },
  {
    ts: 1785892600000,
    surface: "cli_chat",
    provider: "fallback",
    leg: "streak",
    brand: "Freebuff",
    credits: 27,
    error: "claim rejected",
  },
];

const QUIET_SUMMARY = {
  totals: { auction: 0, impression: 0, streak: 0 },
  creditsGranted: 0,
  byProvider: {},
  bySurface: {},
  lastEventAt: null,
  errors: 0,
};

async function mockAds(
  page: Page,
  summary: unknown,
  legs: unknown,
): Promise<void> {
  await page.route("**/admin/api/ads/summary", async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(summary),
    });
  });
  await page.route("**/admin/api/ads/legs*", async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(legs),
    });
  });
}

test.describe("ads telemetry (mock backend)", () => {
  test.use({ expect: { timeout: 10_000 } });

  test("renders real firing data with an error note and no ad URLs", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    await mockAds(page, SUMMARY, LEGS);
    await page.goto(adminUrl("ads"));

    await expect(
      page.getByRole("heading", { level: 1, name: "Ads", exact: true }),
    ).toBeVisible();

    const kpis = page.getByTestId("ads-kpis");
    for (const label of [
      "Auctions",
      "Impressions",
      "Streak",
      "Credits granted",
    ]) {
      await expect(kpis.getByText(label, { exact: true })).toBeVisible();
    }
    await expect(kpis.getByText("7", { exact: true })).toBeVisible();
    await expect(kpis.getByText("5", { exact: true })).toBeVisible();
    await expect(kpis.getByText("2", { exact: true })).toBeVisible();
    await expect(kpis.getByText("42", { exact: true })).toBeVisible();

    const providers = page.getByTestId("ads-by-provider");
    await expect(providers.getByText("default")).toBeVisible();
    await expect(providers.getByText("9", { exact: true })).toBeVisible();
    const surfaces = page.getByTestId("ads-by-surface");
    await expect(surfaces.getByText("waiting_room")).toBeVisible();
    await expect(surfaces.getByText("cli_chat")).toBeVisible();

    const legs = page.getByTestId("ads-leg");
    await expect(legs).toHaveCount(3);
    await expect(legs.first()).toContainText("Solar Pro 4");
    await expect(legs.first()).toContainText("Freebuff");
    await expect(legs.first()).toContainText("15 Freebucks");

    // Ledger errors surface as a persistent note, including the leg message.
    const note = page.getByRole("alert");
    await expect(note).toBeVisible();
    await expect(note).toContainText("claim rejected");

    // Contract: no ad URLs anywhere near the dashboard surface.
    await expect(page.getByText("impUrl")).toHaveCount(0);
    await expect(page.getByText("clickUrl")).toHaveCount(0);
    await expect(page.locator('[data-testid="ads-leg"] a')).toHaveCount(0);
  });

  test("empty state when no legs have fired yet", async ({ page }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    await mockAds(page, QUIET_SUMMARY, []);
    await page.goto(adminUrl("ads"));

    await expect(page.getByText("No ad legs yet")).toBeVisible();
    await expect(
      page.getByText(
        "Auction and impression legs will appear here once the proxy fires them.",
      ),
    ).toBeVisible();
    // The page-level empty state replaces the content sections.
    await expect(page.getByTestId("ads-kpis")).toHaveCount(0);
    await expect(page.getByTestId("ads-leg")).toHaveCount(0);
  });

  test("failed load shows retry and recovers on click", async ({ page }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    let calls = 0;
    await page.route("**/admin/api/ads/summary", async (route) => {
      calls += 1;
      if (calls === 1) {
        await route.fulfill({
          status: 500,
          contentType: "application/json",
          body: JSON.stringify({ ok: false, message: "boom" }),
        });
      } else {
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify(SUMMARY),
        });
      }
    });
    await page.route("**/admin/api/ads/legs*", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(LEGS),
      });
    });
    await page.goto(adminUrl("ads"));

    // The message renders twice (error toast + inline frame); scope to main.
    await expect(page.locator("#main-content").getByText("boom")).toBeVisible();
    await page.getByRole("button", { name: "Retry" }).click();
    await expect(
      page.getByRole("heading", { level: 1, name: "Ads", exact: true }),
    ).toBeVisible();
    await expect(page.getByTestId("ads-kpis")).toContainText("Auctions");
    expect(calls).toBeGreaterThanOrEqual(2);
  });
});
