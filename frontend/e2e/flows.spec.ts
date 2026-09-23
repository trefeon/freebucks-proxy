import { test, expect } from "@playwright/test";
import { loadFixtures, mockDashboard } from "./mocks.js";

const RW = "e2e/fixtures-realworld";
const admin = (hash) => `http://127.0.0.1:4173/admin/#${hash}`;

test.describe("user flows", () => {
  test("models: copy model ID confirms Copied", async ({ page }) => {
    await mockDashboard(page, loadFixtures());
    await page.goto(admin("plans"));
    await page.getByRole("button", { name: "Catalog" }).click();
    await page.getByText("deepseek/deepseek-v4-flash").first().waitFor();
    await page.getByRole("button", { name: "Copy model ID" }).first().click();
    await expect(page.getByText("Copied").first()).toBeVisible();
  });

  test("models: failed load shows retry and recovers", async ({ page }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
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
    await page.goto(admin("plans"));
    await page.getByRole("button", { name: "Catalog" }).click();
    await page.getByRole("button", { name: "Retry" }).click();
    await expect(
      page.getByText("deepseek/deepseek-v4-flash").first(),
    ).toBeVisible();
  });

  test("quota: reset strip renders the shared pacific-midnight countdown", async ({
    page,
  }) => {
    // Fresh reset dates: the archived Sept-6 reset_at is stale, which the
    // vendor refill-pending rule renders as "Updating balance…" instead.
    const f = loadFixtures(RW);
    const list = f.tokens.tokens ?? f.tokens;
    for (const t of Array.isArray(list) ? list : []) {
      if (t.freebucks?.daily)
        t.freebucks.daily.reset_at = "2030-01-01T07:00:00Z";
    }
    await mockDashboard(page, f);
    await page.goto(admin("tokens"));
    await page.getByRole("button", { name: "Allowances" }).click();
    await page.getByText("Account #1").first().waitFor();
    // One strip for the whole page (first account reset time, shared
    // countdown) — no per-row Refresh buttons remain here. One page-level
    // Probe all button (POST /admin/tokens/test-all, zero-cost) serves the
    // whole list instead.
    await expect(page.getByTestId("reset-strip")).toHaveCount(1);
    await expect(page.getByTestId("reset-strip")).toContainText("resets in");
    await expect(page.getByRole("button", { name: "Refresh" })).toHaveCount(0);
    await expect(page.getByRole("button", { name: "Probe all" })).toBeVisible();
  });

  test("quota: exempt account shows quota exempt chip", async ({ page }) => {
    const f = loadFixtures(RW);
    const tokens = JSON.parse(JSON.stringify(f.tokens));
    const list = tokens.tokens ?? tokens;
    const withFb = (Array.isArray(list) ? list : []).find((t) => t.freebucks);
    if (!withFb) throw new Error("RW tokens fixture has no freebucks row");
    withFb.freebucks.quota_exempt = true;
    await mockDashboard(page, f);
    await page.unroute("**/admin/api/tokens*");
    await page.route("**/admin/api/tokens*", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(tokens),
      });
    });
    await page.goto(admin("tokens"));
    await page.getByRole("button", { name: "Allowances" }).click();
    await expect(page.getByText("quota exempt").first()).toBeVisible();
  });
  test("tokens: paywalled model disables spawn", async ({ page }) => {
    const f = loadFixtures(RW);
    const tokens = JSON.parse(JSON.stringify(f.tokens));
    const list = tokens.tokens ?? tokens;
    const metered = (Array.isArray(list) ? list : []).find((t) => t.freebucks);
    if (!metered) throw new Error("RW tokens fixture has no freebucks row");
    metered.freebucks.balance = 1;
    metered.freebucks.prices = { "deepseek/deepseek-v4-flash": 9999 };
    await mockDashboard(page, f);
    await page.unroute("**/admin/api/tokens*");
    await page.route("**/admin/api/tokens*", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(tokens),
      });
    });
    await page.unroute("**/admin/api/config");
    await page.route("**/admin/api/config", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          env_content: "PORT=3457\nAUTH_TOKENS=tok0\nDEVTOOLS_ENABLED=true\n",
          has_env_file: true,
        }),
      });
    });
    await page.goto(admin("tokens"));
    const table = page.locator("table.fp-table");
    await table.locator('button[aria-label*="Expand details"]').first().click();
    await expect(table.getByText("Dev Session:")).toBeVisible();
    const picker = table.locator("select").first();
    await expect(
      picker.locator('option[value="deepseek/deepseek-v4-flash"]'),
    ).toBeDisabled();
    await picker.evaluate((el, v) => {
      (el as HTMLSelectElement).value = v;
      el.dispatchEvent(new Event("change", { bubbles: true }));
    }, "deepseek/deepseek-v4-flash");
    await expect(table.getByText(/Not enough Freebucks/).first()).toBeVisible();
    await expect(
      table.getByRole("button", { name: "Make Session" }),
    ).toBeDisabled();
  });
  test("tokens: spawn picker lists only served models", async ({ page }) => {
    // The 17-row catalog lists withdrawn and tier-only rows the gateway
    // never serves; the spawn picker must offer exactly the served set.
    await mockDashboard(page, loadFixtures());
    await page.goto(admin("tokens"));
    // The per-token Dev Session toolbar renders with DEVTOOLS_ENABLED=true.
    await page.unroute("**/admin/api/config");
    await page.route("**/admin/api/config", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          env_content: "PORT=3457\nAUTH_TOKENS=tok0\nDEVTOOLS_ENABLED=true\n",
          has_env_file: true,
        }),
      });
    });
    await page.reload();
    const table = page.locator("table.fp-table");
    await table.locator('button[aria-label*="Expand details"]').first().click();
    await expect(table.getByText("Dev Session:")).toBeVisible();
    const picker = table.locator("select").first();
    const options = await picker
      .locator("option")
      .evaluateAll((els) =>
        els.map((el) => (el as HTMLOptionElement).value).filter(Boolean),
      );
    expect(options.sort()).toEqual(
      [
        "openai/gpt-6-luna",
        "upstage/solar-mini4",
        "stealth/space-bunny-alpha",
        "meta/muse-spark-1.2-contributor",
        "z-ai/glm-5.3-flash",
        "deepseek/deepseek-v4-flash",
        "mimo/mimo-v2.5",
      ].sort(),
    );
  });
});
