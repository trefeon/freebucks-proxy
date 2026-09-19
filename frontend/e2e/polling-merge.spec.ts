import { test, expect } from "@playwright/test";
import { loadFixtures, mockDashboard, mockSettingsOverlay } from "./mocks.js";
import { adminUrl } from "./mock-data.js";

// Polling + merge behavior against the hermetic mock backend
// (mockDashboard + mockSettingsOverlay): one route layer, inline page.route
// mocks only — this spec never touches fixtures/ or sibling specs.
// Web-first assertions: role/testid locators, fake timers for every tick,
// no sleeps.

test.describe("dashboard polling + merge (mock backend)", () => {
  test.use({ expect: { timeout: 10_000 } });

  test("session countdown ticks locally without firing a poll", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    await mockSettingsOverlay(page, []);
    // Fake timers from boot: the 1s card tick and the 10s poll share the
    // clock, so advancing 5s must move the countdown but not the poll.
    await page.clock.install();
    let tokenCalls = 0;
    page.on("request", (r) => {
      if (r.url().includes("/admin/api/tokens")) tokenCalls += 1;
    });
    await page.goto(adminUrl("tokens"));
    await expect(
      page.getByRole("heading", { name: "Pool", exact: true }),
    ).toBeVisible();

    // Fixture token 0 carries session_remaining_seconds 4620 (1h 17m).
    const countdown = page.getByLabel(/Session time remaining/).first();
    await expect(countdown).toHaveText("1h 17m 0s remaining");
    const callsAtMount = tokenCalls;

    await page.clock.fastForward(5000);
    // Fake timers + real rAF settle race: the countdown must have MOVED
    // backward from mount (local tick works) without firing a poll — pin the
    // direction, not the exact second.
    await expect(countdown).not.toHaveText("1h 17m 0s remaining");
    expect(tokenCalls).toBe(callsAtMount);
  });

  test("page-state unload flush sends the pending snapshot as PUT", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    await mockSettingsOverlay(page, []);
    // Fake timers from boot: the ~1s savePageState debounce stays pending
    // so the unload flush below has a snapshot to send, deterministically.
    await page.clock.install();
    const puts: Array<{ url: string; body: string }> = [];
    await page.route("**/admin/api/pages/*", async (route) => {
      if (route.request().method() === "PUT") {
        puts.push({
          url: route.request().url(),
          body: route.request().postData() ?? "",
        });
      }
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ data: {} }),
      });
    });
    await page.goto(adminUrl("tokens"));
    await expect(
      page.getByRole("heading", { name: "Pool", exact: true }),
    ).toBeVisible();

    // pagehide is the event pageState.js arms its keepalive flush on.
    await page.evaluate(() => window.dispatchEvent(new Event("pagehide")));
    await expect
      .poll(() => puts.length, { timeout: 10_000 })
      .toBeGreaterThan(0);
    expect(puts.some((p) => p.url.includes("/admin/api/pages/"))).toBe(true);
    expect(puts[0].body).toContain('"data"');
  });

  test("device-login wizard completes on the fake-timer poll", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    await mockSettingsOverlay(page, []);
    const loginUrl = "https://freebuff.app/device/login?code=fp-poll-merge";
    await page.route("**/admin/login/start", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          fingerprint: "fp-poll-merge",
          login_url: loginUrl,
        }),
      });
    });
    // The wizard polls every 3s; answer completed so one tick finishes it.
    await page.route("**/admin/login/status**", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ status: "completed", token_index: 5 }),
      });
    });
    await page.goto(adminUrl("tokens"));
    await expect(
      page.getByRole("heading", { name: "Pool", exact: true }),
    ).toBeVisible();

    await page.clock.install();
    const startReq = page.waitForRequest(
      (r) => r.method() === "POST" && r.url().includes("/admin/login/start"),
    );
    await page.getByRole("button", { name: "Device Login" }).click();
    await startReq;
    await expect(
      page.getByText("Open this URL in your browser to sign in:"),
    ).toBeVisible();

    await page.clock.fastForward(3100);
    await expect(page.getByText("added to pool")).toBeVisible();
  });

  test("poll keeps panels rendering after the SSE stream disconnects", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    await mockSettingsOverlay(page, []);
    // The SSE push dies; the tokens poll must carry the page on its own.
    await page.route("**/admin/api/events", (route) => route.abort());
    await page.clock.install();
    await page.goto(adminUrl("tokens"));
    await expect(
      page.getByRole("heading", { name: "Pool", exact: true }),
    ).toBeVisible();

    // Fixture pool: 3 active leases (tokens 0, 1, 4).
    const activeCount = page.locator('dl[aria-label="Pool summary"] dd').nth(1);
    await expect(activeCount).toHaveText("3");

    // Next poll returns token 4 idle: the merged render must follow.
    const tokensDoc = JSON.parse(JSON.stringify(f.tokens)) as {
      tokens: Array<Record<string, unknown>>;
    };
    tokensDoc.tokens[4].session_status = "idle";
    await page.unroute("**/admin/api/tokens*");
    await page.route("**/admin/api/tokens*", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(tokensDoc),
      });
    });
    await page.clock.fastForward(10_000);
    await expect(activeCount).toHaveText("2");
  });
});
