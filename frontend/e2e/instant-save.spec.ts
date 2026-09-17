import { test, expect } from "@playwright/test";
import type { Page } from "@playwright/test";
import {
  loadFixtures,
  mockDashboard,
  mockSettingsOverlay,
  type PostedSetting,
} from "./mocks.js";

// Instant-save dashboard (no batched draft): every tunable row POSTs its key
// to /admin/api/settings ~400ms after edit and reports the server message
// inline via role=status. No row keeps an explicit Save button, and the
// settings page offers no whole-file .env writer at all.

const admin = (hash: string) => `http://127.0.0.1:4173/admin/#${hash}`;

function waitSettingsPost(page: Page) {
  return page.waitForRequest(
    (r) => r.method() === "POST" && r.url().includes("/admin/api/settings"),
    { timeout: 10000 },
  );
}

function postedKeys(posted: PostedSetting[]) {
  return posted.map((p) => p.key);
}

async function assertNoBatchSaveButtons(page: Page) {
  await expect(page.getByRole("button", { name: "Save Changes" })).toHaveCount(
    0,
  );
  await expect(
    page.getByRole("button", { name: "Save", exact: true }),
  ).toHaveCount(0);
}

test.describe("instant-save dashboard", () => {
  test("no per-row Save buttons on any settings surface", async ({ page }) => {
    const posted: PostedSetting[] = [];
    await mockDashboard(page, loadFixtures());
    await mockSettingsOverlay(page, posted);

    await page.goto(admin("settings"));
    await expect(
      page.getByRole("heading", { name: "Settings", exact: true }),
    ).toBeVisible();
    await assertNoBatchSaveButtons(page);

    await page.goto(admin("tokens"));
    await page.getByRole("button", { name: "Controls" }).click();
    await expect(
      page.getByRole("heading", { name: "Pool Controls" }),
    ).toBeVisible();
    await assertNoBatchSaveButtons(page);

    await page.goto(admin("plans"));
    await page.getByRole("button", { name: "Controls" }).click();
    await expect(
      page.getByRole("heading", { name: "Usage Controls" }),
    ).toBeVisible();
    await assertNoBatchSaveButtons(page);

    // LOG_LEVEL's live card sits on Settings (the Logs page has no Logging
    // tab any more); its Settings card title names the key.
    await page.goto(admin("settings"));
    await expect(
      page.getByRole("heading", { name: "Server Log Level", exact: true }),
    ).toBeVisible();
    await assertNoBatchSaveButtons(page);
  });

  test("degraded overlay disables per-key saves", async ({ page }) => {
    const posted: PostedSetting[] = [];
    await mockDashboard(page, loadFixtures());
    await mockSettingsOverlay(page, posted, { degraded: true });
    await page.goto(admin("settings"));
    await expect(
      page.getByText("overlay offline — per-key save unavailable").first(),
    ).toBeVisible();

    const safeMode = page.getByRole("switch", { name: "SAFE_MODE" });
    await expect(safeMode).toBeVisible();
    await safeMode.click();
    // The debounced write never fires while the store is offline.
    await expect.poll(() => posted.length).toBe(0);
    await expect(page.getByRole("status")).toHaveCount(0);
  });

  test("restart-only keys POST instantly and report the restart copy", async ({
    page,
  }) => {
    const posted: PostedSetting[] = [];
    await mockDashboard(page, loadFixtures());
    await mockSettingsOverlay(page, posted);

    // LOG_LEVEL now lives on Settings (the Logs page dropped its Logging
    // tab; Settings renders the live card instead of a link-out stub).
    await page.goto(admin("settings"));
    const logLevel = page.getByRole("combobox", { name: "LOG_LEVEL" });
    await expect(logLevel).toBeVisible();
    const logLevelPost = waitSettingsPost(page);
    await logLevel.selectOption("debug");
    await logLevelPost;
    await expect.poll(() => postedKeys(posted)).toContain("LOG_LEVEL");
    await expect(
      page
        .getByRole("status")
        .filter({ hasText: "LOG_LEVEL saved. It applies after restart." }),
    ).toBeVisible();

    // HTTP_READ_TIMEOUT is env-only (data-architecture decision): the
    // Gateway card renders the effective value read-only with an env-note —
    // never a select, never a save-success/restart copy.
    await expect(
      page.getByRole("combobox", { name: "HTTP_READ_TIMEOUT" }),
    ).toHaveCount(0);
    await expect(
      page.getByText("HTTP_READ_TIMEOUT", { exact: true }),
    ).toBeVisible();
    await expect(
      page.getByText("the reader never consults the overlay").first(),
    ).toBeVisible();
    await expect(
      page.getByRole("status").filter({ hasText: "HTTP_READ_TIMEOUT saved" }),
    ).toHaveCount(0);

    // LOG_FORMAT is not on the LOG_LEVEL "Logging" card: it renders in the
    // "Logging & Diagnostics" card (Settings, general-group keys).
    const logFormat = page.getByRole("combobox", { name: "LOG_FORMAT" });
    await expect(logFormat).toBeVisible();
    const formatPost = waitSettingsPost(page);
    await logFormat.selectOption("json");
    await formatPost;
    await expect.poll(() => postedKeys(posted)).toContain("LOG_FORMAT");
    await expect(
      page
        .getByRole("status")
        .filter({ hasText: "LOG_FORMAT saved. It applies after restart." }),
    ).toBeVisible();
  });
  test("env-only keys POST 400 with the gateway's verbatim pointer", async ({
    page,
  }) => {
    const posted: PostedSetting[] = [];
    await mockDashboard(page, loadFixtures());
    await mockSettingsOverlay(page, posted);
    await page.goto(admin("settings"));
    // The mock models the real backend gate (admin_settings.go): a direct
    // knob write for an env-only key 400s instead of persisting an inert
    // row. Pinned for all five keys; the UI never offers a save control
    // that could hit this path, so the fetch below drives it directly.
    for (const key of [
      "SESSION_STATE_FILE",
      "SESSION_PERSIST",
      "LOG_FILE",
      "HTTP_READ_TIMEOUT",
      "AUTO_DISCOVER_TOKEN",
    ]) {
      const res = await page.evaluate(async (k) => {
        const r = await fetch("/admin/api/settings", {
          method: "POST",
          headers: { "content-type": "application/json" },
          body: JSON.stringify({ key: k, value: "false" }),
        });
        return { status: r.status, body: await r.json() };
      }, key);
      expect(res.status).toBe(400);
      expect(res.body.ok).toBe(false);
      expect(res.body.code).toBe("invalid_setting");
      expect(res.body.message).toBe(
        `${key} is set in the environment or .env file, not as a knob (the reader never consults the overlay).`,
      );
      expect(res.body.message).not.toContain("saved");
    }
    // Rejected writes are never collected as saves.
    expect(postedKeys(posted)).toEqual([]);
  });
  test("env-shadowed row notes the override and keeps it after save", async ({
    page,
  }) => {
    const posted: PostedSetting[] = [];
    await mockDashboard(page, loadFixtures());
    await mockSettingsOverlay(page, posted, {
      seed: [{ key: "SAFE_MODE", value: "true", source: "env" }],
    });
    await page.goto(admin("settings"));
    await expect(
      page.getByText("overridden by process env", { exact: true }),
    ).toBeVisible();

    const savePost = waitSettingsPost(page);
    await page.getByRole("switch", { name: "SAFE_MODE" }).click();
    await savePost;
    await expect.poll(() => postedKeys(posted)).toContain("SAFE_MODE");
    await expect(
      page.getByRole("status").filter({ hasText: "Overridden by process env" }),
    ).toBeVisible();
  });
});
