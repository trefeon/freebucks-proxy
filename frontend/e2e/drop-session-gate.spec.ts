import { test, expect } from "@playwright/test";
import type { Page } from "@playwright/test";
import { loadFixtures, mockDashboard } from "./mocks.js";
import type { Fixtures } from "./mocks.js";

// Drop Session gating: the kill switch must only exist when a live session
// exists for that row. Row-data contract (backend/internal/dashboard/
// dashboard_cards.go tokenSessionQuota, from pool.TokenSnapshot): the live
// session is identified by `session_instance` ("" when the session is
// disabled/absent — pool.go; SessionSnapshot.Usable() is false when empty;
// the session store drops "active"-without-instance entries as poison).
// `session_model` + `session_remaining_seconds` can linger without a live
// instance, so gating the button on those alone offers a phantom Drop
// Session on an idle, session-less row (prod 2026-09-16: Account #1 idle,
// Instance —, red button offered).
type TokenRow = Record<string, unknown>;

// Committed JSON fixture shape, asserted once at this boundary.
interface TokensFixtureDoc {
  tokens: Array<Record<string, unknown>>;
  [key: string]: unknown;
}

// Bespoke two-row pool: a session-less idle row carrying stale model +
// remaining (the prod phantom shape), and a genuinely live row.
function bespokeTokens(fixtures: Fixtures): TokenRow {
  const doc = fixtures.tokens as TokensFixtureDoc;
  if (!Array.isArray(doc.tokens) || doc.tokens.length === 0) {
    throw new Error("tokens fixture has no rows");
  }
  const base: TokenRow = { ...doc.tokens[0] };
  const phantom: TokenRow = {
    ...base,
    index: 0,
    session_status: "idle",
    // No live session: empty instance id, but stale model + remaining.
    session_instance: "",
    session_model: "mimo/mimo-v2.5",
    session_remaining_seconds: 1980,
    session_expires_at: "",
    active_runs: 0,
  };
  const live: TokenRow = {
    ...base,
    index: 1,
    session_status: "active",
    session_instance: "inst-live42…",
    session_model: "stealth/ox-alpha",
    session_remaining_seconds: 4620,
    active_runs: 1,
  };
  const payload: TokenRow = {};
  for (const [key, value] of Object.entries(doc)) {
    if (key !== "tokens") payload[key] = value;
  }
  payload.tokens = [phantom, live];
  payload.token_count = 2;
  return payload;
}

async function gotoTokens(page: Page, fixtures: Fixtures): Promise<void> {
  await mockDashboard(page, fixtures, { tokens: bespokeTokens(fixtures) });
  await page.goto("http://127.0.0.1:4173/admin/#tokens");
  // Desktop renders the table (lg+), mobile the stacked cards (< lg): the
  // page-level text matches the hidden layout twin too, so wait inside the
  // layout that is visible at the current viewport.
  if ((page.viewportSize()?.width ?? 1280) >= 1024) {
    const table = page.locator("table.fp-table");
    await expect(table.getByText("Account #1")).toBeVisible({
      timeout: 10000,
    });
    await expect(table.getByText("Account #2")).toBeVisible();
  } else {
    await expect(
      page.locator('[aria-label="Draggable account card 1"]'),
    ).toBeVisible({ timeout: 10000 });
    await expect(
      page.locator('[aria-label="Draggable account card 2"]'),
    ).toBeVisible();
  }
}

test.describe("drop session gating (desktop table)", () => {
  test("session-less row offers no Drop Session; live row keeps it", async ({
    page,
  }) => {
    await gotoTokens(page, loadFixtures());
    const table = page.locator("table.fp-table");
    const phantom = table.locator("tbody tr").filter({ hasText: "Account #1" });
    const live = table.locator("tbody tr").filter({ hasText: "Account #2" });
    // Phantom row: idle + no instance, yet stale model/remaining present.
    await expect(phantom.getByText("idle").first()).toBeVisible();
    // RED: fails while the button gates on remaining+model instead of the
    // live instance id.
    await expect(
      phantom.getByRole("button", { name: "Drop Session" }),
    ).toHaveCount(0);
    // Live row: identical button, same styling/handler — untouched.
    const btn = live.getByRole("button", { name: "Drop Session" });
    await expect(btn).toBeVisible();
    await expect(btn).toBeEnabled();
  });
});

test.describe("drop session gating (mobile cards)", () => {
  test.use({ viewport: { width: 390, height: 844 } });

  test("session-less card offers no Drop Session; live card keeps it", async ({
    page,
  }) => {
    await gotoTokens(page, loadFixtures());
    const phantom = page.locator('[aria-label="Draggable account card 1"]');
    const live = page.locator('[aria-label="Draggable account card 2"]');
    await expect(
      phantom.getByRole("button", { name: "Drop Session" }),
    ).toHaveCount(0);
    const btn = live.getByRole("button", { name: "Drop Session" });
    await expect(btn).toBeVisible();
    await expect(btn).toBeEnabled();
  });
});
