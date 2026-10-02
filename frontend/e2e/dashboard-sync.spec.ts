import { test, expect } from "@playwright/test";
import { loadFixtures, mockDashboard, mockSettingsOverlay } from "./mocks.js";
import { adminUrl, tokenRow, tokensPayload } from "./mock-data.js";

// Dashboard↔backend sync (mock backend): quarantine chips, probe-fill push,
// stale-flight drops, swap-stable merges, never-probed affordances.
// Web-first assertions, no sleeps — every wait is event- or
// response-driven.

function sseBody(doc: unknown): string {
  return `event: tokens\ndata: ${JSON.stringify(doc)}\n\n`;
}

test.describe("dashboard sync (mock backend)", () => {
  test.use({ expect: { timeout: 10_000 } });

  test("quarantine chip renders from the card field, not session_status", async ({
    page,
  }) => {
    const f = loadFixtures();
    // No ban_type: ban claims priority over quarantine, so the pure
    // quarantine marker (the incident's idle banned account) is isolated
    // here. session_status stays idle — the backend never emits
    // "quarantined" — yet the chip must render from the card field.
    const tokens = [
      tokenRow(0, {
        email: "dead@example.com",
        account_id: "acc-dead",
        session_status: "idle",
        quarantined: true,
        quarantine_reason: "banned",
      }),
    ];
    await mockDashboard(page, f, { tokens: tokensPayload(tokens) });
    await mockSettingsOverlay(page, []);
    await page.goto(adminUrl("tokens"));
    await expect(
      page.getByRole("heading", { name: "Accounts", exact: true }),
    ).toBeVisible();
    // The pool's terminal marker rides its own fields; the backend never
    // emits session_status "quarantined", yet the chip must render.
    await expect(page.getByText("quarantined (banned)").first()).toBeVisible();
    await expect(page.getByText("dead@example.com").first()).toBeVisible();
  });

  test("probe-fill arrives via SSE hash push with no manual refetch", async ({
    page,
  }) => {
    const f = loadFixtures();
    const dark = tokenRow(0, {
      email: "dark@example.com",
      account_id: "acc-dark",
      has_quota: false,
      quota_probed: false,
    });
    delete dark.freebucks;
    const filled = {
      ...structuredClone(dark),
      has_quota: true,
      quota_probed: true,
      session_model: "model-b2",
      freebucks: {
        balance: 15,
        daily: {
          limit: 75,
          spent: 45,
          remaining: 30,
          reset_at_utc: "2026-09-24T07:00:00Z",
          percent_used: 60,
        },
        wallet: { balance: 20, monthly_bonus: 0 },
      },
    };
    const fullDoc = tokensPayload([dark]);
    const filledDoc = tokensPayload([filled]);
    await mockDashboard(page, f, { tokens: fullDoc });
    await mockSettingsOverlay(page, []);
    await page.route("**/admin/api/tokens*", async (route) => {
      const url = route.request().url();
      if (url.includes("view=live")) {
        // Stale live poll: requested before the push lands, delayed past
        // it, carrying an older balance (5 vs the pushed 15). Faithful
        // live shape — the real server always carries freebucks when
        // known — so applying it would visibly regress the header.
        const { promise, resolve } = Promise.withResolvers<void>();
        setTimeout(resolve, 800);
        await promise;
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify(
            tokensPayload([
              {
                index: 0,
                account_id: "acc-dark",
                session_status: "idle",
                has_quota: true,
                quota_probed: true,
                freebucks: {
                  balance: 5,
                  daily: {
                    limit: 75,
                    spent: 70,
                    remaining: 5,
                    reset_at_utc: "2026-09-24T07:00:00Z",
                    percent_used: 93,
                  },
                  wallet: { balance: 5, monthly_bonus: 0 },
                },
              },
            ]),
          ),
        });
      } else {
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify(fullDoc),
        });
      }
    });
    await page.route("**/admin/api/events", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "text/event-stream",
        body: sseBody(filledDoc),
      });
    });

    let tokenCalls = 0;
    page.on("request", (r) => {
      if (r.url().includes("/admin/api/tokens")) tokenCalls += 1;
    });
    // Fake timers from boot: the 10s live poll fires on fastForward while
    // the route-handler delay above stays real (node-side), so the push
    // (mount-time) is strictly newer than the in-flight live poll.
    await page.clock.install();
    await page.goto(adminUrl("tokens"));
    await page.getByRole("button", { name: "Allowances" }).click();

    // The fill lands via push: balance renders with only the mount fetch.
    await expect(page.getByTestId("freebucks-header").first()).toContainText(
      "15",
    );
    expect(tokenCalls).toBe(1);

    // Fire the live poll; its delayed stale response must not regress the
    // push (seq guard drops it): the header keeps the pushed balance.
    await page.clock.fastForward(10_000);
    await page.waitForResponse((r) =>
      r.url().includes("/admin/api/tokens?view=live"),
    );
    await expect(page.getByTestId("freebucks-header").first()).toContainText(
      "15",
    );
  });

  test("token-swap keeps email-tracked rows on live merge", async ({
    page,
  }) => {
    const f = loadFixtures();
    const fullDoc = tokensPayload([
      tokenRow(0, {
        email: "a@example.com",
        account_id: "acc-a",
        session_model: "",
      }),
      tokenRow(1, {
        email: "b@example.com",
        account_id: "acc-b",
        session_model: "",
      }),
    ]);
    // Post-swap roster order: account B sits at index 0, A at index 1.
    const liveDoc = tokensPayload([
      { index: 0, account_id: "acc-b", session_model: "model-b" },
      { index: 1, account_id: "acc-a", session_model: "model-a" },
    ]);
    await mockDashboard(page, f, { tokens: fullDoc });
    await mockSettingsOverlay(page, []);
    await page.route("**/admin/api/tokens*", async (route) => {
      const live = route.request().url().includes("view=live");
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(live ? liveDoc : fullDoc),
      });
    });
    await page.clock.install();
    await page.goto(adminUrl("tokens"));
    await expect(
      page.getByRole("heading", { name: "Accounts", exact: true }),
    ).toBeVisible();
    await page.clock.fastForward(10_000);
    // Static emails must ride onto the matching live rows: a@x pairs with
    // model-a even though the account now sits at index 1.
    const rowA = page.locator("tr", { hasText: "a@example.com" });
    await expect(rowA).toContainText("model-a");
    const rowB = page.locator("tr", { hasText: "b@example.com" });
    await expect(rowB).toContainText("model-b");
  });

  test("never-probed copy shows a probe affordance; probe fills the row", async ({
    page,
  }) => {
    const f = loadFixtures();
    const dark = tokenRow(0, {
      email: "dark@example.com",
      account_id: "acc-dark",
      has_quota: false,
      quota_probed: false,
    });
    delete dark.freebucks;
    const state = { doc: tokensPayload([dark]) };
    await mockDashboard(page, f, { tokens: state.doc });
    await mockSettingsOverlay(page, []);
    // Per-account probe fills the row on the next fetch.
    await page.route("**/admin/tokens/0/test", async (route) => {
      state.doc = tokensPayload([
        {
          ...structuredClone(dark),
          has_quota: true,
          quota_probed: true,
          freebucks: {
            balance: 9,
            daily: {
              limit: 75,
              spent: 10,
              remaining: 65,
              reset_at_utc: "2026-09-24T07:00:00Z",
              percent_used: 13,
            },
            wallet: { balance: 9, monthly_bonus: 0 },
          },
        },
      ]);
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ ok: true, message: "Account #1 probed" }),
      });
    });
    await page.route("**/admin/api/tokens*", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(state.doc),
      });
    });
    await page.goto(adminUrl("tokens"));
    await page.getByRole("button", { name: "Allowances" }).click();
    await expect(
      page.getByText("No Freebucks data yet — probe this account to populate."),
    ).toBeVisible();
    await page.getByRole("button", { name: "Probe", exact: true }).click();
    await expect(page.getByTestId("freebucks-header").first()).toContainText(
      "9",
    );
  });

  test("probed-empty copy states the probe found nothing", async ({ page }) => {
    const f = loadFixtures();
    const empty = tokenRow(0, {
      email: "empty@example.com",
      account_id: "acc-empty",
      has_quota: false,
      quota_probed: true,
    });
    delete empty.freebucks;
    await mockDashboard(page, f, {
      tokens: tokensPayload([empty]),
    });
    await mockSettingsOverlay(page, []);
    await page.goto(adminUrl("tokens"));
    await page.getByRole("button", { name: "Allowances" }).click();
    await expect(
      page.getByText("No Freebucks data — the last probe returned none."),
    ).toBeVisible();
    await expect(
      page.getByRole("button", { name: "Probe", exact: true }),
    ).toHaveCount(0);
  });
});
