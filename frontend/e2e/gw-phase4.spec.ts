import { test, expect } from "@playwright/test";
import { loadFixtures, mockDashboard } from "./mocks.js";

// Phase 4 frontend (R3 stale-status cold render, R4 density + column
// visibility, R5 mutation-toast discipline). Hermetic: the shared mock
// backend serves fixtures; fault injection re-registers a later route (last
// registered wins) to hang/fail one endpoint.
// Web-first assertions only, role/label locators, response-driven waits.
const ADMIN = "http://127.0.0.1:4173/admin/";

function seedCache(
  script: { key: string; value: unknown },
  savedAt = 1720000000000,
) {
  return { key: script.key, value: script.value, savedAt };
}

test.describe("phase 4 frontend (cold render, density, columns, toasts)", () => {
  test("overview cold-renders the seeded snapshot with a stale badge, then goes fresh", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    const seed = seedCache({ key: "overview", value: f.overview });
    await page.addInitScript((s) => {
      try {
        localStorage.setItem(
          `fp-cache:${s.key}`,
          JSON.stringify({ savedAt: s.savedAt, value: s.value }),
        );
      } catch {
        /* blocked storage: skeleton path stands */
      }
    }, seed);
    // Hang the overview API: the first paint must come from the seed.
    let release!: () => void;
    const gate = new Promise<void>((r) => {
      release = r;
    });
    await page.route("**/admin/api/overview*", async (route) => {
      await gate;
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(f.overview),
      });
    });
    await page.goto(`${ADMIN}#overview`);
    // Cold paint: seeded KPIs render with the stale badge before any fetch.
    await expect(page.getByText("Fleet accounts")).toBeVisible();
    await expect(page.getByTestId("stale-badge")).toBeVisible();
    // First confirmed poll replaces the seed and drops the badge.
    release!();
    await page.waitForResponse((r) => r.url().includes("/admin/api/overview"));
    await expect(page.getByTestId("stale-badge")).toHaveCount(0);
    await expect(page.getByText("Fleet accounts")).toBeVisible();
  });

  test("tokens cold-render seeds the table with a stale badge, then goes fresh", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    const seed = seedCache({ key: "tokens", value: f.tokens });
    await page.addInitScript((s) => {
      try {
        localStorage.setItem(
          `fp-cache:${s.key}`,
          JSON.stringify({ savedAt: s.savedAt, value: s.value }),
        );
      } catch {
        /* blocked storage: skeleton path stands */
      }
    }, seed);
    let release!: () => void;
    const gate = new Promise<void>((r) => {
      release = r;
    });
    await page.route("**/admin/api/tokens*", async (route) => {
      await gate;
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(f.tokens),
      });
    });
    await page.goto(`${ADMIN}#tokens`);
    await expect(page.getByText("Account #1").first()).toBeVisible();
    await expect(page.getByTestId("stale-badge")).toBeVisible();
    release!();
    await page.waitForResponse((r) => r.url().includes("/admin/api/tokens"));
    await expect(page.getByTestId("stale-badge")).toHaveCount(0);
  });

  test("tokens density toggle + status/instance column visibility", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    await page.goto(`${ADMIN}#tokens`);
    await expect(page.getByText("Account #1").first()).toBeVisible();

    const statusBtn = page.getByRole("button", {
      name: "Status",
      exact: true,
    });
    const instanceBtn = page.getByRole("button", {
      name: "Instance",
      exact: true,
    });
    const compactBtn = page.getByRole("button", {
      name: "Compact",
      exact: true,
    });
    const table = page.locator("table.fp-table");
    await expect(table.getByText("Status", { exact: true })).toBeVisible();
    await expect(table.getByText("Instance", { exact: true })).toBeVisible();

    // Hide Status: header and every status cell go, the rest stays.
    await statusBtn.click();
    await expect(statusBtn).toHaveAttribute("aria-pressed", "false");
    await expect(table.getByText("Status", { exact: true })).toHaveCount(0);
    await expect(table.getByText("Account #1")).toBeVisible();
    // ... and back.
    await statusBtn.click();
    await expect(statusBtn).toHaveAttribute("aria-pressed", "true");
    await expect(table.getByText("Status", { exact: true })).toBeVisible();

    // Hide Instance: the Drop Session kill switch (an Instance-cell
    // resident) leaves with it.
    await instanceBtn.click();
    await expect(table.getByText("Instance", { exact: true })).toHaveCount(0);
    await instanceBtn.click();
    await expect(table.getByText("Instance", { exact: true })).toBeVisible();

    // Compact density marks the wrapper; rows stay fully populated.
    await compactBtn.click();
    await expect(compactBtn).toHaveAttribute("aria-pressed", "true");
    await expect(page.locator(".density-compact")).toHaveCount(1);
    await expect(table.getByText("Account #1")).toBeVisible();
    await compactBtn.click();
    await expect(compactBtn).toHaveAttribute("aria-pressed", "false");
    await expect(page.locator(".density-compact")).toHaveCount(0);
  });

  test("activity table view: density + time/details column toggles", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    await page.goto(`${ADMIN}#activity`);
    await expect(page.getByText("1 model request").first()).toBeVisible();
    await page.getByRole("button", { name: "Table", exact: true }).click();
    await expect(page.getByText("entries")).toBeVisible();

    // Field values render on the default view (fixture rows carry fields).
    await expect(
      page.getByText("req_id", { exact: true }).first(),
    ).toBeVisible();

    const timeBtn = page.getByRole("button", { name: "Time", exact: true });
    const detailsBtn = page.getByRole("button", {
      name: "Details",
      exact: true,
    });
    const compactBtn = page.getByRole("button", {
      name: "Compact",
      exact: true,
    });

    await detailsBtn.click();
    await expect(detailsBtn).toHaveAttribute("aria-pressed", "false");
    await expect(page.getByText("req_id", { exact: true })).toHaveCount(0);
    await detailsBtn.click();
    await expect(detailsBtn).toHaveAttribute("aria-pressed", "true");
    await expect(
      page.getByText("req_id", { exact: true }).first(),
    ).toBeVisible();

    await timeBtn.click();
    await expect(timeBtn).toHaveAttribute("aria-pressed", "false");
    await timeBtn.click();
    await expect(timeBtn).toHaveAttribute("aria-pressed", "true");

    await compactBtn.click();
    await expect(compactBtn).toHaveAttribute("aria-pressed", "true");
  });

  test("repeated log poll failures render exactly one toast", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    // Fault injection AFTER the shared mocks: the later matching route
    // wins (maturity history comment in mocks.ts), so this 500 owns
    // /admin/api/logs while mockDashboard serves every other endpoint.
    await page.route("**/admin/api/logs**", async (route) => {
      await route.fulfill({
        status: 500,
        contentType: "application/json",
        body: JSON.stringify({ ok: false, message: "boom" }),
      });
    });
    await page.goto(`${ADMIN}#activity`);
    // The 500 must own the logs endpoint: the console error view renders.
    await expect(page.getByRole("button", { name: "Retry" })).toBeVisible({
      timeout: 10000,
    });
    // Mount fetch is operator-initiated: one error toast with the message.
    // (Role-only locator: the alert's accessible name also folds in the
    // dismiss-button label, so a text assertion carries the content.)
    await expect(page.getByRole("alert")).toHaveCount(1);
    await expect(page.getByText(/Could not load log entries/)).toBeVisible();
    // Three more 1s auto-poll failures land: still exactly one toast.
    for (let i = 0; i < 3; i++) {
      await page.waitForResponse((r) => r.url().includes("/admin/api/logs"));
    }
    await expect(page.getByRole("alert")).toHaveCount(1);
  });
});
