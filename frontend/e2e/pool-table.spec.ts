import { test, expect } from "@playwright/test";
import type { Page } from "@playwright/test";
import { loadFixtures, mockDashboard } from "./mocks.js";
import { preciousToken, tokenRow, tokensPayload } from "./mock-data.js";

/**
 * Pool → Accounts (TokenTable/TokenCard) geometry contract.
 *
 * The table is `table-auto` with a single slack-absorbing column (Account);
 * every other column hugs its content. These pins hold the shape that a
 * regression here breaks silently:
 *
 *  - an idle account (no live session) shows a bare em dash in Instance, and
 *    that column hugs it instead of reserving a band (the pre-#569
 *    fixed width floor on that column squeezed its neighbour to 36px of
 *    content, wrapped its text over five lines and inflated the row to 133px),
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

/** Live session holder (MASQ precious shape) shared via mock-data.ts. */
function liveToken() {
  return preciousToken(0);
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
  test("idle accounts show a bare dash while Instance absorbs the slack", async ({
    page,
  }) => {
    await gotoPool(
      page,
      [tokenRow(0, { email: "alpha@example.com" }), tokenRow(1)],
      1440,
    );
    const m = await tableMetrics(page);

    // Idle Instance holds a bare em dash with no truncation — but since the
    // Account→Instance flip it is the slack absorber, so it no longer hugs.
    const instance = column(m, "Instance");
    expect(instance.cellText).toBe("—");
    expect(instance.cellTextWidth).toBeLessThan(20);
    expect(instance.cellOverflow).toBe(0);

    // No other text column is wider than the wider of its header label and
    // its own content (Account hugs its first line by design; Instance takes
    // the card's slack).
    const col = column(m, "Status");
    const contentBound =
      Math.max(col.headerWidth, col.cellTextWidth) + HUG_SLACK;
    expect(col.cellWidth, "Status column width").toBeLessThanOrEqual(
      contentBound,
    );
    expect(col.cellOverflow, "Status cell overflow").toBe(0);
  });

  test("a one-account table stays one slim row", async ({ page }) => {
    for (const width of WIDTHS) {
      await gotoPool(
        page,
        [tokenRow(0, { email: "alpha@example.com" })],
        width,
      );
      const m = await tableMetrics(page);
      // Pre-change: 133px (a squeezed column wrapped its text to five lines inside 60px).
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
  test("account column hugs the first line; the email truncates to it", async ({
    page,
  }) => {
    const longEmail = "very-long-operator-address-for-cap-check@example.com";
    const longId = "inst-ox99-abcdefghijklmnop-qrstuvwxyz-0123456789";
    // 15 rows so the widest first line (Account #15 + longest streak text)
    // defines the hugged column; row 1 carries the long email + live session.
    const tokens = Array.from({ length: 15 }, (_, i) =>
      tokenRow(i, i === 14 ? { streak: 128 } : {}),
    );
    tokens[0] = tokenRow(0, {
      email: longEmail,
      session_status: "active",
      session_instance: longId,
      session_model: "stealth/ox-alpha",
      session_remaining_seconds: 4620,
    });
    for (const width of [1280, 1024]) {
      const table = await gotoPool(page, tokens, width);
      const row = table.locator("tbody tr").filter({ hasText: "Account #1" });
      const accountCell = row.locator("td").nth(1);
      const firstLine = accountCell
        .locator('span:has-text("Account #1")')
        .locator("xpath=..");
      const emailSpan = accountCell.locator(`span[title="${longEmail}"]`);
      await expect(emailSpan).toBeVisible();
      // (a) the email is actually truncated to the hugged width, with the
      // full address kept in `title`.
      await expect(emailSpan).toHaveAttribute("title", longEmail);
      const emailGeom = await emailSpan.evaluate((el) => ({
        trunc: el.scrollWidth - el.clientWidth,
      }));
      expect(emailGeom.trunc, `email truncated at ${width}`).toBeGreaterThan(2);
      // (b) the first line (Account #N + streak badge) never wraps: it is
      // what defines the column width.
      const firstGeom = await firstLine.evaluate((el) => ({
        overflow: el.scrollWidth - el.clientWidth,
      }));
      expect(
        firstGeom.overflow,
        `first line fits at ${width}`,
      ).toBeLessThanOrEqual(1);
      // (c) the column takes the widest first line, not the email: every
      // account cell matches the widest row's first line plus padding.
      const widths = await table.evaluate(() => {
        const rows = Array.from(
          document.querySelectorAll("table.fp-table tbody tr"),
        );
        const firstOf = (r) => {
          const cell = r.querySelectorAll("td")[1];
          const name = Array.from(cell?.querySelectorAll("span") ?? []).find(
            (s) => (s.textContent || "").includes("Account #"),
          );
          return name?.parentElement ?? null;
        };
        const firsts = rows.map((r) => {
          const first = firstOf(r);
          return first ? first.getBoundingClientRect().width : 0;
        });
        const cells = rows.map(
          (r) => r.querySelectorAll("td")[1].getBoundingClientRect().width,
        );
        return {
          widestFirst: Math.max(...firsts),
          widestCell: Math.max(...cells),
        };
      });
      expect(
        widths.widestCell,
        `account column hugs widest first line at ${width}`,
      ).toBeLessThanOrEqual(widths.widestFirst + 40);
      // (d) Instance ELLIPSIS-truncates the long id (no-scroll contract):
      // full text stays in `title`, it never wraps, and the model chip +
      // Drop Session control stay visible beside it.
      const instanceCell = row.locator("td").nth(3);
      const idCode = instanceCell.locator(`code[title="${longId}"]`);
      await expect(idCode).toHaveText(longId);
      await expect(idCode).toHaveAttribute("title", longId);
      const idGeom = await idCode.evaluate((el) => ({
        overflow: el.scrollWidth - el.clientWidth,
        wrap: getComputedStyle(el).whiteSpace,
        ellipsis: getComputedStyle(el).textOverflow,
      }));
      expect(idGeom.wrap, "instance id never wraps").toBe("nowrap");
      expect(idGeom.ellipsis, "instance id ellipsizes").toBe("ellipsis");
      expect(
        idGeom.overflow,
        `instance id truncated at ${width}`,
      ).toBeGreaterThan(2);
      await expect(
        instanceCell.getByText("stealth/ox-alpha"),
        `model badge at ${width}`,
      ).toBeVisible();
      await expect(
        instanceCell.getByRole("button", { name: "Drop Session" }),
        `drop control at ${width}`,
      ).toBeVisible();
    }
  });

  test("idle rows show a bare dash with no Drop Session control", async ({
    page,
  }) => {
    const table = await gotoPool(page, [tokenRow(0)], 1280);
    const row = table.locator("tbody tr").filter({ hasText: "Account #1" });
    const instanceCell = row.locator("td").nth(3);
    expect(
      ((await instanceCell.textContent()) || "").replace(/\s+/g, " ").trim(),
    ).toBe("—");
    await expect(
      instanceCell.getByRole("button", { name: "Drop Session" }),
    ).toHaveCount(0);
  });

  test("live rows show full id plus model chip plus Drop Session", async ({
    page,
  }) => {
    const table = await gotoPool(page, [liveToken()], 1280);
    const row = table.locator("tbody tr").filter({ hasText: "Account #1" });
    const instanceCell = row.locator("td").nth(3);
    await expect(
      instanceCell.getByText("inst-ox99-abcdefghijklmnop"),
    ).toBeVisible();
    await expect(instanceCell.getByText("stealth/ox-alpha")).toBeVisible();
    await expect(
      instanceCell.getByRole("button", { name: "Drop Session" }),
    ).toBeVisible();
  });

  test("mobile cards cap the account email", async ({ page }) => {
    const longEmail = "very-long-operator-address-for-cap-check@example.com";
    const cards = await gotoPoolCards(page, [
      tokenRow(0, { email: longEmail }),
    ]);
    const emailSpan = cards.locator(`span[title="${longEmail}"]`).first();
    await expect(emailSpan).toBeVisible();
    const w = await emailSpan.evaluate(
      (el) => el.getBoundingClientRect().width,
    );
    expect(w, "mobile email box width").toBeLessThanOrEqual(130);
  });

  test("usage accounts list grids two cards per row on desktop", async ({
    page,
  }) => {
    const f = loadFixtures();
    const withQuota = (idx: number) =>
      tokenRow(idx, {
        freebucks: {
          balance: 50,
          daily: { remaining: 30, limit: 75 },
          wallet: { balance: 20 },
          monthly: { remaining: 20 },
        },
      });
    const payload = tokensPayload([
      withQuota(0),
      withQuota(1),
      withQuota(2),
      withQuota(3),
    ]);
    await mockDashboard(page, f, { tokens: payload });
    await page.setViewportSize({ width: 1280, height: 900 });
    await page.goto("/admin/#tokens");
    await page.getByRole("button", { name: "Allowances" }).click();
    const rows = page.getByTestId("account-row");
    await expect(rows.first()).toBeVisible({ timeout: 15000 });
    expect(await rows.count()).toBe(4);
    const boxes = await rows.evaluateAll((els) =>
      els.map((el) => {
        const r = el.getBoundingClientRect();
        return { top: r.top, left: r.left, bottom: r.bottom };
      }),
    );
    // Card #2 shares card #1's row; card #3 starts the second row.
    expect(
      Math.abs(boxes[1].top - boxes[0].top),
      "second card same row",
    ).toBeLessThanOrEqual(2);
    expect(boxes[1].left, "second card to the right").toBeGreaterThan(
      boxes[0].left,
    );
    expect(boxes[2].top, "third card second row").toBeGreaterThan(
      boxes[0].bottom,
    );
    // Cards themselves unchanged: every inner fact still renders.
    const firstCard = rows.first();
    await expect(firstCard.getByTestId("freebucks-header")).toBeVisible();
    // The remade card states the spendable total once, with its decomposition;
    // the daily pool and the wallet follow it exactly once each.
    await expect(firstCard.getByTestId("freebucks-header")).toContainText(
      "50 Freebucks spendable · = 30 daily + 20 wallet",
    );
    await expect(firstCard).toContainText("Used 45 / 75");
    await expect(firstCard).toContainText("30 left");
    await expect(firstCard).toContainText("Wallet 20");
    // Roughly halves the list height vs the old single-column stack.
    const heights = await page.evaluate(() => {
      const ul = document.querySelector('ul[aria-label="Accounts"]');
      if (!ul) throw new Error("accounts list not rendered");
      const grid = ul.getBoundingClientRect().height;
      const prev = (ul as HTMLElement).style.cssText;
      (ul as HTMLElement).style.display = "flex";
      (ul as HTMLElement).style.flexDirection = "column";
      const stacked = ul.getBoundingClientRect().height;
      (ul as HTMLElement).style.cssText = prev;
      return { grid, stacked };
    });
    expect(
      heights.grid,
      `grid height ${heights.grid} vs stacked ${heights.stacked}`,
    ).toBeLessThan(heights.stacked * 0.75);
  });

  test("usage accounts list stays single-column at 390px", async ({ page }) => {
    const f = loadFixtures();
    await mockDashboard(page, f, {
      tokens: tokensPayload([tokenRow(0), tokenRow(1)]),
    });
    await page.setViewportSize({ width: 390, height: 844 });
    await page.goto("/admin/#tokens");
    await page.getByRole("button", { name: "Allowances" }).click();
    const rows = page.getByTestId("account-row");
    await expect(rows.first()).toBeVisible({ timeout: 15000 });
    const boxes = await rows.evaluateAll((els) =>
      els.map((el) => {
        const r = el.getBoundingClientRect();
        return { top: r.top, left: r.left, bottom: r.bottom };
      }),
    );
    // Stacked with no overlap or clipping.
    expect(
      Math.abs(boxes[1].left - boxes[0].left),
      "same column",
    ).toBeLessThanOrEqual(2);
    expect(boxes[1].top, "no overlap").toBeGreaterThanOrEqual(
      boxes[0].bottom - 1,
    );
    const docOverflow = await page.evaluate(
      () =>
        document.documentElement.scrollWidth -
        document.documentElement.clientWidth,
    );
    expect(docOverflow).toBeLessThanOrEqual(1);
  });
});
