import { test, expect } from "@playwright/test";
import type { Page } from "@playwright/test";
import { loadFixtures, mockDashboard, mockSettingsOverlay } from "./mocks.js";
import type {
  Fixtures,
  MockOverrides,
  OverlaySeed,
  PostedSetting,
} from "./mocks.js";
import { tokenRow, tokensPayload } from "./mock-data.js";

// Strategy transparency (probe-once lane): the Pool Strategy card explains
// itself — "How this runs" derives the live decision chain from the five
// owned keys, "Next account" predicts the head of the spill walk from the
// tokens snapshot and never guesses. One factory builds the fake backend
// (mock-data.ts), one route layer serves it (mocks.ts). Web-first
// assertions only: role/testid locators, no sleeps.
async function gotoStrategy(
  page: Page,
  fixtures: Fixtures,
  overrides: MockOverrides = {},
  seed: OverlaySeed[] = [],
) {
  await mockDashboard(page, fixtures, overrides);
  const posted: PostedSetting[] = [];
  await mockSettingsOverlay(page, posted, seed.length ? { seed } : {});
  await page.goto("http://127.0.0.1:4173/admin/#tokens");
  // The settings store fetches the catalog on mount; a second navigation
  // (same helper, new mocks) refetches it — wait for the response the
  // navigation just triggered rather than racing it.
  await page
    .waitForResponse(
      (r) => r.url().includes("/admin/api/config/meta") && r.status() === 200,
      { timeout: 5000 },
    )
    .catch(() => {});
  await page.getByRole("button", { name: "Strategy" }).click();
  return { posted };
}

test.describe("strategy transparency (mock backend)", () => {
  test("how-runs names the active preset numbers", async ({ page }) => {
    const f = loadFixtures();
    await gotoStrategy(page, f);
    const block = page.getByTestId("strategy-how-runs");
    await expect(block).toBeVisible();
    // Stock catalog defaults read as Balance (30s / 16): the posture line
    // names the preset and its numbers, not the preset file text.
    await expect(block).toContainText("Balance runs 3 slots per lane");
    await expect(block).toContainText(
      "parks 30s on a full lane (16 waiters), then spills",
    );
    // The five walk steps: index order → slot take → park → spill → tail,
    // plus the pin rule (nothing pinned in the stock fixtures).
    for (const step of [
      "Walk accounts #1",
      "Take a free slot",
      "A full lane parks",
      "Spill down the full index chain",
      "Tail:",
      "No pins",
    ]) {
      await expect(block).toContainText(step);
    }
  });

  test("next account names the first healthy lane", async ({ page }) => {
    const f = loadFixtures();
    await gotoStrategy(page, f);
    const next = page.getByTestId("strategy-next-account");
    await expect(next).toBeVisible();
    // Fixture lane #1 is active with no cooldown, lock, ban, or pin.
    await expect(next).toContainText(
      "Account #1 (dev@example.com) — warm, live session.",
    );
  });

  test("next account skips parked lanes", async ({ page }) => {
    const f = loadFixtures();
    const rows = [
      tokenRow(0, { locked: true }),
      tokenRow(1, {
        cooldown_active: true,
        cooldown_until: "2026-09-17T10:00:00Z",
      }),
      tokenRow(2, { session_status: "active" }),
    ];
    await gotoStrategy(page, f, { tokens: tokensPayload(rows) });
    const next = page.getByTestId("strategy-next-account");
    await expect(next).toContainText("Account #3");
    await expect(next).toContainText("warm, live session");
  });

  test("all parked lanes name no lane", async ({ page }) => {
    const f = loadFixtures();
    const parked = [
      tokenRow(0, { locked: true }),
      tokenRow(1, { ban_type: "hard" }),
    ];
    await gotoStrategy(page, f, { tokens: tokensPayload(parked) });
    const nextParked = page.getByTestId("strategy-next-account");
    await expect(nextParked).toContainText("Every lane is parked");
    await expect(nextParked).not.toContainText("Account #");
  });

  test("unanimous pin predicts its lane", async ({ page }) => {
    const f = loadFixtures();
    const rows = [
      tokenRow(0, { pinned_model: "deepseek/deepseek-v4-flash" }),
      tokenRow(1),
    ];
    await gotoStrategy(page, f, { tokens: tokensPayload(rows) }, [
      { key: "PIN_MODEL", value: "0:deepseek/deepseek-v4-flash", source: "db" },
    ]);
    const next = page.getByTestId("strategy-next-account");
    await expect(next).toContainText("Account #1");
    await expect(next).toContainText(
      "head of the lane for deepseek/deepseek-v4-flash",
    );
  });

  test("split pins name no lane (never guess)", async ({ page }) => {
    const f = loadFixtures();
    const split = [
      tokenRow(0, { pinned_model: "deepseek/deepseek-v4-flash" }),
      tokenRow(1, { pinned_model: "z-ai/glm-5.2" }),
    ];
    await gotoStrategy(page, f, { tokens: tokensPayload(split) }, [
      {
        key: "PIN_MODEL",
        value: "0:deepseek/deepseek-v4-flash;1:z-ai/glm-5.2",
        source: "db",
      },
    ]);
    const nextSplit = page.getByTestId("strategy-next-account");
    await expect(nextSplit).toContainText("no single lane is named");
    await expect(nextSplit).not.toContainText("Account #");
  });
});
