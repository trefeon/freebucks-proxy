import { test, expect } from "@playwright/test";
import {
  loadFixtures,
  mockDashboard,
  mockSettingsOverlay,
  type PostedSetting,
} from "./mocks.js";

// The Settings page is a read-only view of the effective configuration: it
// renders no whole-file .env writer (the break-glass textarea that rewrote
// the file through POST /admin/config is gone — the Client API Keys editor
// keeps that endpoint) and it never prints a Secret:true value. Keys the
// catalog hides from the cards (environment-only / restart-only /
// deprecated) stay addressable in the "Hidden keys" disclosure, so "hidden"
// can never mean "stranded and invisible" again.

const SETTINGS = "http://127.0.0.1:4173/admin/#settings";
const TOKENS = "http://127.0.0.1:4173/admin/#tokens";

// Planted secret values. The page's payload legitimately carries secret
// material: GET /admin/api/config returns the raw .env document in
// env_content, and GET /admin/api/settings echoes the saved literal of a
// db-tier row. Both are asserted below as non-vacuous inputs, so the
// absence checks prove the DOM stayed clean, not that the fixture was.
const SECRET_SENTINELS: Record<string, string> = {
  AUTH_TOKENS: "SENTINEL-AUTH-TOKENS-9f13",
  API_KEYS: "SENTINEL-API-KEYS-9f13",
  ADMIN_TOKEN: "SENTINEL-ADMIN-TOKEN-9f13",
  WEBHOOK_URL: "https://hooks.invalid/SENTINEL-WEBHOOK-9f13",
};

// Hidden catalog keys that DO have a dedicated editor somewhere else in the
// dashboard (Gateway card / Pool Controls tab). They must keep that editor
// and must not be duplicated into the hidden-keys disclosure.
const OWNED_ELSEWHERE = [
  "HTTP_READ_TIMEOUT",
  "TOKEN_ROTATION",
  "SESSION_RE_ADMIT_LEAD",
  "WAITING_ROOM_CHAIN",
];

type CatalogEntry = {
  key: string;
  hidden?: boolean;
  secret?: boolean;
};

function withSecretSentinels(base: unknown) {
  const fixture = base as {
    env_content?: string;
    effective?: Array<{ key: string; value: string; secret: boolean }>;
  };
  return {
    ...fixture,
    env_content:
      Object.entries(SECRET_SENTINELS)
        .map(([key, value]) => `${key}=${value}`)
        .join("\n") + "\n",
    effective: [
      ...(fixture.effective ?? []),
      ...Object.entries(SECRET_SENTINELS).map(([key, value]) => ({
        key,
        value,
        secret: true,
      })),
    ],
  };
}

test.describe("settings hidden keys", () => {
  test("never renders a Secret:true catalog value", async ({ page }) => {
    const fixtures = loadFixtures();
    const config = withSecretSentinels(fixtures.config);
    await mockDashboard(page, fixtures, { config });
    // Hostile overlay: a db-tier row for every secret key, echoing the raw
    // literal (exactly what a saved-value GET answers).
    await mockSettingsOverlay(page, [], {
      seed: Object.entries(SECRET_SENTINELS).map(([key, value]) => ({
        key,
        value,
        source: "db",
        secret: true,
      })),
    });

    await page.goto(SETTINGS);
    await expect(
      page.getByRole("heading", { name: "Settings", exact: true }),
    ).toBeVisible();

    // Non-vacuity: the payload the page consumed carried every sentinel.
    for (const [key, value] of Object.entries(SECRET_SENTINELS)) {
      expect(config.env_content, `${key} missing from payload`).toContain(
        value,
      );
    }

    // The hidden-keys disclosure is opened too: nothing it (or a closed
    // <details>) holds may carry a secret value.
    await page.getByTestId("hidden-keys-toggle").click();
    const html = await page.locator("#app").innerHTML();
    for (const [key, value] of Object.entries(SECRET_SENTINELS)) {
      expect(html, `${key} value reached the DOM`).not.toContain(value);
    }
    // Secret keys are never addressable rows of the disclosure.
    for (const key of Object.keys(SECRET_SENTINELS)) {
      await expect(page.locator(`[data-setting-key="${key}"]`)).toHaveCount(0);
    }
  });

  test("every hidden non-secret catalog key is reachable, none is stranded", async ({
    page,
  }) => {
    const fixtures = loadFixtures();
    await mockDashboard(page, fixtures);
    await mockSettingsOverlay(page, []);

    await page.goto(SETTINGS);
    await expect(
      page.getByRole("heading", { name: "Settings", exact: true }),
    ).toBeVisible();

    const catalog = fixtures.configMeta as CatalogEntry[];
    const hiddenNonSecret = catalog
      .filter((e) => e.hidden && !e.secret)
      .map((e) => e.key);
    const inSection = hiddenNonSecret.filter(
      (k) => !OWNED_ELSEWHERE.includes(k),
    );

    await page.getByTestId("hidden-keys-toggle").click();
    await expect(page.getByTestId("hidden-keys-count")).toHaveText(
      String(inSection.length),
    );
    for (const key of inSection) {
      await expect(
        page.locator(`[data-setting-key="${key}"]`),
        `${key} is stranded: no row in the hidden-keys section`,
      ).toHaveCount(1);
    }
    // The section is exactly the un-edited remainder: one row per key, and
    // the four exceptions stay with their own editors.
    await expect(page.locator("[data-setting-key]")).toHaveCount(
      inSection.length,
    );
    for (const owned of OWNED_ELSEWHERE) {
      await expect(page.locator(`[data-setting-key="${owned}"]`)).toHaveCount(
        0,
      );
    }
    await expect(
      page.getByRole("combobox", { name: "HTTP_READ_TIMEOUT" }),
    ).toBeVisible();

    await page.goto(TOKENS);
    await page.getByRole("button", { name: "Controls" }).click();
    await expect(page.locator("#setting-SESSION_RE_ADMIT_LEAD")).toBeVisible();
    await expect(page.locator("#setting-WAITING_ROOM_CHAIN")).toBeVisible();
    await expect(
      page.getByRole("radiogroup", { name: "Token Rotation Policy" }),
    ).toBeVisible();
  });

  test("the break-glass raw .env editor is gone from Settings", async ({
    page,
  }) => {
    await mockDashboard(page, loadFixtures());
    await mockSettingsOverlay(page, [], { degraded: true });

    await page.goto(SETTINGS);
    await expect(
      page.getByRole("heading", { name: "Settings", exact: true }),
    ).toBeVisible();
    // The degraded banner still names the offline store, but no longer
    // promises an editor that would have written the file around it.
    await expect(page.getByText("DB overlay unavailable")).toBeVisible();
    await expect(page.getByText(/emergency/i)).toHaveCount(0);

    await expect(page.locator("#emergency-raw-env")).toHaveCount(0);
    await expect(
      page.getByRole("button", { name: "Save raw .env" }),
    ).toHaveCount(0);
    // No whole-file writer control survives anywhere on the page.
    await expect(page.locator("#app textarea")).toHaveCount(0);
  });

  test("db-tier rows offer Reset, env-tier rows stay read-only", async ({
    page,
  }) => {
    const posted: PostedSetting[] = [];
    await mockDashboard(page, loadFixtures());
    const overlay = await mockSettingsOverlay(page, posted, {
      seed: [
        { key: "LISTEN_ADDR", value: "0.0.0.0:9999", source: "db" },
        { key: "COST_MODE", value: "sentinel-env-tier-loses", source: "env" },
      ],
    });

    await page.goto(SETTINGS);
    await expect(
      page.getByRole("heading", { name: "Settings", exact: true }),
    ).toBeVisible();
    await page.getByTestId("hidden-keys-toggle").click();

    // Saved (db) row: effective value + saved marker + working Reset.
    const listenRow = page.locator('[data-setting-key="LISTEN_ADDR"]');
    await expect(listenRow).toContainText("0.0.0.0:9999");
    await expect(listenRow).toContainText("saved value");
    await listenRow.getByRole("button", { name: "Reset" }).click();
    await expect.poll(() => overlay.deleted).toEqual(["LISTEN_ADDR"]);

    // env row: the process environment wins, so the row shows the payload's
    // effective value — a literal that only exists on the env tier never
    // paints into the row — states the override, and offers no Reset (there
    // is no saved value to remove).
    const costRow = page.locator('[data-setting-key="COST_MODE"]');
    await expect(costRow).toContainText("free");
    await expect(costRow).toContainText("overridden by process env");
    await expect(costRow).not.toContainText("sentinel-env-tier-loses");
    await expect(costRow.getByRole("button", { name: "Reset" })).toHaveCount(0);

    // default-tier row: the catalog default is the effective value.
    await expect(
      page.locator('[data-setting-key="REQUEST_TIMEOUT"]'),
    ).toContainText("15m");
    // The disclosure is a reader, never a writer.
    expect(posted).toHaveLength(0);
  });
});
