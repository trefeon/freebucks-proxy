import { test, expect } from "@playwright/test";
import { gotoPoolTokens, mockPool } from "./mock-pool.js";
import { tokenRow } from "./mock-data.js";

// Country region view: per-account upstream country badge (US vs non-US),
// the fleet non-US warning banner with per-reason layer guidance, and the
// blocked-reason breakdown with cooldown state in the banner + drawer.
//
// Row-data contract: pool.TokenSnapshot carries CountryCode /
// CountryBlockReason (backend/internal/pool/snapshot.go); the dashboard
// serves them as the live keys `country_code` / `country_block_reason`
// (omitempty — older servers omit both and the UI degrades to no badge, no
// banner). The server resolves country from egress IP only, so the UI names
// the layer to fix (egress path vs the account floor cleared in the human
// web verify flow at /account?tab=country, text only — never automated).
// Web-first assertions only, role/label locators, no sleeps.
test.describe("country region view (mock roster)", () => {
  test.use({ expect: { timeout: 10_000 } });

  const cooldownUntil = new Date(Date.now() + 12 * 60_000).toISOString();

  async function mockCountryRoster(page) {
    return mockPool(page, {
      rows: [
        tokenRow(0, { country_code: "US" }),
        tokenRow(1, { country_code: "DE" }),
        tokenRow(2, {
          country_code: "NL",
          country_block_reason: "recent_limited_country",
          cooldown_active: true,
          cooldown_until: cooldownUntil,
          cooldown_kind: "country_blocked",
        }),
        tokenRow(3, {
          country_code: "SG",
          country_block_reason: "anonymous_network",
        }),
      ],
    });
  }

  test("badges: US and non-US render per account; unknown renders none", async ({
    page,
  }) => {
    await mockCountryRoster(page);
    const table = await gotoPoolTokens(page);
    await expect(table.getByTestId("country-badge-0")).toHaveText("US");
    await expect(table.getByTestId("country-badge-1")).toHaveText("DE");
    await expect(table.getByTestId("country-badge-2")).toHaveText("NL");
    // Account #4's badge carries the blocked country too.
    await expect(table.getByTestId("country-badge-3")).toHaveText("SG");
  });

  test("banner: non-US warning with blocked breakdown, layer, and cooldown", async ({
    page,
  }) => {
    await mockCountryRoster(page);
    await gotoPoolTokens(page);
    const banner = page.getByTestId("country-banner");
    await expect(banner).toBeVisible();
    await expect(banner).toContainText("3 account(s) read as non-US upstream");
    await expect(banner).toContainText("2 country-blocked");

    // Account floor: reason + cooldown clock + human web flow as text.
    const floor = banner.getByTestId("country-blocked-2");
    await expect(floor).toContainText("recent_limited_country");
    await expect(floor).toContainText("parked until");
    await expect(floor).toContainText("country_blocked");
    await expect(floor).toContainText("/account?tab=country");

    // Egress layer: anonymized egress names the exit path, not the account.
    const egress = banner.getByTestId("country-blocked-3");
    await expect(egress).toContainText("anonymous_network");
    await expect(egress).toContainText("Egress path");

    // Non-US without a block gets the re-admit line, not a reason row.
    const plain = banner.getByTestId("country-nonus-1");
    await expect(plain).toContainText("DE");
    await expect(plain).toContainText("Re-admit over clean US egress");
  });

  test("drawer: expanded row shows country, reason, and layer advice", async ({
    page,
  }) => {
    await mockCountryRoster(page);
    const table = await gotoPoolTokens(page);
    const blocked = table
      .locator("tbody tr")
      .filter({ hasText: "acct2@example.com" });
    await blocked.locator('button[aria-label*="Expand details"]').click();
    const detail = table.getByTestId("country-detail");
    await expect(detail).toContainText("Upstream country");
    await expect(detail).toContainText("NL");
    await expect(detail).toContainText("recent_limited_country");
    await expect(detail).toContainText("/account?tab=country");
  });

  test("all-US fleet: no banner, US badges only", async ({ page }) => {
    await mockPool(page, {
      rows: [tokenRow(0, { country_code: "US" }), tokenRow(1)],
    });
    const table = await gotoPoolTokens(page);
    await expect(table.getByTestId("country-badge-0")).toHaveText("US");
    // Unknown country (old server shape): no badge rendered at all.
    await expect(table.getByTestId("country-badge-1")).toHaveCount(0);
    await expect(page.getByTestId("country-banner")).toHaveCount(0);
  });
});
