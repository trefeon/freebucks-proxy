import type { Page } from "@playwright/test";
import { readFileSync } from "fs";
import { join, dirname } from "path";
import { fileURLToPath } from "url";

const __filename = fileURLToPath(import.meta.url);
const __dirname = dirname(__filename);

export type Fixtures = {
  overview: unknown;
  tokens: unknown;
  models: unknown;
  config: unknown;
  configMeta: unknown;
  logs: {
    entries: Array<{ level: string; message: string; fields?: string }>;
  } & Record<string, unknown>;
  metrics: unknown;
  setup: unknown;
  traces: unknown;
  version: unknown;
  upstreamDrift: unknown;
  authStatus: unknown;
  notices: unknown;
};

/**
 * Load the shared JSON fixtures once. `fixtureDir` overrides the default
 * `e2e/fixtures` directory (useful when a spec wants a bespoke fixture set).
 */
export function loadFixtures(fixtureDir?: string): Fixtures {
  const dir = fixtureDir ?? join(__dirname, "fixtures");
  return {
    overview: JSON.parse(readFileSync(join(dir, "overview.json"), "utf-8")),
    tokens: JSON.parse(readFileSync(join(dir, "tokens.json"), "utf-8")),
    models: JSON.parse(readFileSync(join(dir, "models.json"), "utf-8")),
    config: JSON.parse(readFileSync(join(dir, "config.json"), "utf-8")),
    configMeta: JSON.parse(
      readFileSync(join(dir, "config-meta.json"), "utf-8"),
    ),
    logs: JSON.parse(readFileSync(join(dir, "logs.json"), "utf-8")),
    metrics: JSON.parse(readFileSync(join(dir, "metrics.json"), "utf-8")),
    setup: JSON.parse(readFileSync(join(dir, "setup.json"), "utf-8")),
    traces: JSON.parse(readFileSync(join(dir, "traces.json"), "utf-8")),
    version: JSON.parse(readFileSync(join(dir, "version.json"), "utf-8")),
    upstreamDrift: JSON.parse(
      readFileSync(join(dir, "upstream-drift.json"), "utf-8"),
    ),
    authStatus: JSON.parse(
      readFileSync(join(dir, "auth-status.json"), "utf-8"),
    ),
    notices: JSON.parse(readFileSync(join(dir, "notices.json"), "utf-8")),
  };
}

// The SPA shell served for /admin/* routes (same file serve-static.mjs
// serves); the login-page mock below fulfills with it so it can also issue
// the fb_csrf double-submit cookie like the real gateway does.
const indexPath = join(
  __dirname,
  "../../backend/internal/dashboard/dist/index.html",
);

export type MockOptions = {
  /**
   * Model the full login page the way the real gateway does (the ux.spec
   * journey needs it): GET /admin/login is fulfilled with the built SPA shell
   * plus the non-HttpOnly fb_csrf double-submit cookie. When unset (the
   * dashboard.spec contract), GET /admin/login is passed through so the
   * static dev server serves the SPA without a CSRF cookie.
   */
  loginPage?: boolean;
};

export type MockOverrides = Partial<
  Record<keyof Fixtures | "configWithApiKeys", unknown>
>;

/**
 * Shared hermetic route mock layer (issue #294): every /admin/* endpoint the
 * SPA talks to is fulfilled from the fixture pack. Both dashboard.spec.ts and
 * ux.spec.ts previously copy-pasted this harness; they now import it.
 *
 * `overrides` lets a test substitute one fixture (or inject API_KEYS via the
 * `configWithApiKeys` key). `opts.loginPage` switches the GET /admin/login
 * handling to the full CSRF-modeling variant used by the ux.spec journey.
 */
export async function mockDashboard(
  page: Page,
  fixtures: Fixtures,
  overrides: MockOverrides = {},
  opts: MockOptions = {},
) {
  // Helpers to pick overridden or base fixture
  const pick = (key: keyof Fixtures) =>
    overrides[key] ?? (fixtures as Record<string, unknown>)[key];

  // Overview (also matches ?view=live hot polls: the mock answers the full
  // shape and the SPA merges it like an old server would).
  await page.route("**/admin/api/overview*", async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(pick("overview")),
    });
  });

  // Tokens (also matches ?view=live hot polls, same full-shape answer).
  await page.route("**/admin/api/tokens*", async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(pick("tokens")),
    });
  });

  // Models
  await page.route("**/admin/api/models", async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(pick("models")),
    });
  });

  // Traces
  await page.route("**/admin/api/traces", async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(pick("traces")),
    });
  });

  // Maturity history (ADR-0016 run records): the Maturity page fetches
  // this once per maturity-bearing token for the 7-day strip and the
  // touches/spend/projected ledger. Served for every token; specs needing
  // bespoke events re-route after mockDashboard (their later route wins).
  await page.route("**/admin/api/maturity/history*", async (route) => {
    const url = new URL(route.request().url());
    const token = Number(url.searchParams.get("token") ?? 0);
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({
        enabled: true,
        token,
        events: [
          { ts: 1785900000000, kind: "touch", detail: "admit ok" },
          { ts: 1785903600000, kind: "config", detail: "enabled target=7" },
        ],
      }),
    });
  });

  // Setup
  await page.route("**/admin/api/setup", async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(pick("setup")),
    });
  });

  // Config - Supports override that includes API_KEYS for Tokens parsing test.
  await page.route(/\/admin\/api\/config(\?.*)?$/, async (route) => {
    const cfg = overrides["configWithApiKeys"] ?? pick("config");
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(cfg),
    });
  });

  // Config meta - the Settings page key catalog (JSON array from /admin/api/config/meta).
  await page.route(/\/admin\/api\/config\/meta(\?.*)?$/, async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(pick("configMeta")),
    });
  });

  // Logs - handles ?level= & ?msg= filtering like the real Go handler, and
  // echoes the effective view window (the ?window= override, else the 1h
  // default the fixture stands in for).
  await page.route("**/admin/api/logs**", async (route) => {
    const url = new URL(route.request().url());
    const level = (url.searchParams.get("level") || "").toLowerCase();
    const msg = (url.searchParams.get("msg") || "").trim();
    const base = pick("logs");
    const logsData = base as unknown as {
      entries: Array<{ level: string; message: string }>;
      window?: string;
      truncated?: boolean;
    };
    let entries: Array<{ level: string; message: string }> =
      logsData.entries || [];
    if (level) {
      entries = entries.filter((e) => (e.level || "").toLowerCase() === level);
    }
    if (msg) {
      const low = msg.toLowerCase();
      entries = entries.filter((e) =>
        (e.message || "").toLowerCase().includes(low),
      );
    }
    const body = JSON.stringify({
      enabled: true,
      level: level,
      msg: msg,
      has_filter: !!(level || msg),
      window: url.searchParams.get("window") || logsData.window || "1h0m0s",
      truncated: logsData.truncated === true,
      entries,
    });
    await route.fulfill({ status: 200, contentType: "application/json", body });
  });

  // Metrics
  await page.route("**/admin/api/metrics", async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(pick("metrics")),
    });
  });

  // Version
  await page.route("**/admin/api/version", async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(pick("version")),
    });
  });

  // Upstream drift
  await page.route("**/admin/api/upstream-drift", async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(pick("upstreamDrift")),
    });
  });
  // Notices
  await page.route("**/admin/api/notices", async (route) => {
    const defaultNotices = {
      notices: [
        {
          id: "upstream-tier-change",
          type: "announcement",
          title: "Official Upstream Announcement",
          message:
            "Solar Pro 4 is now unmetered at full access and available with limited access.",
          badge: "Freebuff Team",
          tone: "accent",
        },
      ],
      peak_hours: {
        is_peak: false,
        window_start_utc: "00:00 UTC",
        window_end_utc: "10:00 UTC",
        next_window_in: "12h 0m",
      },
      count: 1,
    };
    const body = overrides["notices"] ?? pick("notices") ?? defaultNotices;
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(body),
    });
  });

  // Auth status
  await page.route("**/admin/api/auth/status", async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(pick("authStatus")),
    });
  });

  // POST /admin/login - default success; individual tests may override for 401 case.
  // The real gateway (backend/internal/server/admin_auth.go) issues the
  // non-HttpOnly fb_csrf double-submit cookie on the login PAGE. Each response
  // carries exactly ONE Set-Cookie: route.fulfill joins multiple values into a
  // single broken cookie.
  await page.route("**/admin/login", async (route) => {
    if (route.request().method() === "POST") {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ ok: true }),
        headers: {
          "Set-Cookie":
            "fb_admin=mock-token; Path=/; HttpOnly; SameSite=Strict",
        },
      });
    } else if (opts.loginPage) {
      await route.fulfill({
        status: 200,
        contentType: "text/html",
        body: readFileSync(indexPath, "utf-8"),
        headers: {
          "Set-Cookie": "fb_csrf=mocknonce123; Path=/; SameSite=Strict",
        },
      });
    } else {
      await route.continue();
    }
  });

  // POST /admin/api/change-password
  await page.route("**/admin/api/change-password", async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ ok: true, message: "Password changed" }),
    });
  });

  // Also mock POST /admin/config save (Tokens add-token flow uses POST /admin/config with form)
  await page.route(/\/admin\/config$/, async (route) => {
    if (route.request().method() === "POST") {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ ok: true, message: "Config saved" }),
      });
    } else {
      await route.continue();
    }
  });
}

export type PostedSetting = { key: string; value: string };
export type OverlaySeed = {
  key: string;
  value: string;
  source?: string;
  restart_only?: boolean;
  secret?: boolean;
};
export type OverlayMockOptions = {
  /** Serve GET /admin/api/settings with degraded:true (offline store). */
  degraded?: boolean;
  /** Non-200 POST status models a rejected write (default 200). */
  postStatus?: number;
  /** Explicit POST success message; defaults mirror the gateway. */
  postMessage?: string;
  /** Keys whose success message reads "<KEY> saved. It applies after restart." */
  restartOnly?: string[];
  /** Seed rows served by GET (saved-value notes + Reset targets). */
  seed?: OverlaySeed[];
};

const OVERLAY_RESTART_ONLY = ["LOG_LEVEL", "LOG_FORMAT", "HTTP_READ_TIMEOUT"];

/**
 * Stateful /admin/api/settings mock for the instant-save dashboard: GET
 * serves {settings, degraded}, POST upserts a db row (mirroring the
 * gateway's live vs restart-only messages plus the env-shadow suffix), and
 * DELETE /admin/api/settings/:key drops the row so saved-value resets
 * round-trip. Every successful POST is collected into `posted`, every
 * DELETE key into the returned `deleted` list.
 */
export async function mockSettingsOverlay(
  page: Page,
  posted: PostedSetting[] = [],
  opts: OverlayMockOptions = {},
): Promise<{ posted: PostedSetting[]; deleted: string[] }> {
  const deleted: string[] = [];
  const restartOnly: Record<string, true> = {};
  for (const k of opts.restartOnly ?? OVERLAY_RESTART_ONLY)
    restartOnly[k] = true;
  let live: OverlaySeed[] = (opts.seed ?? []).map((e) => ({ ...e }));
  await page.route("**/admin/api/settings", async (route) => {
    if (route.request().method() === "POST") {
      let key = "";
      let value = "";
      try {
        const parsed = JSON.parse(route.request().postData() ?? "{}");
        key = String(parsed.key ?? "");
        value = String(parsed.value ?? "");
      } catch {
        /* malformed payload: fall through to a 400 below */
      }
      posted.push({ key, value });
      const status = opts.postStatus ?? 200;
      if (status !== 200 || !key) {
        await route.fulfill({
          status: status !== 200 ? status : 400,
          contentType: "application/json",
          body: JSON.stringify({
            ok: false,
            message: "Setting rejected: boom",
            code: "invalid_setting",
          }),
        });
        return;
      }
      const prev = live.find((e) => e.key === key);
      const source = prev?.source === "env" ? "env" : "db";
      live = [
        ...live.filter((e) => e.key !== key),
        {
          key,
          value,
          source,
          restart_only: prev?.restart_only ?? restartOnly[key] === true,
          secret: prev?.secret ?? false,
        },
      ];
      let message =
        opts.postMessage ??
        (restartOnly[key] === true
          ? `${key} saved. It applies after restart.`
          : `${key} saved and applied live.`);
      if (source === "env") {
        message +=
          " Overridden by process env: the effective value still comes from the environment.";
      }
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          ok: true,
          code:
            restartOnly[key] === true
              ? "setting_restart_only"
              : "setting_saved",
          message,
          restart_only: restartOnly[key] === true ? [key] : [],
        }),
      });
    } else {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          settings: live,
          degraded: opts.degraded === true,
        }),
      });
    }
  });
  // DELETE /admin/api/settings/:key needs its own glob: Playwright * does
  // not cross /, so the base pattern above never sees the keyed path.
  await page.route("**/admin/api/settings/*", async (route) => {
    if (route.request().method() !== "DELETE") {
      await route.continue();
      return;
    }
    const key = new URL(route.request().url()).pathname.split("/").pop() ?? "";
    deleted.push(key);
    live = live.filter((e) => e.key !== key);
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({
        ok: true,
        code: "setting_deleted",
        message: "Saved value removed.",
      }),
    });
  });
  return { posted, deleted };
}
