import { test, expect } from "@playwright/test";
import type { Page } from "@playwright/test";
import { loadFixtures, mockDashboard } from "./mocks.js";

/**
 * Pool → Accounts (TokenTable/TokenCard) geometry contract.
 *
 * The table is `table-auto` with a single slack-absorbing column (Account);
 * every other column hugs its content. These pins hold the shape that a
 * regression here breaks silently:
 *
 *  - an idle account (no live session) shows a bare em dash in Instance, and
 *    that column hugs it instead of reserving a band (the pre-#569
 *    fixed width floor on that column squeezed Usage to 36px of content,
 *    wrapped "3 msgs 24h" over five lines and inflated the row to 133px),
 *  - rows stay slim,
 *  - the table never pushes its card into horizontal scroll at any supported
 *    desktop width, and the stacked mobile cards never scroll at 390,
 *  - a live session keeps its model badge and the inline Drop Session control
 *    in the Instance cell (td 3), still clickable.
 *
 * Fixtures: the parity-guarded `e2e/fixtures` pack (loadFixtures default), the
 * same pack the shared harness uses; tokens are overridden per test.
 */

const WIDTHS = [1440, 1280, 1024] as const;
const MOBILE = { width: 390, height: 844 } as const;

/** Row shape shared with interactions.spec.ts (idle by default). */
function tokenRow(idx: number, over: Record<string, unknown> = {}) {
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

function tokensPayload(tokens: Array<Record<string, unknown>>) {
  return {
    mode: "pooled",
    in_bridge: false,
    bridge_tokens: 0,
    token_count: tokens.length,
    has_tokens: true,
    tokens,
    bridge_token_cards: [],
  };
}

function liveToken() {
  return tokenRow(0, {
    session_status: "active",
    session_instance: "inst-ox99-abcdefghijklmnop",
    session_model: "stealth/ox-alpha",
    session_remaining_seconds: 4620,
    messages_24h: 3,
    active_runs: 3,
    requests: 148,
  });
}

async function gotoPool(
  page: Page,
  tokens: Array<Record<string, unknown>>,
  width: number,
) {
  const f = loadFixtures();
  await mockDashboard(page, f, { tokens: tokensPayload(tokens) });
  await page.setViewportSize({ width, height: 900 });
  await page.goto("/admin/#tokens");
  const table = page.locator("table.fp-table");
  await expect(
    table.locator("tbody tr").first().getByText("Account #1"),
  ).toBeVisible({ timeout: 15000 });
  return table;
}

/** Same mock, for viewports where the desktop table is hidden (cards path). */
async function gotoPoolCards(
  page: Page,
  tokens: Array<Record<string, unknown>>,
) {
  const f = loadFixtures();
  await mockDashboard(page, f, { tokens: tokensPayload(tokens) });
  await page.setViewportSize({ ...MOBILE });
  await page.goto("/admin/#tokens");
  const cards = page.locator("div.lg\\:hidden").first();
  await expect(cards.getByText("Account #1").first()).toBeVisible({
    timeout: 15000,
  });
  return cards;
}

type ColumnMetrics = {
  label: string;
  headerWidth: number;
  cellWidth: number;
  cellTextWidth: number;
  cellText: string;
  cellOverflow: number;
};

type TableMetrics = {
  tableWidth: number;
  wrapperOverflow: number;
  docOverflow: number;
  rowHeight: number;
  columns: ColumnMetrics[];
};

/** Geometry of the first data row plus the scroll state of table and page. */
async function tableMetrics(page: Page): Promise<TableMetrics> {
  return page.evaluate(() => {
    const table = document.querySelector("table.fp-table");
    const firstRow = table?.querySelector("tbody tr");
    if (!table || !firstRow) throw new Error("pool table not rendered");
    const wrap = table.closest("div.overflow-x-auto");
    const textWidth = (el: Element) => {
      const range = document.createRange();
      range.selectNodeContents(el);
      return range.getBoundingClientRect().width;
    };
    const headers = Array.from(table.querySelectorAll("thead th"));
    const cells = Array.from(firstRow.querySelectorAll("td"));
    return {
      tableWidth: table.getBoundingClientRect().width,
      wrapperOverflow: wrap ? wrap.scrollWidth - wrap.clientWidth : 0,
      docOverflow:
        document.documentElement.scrollWidth -
        document.documentElement.clientWidth,
      rowHeight: firstRow.getBoundingClientRect().height,
      columns: headers.map((th, i) => {
        const cell = cells[i];
        const label = (th.textContent || "").trim();
        return {
          label,
          headerWidth: th.getBoundingClientRect().width,
          // The header label width is the floor for a hug column when the
          // cell itself holds less (a bare em dash, for instance).
          cellWidth: cell ? cell.getBoundingClientRect().width : 0,
          cellTextWidth: cell ? textWidth(cell) : 0,
          cellText: cell
            ? (cell.textContent || "").replace(/\s+/g, " ").trim()
            : "",
          cellOverflow: cell ? cell.scrollWidth - cell.clientWidth : 0,
        };
      }),
    };
  });
}

function column(metrics: TableMetrics, label: string): ColumnMetrics {
  const found = metrics.columns.find((c) => c.label === label);
  if (!found) throw new Error(`column ${label} missing`);
  return found;
}

/** Cell padding (8px per side) plus rounding slack for the hug check. */
const HUG_SLACK = 32;

test.describe("Pool accounts table geometry", () => {
  test("idle accounts hug the Instance column instead of reserving a band", async ({
    page,
  }) => {
    await gotoPool(
      page,
      [tokenRow(0, { email: "alpha@example.com" }), tokenRow(1)],
      1440,
    );
    const m = await tableMetrics(page);

    // The pre-change floor plus padding (measured range) reserved a wide
    // band for an em dash.
    const instance = column(m, "Instance");
    expect(instance.cellText).toBe("—");
    expect(instance.cellTextWidth).toBeLessThan(20);
    expect(instance.cellWidth).toBeLessThan(140);

    // No text column is wider than the wider of its header label and its own
    // content (the Account column absorbs the card's slack by design).
    for (const label of ["Status", "Instance", "Usage"]) {
      const col = column(m, label);
      const contentBound =
        Math.max(col.headerWidth, col.cellTextWidth) + HUG_SLACK;
      expect(col.cellWidth, `${label} column width`).toBeLessThanOrEqual(
        contentBound,
      );
      expect(col.cellOverflow, `${label} cell overflow`).toBe(0);
    }
  });

  test("a one-account table stays one slim row", async ({ page }) => {
    for (const width of WIDTHS) {
      await gotoPool(
        page,
        [tokenRow(0, { email: "alpha@example.com" })],
        width,
      );
      const m = await tableMetrics(page);
      // Pre-change: 133px (Usage wrapped to five lines inside a 60px column).
      expect(m.rowHeight, `row height at ${width}`).toBeLessThanOrEqual(64);
    }
  });

  test("no horizontal scroll at 1440/1280/1024, idle or live", async ({
    page,
  }) => {
    const payloads: Record<string, Array<Record<string, unknown>>> = {
      idle: [tokenRow(0), tokenRow(1)],
      live: [liveToken(), tokenRow(1)],
    };
    for (const width of WIDTHS) {
      for (const [name, tokens] of Object.entries(payloads)) {
        await gotoPool(page, tokens, width);
        const m = await tableMetrics(page);
        expect(
          m.wrapperOverflow,
          `${name} table at ${width}`,
        ).toBeLessThanOrEqual(1);
        expect(m.docOverflow, `${name} page at ${width}`).toBeLessThanOrEqual(
          1,
        );
      }
    }
  });

  test("stacked mobile cards stay scroll-free with usable tap targets", async ({
    page,
  }) => {
    const cards = await gotoPoolCards(page, [liveToken(), tokenRow(1)]);

    const overflow = await cards.evaluate(
      (el) => el.scrollWidth - el.clientWidth,
    );
    expect(overflow).toBeLessThanOrEqual(1);
    const docOverflow = await page.evaluate(
      () =>
        document.documentElement.scrollWidth -
        document.documentElement.clientWidth,
    );
    expect(docOverflow).toBeLessThanOrEqual(1);

    const small = await cards.evaluate((el) =>
      Array.from(el.querySelectorAll("button"))
        .filter((b) => b.getBoundingClientRect().height > 0)
        .map((b) => ({
          label: (b.getAttribute("aria-label") || b.textContent || "").trim(),
          h: b.getBoundingClientRect().height,
          w: b.getBoundingClientRect().width,
        }))
        .filter((b) => b.h < 32 || b.w < 32)
        .map((b) => `${b.label}:${Math.round(b.w)}x${Math.round(b.h)}`),
    );
    expect(small).toEqual([]);
  });

  test("live session keeps badge + inline Drop Session, and drops on click", async ({
    page,
  }) => {
    for (const width of [1440, 1280, 1024]) {
      const table = await gotoPool(page, [liveToken(), tokenRow(1)], width);
      const row = table.locator("tbody tr").filter({ hasText: "Account #1" });
      const instanceCell = row.locator("td").nth(3);
      await expect(
        instanceCell.getByText("stealth/ox-alpha"),
        `model badge at ${width}`,
      ).toBeVisible();
      const drop = instanceCell.getByRole("button", { name: "Drop Session" });
      await expect(drop, `drop control at ${width}`).toBeVisible();
      // #564 keeps the kill switch in the Instance cell, never in Status.
      await expect(
        row.locator("td").nth(2).getByRole("button", { name: "Drop Session" }),
      ).toHaveCount(0);
    }

    // Reachable and wired: the click runs the confirm flow and POSTs the
    // per-token drop-session endpoint.
    const dropped: string[] = [];
    await page.route("**/admin/tokens/*/drop-session", async (route) => {
      dropped.push(route.request().url());
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ ok: true }),
      });
    });
    page.on("dialog", (dialog) => void dialog.accept());
    await page
      .locator("table.fp-table tbody tr")
      .filter({ hasText: "Account #1" })
      .locator("td")
      .nth(3)
      .getByRole("button", { name: "Drop Session" })
      .click();
    await expect.poll(() => dropped.length, { timeout: 5000 }).toBe(1);
    expect(dropped[0]).toContain("/admin/tokens/0/drop-session");
  });
});
