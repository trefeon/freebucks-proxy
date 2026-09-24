import { test, expect } from "@playwright/test";
import { loadFixtures, mockDashboard, mockSettingsOverlay } from "./mocks.js";
import { adminUrl } from "./mock-data.js";

// Dashboard local-time coverage: one fixed server instant must render as
// different wall clocks per viewer zone while the countdown stays anchored
// to the same server instant. The reset strip ticks from the wire instant
// (never from a rendered string), and the tz probe names the browser zone
// so skew debugs to the viewer, not the server.
test.describe("dashboard local time (mock backend)", () => {
  test.use({ expect: { timeout: 10_000 } });

  // 2026-10-02T07:00:00Z: midnight PDT on the server, 03:00 AM in New York,
  // 02:00 PM in Jakarta — same instant, three wall clocks, two calendar
  // days apart from UTC's perspective but one shared countdown.
  const RESET_AT = "2026-10-02T07:00:00Z";
  // Frozen page clock: 19h00m before the reset, so the strip countdown is
  // exactly "19h 0m" in every zone.
  const FROZEN_NOW = Date.UTC(2026, 9, 1, 12, 0, 0);

  function tokensPayload() {
    return {
      mode: "pool-only",
      token_count: 1,
      has_tokens: true,
      tokens: [
        {
          index: 0,
          email: "op@example.com",
          session_status: "idle",
          session_instance: "",
          session_model: "",
          session_remaining_seconds: 0,
          cooldown_active: false,
          cooldown_until: "",
          requests: 0,
          requests_per_day: 0,
          freebucks: {
            balance: 30,
            daily: {
              limit: 20,
              spent: 5,
              remaining: 15,
              reset_at: RESET_AT,
              reset_at_utc: RESET_AT,
            },
            wallet: { balance: 15 },
            prices: {},
          },
          quota: [],
          has_quota: false,
        },
      ],
    };
  }

  async function stripInZone(browser, timezoneId: string) {
    const context = await browser.newContext({
      timezoneId,
      locale: "en-US",
    });
    const page = await context.newPage();
    // Freeze the page clock before the SPA mounts: the strip countdown then
    // reads exactly the server instant minus this time, in every zone.
    await page.clock.install();
    await page.clock.setFixedTime(FROZEN_NOW);
    const fixtures = loadFixtures();
    await mockDashboard(page, fixtures, { tokens: tokensPayload() });
    await mockSettingsOverlay(page, []);
    await page.goto(adminUrl("tokens"));
    await page.getByRole("button", { name: "Allowances" }).click();
    const strip = page.getByTestId("reset-strip");
    await expect(strip).toBeVisible();
    const stripText = (await strip.textContent())?.trim() ?? "";
    const probe = page.getByTestId("tz-probe");
    await expect(probe).toBeVisible();
    const probeText = (await probe.textContent())?.trim() ?? "";
    await context.close();
    return { stripText, probeText };
  }

  test("fixed server instant renders per-zone clocks with one countdown", async ({
    browser,
  }) => {
    const jakarta = await stripInZone(browser, "Asia/Jakarta");
    const york = await stripInZone(browser, "America/New_York");

    // Same instant, different viewer wall clocks (en-US clock shape pinned
    // by the formatter, so the hours are deterministic).
    expect(jakarta.stripText).toContain("Oct 2, 02:00 PM");
    expect(york.stripText).toContain("Oct 2, 03:00 AM");
    expect(jakarta.stripText).not.toEqual(york.stripText);

    // The raw server stamp is never printed: clocks are localized, never
    // echoed.
    expect(jakarta.stripText).not.toContain(RESET_AT);
    expect(york.stripText).not.toContain(RESET_AT);

    // Identical countdown target in both zones: 19h00m to the same instant.
    expect(jakarta.stripText).toContain("19h 0m");
    expect(york.stripText).toContain("19h 0m");

    // Skew probe names the browser zone behind each rendering.
    expect(jakarta.probeText).toContain("Asia/Jakarta");
    expect(york.probeText).toContain("America/New_York");
  });
});
