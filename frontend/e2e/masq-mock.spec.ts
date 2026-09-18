import { test, expect } from "@playwright/test";
import { adminUrl, mockMasqScenario } from "./mock-data.js";

// MASQ pool states through the centralized mock-data factory: every test
// builds its fake backend from mock-data.ts (one factory) and serves it
// through mocks.ts mockDashboard (one route layer). Web assertions only,
// role/label locators, no sleeps.
test.describe("MASQ mock-data scenarios (centralized factory)", () => {
  test.use({ expect: { timeout: 10_000 } });

  test("spill-active: bounded spill bound renders on the strategy card", async ({
    page,
  }) => {
    await mockMasqScenario(page, "spill-active", { loginPage: true });
    const metaResp = page.waitForResponse(
      (r) => r.url().includes("/admin/api/config/meta") && r.status() === 200,
      { timeout: 5000 },
    );
    await page.goto(adminUrl("tokens"));
    await metaResp;
    // Head lane is saturated in the scenario payload (Account #1 waits).
    const table = page.locator("table.fp-table");
    await expect(table.getByText("Account #1")).toBeVisible({
      timeout: 10000,
    });
    await expect(table.getByText("Account #2")).toBeVisible();
    await page.getByRole("button", { name: "Controls" }).click();
    // Bounded spill from the scenario seed: 1 continuation account.
    await expect(
      page.locator('input[aria-label="MAX_SPILL_ACCOUNTS"]'),
    ).toHaveValue("1");
  });

  test("queue-wait: parked request shows its QUEUED chip and ACCT numbers", async ({
    page,
  }) => {
    await mockMasqScenario(page, "queue-wait");
    await page.goto(adminUrl("activity"));
    await expect(page.getByText("1 model request")).toBeVisible();

    const chip = page.getByText("QUEUED 850ms");
    await expect(chip).toBeVisible();
    await expect(chip).toHaveAttribute("title", /spill-lane queue/);
    await expect(chip).toHaveAttribute(
      "title",
      /not tokens, not request latency/,
    );
    await expect(page.getByText(/^QUEUED /)).toHaveCount(1);

    const acct = page.getByTitle(/Pool account 1 — not token usage/);
    await expect(acct).toBeVisible();
    await expect(acct).toHaveAttribute("title", /2 live turns, 1 waiting/);
    await expect(acct).toHaveAttribute("title", /oldest wait 1200ms/);
  });

  test("pinned: drawer shows the pin and its skip count", async ({ page }) => {
    await mockMasqScenario(page, "pinned");
    await page.goto(adminUrl("tokens"));
    const table = page.locator("table.fp-table");
    await expect(table.getByText("Account #1")).toBeVisible({
      timeout: 10000,
    });
    await table.locator('button[aria-label*="Expand details"]').first().click();
    await expect(page.getByText("Pinned model").first()).toBeVisible();
    await expect(page.getByText("z-ai/glm-5.2").first()).toBeVisible();
    await expect(
      page.getByText("3 request(s) routed elsewhere by this pin").first(),
    ).toBeVisible();
  });

  test("precious: live holder keeps Drop Session, idle row offers none", async ({
    page,
  }) => {
    await mockMasqScenario(page, "precious");
    await page.goto(adminUrl("tokens"));
    const table = page.locator("table.fp-table");
    await expect(table.getByText("Account #1")).toBeVisible({
      timeout: 10000,
    });
    const idle = table.locator("tbody tr").filter({ hasText: "Account #1" });
    const live = table.locator("tbody tr").filter({ hasText: "Account #2" });
    await expect(
      idle.getByRole("button", { name: "Drop Session" }),
    ).toHaveCount(0);
    const btn = live.getByRole("button", { name: "Drop Session" });
    await expect(btn).toBeVisible();
    await expect(btn).toBeEnabled();
  });

  test("quota-tracker: Freebucks account rows render with a shared reset", async ({
    page,
  }) => {
    await mockMasqScenario(page, "quota-tracker");
    await page.goto(adminUrl("plans"));
    await page.getByRole("button", { name: "Accounts" }).click();
    await expect(
      page.getByRole("heading", { name: "Usage", exact: true }),
    ).toBeVisible();
    await expect(
      page.getByRole("heading", { name: "Account #1" }),
    ).toBeVisible();
    await expect(page.getByText("dev@example.com").first()).toBeVisible();
    // Vendor formatFreebucks rounds (2.5 -> 3, 7.5 -> 8).
    await expect(page.getByText("Used 3 / 10")).toBeVisible();
    await expect(page.getByText("Used 42 / 300")).toBeVisible();
    await expect(
      page.locator('[data-testid="freebucks-header"]').first(),
    ).toContainText(/8\/10 Freebucks daily/);
    await expect(page.getByTestId("reset-strip")).toContainText("resets in");
  });

  test("pool-ceiling: cap times accounts renders the honest concurrent turns", async ({
    page,
  }) => {
    await mockMasqScenario(page, "pool-ceiling", { loginPage: true });
    const metaResp = page.waitForResponse(
      (r) => r.url().includes("/admin/api/config/meta") && r.status() === 200,
      { timeout: 5000 },
    );
    await page.goto(adminUrl("tokens"));
    await metaResp;
    await page.getByRole("button", { name: "Controls" }).click();
    // Seed cap 2 x 3 scenario accounts = 6 concurrent turns.
    await expect(page.getByTestId("pool-ceiling")).toContainText(
      /2 per account.*3 accounts.*6 concurrent turns/,
    );
  });
});
