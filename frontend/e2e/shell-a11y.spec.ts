import { test, expect } from "@playwright/test";
import { loadFixtures, mockDashboard, mockSettingsOverlay } from "./mocks.js";
import { adminUrl } from "./mock-data.js";

// Shell chrome + a11y behavior against the hermetic mock backend
// (mockDashboard + mockSettingsOverlay): one route layer, inline overrides
// only — this spec never touches fixtures/ or sibling specs. Web-first
// assertions: role/name locators, fake timers for timed flips, no sleeps.

const DEFAULT_TOKEN_STATUS = {
  authenticated: true,
  is_default_admin_token: true,
  require_login: false,
  has_password: true,
};

test.describe("dashboard shell a11y (mock backend)", () => {
  test.use({ expect: { timeout: 10_000 } });

  test("nav marks only the active tab with aria-current", async ({ page }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    await mockSettingsOverlay(page, []);
    await page.goto(adminUrl("overview"));

    const sidebar = page.locator('aside[aria-label="Sidebar"]');
    await expect(
      sidebar.locator('nav[aria-label="Main navigation"]'),
    ).toBeVisible();
    const overviewLink = sidebar.getByRole("link", { name: "Overview" });
    const poolLink = sidebar.getByRole("link", { name: "Pool" });
    await expect(overviewLink).toHaveAttribute("aria-current", "page");
    await expect(poolLink).not.toHaveAttribute("aria-current", "page");

    await poolLink.click();
    await expect(
      page.getByRole("heading", { name: "Pool", exact: true }),
    ).toBeVisible();
    await expect(poolLink).toHaveAttribute("aria-current", "page");
    await expect(overviewLink).not.toHaveAttribute("aria-current", "page");
  });

  test("default admin token forces the change-password flow and posts the update", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f, { authStatus: DEFAULT_TOKEN_STATUS });
    await mockSettingsOverlay(page, []);
    await page.goto(adminUrl("overview"));

    // The forced flow: banner names the risk, its button opens the modal.
    await expect(page.getByText("Default password in use")).toBeVisible();
    await page.getByRole("button", { name: "Change password" }).click();
    const dialog = page.getByRole("dialog", {
      name: "Change Admin Password",
    });
    await expect(dialog).toBeVisible();

    const changeReq = page.waitForRequest(
      (r) =>
        r.method() === "POST" && r.url().includes("/admin/api/change-password"),
    );
    await page.locator("#current-password").fill("123456");
    await page.locator("#new-password").fill("s3cure-new-pass");
    await page.getByRole("button", { name: "Update Password" }).click();
    const req = await changeReq;
    expect(req.postDataJSON()).toEqual({
      current_password: "123456",
      new_password: "s3cure-new-pass",
    });
    await expect(page.getByText("Password changed")).toBeVisible();
  });

  test("change-password modal closes on Escape and restores focus", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f, { authStatus: DEFAULT_TOKEN_STATUS });
    await mockSettingsOverlay(page, []);
    await page.goto(adminUrl("overview"));

    const changeBtn = page.getByRole("button", { name: "Change password" });
    await changeBtn.click();
    const dialog = page.getByRole("dialog", {
      name: "Change Admin Password",
    });
    await expect(dialog).toBeVisible();

    await page.keyboard.press("Escape");
    await expect(dialog).toBeHidden();
    await expect(changeBtn).toBeFocused();
  });

  test("change-password modal closes on backdrop click and restores focus", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f, { authStatus: DEFAULT_TOKEN_STATUS });
    await mockSettingsOverlay(page, []);
    await page.goto(adminUrl("overview"));

    const changeBtn = page.getByRole("button", { name: "Change password" });
    await changeBtn.click();
    const dialog = page.getByRole("dialog", {
      name: "Change Admin Password",
    });
    await expect(dialog).toBeVisible();

    // Backdrop and the header X share the "Close dialog" name; the backdrop
    // is first in DOM order. The dialog card covers the backdrop center, so
    // click a corner that only the backdrop occupies.
    await page
      .getByRole("button", { name: "Close dialog" })
      .first()
      .click({ position: { x: 10, y: 10 } });
    await expect(dialog).toBeHidden();
    await expect(changeBtn).toBeFocused();
  });

  test("AccessSecurity inline form posts change-password", async ({ page }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    await mockSettingsOverlay(page, []);
    await page.goto(adminUrl("settings"));
    await expect(
      page.getByRole("heading", { name: "Access and Security" }),
    ).toBeVisible();

    const changeReq = page.waitForRequest(
      (r) =>
        r.method() === "POST" && r.url().includes("/admin/api/change-password"),
    );
    await page.locator("#sec-current-password").fill("123456");
    await page.locator("#sec-new-password").fill("another-new-pass");
    await page.getByRole("button", { name: "Update Password" }).click();
    const req = await changeReq;
    expect(req.postDataJSON()).toEqual({
      current_password: "123456",
      new_password: "another-new-pass",
    });
    await expect(page.getByText("Password changed")).toBeVisible();
  });

  test("device-login copy button flips to Copied via the clipboard stub", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    await mockSettingsOverlay(page, []);
    await page.addInitScript(() => {
      (window as unknown as Record<string, unknown>).__copied = null;
      Object.defineProperty(navigator, "clipboard", {
        value: {
          writeText: async (t: string) => {
            (window as unknown as Record<string, unknown>).__copied = t;
          },
        },
        configurable: true,
      });
    });
    const loginUrl = "https://freebuff.app/device/login?code=fp-shell-a11y";
    await page.route("**/admin/login/start", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          fingerprint: "fp-shell-a11y",
          login_url: loginUrl,
        }),
      });
    });
    await page.route("**/admin/login/status**", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ status: "pending" }),
      });
    });
    await page.goto(adminUrl("tokens"));
    await expect(
      page.getByRole("heading", { name: "Pool", exact: true }),
    ).toBeVisible();

    // Fake timers from here: the 1.5s Copied flip-back is timer-driven.
    await page.clock.install();
    await page.getByRole("button", { name: "Device Login" }).click();
    await expect(
      page.getByText("Open this URL in your browser to sign in:"),
    ).toBeVisible();
    await page.getByRole("button", { name: "Copy link" }).click();
    await expect(page.getByRole("button", { name: "Copied" })).toBeVisible();
    expect(
      await page.evaluate(
        () => (window as unknown as Record<string, unknown>).__copied,
      ),
    ).toBe(loginUrl);
    await page.clock.fastForward(1600);
    await expect(page.getByRole("button", { name: "Copy link" })).toBeVisible();
  });
});
