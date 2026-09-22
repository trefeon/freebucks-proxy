import { test, expect } from "@playwright/test";
import { loadFixtures, mockDashboard } from "./mocks.js";

// Real-world pack: every fixture carries production-shaped data (bridge
// cards, bans, locks, cooldowns, freebucks, streaks, traffic counters, peak
// pricing) so each page proves it renders the full backend contract.
const RW = "e2e/fixtures-realworld";
const admin = (hash: string) => `http://127.0.0.1:4173/admin/#${hash}`;

test.describe("real-world data", () => {
  test("overview: KPIs, both notices, peak window, bridge card", async ({
    page,
  }) => {
    const f = loadFixtures(RW);
    // Pin the peak window relative to now: the static fixture date would
    // otherwise age the live countdown into fallback text on later runs.
    const notices = JSON.parse(JSON.stringify(f.notices));
    notices.peak_hours.next_window_at = new Date(
      Date.now() + (19 * 3600 + 30) * 1000,
    ).toISOString();
    await mockDashboard(page, { ...f, notices });
    await page.goto(admin("overview"));
    await expect(page.getByText("Pool total")).toBeVisible();
    await expect(page.getByText("548")).toBeVisible();
    await expect(
      page.getByText("Official Upstream Announcement"),
    ).toBeVisible();
    await expect(page.getByText("DeepSeek peak pricing active")).toBeVisible();
    await expect(page.getByText(/Peak ends .*\(19h/)).toBeVisible();
    await expect(
      page
        .getByLabel("Client integration")
        .getByText("http://127.0.0.1:3457/v1"),
    ).toBeVisible();
  });

  test("peak badge ticks live instead of freezing at the fetch value", async ({
    page,
  }) => {
    const f = loadFixtures(RW);
    const notices = JSON.parse(JSON.stringify(f.notices));
    // Live-shaped row: window ends 75s out so the badge must count down.
    notices.peak_hours.next_window_at = new Date(
      Date.now() + 75_000,
    ).toISOString();
    await mockDashboard(page, f, { notices });
    await page.goto(admin("overview"));
    const badge = page.getByText(/Peak (starts|ends) /);
    await expect(badge).toBeVisible();
    const first = await badge.textContent();
    await page.waitForTimeout(2200);
    const second = await badge.textContent();
    expect(second).not.toEqual(first);
  });

  test("tokens: every account state + bridge clients", async ({ page }) => {
    const f = loadFixtures(RW);
    // Pin the banned account's cooldown 30d out: the static fixture date
    // would otherwise age past "Nd" into "expiring" and break the countdown
    // assert on later runs.
    const tokens = JSON.parse(JSON.stringify(f.tokens));
    const list = tokens.tokens ?? tokens;
    const banned = (Array.isArray(list) ? list : []).find(
      (t) => t.cooldown_active,
    );
    if (banned)
      banned.cooldown_until = new Date(Date.now() + 30 * 864e5).toISOString();
    await mockDashboard(page, { ...f, tokens });
    await page.goto(admin("tokens"));
    for (const n of [1, 2, 3, 4, 5]) {
      await expect(
        page.getByText(`Account #${n}`, { exact: true }).first(),
      ).toBeVisible();
    }
    await expect(page.getByText("BANNED (TEMPORARY)").first()).toBeVisible();
    // New contract: the desktop row carries the banned state in its Status
    // cell (exact case-sensitive match pins the cell text; line 75's loose
    // match could hit anywhere).
    const bannedRow = page.locator("table tbody tr", {
      hasText: "Account #4",
    });
    await expect(
      bannedRow.getByText("banned (temporary)", { exact: true }),
    ).toBeVisible();
    await expect(page.getByText("LOCKED").first()).toBeVisible();
    await expect(
      page.getByRole("button", { name: "Unlock" }).first(),
    ).toBeVisible();
    await expect(page.getByText("Bridge Clients")).toBeVisible();
    await expect(
      page.getByText(
        "2 active bridge client(s) relaying their own FreeBuff tokens",
      ),
    ).toBeVisible();
    await expect(page.getByText("Requests 37")).toBeVisible();
    await expect(page.getByText("SPEND TODAY")).toHaveCount(2);
    // Bridge cards render the Freebucks bar now (session quota bars gone).
    await expect(page.getByText("Daily").first()).toBeVisible();
    // Window reset clock: the fixture carries BOTH the vendor display string
    // ("15:04 Jan 2") and the absolute instant, and the absolute one renders —
    // re-anchored to the operator's zone. Asserted on the clock span because
    // Playwright matches text per text node, not per composed line.
    const windowClock = page
      .locator("span")
      .filter({ hasText: "Resets in" })
      .first();
    await expect(windowClock).toContainText(
      /Resets in\s+[\s\S]*Jan 1, \d{2}:\d{2} [AP]M \(.+\)/,
    );
    // Display-only window (no absolute stamp): the vendor clock renders with
    // the zone it was formatted in and is never read as an elapsed local
    // reset, so the refill-pending copy stays out of both windows.
    await expect(page.getByText(/15:04 Jan 2 \(.+\)/).first()).toBeVisible();
    await expect(page.getByText("Updating balance…")).toHaveCount(0);
    await expect(page.getByText("Banned — TEMPORARY")).toBeVisible();
    // Drawer: standing + session + pinned models for the trusted account.
    await page.locator("table tbody tr button[aria-expanded]").first().click();
    await expect(page.getByText("Trusted").first()).toBeVisible();
    await expect(page.getByText("stealth/ox-alpha").first()).toBeVisible();
  });

  test("tokens: spawn posts the picked model as JSON", async ({ page }) => {
    await mockDashboard(page, loadFixtures(RW));
    let posted: unknown = null;
    await page.route("**/admin/tokens/*/session", async (route) => {
      try {
        posted = route.request().postDataJSON();
      } catch {
        posted = null;
      }
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ ok: true, message: "created" }),
      });
    });
    await page.goto(admin("tokens"));
    // The per-token Dev Session toolbar renders with DEVTOOLS_ENABLED=true.
    await page.unroute("**/admin/api/config");
    await page.route("**/admin/api/config", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          env_content: "PORT=3457\nAUTH_TOKENS=tok0\nDEVTOOLS_ENABLED=true\n",
          has_env_file: true,
        }),
      });
    });
    await page.reload();
    const table = page.locator("table.fp-table");
    await table.locator('button[aria-label*="Expand details"]').first().click();
    await expect(table.getByText("Dev Session:")).toBeVisible();
    const picker = table.locator("select").first();
    await expect(picker.locator("option")).not.toHaveCount(0);
    const options = await picker
      .locator("option")
      .evaluateAll((els) =>
        els.map((el) => (el as HTMLOptionElement).value).filter(Boolean),
      );
    await picker.selectOption(options[1]);
    page.on("dialog", (d) => d.accept());
    await table.getByRole("button", { name: "Make Session" }).first().click();
    await expect
      .poll(() => posted, { timeout: 8000 })
      .toEqual({ model: options[1] });
  });

  test("quota: compact account rows plus shared reset strip", async ({
    page,
  }) => {
    await mockDashboard(page, loadFixtures(RW));
    await page.goto(admin("tokens"));
    await page.getByRole("button", { name: "Allowances" }).click();
    await expect(
      page.getByRole("heading", { name: "Accounts", exact: true }),
    ).toBeVisible();
    await expect(
      page.getByRole("heading", { name: "Account #1" }),
    ).toBeVisible();
    await expect(page.getByText("dev@example.com").first()).toBeVisible();
    // Vendor formatFreebucks rounds (7.5 -> 8); the archived Sept-6 reset_at
    // is stale, so the strip shows the refill-pending copy, not a countdown.
    await expect(page.getByText("Balance 8")).toBeVisible();
    await expect(page.getByText("Used 3 / 10")).toBeVisible();
    await expect(page.getByText("Used 42 / 300")).toBeVisible();
    // Row header line (issue #364): daily fraction · wallet · monthly.
    // The countdown renders once in the shared strip, never per row —
    // here the pending copy, since the archived reset already passed.
    await expect(
      page.locator('[data-testid="freebucks-header"]').first(),
    ).toContainText(
      /8\/10 Freebucks daily · 5 in wallet · \$258 monthly usage left/,
    );
    await expect(page.getByTestId("reset-strip")).toContainText(
      "Updating balance…",
    );
  });

  test("quota: models tab shows ids plus priced Freebucks suffix", async ({
    page,
  }) => {
    const f = loadFixtures(RW);
    const tokens = JSON.parse(JSON.stringify(f.tokens));
    const list = tokens.tokens ?? tokens;
    const first = Array.isArray(list) ? list[0] : tokens;
    first.freebucks.prices["upstage/solar-pro4"] = 0;
    tokens.unmetered_models = [
      { id: "upstage/solar-pro4", name: "Solar Pro 4" },
    ];
    await mockDashboard(page, f, { tokens });
    await page.goto(admin("plans"));
    await page.getByRole("button", { name: "Catalog" }).click();
    await expect(page.getByTestId("models-note")).toContainText(
      "identical for every account in the region",
    );
    // Cost-class badge is gone: no Free/Premium word renders on served rows.
    await expect(page.getByText("Free", { exact: true })).toHaveCount(0);
    await expect(page.getByText("Premium", { exact: true })).toHaveCount(0);
    // Bare ids render with the priced Freebucks suffix where a price exists.
    await expect(page.getByText("upstage/solar-pro4").first()).toBeVisible();
    await expect(page.getByText("0 Freebucks/hr").first()).toBeVisible();
  });
  test("models/logs/traces/metrics render production rows", async ({
    page,
  }) => {
    await mockDashboard(page, loadFixtures(RW));
    await page.goto(admin("plans"));
    await page.getByRole("button", { name: "Catalog" }).click();
    // Vendor-catalog copy tracks the tier catalog (the Labor-Day promo
    // notice and Free/Premium cost badges are gone from served rows).
    await expect(page.getByText("Smart & Fast").first()).toBeVisible();
    await expect(page.getByText("0 Freebucks/hr").first()).toBeVisible();
    await expect(page.getByText("20 Freebucks/hr").first()).toBeVisible();
    await expect(page.getByText("Referral grant").first()).toBeVisible();
    await expect(page.getByText("Referral only").first()).toBeVisible();
    await expect(page.getByText("paid plan").first()).toBeVisible();
    await expect(page.getByText("limited trial").first()).toBeVisible();
    // Served stat tells the truth about 13 rows.
    await expect(page.getByText("6 of 14")).toBeVisible();
    await expect(page.getByText("14 registered · 50 agents")).toBeVisible();
    // Five withdrawn rows name their replacement in both renderings.
    await expect(page.getByTestId("model-withdrawn")).toHaveCount(10);
    // The offer row shows the live campaign counts in both renderings.
    await expect(page.getByTestId("model-offer")).toHaveCount(2);
    await expect(page.getByTestId("model-offer").first()).toContainText(
      "3 of 10 sessions left",
    );
    // No "referral" badge renders anywhere on the tab.
    await expect(page.getByText("referral", { exact: true })).toHaveCount(0);
    await expect(page.getByText("low/high/max").first()).toBeVisible();
    await expect(page.getByText("Price").first()).toBeVisible();
    await page.goto(admin("activity"));
    await expect(page.getByText("2 model requests")).toBeVisible();
    await expect(page.getByText("502").first()).toBeVisible();
    await page.getByRole("button", { name: "Table" }).click();
    await expect(
      page.getByText("error handling request 1: upstream timeout"),
    ).toBeVisible();
    await expect(page.getByText("req_id=req-bbb2")).toHaveCount(4);
    await page.goto(admin("activity"));
    await page.getByRole("button", { name: "Traces" }).click();
    const traceTable = page.locator("table");
    await expect(
      traceTable.getByText("deepseek/deepseek-v4-flash"),
    ).toBeVisible();
    await expect(traceTable.getByText("upstream timeout")).toBeVisible();
    await expect(traceTable.getByText("acquire_ms")).toBeVisible();
    await page.goto(admin("activity"));
    await page.getByRole("button", { name: "Metrics" }).click();
    await expect(
      page.getByText("Account fleet activity").first(),
    ).toBeVisible();
    await expect(page.getByText("Requests (24h)").first()).toBeVisible();
  });

  test("pool renders traffic caps, hybrid bridge", async ({ page }) => {
    await mockDashboard(page, loadFixtures(RW));
    await page.goto(admin("tokens"));
    // Pool controls moved behind the Controls tab.
    await page.getByRole("button", { name: "Strategy" }).click();
    // Exact label match: the live catalog's RATE_LIMIT_BURST row documents
    // itself as "2 × RATE_LIMIT_PER_IP", so a substring locator is ambiguous
    // once the pack describes the real rows.
    await expect(
      page.getByText("RATE_LIMIT_PER_IP", { exact: true }),
    ).toBeVisible();
    // The pack must describe the live catalog: these pool rows render from
    // config-meta, and the superseded pack hid them (or lacked the key), so
    // these assertions fail when the pack drifts again.
    // QUOTA_AUTO_PROBE was excised with the Fase E prober removal, so the
    // MASQ queue-posture rows stand in as the catalog-rendered pool proof.
    await expect(
      page.getByText("SLOTS_PER_ACCOUNT", { exact: true }),
    ).toBeVisible();
    await expect(
      page.getByText("QUEUE_WAIT", { exact: true }).first(),
    ).toBeVisible();
    await expect(
      page.getByText("QUEUE_DEPTH", { exact: true }).first(),
    ).toBeVisible();
    await expect(
      page.getByText("RATE_LIMIT_BURST", { exact: true }),
    ).toBeVisible();
    await page.goto(admin("overview"));
    await expect(
      page.getByRole("heading", { name: "Client Integration" }),
    ).toBeVisible();
  });

  test("tokens: banned cooldown countdown survives on the mobile card", async ({
    page,
  }) => {
    // The desktop Usage cell is deleted; the per-token cooldown countdown
    // now renders only in the mobile card's usage block.
    await page.setViewportSize({ width: 390, height: 844 });
    const f = loadFixtures(RW);
    const tokens = JSON.parse(JSON.stringify(f.tokens));
    const list = tokens.tokens ?? tokens;
    const banned = (Array.isArray(list) ? list : []).find(
      (t) => t.cooldown_active,
    );
    if (banned)
      banned.cooldown_until = new Date(Date.now() + 30 * 864e5).toISOString();
    await mockDashboard(page, { ...f, tokens });
    await page.goto(admin("tokens"));
    await expect(page.getByText("Cooldown").first()).toBeVisible();
    await expect(
      page.getByText(/\d+d( \d+h)?\s+remaining/).first(),
    ).toBeVisible();
  });
});
