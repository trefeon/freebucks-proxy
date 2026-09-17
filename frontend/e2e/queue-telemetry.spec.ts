import { test, expect } from "@playwright/test";
import { loadFixtures, mockDashboard } from "./mocks.js";
import { adminUrl, requestLines } from "./mock-data.js";

// Queue-wait telemetry in the Logs console:
//   (a) a request whose chat trace carries the queue_wait_ms phase renders a
//       QUEUED chip stating the queue time (not tokens, not latency), and its
//       tooltip says what the wait means;
//   (b) a request that never parked renders no QUEUED chip at all (the phase
//       is absent server-side, so the chip must not be invented);
//   (c) the ACCT tooltip states the account's live/queued numbers from the
//       tokens payload, so an operator can tell a saturated account from a
//       free one.
// All fixtures are the parity-guarded pack (e2e/fixtures); the tokens payload
// is overridden per test with the telemetry numbers.

const ACTIVITY = adminUrl("activity");

test.describe("queue-wait telemetry", () => {
  test("a parked request shows its queue time as a QUEUED chip", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f, {
      logs: {
        entries: requestLines(
          "qw-parked",
          "total_ms=1820  queue_wait_ms=850  upstream_ttfb_ms=120  token=1",
        ),
      },
    });

    await page.goto(ACTIVITY);
    await expect(page.getByText("1 model request")).toBeVisible();

    const chip = page.getByText("QUEUED 850ms");
    await expect(chip).toBeVisible();
    // The chip states queue time and must not claim tokens or latency.
    await expect(chip).toHaveAttribute("title", /spill-lane queue/);
    await expect(chip).toHaveAttribute(
      "title",
      /not tokens, not request latency/,
    );
    // The queue chip is one badge, and it is the only queue badge here.
    await expect(page.getByText(/^QUEUED /)).toHaveCount(1);
  });

  test("a request that never parked shows no QUEUED chip", async ({ page }) => {
    const f = loadFixtures();
    await mockDashboard(page, f, {
      logs: {
        entries: requestLines(
          "qw-granted",
          "total_ms=920  upstream_ttfb_ms=110  token=1",
        ),
      },
    });

    await page.goto(ACTIVITY);
    await expect(page.getByText("1 model request")).toBeVisible();
    // No phase, no chip: an immediate grant must not render a 0ms badge.
    await expect(page.getByText(/^QUEUED /)).toHaveCount(0);
  });

  test("ACCT tooltip reports the account's live turns and waiters", async ({
    page,
  }) => {
    const f = loadFixtures();
    const tokens = JSON.parse(JSON.stringify(f.tokens)) as {
      tokens: Array<Record<string, unknown>>;
    };
    // Distinct numbers per account so a wrong lookup cannot pass: the ACCT
    // chip carries the 1-based account number ("Account #1"), which is pool
    // index 0 in the payload.
    tokens.tokens[0].live_turns = 2;
    tokens.tokens[0].queued_waiters = 1;
    tokens.tokens[0].oldest_waiter_ms = 1200;
    tokens.tokens[1].live_turns = 0;
    tokens.tokens[1].queued_waiters = 0;
    tokens.tokens[1].oldest_waiter_ms = 0;
    await mockDashboard(page, f, {
      tokens,
      logs: {
        entries: requestLines(
          "qw-acct",
          "total_ms=1820  queue_wait_ms=850  token=1",
        ),
      },
    });

    await page.goto(ACTIVITY);
    await expect(page.getByText("1 model request")).toBeVisible();

    const acct = page.getByTitle(/Pool account 1 — not token usage/);
    await expect(acct).toBeVisible();
    await expect(acct).toHaveAttribute("title", /2 live turns, 1 waiting/);
    await expect(acct).toHaveAttribute("title", /oldest wait 1200ms/);
    // Still the glossary text: a pool account index, never token usage.
    await expect(acct).toHaveAttribute("title", /not token usage/);
  });
});
