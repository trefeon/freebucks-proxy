import { test, expect } from "@playwright/test";
import type { Page } from "@playwright/test";
import type { PostedSetting } from "./mocks.js";
import {
  ALL_MATRIX_KEYS,
  CUSTOM_ADVANCED_KEYS,
  GATEWAY_KEYS,
  KEY_HOME,
  LOGGING_KEYS,
  MATRIX_LIVE_ENV,
  POOL_CONTROLS_KEYS,
  POOL_TUNING_KEYS,
  SECURITY_KEYS,
  STRATEGY_KEYS,
  UPSTREAM_QUOTA_KEYS,
  balanceStrategySeed,
  editor,
  fullMatrixDbSeed,
  gotoControls,
  gotoSettings,
  mockSettingsMatrix,
} from "./mock-settings.js";

// Settings + control-switches matrix: every catalog key with a real editor
// renders exactly once in exactly one home, and every home's editors
// instant-save through the DB overlay. One factory (mock-settings.ts) builds
// the fake backend state, one route layer (mocks.ts) serves it. Web-first
// assertions only, role/label locators, no sleeps, DAMP specs.
//
// Deliberately NOT re-pinned here (owned by siblings): the Drain/Balance
// preset write set, the Go-normalized "1m0s" echo round-trip, blank-row
// loader fallbacks (dashboard.spec); the generic rejected-save Retry row
// (interactables-db.spec); LOG_LEVEL/HTTP_READ_TIMEOUT/LOG_FORMAT restart
// copy and SAFE_MODE env-shadow (instant-save.spec).
async function expectPosted(
  posted: PostedSetting[],
  key: string,
  value: string,
): Promise<void> {
  await expect
    .poll(() => posted.filter((p) => p.key === key).map((p) => p.value), {
      timeout: 15000,
    })
    .toContain(value);
}

async function fillKey(page: Page, key: string, value: string): Promise<void> {
  await editor(page, key).fill(value);
}

async function toggleKey(page: Page, key: string): Promise<void> {
  await page.getByRole("switch", { name: key }).click();
}

test.describe("settings matrix: single-home render", () => {
  test.use({ expect: { timeout: 10_000 } });

  test("pool keys render once on Controls and nowhere else", async ({
    page,
  }) => {
    const poolKeys = [
      ...STRATEGY_KEYS,
      ...POOL_CONTROLS_KEYS,
      ...CUSTOM_ADVANCED_KEYS,
      ...POOL_TUNING_KEYS,
    ];
    expect(ALL_MATRIX_KEYS.filter((k) => KEY_HOME[k] === "pool")).toEqual(
      poolKeys,
    );
    await mockSettingsMatrix(page, { seed: fullMatrixDbSeed() });

    await gotoControls(page, "tokens");
    for (const key of poolKeys) {
      await expect(
        editor(page, key),
        `${key} has exactly one editor on Pool Controls`,
      ).toHaveCount(1);
    }

    await gotoSettings(page);
    for (const key of poolKeys) {
      await expect(
        editor(page, key),
        `${key} has no second editor on Settings`,
      ).toHaveCount(0);
    }

    await gotoControls(page, "plans");
    for (const key of poolKeys) {
      await expect(
        editor(page, key),
        `${key} has no second editor on Usage Controls`,
      ).toHaveCount(0);
    }
  });

  test("settings keys render once on Settings and nowhere else", async ({
    page,
  }) => {
    const settingsKeys = [...GATEWAY_KEYS, ...LOGGING_KEYS, ...SECURITY_KEYS];
    expect(ALL_MATRIX_KEYS.filter((k) => KEY_HOME[k] === "settings")).toEqual(
      settingsKeys,
    );
    await mockSettingsMatrix(page, { seed: fullMatrixDbSeed() });

    await gotoSettings(page);
    for (const key of settingsKeys) {
      await expect(
        editor(page, key),
        `${key} has exactly one editor on Settings`,
      ).toHaveCount(1);
    }

    await gotoControls(page, "tokens");
    for (const key of settingsKeys) {
      await expect(
        editor(page, key),
        `${key} has no second editor on Pool Controls`,
      ).toHaveCount(0);
    }

    await gotoControls(page, "plans");
    for (const key of settingsKeys) {
      await expect(
        editor(page, key),
        `${key} has no second editor on Usage Controls`,
      ).toHaveCount(0);
    }
  });

  test("usage keys render once on Usage Controls and nowhere else", async ({
    page,
  }) => {
    expect(ALL_MATRIX_KEYS.filter((k) => KEY_HOME[k] === "usage")).toEqual([
      ...UPSTREAM_QUOTA_KEYS,
    ]);
    await mockSettingsMatrix(page, { seed: fullMatrixDbSeed() });

    await gotoControls(page, "plans");
    await expect(page.getByText("Upstream & Quota")).toBeVisible();
    for (const key of UPSTREAM_QUOTA_KEYS) {
      await expect(
        editor(page, key),
        `${key} has exactly one editor on Usage Controls`,
      ).toHaveCount(1);
    }

    await gotoControls(page, "tokens");
    for (const key of UPSTREAM_QUOTA_KEYS) {
      await expect(
        editor(page, key),
        `${key} has no second editor on Pool Controls`,
      ).toHaveCount(0);
    }

    await gotoSettings(page);
    for (const key of UPSTREAM_QUOTA_KEYS) {
      await expect(
        editor(page, key),
        `${key} has no second editor on Settings`,
      ).toHaveCount(0);
    }
  });
  test("MODELS_ALLOW lives in the Upstream & Quota card", async ({ page }) => {
    await mockSettingsMatrix(page, { seed: fullMatrixDbSeed() });
    await gotoControls(page, "plans");
    // Card-scoped: the allowlist must render inside Upstream & Quota, not
    // just somewhere on the Usage page (it was COVERED yet rendered
    // nowhere before it gained this home).
    const card = page.locator("div.bg-surface", {
      has: page.getByRole("heading", { name: "Upstream & Quota" }),
    });
    await expect(card.locator('[aria-label="MODELS_ALLOW"]')).toHaveCount(1);
    await expect(card.locator('[aria-label="MODELS_ALLOW"]')).toHaveValue(
      "deepseek/deepseek-v4-flash",
    );
  });
});

test.describe("settings matrix: edits persist via the overlay", () => {
  test.use({ expect: { timeout: 10_000 } });

  test("strategy + pool-controls edits post their keys", async ({ page }) => {
    const { posted } = await mockSettingsMatrix(page, {
      seed: balanceStrategySeed(),
    });
    await gotoControls(page, "tokens");

    await fillKey(page, "SLOTS_PER_ACCOUNT", "3");
    await fillKey(page, "MAX_SPILL_ACCOUNTS", "1");
    await fillKey(page, "QUEUE_WAIT", "45s");
    await fillKey(page, "QUEUE_DEPTH", "32");
    await fillKey(page, "RATE_LIMIT_PER_IP", "20");
    // Bridge last: turning it off hides the BRIDGE_IDLE_EVICT row below.
    await toggleKey(page, "BRIDGE_ENABLED");

    await expectPosted(posted, "SLOTS_PER_ACCOUNT", "3");
    await expectPosted(posted, "MAX_SPILL_ACCOUNTS", "1");
    await expectPosted(posted, "QUEUE_WAIT", "45s");
    await expectPosted(posted, "QUEUE_DEPTH", "32");
    await expectPosted(posted, "RATE_LIMIT_PER_IP", "20");
    await expectPosted(posted, "BRIDGE_ENABLED", "false");
    // Hand-editing the posture flips the badge to Custom...
    await expect(
      page
        .getByRole("radiogroup", { name: "Pool strategy" })
        .getByText("Custom", { exact: true }),
    ).toBeVisible();
    await expect(
      page.getByRole("status").filter({ hasText: "QUEUE_WAIT saved" }),
    ).toBeVisible();
  });

  test("custom advanced + pool tuning edits post their keys", async ({
    page,
  }) => {
    const { posted } = await mockSettingsMatrix(page);
    await gotoControls(page, "tokens");

    await fillKey(page, "MODEL_UNAVAILABLE_CACHE_TTL", "2h");
    await fillKey(page, "SESSION_PROBE_CACHE_TTL", "30s");
    await fillKey(page, "SESSION_RE_ADMIT_LEAD", "90s");
    await toggleKey(page, "WAITING_ROOM_CHAIN");
    await toggleKey(page, "SESSION_PERSIST");
    await toggleKey(page, "ADOPT_CLI_SESSION");
    await fillKey(page, "BRIDGE_IDLE_EVICT", "48h");
    await fillKey(page, "IDLE_ROTATION_TIMEOUT", "1h");
    await fillKey(page, "RATE_LIMIT_BURST", "40");

    await expectPosted(posted, "MODEL_UNAVAILABLE_CACHE_TTL", "2h");
    await expectPosted(posted, "SESSION_PROBE_CACHE_TTL", "30s");
    await expectPosted(posted, "SESSION_RE_ADMIT_LEAD", "90s");
    await expectPosted(posted, "WAITING_ROOM_CHAIN", "true");
    await expectPosted(posted, "SESSION_PERSIST", "false");
    await expectPosted(posted, "ADOPT_CLI_SESSION", "true");
    await expectPosted(posted, "BRIDGE_IDLE_EVICT", "48h");
    await expectPosted(posted, "IDLE_ROTATION_TIMEOUT", "1h");
    await expectPosted(posted, "RATE_LIMIT_BURST", "40");
    // Restart-only rows keep their honest copy, not a live-apply claim.
    await expect(
      page
        .getByRole("status")
        .filter({ hasText: "ADOPT_CLI_SESSION saved" })
        .filter({ hasText: "restart" }),
    ).toBeVisible();
  });

  test("gateway + logging + security edits post their keys", async ({
    page,
  }) => {
    const { posted } = await mockSettingsMatrix(page);
    await gotoSettings(page);

    await toggleKey(page, "SAFE_MODE");
    await editor(page, "HTTP_READ_TIMEOUT").selectOption("120s");
    await editor(page, "LOG_LEVEL").selectOption("debug");
    await toggleKey(page, "DEBUG_DUMP");
    await toggleKey(page, "DEVTOOLS_ENABLED");
    await toggleKey(page, "LOG_ACCESS");
    await editor(page, "LOG_FORMAT").selectOption("json");
    await fillKey(page, "CORS_ALLOWED_ORIGIN", "https://example.com");
    await toggleKey(page, "DASHBOARD_REQUIRE_LOGIN");

    await expectPosted(posted, "SAFE_MODE", "false");
    await expectPosted(posted, "HTTP_READ_TIMEOUT", "120s");
    await expectPosted(posted, "LOG_LEVEL", "debug");
    await expectPosted(posted, "DEBUG_DUMP", "true");
    await expectPosted(posted, "DEVTOOLS_ENABLED", "true");
    await expectPosted(posted, "LOG_ACCESS", "false");
    await expectPosted(posted, "LOG_FORMAT", "json");
    await expectPosted(posted, "CORS_ALLOWED_ORIGIN", "https://example.com");
    await expectPosted(posted, "DASHBOARD_REQUIRE_LOGIN", "false");
    await expect(
      page
        .getByRole("status")
        .filter({ hasText: "DEBUG_DUMP saved" })
        .filter({ hasText: "restart" }),
    ).toBeVisible();
    await expect(
      page.getByRole("status").filter({ hasText: "SAFE_MODE saved" }),
    ).toBeVisible();
  });

  test("upstream edits post their keys", async ({ page }) => {
    const { posted } = await mockSettingsMatrix(page);
    await gotoControls(page, "plans");

    await toggleKey(page, "CACHE_CONTROL_INJECTION");
    await toggleKey(page, "COMPRESS_PROMPT");
    await toggleKey(page, "MODELS_HIDE_UNAVAILABLE");
    await fillKey(page, "REGISTRY_REFRESH", "12h");
    await fillKey(page, "MODELS_ALLOW", "deepseek/deepseek-v4-flash");
    await toggleKey(page, "REASONING_IN_CONTENT");

    await expectPosted(posted, "CACHE_CONTROL_INJECTION", "false");
    await expectPosted(posted, "COMPRESS_PROMPT", "true");
    await expectPosted(posted, "MODELS_HIDE_UNAVAILABLE", "true");
    await expectPosted(posted, "REGISTRY_REFRESH", "12h");
    await expectPosted(posted, "MODELS_ALLOW", "deepseek/deepseek-v4-flash");
    await expectPosted(posted, "REASONING_IN_CONTENT", "true");
    await expect(
      page.getByRole("status").filter({ hasText: "REGISTRY_REFRESH saved" }),
    ).toBeVisible();
  });
});

test.describe("settings matrix: threshold slider and overlay states", () => {
  test.use({ expect: { timeout: 10_000 } });

  test("balance threshold slider spans 5-300s and writes QUEUE_WAIT", async ({
    page,
  }) => {
    const { posted } = await mockSettingsMatrix(page, {
      seed: balanceStrategySeed(),
    });
    await gotoControls(page, "tokens");
    const slider = page.locator(
      'input[type="range"][aria-label="Balance threshold (QUEUE_WAIT)"]',
    );
    await expect(slider).toBeVisible();
    await expect(slider).toHaveAttribute("min", "5");
    await expect(slider).toHaveAttribute("max", "300");
    await slider.evaluate((el) => {
      const input = el as HTMLInputElement;
      input.value = "300";
      input.dispatchEvent(new Event("input", { bubbles: true }));
      input.dispatchEvent(new Event("change", { bubbles: true }));
    });
    await expectPosted(posted, "QUEUE_WAIT", "300s");
    // The slider and the picker are two inputs onto the one QUEUE_WAIT row.
    await expect(editor(page, "QUEUE_WAIT")).toHaveValue("300s");
    // Drain hides the slider and takes the badge.
    await page.getByRole("radio", { name: "Drain", exact: true }).click();
    await expect(
      page.getByRole("radio", { name: "Drain", exact: true }),
    ).toHaveAttribute("aria-checked", "true");
    await expect(slider).toHaveCount(0);
  });

  test("saved db rows win the display over file defaults", async ({ page }) => {
    const { posted } = await mockSettingsMatrix(page, {
      envText: `${MATRIX_LIVE_ENV}QUEUE_WAIT=5s\nRATE_LIMIT_PER_IP=0\n`,
      seed: [
        { key: "QUEUE_WAIT", value: "45s", source: "db" },
        { key: "RATE_LIMIT_PER_IP", value: "20", source: "db" },
      ],
    });
    await gotoControls(page, "tokens");
    await expect(editor(page, "QUEUE_WAIT")).toHaveValue("45s");
    await expect(editor(page, "RATE_LIMIT_PER_IP")).toHaveValue("20");
    await expect(
      page.getByText("saved value", { exact: true }).first(),
    ).toBeVisible();
  });

  test("env-shadowed pool row keeps its override note after save", async ({
    page,
  }) => {
    const { posted } = await mockSettingsMatrix(page, {
      seed: [{ key: "QUEUE_WAIT", value: "45s", source: "env" }],
    });
    await gotoControls(page, "tokens");
    await expect(
      page.getByText("overridden by process env", { exact: true }).first(),
    ).toBeVisible();

    await fillKey(page, "QUEUE_WAIT", "50s");
    await expectPosted(posted, "QUEUE_WAIT", "50s");
    await expect(
      page.getByRole("status").filter({ hasText: "Overridden by process env" }),
    ).toBeVisible();
  });

  test("degraded Controls tab is honest and saves nothing", async ({
    page,
  }) => {
    const { posted } = await mockSettingsMatrix(page, { degraded: true });
    await gotoControls(page, "tokens");
    await expect(
      page.getByText("DB overlay unavailable").first(),
    ).toBeVisible();
    await expect(
      page.getByText("overlay offline — per-key save unavailable").first(),
    ).toBeVisible();

    await fillKey(page, "SLOTS_PER_ACCOUNT", "3");
    // The debounced write never fires while the store is offline.
    await expect.poll(() => posted.length).toBe(0);
    await expect(page.getByRole("status")).toHaveCount(0);
  });
});
