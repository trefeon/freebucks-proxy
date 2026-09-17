import { expect, type Page } from "@playwright/test";
import {
  adminUrl,
  MASQ_STRATEGY_SEED,
  pinnedToken,
  preciousToken,
  quotaTrackerToken,
  saturatedLaneToken,
  tokenRow,
  tokensPayload,
  type TokenRow,
} from "./mock-data.js";
import {
  loadFixtures,
  mockDashboard,
  mockSettingsOverlay,
  type Fixtures,
  type PostedSetting,
} from "./mocks.js";

// Pool + accounts + tokens flows against one 22-row mock roster.
//
// mock-data.ts owns the row builders, mocks.ts owns the routes; this module
// owns the pool-lane roster shape (every operator-visible account state in
// one pool) plus the stateful mutation endpoints the Tokens page posts to
// (swap / remove / per-token lock / drop-session). Specs assert web state
// only, with role/label locators and no sleeps.
//
// Post-#608 shapes: EXPIRED rows carry no instance/model (the snapshot
// clears live facts on the terminal path, so the row renders like IDLE);
// drain-window grace rows still show their live facts but offer no Drop
// Session (the kill switch gates on session_status === "active").

export const MODEL_A = "deepseek/deepseek-v4-flash";
export const MODEL_B = "z-ai/glm-5.2";

// A saturated lane on MODEL_B: the cross-model half of the 2-slots-per-
// (account,model) contract (the MODEL_A half is saturatedLaneToken(0)).
// One account may hold 2xA + 2xB at once; the roster shows both lanes at
// their 2-slot cap on different models.
function saturatedLaneTokenB(idx: number): TokenRow {
  return saturatedLaneToken(idx, {
    session_instance: `inst-masq${String(idx).padStart(2, "0")}-abcdefghijklmnop`,
    session_model: MODEL_B,
    queued_waiters: 1,
    oldest_waiter_ms: 2100,
  });
}

// The full pooled roster: 22 rows covering every Tokens-page state.
// Indexes double as pool positions (Account #N = index N-1).
export function poolRoster(): TokenRow[] {
  const rows: TokenRow[] = [
    // 0: spill head, both MODEL_A lane slots live with FIFO waiters parked.
    saturatedLaneToken(0),
    // 1: second saturated lane on MODEL_B (cross-model 2xA + 2xB).
    saturatedLaneTokenB(1),
    // 2: queued waiter behind a saturated lane.
    tokenRow(2, {
      session_status: "queued",
      queue_position: 1,
      queue_depth: 2,
    }),
    // 3: precious live holder (never proactively dropped).
    preciousToken(3),
    // 4: pinned account with routed-elsewhere skips.
    pinnedToken(4, { pinned_model: MODEL_B, pin_skips: 5 }),
    // 5: Freebucks quota tracker (active without a session instance: the
    // phantom shape that must NOT offer Drop Session).
    quotaTrackerToken(5),
    // 6: terminal EXPIRED row: no instance, no model, no countdown.
    tokenRow(6, {
      session_status: "expired",
      session_instance: "",
      session_model: "",
      session_remaining_seconds: 0,
      session_expires_at: "",
      queue_position: 0,
      queue_depth: 0,
    }),
    // 7: grace drain: expiry crossed but the drain window is open, so the
    // live facts stay visible while Drop Session stays hidden.
    tokenRow(7, {
      session_status: "grace",
      session_instance: "inst-grace07-abcdefghijklmnop",
      session_model: MODEL_A,
      session_remaining_seconds: 900,
      active_runs: 1,
    }),
    // 8: idle with a live streak.
    tokenRow(8, { streak: 7 }),
    // 9: plain idle, no streak.
    tokenRow(9),
    // 10: locked out of rotation.
    tokenRow(10, { locked: true }),
    // 11: cooling down with a clearable cooldown.
    tokenRow(11, {
      cooldown_active: true,
      cooldown_until: new Date(Date.now() + 5 * 60_000).toISOString(),
    }),
    // 12: idle with a short streak.
    tokenRow(12, { streak: 3 }),
  ];
  for (let i = 13; i < 22; i++) rows.push(tokenRow(i));
  return rows;
}

export type PoolState = { tokens: TokenRow[] };

function reindex(state: PoolState): void {
  state.tokens.forEach((t, i) => {
    t.index = i;
  });
}

// Stateful mutation endpoints for the Tokens page actions. The tokens GET
// route from mockDashboard stringifies its payload per request, so mutating
// the same row objects here is visible on the next poll/refresh with no
// re-routing.
export async function mockPoolMutations(
  page: Page,
  state: PoolState,
): Promise<void> {
  await page.route("**/admin/tokens/swap", async (route) => {
    const body = (await route.request().postDataJSON()) as {
      from: number;
      to: number;
    };
    const moved = state.tokens.splice(body.from, 1)[0];
    state.tokens.splice(body.to, 0, moved);
    reindex(state);
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ ok: true, message: "Pool order updated." }),
    });
  });
  await page.route("**/admin/tokens/remove", async (route) => {
    const body = (await route.request().postDataJSON()) as { token: number };
    state.tokens.splice(body.token, 1);
    reindex(state);
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ ok: true, message: "Account removed." }),
    });
  });
  await page.route(/\/admin\/tokens\/\d+\/[\w-]+$/, async (route) => {
    const match = new URL(route.request().url()).pathname.match(
      /\/admin\/tokens\/(\d+)\/([\w-]+)$/,
    );
    const idx = Number(match?.[1]);
    const action = match?.[2];
    const row = state.tokens.find((t) => t.index === idx) ?? state.tokens[idx];
    let message = `${action} done.`;
    if (row) {
      if (action === "lock") {
        row.locked = true;
        message = "Account locked.";
      } else if (action === "unlock-lock") {
        row.locked = false;
        message = "Account unlocked.";
      } else if (action === "unlock") {
        row.cooldown_active = false;
        row.cooldown_until = "";
        message = "Cooldown cleared.";
      } else if (action === "drop-session") {
        row.session_status = "idle";
        row.session_instance = "";
        row.session_model = "";
        row.session_remaining_seconds = 0;
        row.session_expires_at = "";
        row.active_runs = 0;
        message = "Session dropped.";
      }
    }
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ ok: true, message }),
    });
  });
}

export type PoolSetup = {
  fixtures: Fixtures;
  posted: PostedSetting[];
  state: PoolState;
};

export type PoolOptions = {
  rows?: TokenRow[];
  loginPage?: boolean;
};

// One-call hook for pool-lane specs: fake backend state from the roster,
// served through mockDashboard, with the strategy posture seeded on the
// settings overlay (source "db" so saved rows win the row display).
export async function mockPool(
  page: Page,
  opts: PoolOptions = {},
): Promise<PoolSetup> {
  const rows = opts.rows ?? poolRoster();
  const fixtures = loadFixtures();
  await mockDashboard(
    page,
    fixtures,
    { tokens: tokensPayload(rows) },
    {
      loginPage: opts.loginPage,
    },
  );
  const posted: PostedSetting[] = [];
  await mockSettingsOverlay(page, posted, { seed: [...MASQ_STRATEGY_SEED] });
  return { fixtures, posted, state: { tokens: rows } };
}

export async function gotoPoolTokens(page: Page) {
  await page.goto(adminUrl("tokens"));
  const table = page.locator("table.fp-table");
  await expect(table.getByText("Account #1", { exact: true })).toBeVisible({
    timeout: 10000,
  });
  return table;
}
