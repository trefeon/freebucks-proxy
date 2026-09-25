import { test, expect } from "@playwright/test";
import { loadFixtures, mockDashboard, mockSettingsOverlay } from "./mocks.js";
import { adminUrl } from "./mock-data.js";

// Dashboard local-time coverage: account clocks remain in each account's
// upstream reset zone while browser-local conversions follow the viewer zone.
// The countdown remains anchored to the same absolute wire instant.
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
    const account = (
      index: number,
      email: string,
      country?: string,
      zone?: string,
      resetAt = RESET_AT,
      absolute = true,
      legacyReset = true,
      includeFreebucks = true,
    ) => ({
      index,
      email,
      ...(country ? { country_code: country } : {}),
      session_status: "idle",
      session_instance: "",
      session_model: "",
      session_remaining_seconds: 0,
      cooldown_active: false,
      cooldown_until: "",
      requests: 0,
      requests_per_day: 0,
      ...(includeFreebucks
        ? {
            freebucks: {
              balance: 30,
              daily: {
                limit: 20,
                spent: 5,
                remaining: 15,
                ...(absolute ? { reset_at_utc: resetAt } : {}),
                ...(legacyReset ? { reset_at: resetAt } : {}),
                ...(zone ? { reset_time_zone: zone } : {}),
              },
              wallet: { balance: 15 },
              prices: {},
            },
          }
        : {}),
      quota: [],
      has_quota: false,
    });

    return {
      mode: "pool-only",
      token_count: 7,
      has_tokens: true,
      tokens: [
        account(0, "op@example.com", "US", "America/Los_Angeles"),
        account(1, "jp@example.com", "JP", "Asia/Tokyo"),
        account(2, "unknown@example.com"),
        account(
          3,
          "display@example.com",
          "GB",
          "Europe/London",
          "15:04 Jan 2",
          false,
        ),
        account(
          4,
          "no-zone@example.com",
          "AU",
          undefined,
          "15:04 Jan 2",
          false,
        ),
        account(
          5,
          "country-only@example.com",
          "CA",
          undefined,
          undefined,
          true,
          true,
          false,
        ),
        account(
          6,
          "offsetless@example.com",
          "NZ",
          undefined,
          "2026-10-02T07:00:00",
          true,
          false,
        ),
      ],
    };
  }

  async function renderedAccountsInZone(
    browser,
    timezoneId: string,
    locale: string,
    tokenData = tokensPayload(),
  ) {
    const context = await browser.newContext({ timezoneId, locale });
    const page = await context.newPage();
    await page.clock.install();
    await page.clock.setFixedTime(FROZEN_NOW);
    const fixtures = loadFixtures();
    await mockDashboard(page, fixtures, { tokens: tokenData });
    await mockSettingsOverlay(page, []);
    await page.goto(adminUrl("tokens"));
    await page.getByRole("button", { name: "Allowances" }).click();

    const strip = page.getByTestId("reset-strip");
    const stripText =
      (await strip.count()) > 0
        ? ((await strip.textContent())?.trim() ?? "")
        : "";
    const probe = page.getByTestId("tz-probe");
    await expect(probe).toBeVisible();
    await expect(
      page.getByText(
        "Browser locale and timezone only control this local display; upstream-reported country is separate.",
      ),
    ).toBeVisible();
    const probeText = (await probe.textContent())?.trim() ?? "";
    const resetLines = page.getByTestId("account-reset-line");
    const accountTexts = await resetLines.allTextContents();
    const countryLines = page.getByTestId("account-country-line");
    await expect(countryLines).toHaveCount(tokenData.tokens.length);
    const countryTexts = await countryLines.allTextContents();
    await context.close();
    return { stripText, probeText, accountTexts, countryTexts };
  }

  test("per-account reset zones and browser-local clocks stay distinct", async ({
    browser,
  }) => {
    const jakarta = await renderedAccountsInZone(
      browser,
      "Asia/Jakarta",
      "id-ID",
    );
    const york = await renderedAccountsInZone(
      browser,
      "America/New_York",
      "en-US",
    );

    for (const view of [jakarta, york]) {
      expect(view.accountTexts).toHaveLength(5);
      expect(view.accountTexts[0]).toContain("Oct 2, 12:00 AM");
      expect(view.accountTexts[0]).toContain("America/Los_Angeles");
      expect(view.accountTexts[1]).toContain("Oct 2, 04:00 PM");
      expect(view.accountTexts[1]).toContain("Asia/Tokyo");
      expect(view.accountTexts[2]).toContain(
        `Browser-local reset time: Oct 2, ${view === jakarta ? "02:00 PM" : "03:00 AM"}`,
      );
      expect(view.accountTexts[2]).not.toContain("Browser locale region");
      expect(view.accountTexts[2]).not.toContain("America/");
      expect(view.accountTexts[2]).not.toContain("Asia/");
      expect(view.accountTexts[3]).toContain("15:04 Jan 2");
      expect(view.accountTexts[3]).toContain("Europe/London");
      expect(view.accountTexts[4]).toContain(
        "Reset time (source timezone not reported): 15:04 Jan 2",
      );
      expect(view.accountTexts[4]).not.toContain("browser time");
      expect(view.accountTexts[4]).not.toContain("Browser-local reset time");
      expect(view.accountTexts[3]).not.toContain("browser time");
      expect(
        view.accountTexts[2].match(/Oct 2, (?:02:00 PM|03:00 AM)/g) ?? [],
      ).toHaveLength(1);
      expect(view.stripText).toContain("Account #1 resets in 19h 0m");
      expect(view.stripText).not.toContain("shared for all accounts");
    }

    expect(jakarta.accountTexts[0]).toContain(
      "browser time (Asia/Jakarta): Oct 2, 02:00 PM",
    );
    expect(jakarta.accountTexts[1]).toContain(
      "browser time (Asia/Jakarta): Oct 2, 02:00 PM",
    );
    expect(york.accountTexts[0]).toContain(
      "browser time (America/New_York): Oct 2, 03:00 AM",
    );
    expect(york.accountTexts[1]).toContain(
      "browser time (America/New_York): Oct 2, 03:00 AM",
    );

    // Locale region is display metadata, never substituted for the upstream
    // country reported on either account row.
    expect(jakarta.probeText).toContain("Browser locale region: ID");
    expect(jakarta.probeText).toContain("browser timezone: Asia/Jakarta");
    expect(york.probeText).toContain("Browser locale region: US");
    expect(york.probeText).toContain("browser timezone: America/New_York");
    for (const view of [jakarta, york]) {
      expect(view.countryTexts[0]).toContain(
        "Last upstream-reported country: US",
      );
      expect(view.countryTexts[1]).toContain(
        "Last upstream-reported country: JP",
      );
      expect(view.countryTexts[2]).toContain(
        "Last upstream-reported country: not reported",
      );
      expect(view.countryTexts[3]).toContain(
        "Last upstream-reported country: GB",
      );
      expect(view.countryTexts[4]).toContain(
        "Last upstream-reported country: AU",
      );
      expect(view.countryTexts[5]).toContain(
        "Last upstream-reported country: CA",
      );
      expect(view.countryTexts[6]).toContain(
        "Last upstream-reported country: NZ",
      );
    }
  });
  test("display-only reset clocks stay in their source zone", async ({
    browser,
  }) => {
    const payload = tokensPayload();
    const displayOnly = {
      ...payload,
      token_count: 1,
      tokens: [payload.tokens[3]],
    };
    const displayOnlyView = await renderedAccountsInZone(
      browser,
      "Asia/Jakarta",
      "id-ID",
      displayOnly,
    );
    expect(displayOnlyView.stripText).toBe("");
    expect(displayOnlyView.accountTexts).toHaveLength(1);
    expect(displayOnlyView.accountTexts[0]).toContain("15:04 Jan 2");
    expect(displayOnlyView.accountTexts[0]).toContain("Europe/London");
    expect(displayOnlyView.accountTexts[0]).not.toContain("browser time");

    const noZone = {
      ...payload,
      token_count: 1,
      tokens: [payload.tokens[4]],
    };
    const noZoneView = await renderedAccountsInZone(
      browser,
      "Asia/Jakarta",
      "id-ID",
      noZone,
    );
    expect(noZoneView.accountTexts).toHaveLength(1);
    expect(noZoneView.stripText).toBe("");
    expect(noZoneView.accountTexts[0]).toContain(
      "Reset time (source timezone not reported): 15:04 Jan 2",
    );
    expect(noZoneView.accountTexts[0]).not.toContain("browser time");
    expect(noZoneView.accountTexts[0]).not.toContain(
      "Browser-local reset time",
    );
    const offsetless = {
      ...payload,
      token_count: 1,
      tokens: [payload.tokens[6]],
    };
    const offsetlessView = await renderedAccountsInZone(
      browser,
      "Asia/Jakarta",
      "id-ID",
      offsetless,
    );
    expect(offsetlessView.accountTexts).toHaveLength(0);
    expect(offsetlessView.stripText).toBe("");
    expect(offsetlessView.countryTexts[0]).toContain(
      "Last upstream-reported country: NZ",
    );
  });
});
