import { test, expect } from "@playwright/test";
import { loadFixtures, mockDashboard, mockSettingsOverlay } from "./mocks.js";
import type { PostedSetting } from "./mocks.js";

function maintenanceTokens() {
  return {
    mode: "pooled",
    in_bridge: false,
    show_bridge: false,
    bridge_tokens: 0,
    token_count: 3,
    has_tokens: true,
    maturity_enabled: true,
    maturity_window_start: "2026-09-12T06:45:00Z",
    maturity_window_end: "2026-09-12T07:00:00Z",
    tokens: [
      {
        index: 0,
        email: "warm@example.com",
        session_status: "active",
        locked: true,
        streak: 3,
        today_used: false,
        maturity: {
          enabled: true,
          target: 7,
          mode: "unmetered",
          badge: "Warming",
          slot: "2026-09-05T07:30:00Z",
          slot_day: "2026-09-05",
          last_touch: "2026-09-05T07:31:00Z",
          touch_day: "2026-09-05",
          last_action: "admit",
          last_result: "ok",
          last_advanced: "yes",
          effective_touch_model: "mimo/mimo-v2.5",
          auto_touch_model: "mimo/mimo-v2.5",
          auto_touch_reason: "auto:unmetered",
        },
      },
      {
        index: 1,
        email: "fresh@example.com",
        session_status: "active",
        locked: false,
      },
      {
        index: 2,
        email: "cool@example.com",
        session_status: "active",
        locked: false,
        streak: 1,
        today_used: false,
        maturity: {
          enabled: true,
          target: 7,
          mode: "unmetered",
          badge: "Warming",
          slot: "2026-09-05T07:30:00Z",
          slot_day: "2026-09-05",
          last_touch: "2026-09-06T07:31:00Z",
          touch_day: "2026-09-04",
          last_action: "",
          last_result: "skip:cooling",
          effective_touch_model: "mimo/mimo-v2.5",
          auto_touch_model: "mimo/mimo-v2.5",
          auto_touch_reason: "auto:unmetered",
        },
      },
    ],
  };
}

function maintenanceConfig() {
  return {
    env_content: "AUTH_TOKENS=a,b\nMATURITY_ENABLED=true\n",
    has_env_file: true,
    effective: [
      { key: "MATURITY_ENABLED", value: "true", secret: false },
      { key: "MATURITY_TOUCH_MODEL", value: "auto", secret: false },
    ],
  };
}

async function gotoWarming(page) {
  await page.goto("http://127.0.0.1:4173/admin/#tokens");
  await page.getByRole("button", { name: "Warming" }).click();
  await expect(
    page.getByRole("heading", { name: "Pool", exact: true }),
  ).toBeVisible();
  await expect(page.getByRole("button", { name: "Warming" })).toHaveAttribute(
    "aria-pressed",
    "true",
  );
}

test.describe("streak maintenance", () => {
  test("board carries the switch plus touch-model row", async ({ page }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    await page.unroute("**/admin/api/tokens*");
    await page.route("**/admin/api/tokens*", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(maintenanceTokens()),
      });
    });
    await page.unroute("**/admin/api/config");
    await page.route("**/admin/api/config", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(maintenanceConfig()),
      });
    });

    await gotoWarming(page);
    await expect(
      page.getByRole("heading", { name: "Streak Maintenance" }),
    ).toBeVisible();
    // Kill-switch plus the touch-model select under it. The master toggle
    // reads unambiguously: Streak maintenance (nightly touches on/off).
    // Still no Touch-now buttons anywhere.
    await expect(
      page.getByRole("switch", { name: "Streak maintenance" }),
    ).toBeVisible();
    await expect(
      page.getByText("Streak maintenance", { exact: true }),
    ).toBeVisible();
    await expect(page.getByLabel("MATURITY_TOUCH_MODEL")).toBeVisible();
    await expect(page.getByLabel("Global touch model")).toHaveCount(0);
    await expect(page.getByRole("button", { name: "Touch now" })).toHaveCount(
      0,
    );
    // Dry-run is gone: no switch, no badge, no copy anywhere on the board.
    await expect(
      page.getByRole("switch", { name: "MATURITY_DRY_RUN" }),
    ).toHaveCount(0);
    await expect(page.getByText("Dry run")).toHaveCount(0);
    await expect(
      page.getByText("Dry run (probe only, claims nothing)"),
    ).toHaveCount(0);
    // Fixed pre-reset window copy + countdown.
    await expect(
      page.getByText("Nightly window 23:45–00:00 Pacific"),
    ).toBeVisible();
    await expect(page.getByLabel("Next maintenance run")).toBeVisible();
    // One row per account: touched with the resolved model id, skipped
    // with the exact ledger reason, pending without a ledger.
    await expect(page.getByText("Touched").first()).toBeVisible();
    await expect(
      page.locator("code", { hasText: "mimo/mimo-v2.5" }).first(),
    ).toBeVisible();
    await expect(page.getByText("Skipped · skip:cooling")).toBeVisible();
    await expect(page.getByText("Pending").first()).toBeVisible();
    // Last-run ledger summary: time, touched, skipped with reasons.
    await expect(page.getByLabel("Last maintenance run")).toContainText(
      /touched\s+1/,
    );
    await expect(page.getByLabel("Last maintenance run")).toContainText(
      /skipped\s+1/,
    );
    await expect(page.getByLabel("Last maintenance run")).toContainText(
      "skip:cooling",
    );
  });

  test("today-used rows explain when, where, and the reset", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    await page.unroute("**/admin/api/tokens*");
    await page.route("**/admin/api/tokens*", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          mode: "pooled",
          token_count: 1,
          has_tokens: true,
          maturity_enabled: true,
          maturity_window_start: "2026-09-12T06:45:00Z",
          maturity_window_end: "2026-09-12T07:00:00Z",
          tokens: [
            {
              index: 0,
              email: "used@example.com",
              session_status: "active",
              locked: false,
              requests_per_day: 0,
              last_usage: "2026-09-11T15:00:00Z",
              maturity: {
                enabled: true,
                target: 7,
                mode: "unmetered",
                badge: "Warming",
                slot: "2026-09-11T06:30:00Z",
                slot_day: "2026-09-11",
                last_touch: "2026-09-11T06:56:00Z",
                last_action: "",
                last_result: "skip:today-used",
                effective_touch_model: "upstage/solar-pro4",
                auto_touch_model: "upstage/solar-pro4",
              },
            },
          ],
        }),
      });
    });
    await gotoWarming(page);
    const row = page
      .getByText("Account #1")
      .locator("..")
      .locator("..")
      .locator("..");
    await expect(row.getByText(/day already used/)).toBeVisible();
    await expect(row.getByText(/last activity Sep 11/)).toBeVisible();
    await expect(row.getByText(/used outside this proxy/)).toBeVisible();
    await expect(row.getByText(/skip:today-used/)).toBeVisible();
    // Reset-anchored countdown and Pacific-day last run.
    await expect(page.getByLabel("Next maintenance run")).toContainText(
      /reset in/,
    );
    await expect(page.getByLabel("Last maintenance run")).toContainText(
      /Sep 10 Pacific day/,
    );
  });

  test("touch-only rows read as automation, not outside use", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    await page.unroute("**/admin/api/tokens*");
    await page.route("**/admin/api/tokens*", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          mode: "pooled",
          token_count: 1,
          has_tokens: true,
          maturity_enabled: true,
          maturity_window_start: "2026-09-12T06:45:00Z",
          maturity_window_end: "2026-09-12T07:00:00Z",
          tokens: [
            {
              index: 0,
              email: "auto@example.com",
              session_status: "active",
              locked: false,
              requests_per_day: 0,
              maturity: {
                enabled: true,
                target: 7,
                mode: "unmetered",
                badge: "Warming",
                slot: "2026-09-11T06:30:00Z",
                slot_day: "2026-09-11",
                last_touch: "2026-09-11T06:56:00Z",
                last_action: "",
                last_result: "skip:today-used",
                effective_touch_model: "upstage/solar-pro4",
                auto_touch_model: "upstage/solar-pro4",
              },
            },
          ],
        }),
      });
    });
    await gotoWarming(page);
    const row = page
      .getByText("Account #1")
      .locator("..")
      .locator("..")
      .locator("..");
    await expect(row.getByText(/nightly touch only/)).toBeVisible();
    // Reset-anchored countdown and Pacific-day last run.
    await expect(page.getByLabel("Next maintenance run")).toContainText(
      /reset in/,
    );
    await expect(page.getByLabel("Last maintenance run")).toContainText(
      /Sep 10 Pacific day/,
    );
  });

  test("universal switch writes the global kill-switch", async ({ page }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    await page.unroute("**/admin/api/tokens*");
    await page.route("**/admin/api/tokens*", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(maintenanceTokens()),
      });
    });

    const posts: Array<{ url: string; body: string }> = [];
    await page.route("**/admin/api/settings", async (route) => {
      if (route.request().method() === "POST") {
        posts.push({
          url: route.request().url(),
          body: route.request().postData() ?? "",
        });
      }
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ ok: true, message: "saved." }),
      });
    });

    await gotoWarming(page);
    const saveReq = page.waitForRequest(
      (r) => r.method() === "POST" && r.url().includes("/admin/api/settings"),
    );
    await page.getByRole("switch", { name: "Streak maintenance" }).click();
    await saveReq;
    expect(posts[0].body).toContain("MATURITY_ENABLED");
  });

  test("accounts rows carry no maturity controls or chips", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    await page.unroute("**/admin/api/tokens*");
    await page.route("**/admin/api/tokens*", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(maintenanceTokens()),
      });
    });

    // Accounts tab (default): rows show serving status only — no maturity
    // toggle, no Touch-now, no Cold/Warming/Not-enrolled chip. The streak
    // day count stays as pure info where shown.
    await page.goto("http://127.0.0.1:4173/admin/#tokens");
    await expect(
      page.getByRole("heading", { name: "Pool", exact: true }),
    ).toBeVisible();
    await expect(
      page.getByRole("switch", { name: "Maturity for Account #1" }),
    ).toHaveCount(0);
    await expect(
      page.getByRole("switch", { name: "Maturity for Account #2" }),
    ).toHaveCount(0);
    await expect(page.getByRole("button", { name: "Touch now" })).toHaveCount(
      0,
    );
    await expect(page.getByText("Locked").first()).toBeVisible();
  });

  test("warming tab wires the touch model instant-save", async ({ page }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    await page.unroute("**/admin/api/tokens*");
    await page.route("**/admin/api/tokens*", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(maintenanceTokens()),
      });
    });
    await page.unroute("**/admin/api/config");
    await page.route("**/admin/api/config", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(maintenanceConfig()),
      });
    });
    const posted: PostedSetting[] = [];
    await mockSettingsOverlay(page, posted);
    await gotoWarming(page);
    // The streak knob on the Warming card, under the kill-switch:
    // MATURITY_TOUCH_MODEL as the Auto select, instant-saving to the
    // overlay on edit. No dry-run switch exists anymore.
    await expect(
      page.getByRole("switch", { name: "MATURITY_DRY_RUN" }),
    ).toHaveCount(0);
    const select = page.getByLabel("MATURITY_TOUCH_MODEL");
    await expect(select).toBeVisible();
    // Picking a model auto-POSTs the overlay path with the key (debounced).
    const saveReq = page.waitForRequest(
      (r) => r.method() === "POST" && r.url().includes("/admin/api/settings"),
      { timeout: 10_000 },
    );
    await select.selectOption("upstage/solar-pro4");
    await saveReq;
    await expect
      .poll(
        () => posted.filter((p) => p.key === "MATURITY_TOUCH_MODEL").length,
        {
          timeout: 10_000,
        },
      )
      .toBeGreaterThan(0);
  });

  test("touch-model select shows the saved model after save", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    await page.unroute("**/admin/api/tokens*");
    await page.route("**/admin/api/tokens*", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(maintenanceTokens()),
      });
    });
    // Stateful config mock: the overlay POST updates the served effective
    // value, so the post-save refetch returns what was saved (like the
    // gateway instead of a frozen fixture).
    let touchSaved = "";
    await page.unroute("**/admin/api/config");
    await page.route("**/admin/api/config", async (route) => {
      const cfg = maintenanceConfig();
      cfg.effective = cfg.effective.map((e) =>
        e.key === "MATURITY_TOUCH_MODEL" ? { ...e, value: touchSaved } : e,
      );
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(cfg),
      });
    });
    const posted: PostedSetting[] = [];
    await mockSettingsOverlay(page, posted);
    await gotoWarming(page);
    const select = page.getByLabel("MATURITY_TOUCH_MODEL");
    await expect(select).toBeVisible();
    // Default state reads Auto.
    await expect(select).toHaveValue("auto");
    // Pick a non-default served model: the row instant-saves on edit, then
    // reload: the select must show the saved model, never the old default.
    // Reloading (instead of trusting the post-save refetch) also dodges the
    // refetch/edit race where a late refetch clobbers a newer draft. The
    // request watcher arms before the edit so the debounced POST cannot slip
    // past it.
    const saveReq = page.waitForRequest(
      (r) => r.method() === "POST" && r.url().includes("/admin/api/settings"),
      { timeout: 10_000 },
    );
    await select.selectOption("upstage/solar-pro4");
    // Wait for the draft edit to flush to the row (the select's title binds
    // the same derived draft the instant-save posts).
    await expect(select).toHaveAttribute("title", "upstage/solar-pro4");
    await saveReq;
    await expect
      .poll(
        () => posted.filter((p) => p.key === "MATURITY_TOUCH_MODEL").length,
        { timeout: 10_000 },
      )
      .toBeGreaterThan(0);
    // Mirror the saved value into the stateful config mock so the post-save
    // refetch returns what was saved (like the gateway).
    touchSaved = posted[posted.length - 1]?.value ?? "";
    await page.reload();
    await gotoWarming(page);
    await expect(page.getByLabel("MATURITY_TOUCH_MODEL")).toHaveValue(
      "upstage/solar-pro4",
    );
    // Back to Auto canonicalizes to the empty catalog default on the
    // overlay path; reload again and the select still reads Auto.
    const reselected = page.getByLabel("MATURITY_TOUCH_MODEL");
    const autoReq = page.waitForRequest(
      (r) => r.method() === "POST" && r.url().includes("/admin/api/settings"),
      { timeout: 10_000 },
    );
    await reselected.selectOption("auto");
    await expect(reselected).toHaveAttribute("title", "auto");
    await autoReq;
    await expect
      .poll(() => posted.length, { timeout: 10_000 })
      .toBeGreaterThan(1);
    touchSaved = "";
    await page.reload();
    await gotoWarming(page);
    await expect(page.getByLabel("MATURITY_TOUCH_MODEL")).toHaveValue("auto");
  });

  test("touch-model select keeps a saved model the catalog omits", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    await page.unroute("**/admin/api/tokens*");
    await page.route("**/admin/api/tokens*", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(maintenanceTokens()),
      });
    });
    // The saved model retired from the served catalog (stale snapshot):
    // the select must still display the saved value, never fall back to
    // the Auto default.
    await page.unroute("**/admin/api/config");
    await page.route("**/admin/api/config", async (route) => {
      const cfg = maintenanceConfig();
      cfg.effective = cfg.effective.map((e) =>
        e.key === "MATURITY_TOUCH_MODEL"
          ? { ...e, value: "retired/old-model" }
          : e,
      );
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(cfg),
      });
    });
    await gotoWarming(page);
    await expect(page.getByLabel("MATURITY_TOUCH_MODEL")).toHaveValue(
      "retired/old-model",
    );
  });

  test("no per-account target stepper or model select remains", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    await page.unroute("**/admin/api/tokens*");
    await page.route("**/admin/api/tokens*", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(maintenanceTokens()),
      });
    });
    await gotoWarming(page);
    await expect(page.getByLabel("Streak target for Account #1")).toHaveCount(
      0,
    );
    await expect(page.getByLabel("Touch model for Account #1")).toHaveCount(0);
    await expect(page.getByLabel("Touch model for Account #2")).toHaveCount(0);
  });

  test("maintenance warns while the global kill-switch is off", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    await page.unroute("**/admin/api/tokens*");
    await page.route("**/admin/api/tokens*", async (route) => {
      const body = maintenanceTokens();
      body.maturity_enabled = false;
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(body),
      });
    });
    await page.unroute("**/admin/api/config");
    await page.route("**/admin/api/config", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          env_content: "AUTH_TOKENS=a,b\nMATURITY_ENABLED=false\n",
          has_env_file: true,
          effective: [
            { key: "MATURITY_ENABLED", value: "false", secret: false },
          ],
        }),
      });
    });

    await gotoWarming(page);
    await expect(
      page.getByText("Maturity automation is globally off"),
    ).toBeVisible();
  });

  test("board renders rows without any event timeline", async ({ page }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    await page.unroute("**/admin/api/tokens*");
    await page.route("**/admin/api/tokens*", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(maintenanceTokens()),
      });
    });
    await gotoWarming(page);
    // No per-account event list anywhere on the board.
    await expect(
      page.getByRole("list", { name: "Maturity history for Account #1" }),
    ).toHaveCount(0);
    await expect(
      page.getByRole("button", { name: /Show \d+ more|Show less/ }),
    ).toHaveCount(0);
    // Rows, statuses, and the operator lock stay readable (no badge chips).
    await expect(page.getByText("Touched").first()).toBeVisible();
    await expect(page.getByText("Pending").first()).toBeVisible();
    await expect(page.getByText("Locked").first()).toBeVisible();
    await expect(
      page.locator("code", { hasText: "mimo/mimo-v2.5" }).first(),
    ).toBeVisible();
  });

  test("board shows the last-run ledger summary", async ({ page }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    await page.unroute("**/admin/api/tokens*");
    await page.route("**/admin/api/tokens*", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(maintenanceTokens()),
      });
    });
    await gotoWarming(page);
    // Last-run ledger: time, touched, skipped with exact reasons.
    const ledger = page.getByLabel("Last maintenance run");
    await expect(ledger).toContainText(/touched\s+1/);
    await expect(ledger).toContainText(/skipped\s+1/);
    await expect(ledger).toContainText("skip:cooling");
  });

  test("shared harness renders the seeded board without clipping", async ({
    page,
  }) => {
    const f = loadFixtures();
    await mockDashboard(page, f);
    await gotoWarming(page);
    // Token #1 carries a maturity object in the shared tokens fixture, so
    // the board renders with no bespoke mocks — and no event timeline.
    await expect(
      page.getByRole("list", { name: "Maturity history for Account #1" }),
    ).toHaveCount(0);
    await expect(page.getByLabel("Last maintenance run")).toBeVisible();
    // Long model ids must wrap instead of clipping header actions:
    // no card header may overflow horizontally.
    const overflow = await page.evaluate(
      () =>
        Array.from(document.querySelectorAll("section.fp-card header")).filter(
          (el) => el.scrollWidth > el.clientWidth + 1,
        ).length,
    );
    expect(overflow).toBe(0);
  });
});
