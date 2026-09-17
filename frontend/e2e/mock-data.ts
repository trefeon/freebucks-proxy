import type { Page } from "@playwright/test";
import { loadFixtures, mockDashboard, mockSettingsOverlay } from "./mocks.js";
import type {
  Fixtures,
  MockOverrides,
  OverlaySeed,
  PostedSetting,
} from "./mocks.js";

// Centralized mock-data factory + scenario hook for dashboard e2e specs.
//
// History: every spec copy-pasted its own tokenRow/tokensPayload/requestLines
// helpers (ux, interactions, pool-table, queue-telemetry, interactables-db).
// This module is the single source for that fake data; mocks.ts stays the
// single route layer (mockDashboard). Specs build state here and hand it to
// mockDashboard directly, or use mockMasqScenario for the canned MASQ states.
// No second parallel mock system: data lives here, routes live in mocks.ts.

export const ADMIN_ORIGIN = "http://127.0.0.1:4173";
export const adminUrl = (hash: string): string =>
  `${ADMIN_ORIGIN}/admin/#${hash}`;

export type TokenRow = Record<string, unknown>;

// Canonical idle pool row mirroring the real /admin/api/tokens row shape.
// Callers specialize via `over`; never mutate the shared fixtures in place.
export function tokenRow(
  idx: number,
  over: Record<string, unknown> = {},
): TokenRow {
  return {
    index: idx,
    email: `acct${idx}@example.com`,
    session_status: "idle",
    queue_position: 0,
    queue_depth: 0,
    active_runs: 0,
    requests: 0,
    messages_24h: 0,
    cooldown_active: false,
    cooldown_until: "",
    locked: false,
    transient_retries: 1,
    has_standing: false,
    session_instance: "",
    session_model: "",
    session_remaining_seconds: 0,
    has_quota: false,
    ...over,
  };
}

export function tokensPayload(
  tokens: TokenRow[],
  extra: Record<string, unknown> = {},
): Record<string, unknown> {
  return {
    mode: "pooled",
    in_bridge: false,
    bridge_tokens: 0,
    token_count: tokens.length,
    has_tokens: true,
    tokens,
    bridge_token_cards: [],
    ...extra,
  };
}

// Copies the committed fixture's token rows into fresh objects (never mutate
// shared fixtures) after a runtime shape check.
export function tokenRowsOf(value: unknown): TokenRow[] {
  if (typeof value !== "object" || value === null) return [];
  if (!("tokens" in value)) return [];
  const arr = value.tokens;
  if (!Array.isArray(arr)) return [];
  return arr
    .filter(
      (t): t is Record<string, unknown> => typeof t === "object" && t !== null,
    )
    .map((t) => ({ ...t }));
}

// Settings fixtures carry a live .env so the form cards render with values.
export const DEFAULT_LIVE_ENV =
  "LISTEN_ADDR=127.0.0.1:3457\nAUTH_TOKENS=tok0,tok1\nAPI_KEYS=sk-local-xyz\nSAFE_MODE=true\nLOG_LEVEL=info\n";

export function settingsConfig(
  f: Fixtures,
  envText: string = DEFAULT_LIVE_ENV,
): Record<string, unknown> {
  const base = f.config && typeof f.config === "object" ? f.config : {};
  return {
    ...base,
    env_content: envText,
    has_env_file: true,
  };
}

// Console log lines for the LiveConsole request cards.
export type LogEntry = {
  time: string;
  level: string;
  message: string;
  fields: string;
};
// One console log line.
export function logEntry(
  message: string,
  fields: string,
  t = "2026-09-16T10:00:00Z",
): LogEntry {
  return { time: t, level: "INFO", message, fields };
}

// Per-request cluster the LiveConsole groups into one card: chat request,
// routing, access, chat trace, chat done, all sharing one req_id.
export function requestLines(reqId: string, traceFields: string): LogEntry[] {
  return [
    logEntry(
      "chat request",
      `req_id=${reqId}  model=deepseek/deepseek-v4-flash  msgs=1`,
    ),
    logEntry("chat routing", `req_id=${reqId}  agent=base2-free-deepseek`),
    logEntry(
      "access",
      `req_id=${reqId}  method=POST  path=/v1/chat/completions  status=200  ms=140`,
    ),
    logEntry(
      "chat trace",
      `req_id=${reqId}  model=deepseek/deepseek-v4-flash  status=ok  ${traceFields}`,
    ),
    logEntry("chat done", `req_id=${reqId}  ms=140`),
  ];
}

// MASQ state builders. Each covers one operator-visible pool state:
// - saturatedLaneToken: head lane at the slot cap with FIFO waiters parked
//   (spill source: the next account takes the overflow after QUEUE_WAIT).
// - preciousToken: live upstream session holder (never proactively dropped;
//   Drop Session gates on session_instance, so this row keeps the button).
// - pinnedToken: per-account model pin with routed-elsewhere skip count.
// - quotaTrackerToken: Freebucks daily/monthly windows for the Plans page.
export function saturatedLaneToken(
  idx: number,
  over: Record<string, unknown> = {},
): TokenRow {
  return tokenRow(idx, {
    session_status: "active",
    queue_position: 1,
    queue_depth: 2,
    active_runs: 2,
    requests: 148,
    messages_24h: 3,
    session_instance: "inst-masq00-abcdefghijklmnop",
    session_model: "deepseek/deepseek-v4-flash",
    session_remaining_seconds: 4620,
    live_turns: 2,
    queued_waiters: 2,
    oldest_waiter_ms: 4500,
    ...over,
  });
}

export function preciousToken(
  idx = 0,
  over: Record<string, unknown> = {},
): TokenRow {
  return tokenRow(idx, {
    session_status: "active",
    session_instance: "inst-ox99-abcdefghijklmnop",
    session_model: "stealth/ox-alpha",
    session_remaining_seconds: 4620,
    messages_24h: 3,
    active_runs: 3,
    requests: 148,
    ...over,
  });
}

export function pinnedToken(
  idx = 0,
  over: Record<string, unknown> = {},
): TokenRow {
  return tokenRow(idx, {
    pinned_model: "z-ai/glm-5.2",
    pin_skips: 3,
    ...over,
  });
}

export function quotaTrackerToken(
  idx = 0,
  over: Record<string, unknown> = {},
): TokenRow {
  return tokenRow(idx, {
    email: "dev@example.com",
    session_status: "active",
    freebucks: {
      balance: 7.5,
      daily: {
        limit: 10.0,
        spent: 2.5,
        remaining: 7.5,
        reset_at: "2030-01-01T07:00:00Z",
        percent_used: 25.0,
      },
      wallet: { balance: 5.0, monthly_bonus: 0.0 },
      spend: { limit_usd: 10.0, reset_at: "2030-10-01T00:00:00Z" },
      monthly: {
        limit: 300.0,
        spent: 42.0,
        remaining: 258.0,
        percent_used: 14.0,
      },
      plan_id: "pro",
      prices: { "deepseek/deepseek-v4-flash": 0.01 },
    },
    ...over,
  });
}

// Strategy seed served by the settings overlay mock: Balance posture with a
// bounded spill (1 continuation account), so the spill bound and the pool
// ceiling render deterministically. source "db" is required: the settings
// store only lets saved (db) rows win the row display.
export const MASQ_STRATEGY_SEED: OverlaySeed[] = [
  { key: "SLOTS_PER_ACCOUNT", value: "2", source: "db" },
  { key: "QUEUE_WAIT", value: "30s", source: "db" },
  { key: "QUEUE_DEPTH", value: "16", source: "db" },
  { key: "MAX_SPILL_ACCOUNTS", value: "1", source: "db" },
];

export type MasqScenario =
  | "spill-active"
  | "queue-wait"
  | "pinned"
  | "precious"
  | "quota-tracker"
  | "pool-ceiling";

// Builds the mockDashboard overrides for one MASQ scenario. Tokens carry the
// lane/session/pin/quota numbers; queue-wait additionally carries the console
// log cluster with the queue_wait_ms phase.
export function masqOverrides(scenario: MasqScenario): MockOverrides {
  switch (scenario) {
    case "spill-active":
      return {
        tokens: tokensPayload([saturatedLaneToken(0), tokenRow(1)]),
      };
    case "queue-wait": {
      // Distinct numbers per account so a wrong lookup cannot pass: the ACCT
      // chip carries the 1-based account number ("Account #1"), which is pool
      // index 0 in the payload.
      const head = tokenRow(0, {
        live_turns: 2,
        queued_waiters: 1,
        oldest_waiter_ms: 1200,
      });
      const next = tokenRow(1, {
        live_turns: 0,
        queued_waiters: 0,
        oldest_waiter_ms: 0,
      });
      return {
        tokens: tokensPayload([head, next]),
        logs: {
          entries: requestLines(
            "qw-masq",
            "total_ms=1820  queue_wait_ms=850  upstream_ttfb_ms=120  token=1",
          ),
        },
      };
    }
    case "pinned":
      return { tokens: tokensPayload([pinnedToken(0)]) };
    case "precious":
      return { tokens: tokensPayload([tokenRow(0), preciousToken(1)]) };
    case "quota-tracker":
      return { tokens: tokensPayload([quotaTrackerToken(0)]) };
    case "pool-ceiling":
      return {
        tokens: tokensPayload([tokenRow(0), tokenRow(1), tokenRow(2)]),
      };
  }
}

export type MasqMockOptions = {
  fixtures?: Fixtures;
  loginPage?: boolean;
  seed?: OverlaySeed[];
};

// One-call hook for the canned MASQ states: fulfills every /admin/* endpoint
// the SPA talks to (via mockDashboard) and seeds the settings overlay with
// the strategy posture. Returns the posted overlay writes for save probes.
export async function mockMasqScenario(
  page: Page,
  scenario: MasqScenario,
  opts: MasqMockOptions = {},
): Promise<{ fixtures: Fixtures; posted: PostedSetting[] }> {
  const fixtures = opts.fixtures ?? loadFixtures();
  await mockDashboard(page, fixtures, masqOverrides(scenario), {
    loginPage: opts.loginPage,
  });
  const posted: PostedSetting[] = [];
  await mockSettingsOverlay(page, posted, {
    seed: opts.seed ?? MASQ_STRATEGY_SEED,
  });
  return { fixtures, posted };
}
