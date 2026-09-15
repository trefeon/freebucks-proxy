import { test, expect } from "@playwright/test";
import { loadFixtures, mockDashboard, mockSettingsOverlay } from "./mocks.js";
import type { PostedSetting } from "./mocks.js";

const root = "http://127.0.0.1:4173/admin/";
const admin = (hash: string) => `http://127.0.0.1:4173/admin/#${hash}`;

/**
 * In-memory pages_state backend for the suite: GET returns the seeded
 * snapshot ({} when absent, never 404); PUT stores {data} verbatim. The
 * returned map lets tests assert what the SPA persisted.
 */
async function mockPageState(
  page: Parameters<typeof mockDashboard>[0],
  seed: Record<string, unknown> = {},
) {
  const state = new Map<string, unknown>(Object.entries(seed));
  await page.route("**/admin/api/pages/*", async (route) => {
    const id = new URL(route.request().url()).pathname.split("/").pop() ?? "";
    if (route.request().method() === "GET") {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ data: state.get(id) ?? {} }),
      });
    } else {
      let data: unknown = {};
      try {
        data = JSON.parse(route.request().postData() ?? "{}").data ?? {};
      } catch {
        data = {};
      }
      state.set(id, data);
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          ok: true,
          message: "Page state saved.",
          code: "page_saved",
        }),
      });
    }
  });
  return state;
}

test.describe("per-page persist", () => {
  test("boot with no hash restores the last-visited page", async ({ page }) => {
    await mockDashboard(page, loadFixtures());
    await mockPageState(page, { shell: { lastHash: "tokens" } });
    await page.goto(root);
    await expect
      .poll(() => new URL(page.url()).hash, { timeout: 10_000 })
      .toBe("#tokens");
    await expect(
      page.getByRole("heading", { name: "Pool", exact: true }),
    ).toBeVisible();
  });

  test("an explicit hash always wins over the stored lastHash", async ({
    page,
  }) => {
    await mockDashboard(page, loadFixtures());
    await mockPageState(page, { shell: { lastHash: "tokens" } });
    // A legacy hash is an explicit route too: it redirects to its target
    // (and the normalized hash wins over the stored lastHash).
    await page.goto(admin("models"));
    await expect(
      page.getByRole("heading", { name: "Usage", exact: true }),
    ).toBeVisible();
    expect(new URL(page.url()).hash).toBe("#plans");
  });

  test("logs filter text round-trips across reload", async ({ page }) => {
    await mockDashboard(page, loadFixtures());
    const state = await mockPageState(page);
    await page.goto(admin("activity"));
    // Filters live in the Table view (Console is the default).
    await page.getByRole("button", { name: "Table" }).click();
    const filter = page.locator("#log-msg");
    await filter.fill("zz-filter-1");
    // The debounced save (~1s) PUTs the snapshot; the mount visit PUT may
    // land first, so poll until the filter payload arrives.
    await expect
      .poll(
        () => {
          const snapshot = state.get("logs");
          if (
            snapshot &&
            typeof snapshot === "object" &&
            "filterMsg" in snapshot
          ) {
            return snapshot.filterMsg;
          }
          return undefined;
        },
        {
          timeout: 10_000,
        },
      )
      .toBe("zz-filter-1");
    await page.reload();
    await page.getByRole("button", { name: "Table" }).click();
    await expect(page.locator("#log-msg")).toHaveValue("zz-filter-1");
  });

  test("tokens expanded row restores from the snapshot", async ({ page }) => {
    await mockDashboard(page, loadFixtures());
    await mockPageState(page, { tokens: { expandedToken: 0 } });
    await page.goto(admin("tokens"));
    const table = page.locator("table.fp-table");
    await expect(table.getByText("Account #1")).toBeVisible({
      timeout: 10_000,
    });
    // Expanded without any click: the snapshot drove expandedToken. The
    // live countdown renders in the status cell; the drawer proves itself
    // open via its pin select — Drop Session now lives in the status cell,
    // so it can no longer prove the drawer opened.
    await expect(table.getByText("Active Session:")).toHaveCount(0);
    await expect(
      table.locator('[aria-label^="Session time remaining"]').first(),
    ).toBeVisible();
    await expect(table.getByLabel("Pin a model to this token")).toBeVisible();
  });

  test("tokens out-of-range index drops the drawer instead of opening the wrong row", async ({
    page,
  }) => {
    await mockDashboard(page, loadFixtures());
    // Five pooled accounts in fixtures; index 99 matches none.
    await mockPageState(page, { tokens: { expandedToken: 99 } });
    await page.goto(admin("tokens"));
    const table = page.locator("table.fp-table");
    await expect(table.getByText("Account #1")).toBeVisible({
      timeout: 10_000,
    });
    // No drawer opened (and the stale index is dropped, never re-persisted).
    // Status-cell Drop Session buttons still render for active rows, so the
    // drawer-absent proof is the drawer-only pin select.
    await expect(table.getByLabel("Pin a model to this token")).toHaveCount(0);
  });

  test("logs full filter set round-trips across reload", async ({ page }) => {
    await mockDashboard(page, loadFixtures());
    const state = await mockPageState(page);
    await page.goto(admin("activity"));
    // Filters live in the Table view (Console is the default).
    await page.getByRole("button", { name: "Table" }).click();
    await page.locator("#log-level").selectOption("info");
    await page.locator("#log-msg").fill("request");
    // Hide-admin defaults on; flipping it off is part of the persisted set.
    await page.getByRole("button", { name: "Hide admin" }).click();
    // 13 info+request fixture entries → page 2 exists at 10 rows/page.
    await page.getByRole("button", { name: "Next" }).click();
    // The debounced save (~1s) PUTs the snapshot; poll until the full
    // filter payload arrives.
    await expect
      .poll(
        () => {
          const snapshot = state.get("logs");
          if (snapshot && typeof snapshot === "object") {
            const s = snapshot as Record<string, unknown>;
            if (
              s.filterMsg === "request" &&
              s.filterLevel === "info" &&
              s.hideAdmin === false &&
              s.viewMode === "table" &&
              s.page === 1
            )
              return "ready";
          }
          return undefined;
        },
        { timeout: 10_000 },
      )
      .toBe("ready");
    await page.reload();
    // Table view restored without clicking: the stored viewMode drove it.
    await expect(page.locator("#log-level")).toBeVisible({ timeout: 10_000 });
    await expect(page.locator("#log-level")).toHaveValue("info");
    await expect(page.locator("#log-msg")).toHaveValue("request");
    await expect(
      page.getByRole("button", { name: "Hide admin" }),
    ).toHaveAttribute("aria-pressed", "false");
    await expect(page.getByText("Page 2 / 2")).toBeVisible();
  });

  test("an unknown hash is never persisted to shell.lastHash", async ({
    page,
  }) => {
    await mockDashboard(page, loadFixtures());
    const state = await mockPageState(page);
    await page.goto(admin("no-such-page"));
    // Past the debounce window there must still be no shell snapshot: the
    // App shell only remembers known page ids.
    await page.waitForTimeout(1500);
    expect(state.get("shell")).toBeUndefined();
  });
});

test.describe("settings saved values", () => {
  test("model routing rows mount with saved-value notes and per-key toggle auto-saves", async ({
    page,
  }) => {
    await mockDashboard(page, loadFixtures());
    const posted: PostedSetting[] = [];
    await mockSettingsOverlay(page, posted, {
      seed: [
        { key: "LOG_LEVEL", value: "info", source: "db" },
        { key: "REASONING_IN_CONTENT", value: "true", source: "db" },
      ],
    });
    await mockPageState(page);
    await page.goto(admin("plans"));
    await expect(
      page.getByRole("heading", { name: "Usage", exact: true }),
    ).toBeVisible({ timeout: 10_000 });
    // Controls live behind the Usage Controls tab now.
    await page.getByRole("button", { name: "Controls" }).click();
    const input = page.locator('input[aria-label="REASONING_IN_CONTENT"]');
    await expect(input).toBeVisible();
    await expect(
      page.getByText("saved value", { exact: true }).first(),
    ).toBeVisible();
    // Row-anchored: the locator binds to the REASONING_IN_CONTENT row
    // itself, so sibling cards cannot shadow its status.
    const row = page.locator("div.py-4", { has: input });
    const rowSwitch = row.getByRole("switch", {
      name: "REASONING_IN_CONTENT",
    });
    await expect(rowSwitch).toHaveAttribute("aria-checked", "true");
    // Instant-save: toggling the switch auto-POSTs after the debounce —
    // no per-key Save button exists anymore.
    const saveReq = page.waitForRequest(
      (r) => r.method() === "POST" && r.url().includes("/admin/api/settings"),
      { timeout: 10_000 },
    );
    await rowSwitch.click();
    await saveReq;
    await expect
      .poll(
        () => posted.filter((p) => p.key === "REASONING_IN_CONTENT").length,
        {
          timeout: 10_000,
        },
      )
      .toBeGreaterThan(0);
    await expect(
      row.locator('span[role="status"]', {
        hasText: "saved and applied live",
      }),
    ).toBeVisible({ timeout: 10_000 });
    await expect(rowSwitch).toHaveAttribute("aria-checked", "false");
    // Reload: the overlay GET reflects the POST, so the switch stays off.
    await page.reload();
    await expect(
      page.getByRole("heading", { name: "Usage", exact: true }),
    ).toBeVisible({ timeout: 10_000 });
    await page.getByRole("button", { name: "Controls" }).click();
    await expect(
      page.locator('input[aria-label="REASONING_IN_CONTENT"]'),
    ).toBeVisible();
    await expect(
      page
        .locator("div.py-4", {
          has: page.locator('input[aria-label="REASONING_IN_CONTENT"]'),
        })
        .getByRole("switch", { name: "REASONING_IN_CONTENT" }),
    ).toHaveAttribute("aria-checked", "false");
  });
  test("a rejected saved value surfaces inline on the row", async ({
    page,
  }) => {
    await mockDashboard(page, loadFixtures());
    const posted: PostedSetting[] = [];
    await mockSettingsOverlay(page, posted, { postStatus: 400 });
    await mockPageState(page);
    await page.goto(admin("plans"));
    await page.getByRole("button", { name: "Controls" }).click();
    const input = page.locator('input[aria-label="REASONING_IN_CONTENT"]');
    await expect(input).toBeVisible({ timeout: 10_000 });
    // Row-anchored like the save test: the rejection must surface on the
    // REASONING_IN_CONTENT row itself.
    const row = page.locator("div.py-4", { has: input });
    const rowSwitch = row.getByRole("switch", {
      name: "REASONING_IN_CONTENT",
    });
    const firstPost = page.waitForRequest(
      (r) => r.method() === "POST" && r.url().includes("/admin/api/settings"),
      { timeout: 10_000 },
    );
    await rowSwitch.click();
    await firstPost;
    await expect
      .poll(() => posted.length, { timeout: 10_000 })
      .toBeGreaterThan(0);
    await expect(
      row.locator('span[role="status"]', { hasText: "Setting rejected" }),
    ).toBeVisible();
    await expect(row.getByRole("button", { name: "Retry" })).toBeVisible();
    const secondPost = page.waitForRequest(
      (r) => r.method() === "POST" && r.url().includes("/admin/api/settings"),
      { timeout: 10_000 },
    );
    await row.getByRole("button", { name: "Retry" }).click();
    await secondPost;
    await expect
      .poll(() => posted.length, { timeout: 10_000 })
      .toBeGreaterThan(1);
  });

  test("saved-value reset deletes the key and refetches the form", async ({
    page,
  }) => {
    await mockDashboard(page, loadFixtures());
    const posted: PostedSetting[] = [];
    const { deleted } = await mockSettingsOverlay(page, posted, {
      seed: [
        { key: "LOG_LEVEL", value: "info", source: "db" },
        { key: "REASONING_IN_CONTENT", value: "true", source: "db" },
      ],
    });
    // Moved keys render inline on their section pages now: LOG_LEVEL on
    // the Logs Logging tab, REASONING_IN_CONTENT on the Usage Controls tab.
    // One saved-value note per page.
    await page.goto(admin("activity"));
    await page.getByRole("button", { name: "Logging" }).click();
    await expect(page.getByRole("combobox", { name: "LOG_LEVEL" })).toBeVisible(
      { timeout: 10_000 },
    );
    await expect(page.getByText("saved value", { exact: true })).toHaveCount(1);
    await page.goto(admin("plans"));
    await page.getByRole("button", { name: "Controls" }).click();
    await expect(
      page.locator('input[aria-label="REASONING_IN_CONTENT"]'),
    ).toBeVisible({ timeout: 10_000 });
    await expect(page.getByText("saved value", { exact: true })).toHaveCount(1);
    // Reset on the Usage row drops REASONING_IN_CONTENT and refetches the form.
    const delReq = page.waitForRequest(
      (r) =>
        r.method() === "DELETE" && r.url().includes("/admin/api/settings/"),
    );
    await page.getByRole("button", { name: "Reset" }).first().click();
    await delReq;
    expect(deleted).toEqual(["REASONING_IN_CONTENT"]);
    await expect(page.getByText("saved value", { exact: true })).toHaveCount(0);
    await expect(page.getByText("Saved value removed.")).toBeVisible();
  });

  test("degraded store banners read-only while the .env form stays usable", async ({
    page,
  }) => {
    await mockDashboard(page, loadFixtures());
    const posted: PostedSetting[] = [];
    await mockSettingsOverlay(page, posted, { degraded: true });
    // Degraded banner + offline row note render on the Logs Logging tab.
    await page.goto(admin("activity"));
    await page.getByRole("button", { name: "Logging" }).click();
    await expect(page.getByRole("combobox", { name: "LOG_LEVEL" })).toBeVisible(
      { timeout: 10_000 },
    );
    await expect(page.getByText("DB overlay unavailable")).toBeVisible();
    await expect(
      page.getByText("overlay offline — per-key save unavailable").first(),
    ).toBeVisible();
    await expect.poll(() => posted.length).toBe(0);
    // Editing LOG_LEVEL while degraded issues no settings POST: the row
    // stays read-only for saves through the debounce window.
    const combo = page.getByRole("combobox", { name: "LOG_LEVEL" });
    const current = await combo.inputValue();
    await combo.selectOption(current === "debug" ? "info" : "debug");
    await expect.poll(() => posted.length).toBe(0);
    const noPost = await page
      .waitForRequest(
        (r) => r.method() === "POST" && r.url().includes("/admin/api/settings"),
        { timeout: 1500 },
      )
      .then(
        () => false,
        () => true,
      );
    expect(noPost).toBe(true);
    expect(posted).toHaveLength(0);
    // The break-glass whole-file path stays available on Settings.
    await page.goto(admin("settings"));
    await expect(
      page.getByRole("heading", { name: "Emergency raw .env editor" }),
    ).toBeVisible({ timeout: 10_000 });
    await expect(
      page.getByRole("button", { name: "Save raw .env" }),
    ).toBeVisible();
  });
});
