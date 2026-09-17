import type { Page } from "@playwright/test";
import { adminUrl } from "./mock-data.js";
import {
  loadFixtures,
  mockDashboard,
  mockSettingsOverlay,
  type Fixtures,
  type OverlaySeed,
  type PostedSetting,
} from "./mocks.js";

// Settings + control-switches matrix seeders (one factory, one route layer).
//
// Data lives here, routes stay in mocks.ts: every spec builds its fake
// backend state from the rosters/seeds below and serves it through
// mockDashboard + mockSettingsOverlay. No second mock system.
//
// Roster scope is origin/main: the catalog keys with a real editor on one
// of the three settings surfaces (Pool Controls tab, Settings page, Usage
// Controls tab). Deliberately OUT of the matrix, with reasons:
// - PIN_MODEL: drawer-owned (TokenDetailsDrawer is its only editor).
// - QUOTA_PROBE_*/MATURITY_*/QUOTA_AUTO_PROBE: excised from the catalog
//   with the prober removal; no card names them, so no section renders.
// - Secrets (API_KEYS/AUTH_TOKENS/ADMIN_TOKEN/WEBHOOK_URL): never listed,
//   managed on their own surfaces.
// - Hidden infra keys (SESSION_STATE_FILE, ROTATION_INTERVAL, ...): shown
//   read-only in the Hidden keys disclosure, no editors.

// Pool Strategy card (Pool page Controls tab): the four preset-written keys.
export const STRATEGY_KEYS = [
  "SLOTS_PER_ACCOUNT",
  "MAX_SPILL_ACCOUNTS",
  "QUEUE_WAIT",
  "QUEUE_DEPTH",
] as const;

// Pool Controls card (Pool page Controls tab).
export const POOL_CONTROLS_KEYS = [
  "RATE_LIMIT_PER_IP",
  "BRIDGE_ENABLED",
] as const;

// Custom advanced card (Pool page Controls tab): hand-tuned
// admission-cache and session knobs that still exist in the catalog.
export const CUSTOM_ADVANCED_KEYS = [
  "MODEL_UNAVAILABLE_CACHE_TTL",
  "SESSION_PROBE_CACHE_TTL",
  "SESSION_RE_ADMIT_LEAD",
  "WAITING_ROOM_CHAIN",
  "SESSION_PERSIST",
  "ADOPT_CLI_SESSION",
] as const;

// Pool Tuning card (Pool page Controls tab): remaining visible pool keys.
export const POOL_TUNING_KEYS = [
  "BRIDGE_IDLE_EVICT",
  "IDLE_ROTATION_TIMEOUT",
  "RATE_LIMIT_BURST",
] as const;

// Gateway card (Settings page).
export const GATEWAY_KEYS = ["SAFE_MODE", "HTTP_READ_TIMEOUT"] as const;

// Logging cards (Settings page): Server Log Level + Logging & Diagnostics.
export const LOGGING_KEYS = [
  "LOG_LEVEL",
  "DEBUG_DUMP",
  "DEVTOOLS_ENABLED",
  "LOG_ACCESS",
  "LOG_FORMAT",
] as const;

// Access + Security cards (Settings page).
export const SECURITY_KEYS = [
  "DASHBOARD_REQUIRE_LOGIN",
  "CORS_ALLOWED_ORIGIN",
] as const;

// Usage Controls + Upstream & Quota cards (Usage page Controls tab).
// MODELS_ALLOW lives in Upstream & Quota (AdvancedSettings onlyGroups
// upstream): the catalog's only list-kind upstream row with a real editor.
export const UPSTREAM_QUOTA_KEYS = [
  "REASONING_IN_CONTENT",
  "CACHE_CONTROL_INJECTION",
  "COMPRESS_PROMPT",
  "MODELS_HIDE_UNAVAILABLE",
  "REGISTRY_REFRESH",
  "MODELS_ALLOW",
] as const;

// Every matrix key in surface order: pool, gateway, logging, security,
// upstream. PIN_MODEL/excised/secret/hidden keys excluded (see the header
// note for why each has no matrix row).
export const ALL_MATRIX_KEYS: readonly string[] = [
  ...STRATEGY_KEYS,
  ...POOL_CONTROLS_KEYS,
  ...CUSTOM_ADVANCED_KEYS,
  ...POOL_TUNING_KEYS,
  ...GATEWAY_KEYS,
  ...LOGGING_KEYS,
  ...SECURITY_KEYS,
  ...UPSTREAM_QUOTA_KEYS,
];

// Where each matrix key's single editor lives.
export const KEY_HOME: Record<string, "pool" | "settings" | "usage"> = {
  SLOTS_PER_ACCOUNT: "pool",
  MAX_SPILL_ACCOUNTS: "pool",
  QUEUE_WAIT: "pool",
  QUEUE_DEPTH: "pool",
  RATE_LIMIT_PER_IP: "pool",
  BRIDGE_ENABLED: "pool",
  MODEL_UNAVAILABLE_CACHE_TTL: "pool",
  SESSION_PROBE_CACHE_TTL: "pool",
  SESSION_RE_ADMIT_LEAD: "pool",
  WAITING_ROOM_CHAIN: "pool",
  SESSION_PERSIST: "pool",
  ADOPT_CLI_SESSION: "pool",
  BRIDGE_IDLE_EVICT: "pool",
  IDLE_ROTATION_TIMEOUT: "pool",
  RATE_LIMIT_BURST: "pool",
  SAFE_MODE: "settings",
  HTTP_READ_TIMEOUT: "settings",
  LOG_LEVEL: "settings",
  DEBUG_DUMP: "settings",
  DEVTOOLS_ENABLED: "settings",
  LOG_ACCESS: "settings",
  LOG_FORMAT: "settings",
  DASHBOARD_REQUIRE_LOGIN: "settings",
  CORS_ALLOWED_ORIGIN: "settings",
  REASONING_IN_CONTENT: "usage",
  CACHE_CONTROL_INJECTION: "usage",
  COMPRESS_PROMPT: "usage",
  MODELS_HIDE_UNAVAILABLE: "usage",
  REGISTRY_REFRESH: "usage",
  MODELS_ALLOW: "usage",
};

// Balance posture seed (overlay wins the display, so rows show these).
export function balanceStrategySeed(): OverlaySeed[] {
  return [
    { key: "SLOTS_PER_ACCOUNT", value: "2", source: "db" },
    { key: "MAX_SPILL_ACCOUNTS", value: "0", source: "db" },
    { key: "QUEUE_WAIT", value: "60s", source: "db" },
    { key: "QUEUE_DEPTH", value: "16", source: "db" },
  ];
}

// Drain posture seed: deep queues before the spill.
export function drainStrategySeed(): OverlaySeed[] {
  return [
    { key: "SLOTS_PER_ACCOUNT", value: "2", source: "db" },
    { key: "MAX_SPILL_ACCOUNTS", value: "0", source: "db" },
    { key: "QUEUE_WAIT", value: "300s", source: "db" },
    { key: "QUEUE_DEPTH", value: "1024", source: "db" },
  ];
}

// One saved row for every matrix key (non-default values, source db).
// Overlay seed rows use source:db or the display loses to file defaults.
export function fullMatrixDbSeed(): OverlaySeed[] {
  const values: Record<string, string> = {
    SLOTS_PER_ACCOUNT: "3",
    MAX_SPILL_ACCOUNTS: "1",
    QUEUE_WAIT: "45s",
    QUEUE_DEPTH: "32",
    RATE_LIMIT_PER_IP: "20",
    BRIDGE_ENABLED: "true",
    MODEL_UNAVAILABLE_CACHE_TTL: "2h",
    SESSION_PROBE_CACHE_TTL: "30s",
    SESSION_RE_ADMIT_LEAD: "90s",
    WAITING_ROOM_CHAIN: "true",
    SESSION_PERSIST: "false",
    ADOPT_CLI_SESSION: "true",
    BRIDGE_IDLE_EVICT: "48h",
    IDLE_ROTATION_TIMEOUT: "1h",
    RATE_LIMIT_BURST: "40",
    SAFE_MODE: "false",
    HTTP_READ_TIMEOUT: "120s",
    LOG_LEVEL: "debug",
    DEBUG_DUMP: "true",
    DEVTOOLS_ENABLED: "true",
    LOG_ACCESS: "false",
    LOG_FORMAT: "json",
    DASHBOARD_REQUIRE_LOGIN: "true",
    CORS_ALLOWED_ORIGIN: "https://example.com",
    REASONING_IN_CONTENT: "true",
    CACHE_CONTROL_INJECTION: "false",
    MODELS_HIDE_UNAVAILABLE: "true",
    REGISTRY_REFRESH: "12h",
    MODELS_ALLOW: "deepseek/deepseek-v4-flash",
  };
  return ALL_MATRIX_KEYS.map((key) => ({
    key,
    value: values[key] ?? "true",
    source: "db",
  }));
}

// Live .env document for the config mock: file-derived display values and
// the parseEnv default chips read from this text.
export const MATRIX_LIVE_ENV =
  "LISTEN_ADDR=127.0.0.1:3457\nAUTH_TOKENS=tok0,tok1\nAPI_KEYS=sk-local-xyz\nSAFE_MODE=true\nLOG_LEVEL=info\n";

// Config fixture override serving a bespoke .env document (db-win probes:
// the file says one thing, the overlay seed says another).
export function configWithEnv(envText: string): Record<string, unknown> {
  return {
    env_content: envText,
    has_env_file: true,
    effective: [
      { key: "LISTEN_ADDR", value: "127.0.0.1:3457", secret: false },
      { key: "SAFE_MODE", value: "true", secret: false },
    ],
  };
}

export type MatrixOptions = {
  // .env document served by /admin/api/config (default MATRIX_LIVE_ENV).
  envText?: string;
  // Saved rows served by GET /admin/api/settings (source:db wins display).
  seed?: OverlaySeed[];
  // Serve GET /admin/api/settings with degraded:true (offline store).
  degraded?: boolean;
  // Non-200 POST status models a rejected write (default 200).
  postStatus?: number;
};

export type MatrixSetup = {
  fixtures: Fixtures;
  posted: PostedSetting[];
  deleted: string[];
};

// One-call hook for the matrix: fulfills every /admin/* endpoint the SPA
// talks to (via mockDashboard) plus the stateful settings overlay, without
// navigating. Returns the posted overlay writes for save probes.
export async function mockSettingsMatrix(
  page: Page,
  opts: MatrixOptions = {},
): Promise<MatrixSetup> {
  const fixtures = loadFixtures();
  await mockDashboard(
    page,
    fixtures,
    opts.envText !== undefined ? { config: configWithEnv(opts.envText) } : {},
    { loginPage: true },
  );
  const posted: PostedSetting[] = [];
  const { deleted } = await mockSettingsOverlay(page, posted, {
    degraded: opts.degraded,
    postStatus: opts.postStatus,
    seed: opts.seed,
  });
  return { fixtures, posted, deleted };
}

// Navigate to a Controls tab (Pool or Usage): waits for the key catalog,
// then opens the tab. The settings store hydrates from the same mocks.
export async function gotoControls(
  page: Page,
  hash: "tokens" | "plans",
): Promise<void> {
  const metaResp = page.waitForResponse(
    (r) => r.url().includes("/admin/api/config/meta") && r.status() === 200,
    { timeout: 5000 },
  );
  await page.goto(adminUrl(hash));
  await metaResp;
  await page.getByRole("button", { name: "Controls" }).click();
}

// Navigate to the Settings page with the catalog loaded.
export async function gotoSettings(page: Page): Promise<void> {
  const metaResp = page.waitForResponse(
    (r) => r.url().includes("/admin/api/config/meta") && r.status() === 200,
    { timeout: 5000 },
  );
  await page.goto(adminUrl("settings"));
  await metaResp;
}

// Every settings editor carries aria-label=KEY (switches, selects,
// steppers, pickers, plain inputs alike), so one selector counts homes.
export function editor(page: Page, key: string) {
  return page.locator(`[aria-label="${key}"]`);
}
