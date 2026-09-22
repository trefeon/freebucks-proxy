import { test, expect } from "@playwright/test";
import { loadFixtures, mockDashboard, mockSettingsOverlay } from "./mocks.js";
import { adminUrl } from "./mock-data.js";

// Off-peak quote coverage (Usage #plans Accounts/Models tabs).
// One factory builds the fake backend (mock-data.ts), one route layer serves
// it (mocks.ts mockDashboard/mockSettingsOverlay). Web-first assertions only:
// role/testid locators, no sleeps, no brittle selectors.
test.describe("off-peak quote (mock backend)", () => {
  test.use({ expect: { timeout: 10_000 } });

  test("accounts tab renders the off-peak line and never stalls on Loading", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    await mockSettingsOverlay(page, []);
    await page.goto(adminUrl("tokens"));
    await page.getByRole("button", { name: "Allowances" }).click();
    await expect(
      page.getByRole("heading", { name: "Account #1" }),
    ).toBeVisible();
    // Fixture token 0 carries the live-shape snake_case offer (22–06 UTC at
    // 10/hr against a regular 15/hr): the account card renders its line.
    const line = page.getByTestId("off-peak-line").first();
    await expect(line).toBeVisible();
    await expect(line).toContainText(/Off-peak/);
    // The quote resolves client-side without throwing, so the panel leaves
    // the loading state behind instead of freezing on it.
    await expect(page.getByRole("status", { name: "Loading" })).toHaveCount(0);
  });

  test("models tab renders the Off-peak line on the quoted row", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    await mockSettingsOverlay(page, []);
    await page.goto(adminUrl("plans"));
    await page.getByRole("button", { name: "Catalog" }).click();
    await expect(
      page.getByRole("heading", { level: 1, name: "Models", exact: true }),
    ).toBeVisible();
    await expect(
      page.getByText("deepseek/deepseek-v4-flash").first(),
    ).toBeVisible();
    // The resolved quote owns the badge copy: the deepseek row carries the
    // Off-peak line next to its live price.
    const row = page.locator("table tbody tr", {
      hasText: "deepseek/deepseek-v4-flash",
    });
    await expect(row.getByText(/Off-peak/)).toBeVisible();
  });

  test("offer with missing hours renders the account with no off-peak line", async ({
    page,
  }) => {
    const f = loadFixtures();
    const tokens = JSON.parse(JSON.stringify(f.tokens));
    const offer =
      tokens.tokens[0].freebucks.off_peak["deepseek/deepseek-v4-flash"];
    delete offer.start_hour_utc;
    delete offer.end_hour_utc;
    await mockDashboard(page, f, { tokens });
    await mockSettingsOverlay(page, []);
    await page.goto(adminUrl("tokens"));
    await page.getByRole("button", { name: "Allowances" }).click();
    // A windowless offer resolves to no copy (never an Invalid Date throw),
    // so the account still renders — minus the off-peak line — and Loading
    // clears instead of freezing the tab.
    await expect(
      page.getByRole("heading", { name: "Account #1" }),
    ).toBeVisible();
    await expect(page.getByTestId("off-peak-line")).toHaveCount(0);
    await expect(page.getByRole("status", { name: "Loading" })).toHaveCount(0);
  });

  test("camelCase-keyed offer still renders the off-peak line", async ({
    page,
  }) => {
    const f = loadFixtures();
    const tokens = JSON.parse(JSON.stringify(f.tokens));
    tokens.tokens[0].freebucks = {
      prices: { "deepseek/deepseek-v4-flash": 10 },
      offPeak: {
        "deepseek/deepseek-v4-flash": {
          startHourUtc: 22,
          endHourUtc: 6,
          price: 10,
          regularPrice: 15,
        },
      },
    };
    await mockDashboard(page, f, { tokens });
    await mockSettingsOverlay(page, []);
    await page.goto(adminUrl("tokens"));
    await page.getByRole("button", { name: "Allowances" }).click();
    // The wire-camelCase twin of the fixture offer renders the same line:
    // assert the render, not the absence of a throw.
    await expect(
      page.getByRole("heading", { name: "Account #1" }),
    ).toBeVisible();
    const line = page.getByTestId("off-peak-line").first();
    await expect(line).toBeVisible();
    await expect(line).toContainText(/Off-peak/);
  });
});
