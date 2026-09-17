import type { Page } from "@playwright/test";
import {
  loadFixtures,
  mockDashboard,
  mockSettingsOverlay,
  type Fixtures,
  type MockOverrides,
  type PostedSetting,
} from "./mocks.js";
import { adminUrl, tokenRow, tokensPayload } from "./mock-data.js";

// Slice-local builders for the keys / auth / usage / logs flows.
//
// Data still comes from the shared factory (mock-data.ts tokenRow /
// tokensPayload) and routes still go through the shared layer (mocks.ts
// mockDashboard / mockSettingsOverlay). This module only holds the small
// payload shapes this slice needs so the spec stays DAMP without growing
// a second mock system.

// --- Client API Keys (.env document shapes) ---

export const SEED_KEYS = ["sk-fb-seedkey0001", "sk-fb-seedkey0002"];

// .env document carrying exactly `keys` in API_KEYS.
export function keysEnv(keys: string[]): string {
  const line = keys.length > 0 ? `API_KEYS=${keys.join(",")}\n` : "";
  return `AUTH_TOKENS=tok0\n${line}`;
}

// /admin/api/config override serving the given API_KEYS document.
export function configWithKeysEnv(
  f: Fixtures,
  keys: string[],
): Record<string, unknown> {
  return {
    ...f.config,
    env_content: keysEnv(keys),
    has_env_file: true,
  };
}

// Later route wins over mockDashboard's default POST /admin/config
// fulfillment, so bodies are observable without touching the shared layer.
export async function captureConfigPosts(
  page: Page,
  bodies: string[],
  status = 200,
): Promise<void> {
  await page.route("**/admin/config", async (route) => {
    if (route.request().method() === "POST") {
      bodies.push(route.request().postData() ?? "");
      await route.fulfill({
        status,
        contentType: "application/json",
        body: JSON.stringify(
          status === 200
            ? { ok: true, message: "Config saved" }
            : { ok: false, message: "Config save failed" },
        ),
      });
    } else {
      await route.continue();
    }
  });
}

// --- Auth tiers (login gate) ---

// Force POST /admin/login to fail like the real gateway does for a wrong
// admin token.
export async function forceLogin401(page: Page): Promise<void> {
  await page.unroute("**/admin/login");
  await page.route("**/admin/login", async (route) => {
    if (route.request().method() === "POST") {
      await route.fulfill({
        status: 401,
        contentType: "application/json",
        body: JSON.stringify({ error: "Invalid admin token." }),
      });
    } else {
      await route.continue();
    }
  });
}

// Kill the tokens API with an auth failure so the next fetch trips the
// session-expired gate (client.js marks the session dead on any 401).
export async function killTokensApi(page: Page): Promise<void> {
  await page.unroute("**/admin/api/tokens*");
  await page.route("**/admin/api/tokens*", async (route) => {
    await route.fulfill({
      status: 401,
      contentType: "application/json",
      body: JSON.stringify({ error: { message: "Unauthorized" } }),
    });
  });
}

// --- Usage (Accounts) token shapes ---
// Minimal Freebucks windows mirroring the dashboard.spec payloads: the
// row keeps daily figures + wallet while the live countdown renders once
// in the shared reset strip.

export function meteredToken(
  idx: number,
  over: Record<string, unknown> = {},
): Record<string, unknown> {
  return tokenRow(idx, {
    freebucks: {
      balance: 50,
      daily: { remaining: 30, limit: 75, reset_at: "2030-01-01T00:00:00Z" },
      wallet: { balance: 20 },
      monthly: { remaining: 20 },
      prices: {},
    },
    ...over,
  });
}

export function discountToken(
  idx: number,
  available: boolean,
): Record<string, unknown> {
  return tokenRow(idx, {
    freebucks: {
      balance: 50,
      daily: {
        remaining: 30,
        limit: 75,
        reset_at: "2030-01-01T00:00:00Z",
        reset_time_zone: "America/New_York",
      },
      wallet: { balance: 20 },
      prices: {},
      first_tab_discount: available
        ? { amount: 3, available: true }
        : { amount: 3, available: false, holder_surface: "desktop" },
    },
  });
}

export function refundPendingToken(idx: number): Record<string, unknown> {
  return tokenRow(idx, {
    access_tier: "full",
    freebucks: {
      balance: 50,
      daily: { remaining: 95, limit: 100, reset_at: "2030-01-01T00:00:00Z" },
      wallet: { balance: 2.5 },
      prices: {},
    },
    pending_refund: "inst-abc-123",
  });
}

export function settledRefundToken(
  idx: number,
  amount: number,
): Record<string, unknown> {
  return tokenRow(idx, { last_refund: amount });
}

// Narrow the mutable payload back to its token rows (built by
// tokensPayload above, so the shape is already known at this boundary).
function refundTokensOf(state: Record<string, unknown>) {
  const toks = state["tokens"];
  if (!Array.isArray(toks)) throw new Error("refund state has no tokens");
  return toks as Record<string, unknown>[];
}

// Mutable tokens route + refund-refresh replay: the pending line fires one
// automatic refresh on first render, the mock settles the parked release,
// and the next tokens fetch carries the receipt.
export async function mockRefundReplay(
  page: Page,
  state: Record<string, unknown>,
  idx = 0,
  amount = 1.5,
): Promise<void> {
  await page.unroute("**/admin/api/tokens*");
  await page.route("**/admin/api/tokens*", async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(state),
    });
  });
  await page.route(`**/admin/tokens/${idx}/refund-refresh`, async (route) => {
    const toks = refundTokensOf(state);
    delete toks[idx].pending_refund;
    toks[idx].last_refund = amount;
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({
        ok: true,
        message: `Token ${idx} refund settled: ${amount} Freebucks returned to wallet.`,
      }),
    });
  });
}

// --- Slice setup: fixtures + dashboard routes + settings overlay ---

export type UsageMockOptions = {
  overrides?: MockOverrides;
  loginPage?: boolean;
};

export async function mockUsage(
  page: Page,
  opts: UsageMockOptions = {},
): Promise<{ fixtures: Fixtures; posted: PostedSetting[] }> {
  const fixtures = loadFixtures();
  await mockDashboard(page, fixtures, opts.overrides ?? {}, {
    loginPage: opts.loginPage,
  });
  const posted: PostedSetting[] = [];
  await mockSettingsOverlay(page, posted);
  return { fixtures, posted };
}

// --- Team usage (Activity Team tab, #609) ---
// Per-client API key row mirroring aggregateUsageByKey: short key hash
// label (never a raw key), 0..1 success rate, token totals, per-model
// splits, first/last seen bounds, wire-price freebucks.
export function teamKeyRow(
  over: Record<string, unknown> = {},
): Record<string, unknown> {
  return {
    key_id: "aaaabbbbccccdddd",
    requests: 2,
    success_rate: 0.5,
    total_tokens: 165,
    by_model: {
      "test/priced": {
        requests: 2,
        tokens: 165,
        prompt: 110,
        completion: 55,
        reasoning: 4,
      },
    },
    first_seen: 1785890000000,
    last_seen: 1785893600000,
    freebucks: 40,
    ...over,
  };
}

// Serves GET /admin/api/usage?group_by=key for the Team tab. mocks.ts
// predates #609 and has no usage route, so the slice serves it here
// (later route wins; no shared file touched).
export async function mockTeamUsage(
  page: Page,
  keys: Record<string, unknown>[],
): Promise<void> {
  await page.route("**/admin/api/usage*", async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ range: "7d", keys }),
    });
  });
}

export { adminUrl, tokenRow, tokensPayload };
