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
// inline via role=status. Only the emergency raw .env editor keeps explicit
// Save/Revert buttons.

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
  test("no per-row Save buttons outside the emergency editor", async ({
    page,
  }) => {
    const posted: PostedSetting[] = [];
    await mockDashboard(page, loadFixtures());
    await mockSettingsOverlay(page, posted);

    await page.goto(admin("settings"));
    await expect(
      page.getByRole("heading", { name: "Settings", exact: true }),
    ).toBeVisible();
    await assertNoBatchSaveButtons(page);
    // The break-glass path keeps its own buttons: heading + Save + textarea.
    await expect(
      page.getByRole("heading", { name: "Emergency raw .env editor" }),
    ).toBeVisible();
    await expect(
      page.getByRole("button", { name: "Save raw .env" }),
    ).toBeVisible();
    await expect(page.locator("#emergency-raw-env")).toBeVisible();

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

    // HTTP_READ_TIMEOUT lives in the Settings Gateway card; 120s is a real
    // catalog option (30s is not — the select only offers listed values).
    const readTimeout = page.getByRole("combobox", {
      name: "HTTP_READ_TIMEOUT",
    });
    await expect(readTimeout).toBeVisible();
    const timeoutPost = waitSettingsPost(page);
    await readTimeout.selectOption("120s");
    await timeoutPost;
    await expect.poll(() => postedKeys(posted)).toContain("HTTP_READ_TIMEOUT");
    await expect(
      page.getByRole("status").filter({
        hasText: "HTTP_READ_TIMEOUT saved. It applies after restart.",
      }),
    ).toBeVisible();

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

  test("emergency editor reverts locally and saves through /admin/config", async ({
    page,
  }) => {
    await mockDashboard(page, loadFixtures());
    await mockSettingsOverlay(page, []);
    // Flag route for the whole-file save (registered after mockDashboard so
    // it wins for POST /admin/config).
    let configPosts = 0;
    await page.route("**/admin/config", async (route) => {
      if (route.request().method() === "POST") {
        configPosts += 1;
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify({ ok: true, message: "Config saved" }),
        });
      } else {
        await route.continue();
      }
    });

    await page.goto(admin("settings"));
    const editor = page.locator("#emergency-raw-env");
    await expect(editor).toBeVisible();
    await expect(editor).not.toHaveValue("", { timeout: 10000 });
    const base = await editor.inputValue();

    await editor.fill(`${base}\n# e2e-probe=1\n`);
    await expect(page.getByRole("button", { name: "Revert" })).toBeVisible();
    await page.getByRole("button", { name: "Revert" }).click();
    await expect(editor).toHaveValue(base);
    expect(configPosts).toBe(0);

    await editor.fill(`${base}\n# e2e-probe=1\n`);
    const saveReq = page.waitForRequest(
      (r) => r.method() === "POST" && r.url().includes("/admin/config"),
      { timeout: 10000 },
    );
    page.once("dialog", (d) => d.accept());
    await page.getByRole("button", { name: "Save raw .env" }).click();
    const req = await saveReq;
    expect(decodeURIComponent(req.postData() ?? "")).toContain("e2e-probe=1");
  });
});
