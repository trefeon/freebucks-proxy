import { expect, test, type Locator, type Page } from "@playwright/test";
import { loadFixtures, mockDashboard } from "./mocks";

// The Trace log is a 200-row ring, so density is the whole game: one slim
// line per row, no horizontal scroller on the card, and nothing dropped. The
// exact values that no longer fit inline stay reachable (title + the row's
// expanded detail), and the stacked cards below lg carry the same fields.
//
// Bounds: a cell is `.fp-table` padding (8px top/bottom) plus one line box at
// 11-13px type, so a row that never wraps lands near 36px; 40 leaves room for
// font metric drift without admitting a wrapped cell.
const ROW_MAX_PX = 40;
const PHASES_CELL_MAX_PX = 24;
const CARD_MAX_PX = 120;
const ROW_WIDTHS = [1440, 1280, 1024];

// Server-built and server-ordered: the renderer must not know this list.
// queue_wait_ms is the newest phase and has to appear on its own.
const PHASE_NAMES = [
  "acquire_ms",
  "session_refresh_ms",
  "run_acquire_ms",
  "upstream_ttfb_ms",
  "total_ms",
  "queue_wait_ms",
];
const PHASE_MS = [5733, 1240, 641, 8120, 15734, 240];
const TOKENS = { input: 276467, cached: 272512, output: 1423, reasoning: 1088 };

/**
 * The phases row `index` carries, in pipeline order, exactly as the payload
 * builds them (PHASE_MS offset by the row index in makeTraces).
 */
const expectedPhases = (index: number) =>
  PHASE_NAMES.map((name, j) => `${name} ${PHASE_MS[j] + index}ms`).join(" · ");

/** Index of the row that carries both an error and a full phase list. */
const ERROR_ROW = 3;
/** Index of the clean row: no error, no phases, usage present. */
const CLEAN_ROW = 0;

function makeTraces(count: number) {
  const traces = [];
  for (let i = 0; i < count; i++) {
    const withError = i === ERROR_ROW;
    // Row 0 (and every fifth after it) is a rate-limited shape: no phases at
    // all, which is the common case for rows that never reached upstream.
    const withPhases = i % 5 !== 0;
    traces.push({
      time: new Date(Date.UTC(2026, 8, 16, 10, 0, 0) + i * 1000).toISOString(),
      token: String(i % 4),
      model: i % 3 === 0 ? "deepseek/deepseek-v4-flash" : "openai/gpt-5.6-luna",
      agent: "base2-free-deepseek-v4-flash",
      status: withError ? "error" : "ok",
      ms: `${12000 + i}ms`,
      req_id: `req-${String(i).padStart(4, "0")}`,
      ...TOKENS,
      total: TOKENS.input + TOKENS.output,
      error: withError ? "upstream timeout" : undefined,
      phases: withPhases
        ? PHASE_NAMES.map((name, j) => ({ name, ms: PHASE_MS[j] + i }))
        : undefined,
    });
  }
  return { enabled: true, traces };
}

async function openTraces(page: Page, width: number) {
  await page.setViewportSize({ width, height: 900 });
  const fixtures = loadFixtures();
  await mockDashboard(page, fixtures);
  await page.unroute("**/admin/api/traces");
  await page.route("**/admin/api/traces", async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(makeTraces(200)),
    });
  });
  await page.goto("/admin/#activity");
  await page.getByRole("button", { name: "Traces" }).click();
  await expect(page.locator("table tbody tr")).toHaveCount(200);
}

test.describe("traces table density", () => {
  test("200 rows stay one slim line each and the card never scrolls sideways", async ({
    page,
  }) => {
    for (const width of ROW_WIDTHS) {
      await page.setViewportSize({ width, height: 900 });
      await openTraces(page, width);
      const rows = page.locator("table tbody tr");
      await expect(rows).toHaveCount(200);

      const measured = await page.evaluate(() => {
        const tables = [...document.querySelectorAll("table")];
        const table = tables[tables.length - 1];
        // The card that owns the table, not the first card on the page: the
        // Logs page grows other cards around this panel.
        const card = table.closest("section.fp-card") as HTMLElement;
        const heights = [...table.querySelectorAll("tbody tr")]
          .filter((tr) => !tr.querySelector("td[colspan]"))
          .map((tr) => tr.getBoundingClientRect().height);
        return {
          rows: heights.length,
          tallest: Math.round(Math.max(...heights)),
          tableWidth: Math.round(table.getBoundingClientRect().width),
          tableScrollWidth: table.scrollWidth,
          cardInner: card.clientWidth,
          pageScrollWidth: document.documentElement.scrollWidth,
          pageInner: document.documentElement.clientWidth,
        };
      });

      expect(measured.rows).toBe(200);
      // Before this change the same ring rendered 48-173px rows (five stacked
      // phase chips), 1383px of table inside a 750-1150px card.
      expect(measured.tallest, `tallest row at ${width}px`).toBeLessThanOrEqual(
        ROW_MAX_PX,
      );
      expect(
        measured.tableScrollWidth,
        `table fits the card at ${width}px`,
      ).toBeLessThanOrEqual(measured.cardInner);
      expect(measured.tableWidth).toBeLessThanOrEqual(measured.cardInner);
      expect(
        measured.pageScrollWidth,
        `page does not scroll sideways at ${width}px`,
      ).toBeLessThanOrEqual(measured.pageInner);
    }
  });

  test("numbers stay right-aligned, tokens read short, status keeps its tone", async ({
    page,
  }) => {
    await openTraces(page, 1280);
    const row = page.locator("table tbody tr").nth(1);
    const align = (cell: Locator) =>
      cell.evaluate((el) => getComputedStyle(el).textAlign);

    expect(await align(row.locator("td").nth(3))).toBe("right"); // tokens
    expect(await align(row.locator("td").nth(5))).toBe("right"); // latency
    expect(await align(row.locator("td").nth(0))).toBe("start"); // time

    // 277,890 tokens read as 277.9k; the exact counts stay in the title.
    await expect(row.locator("td").nth(3)).toContainText("277.9k");
    await expect(row.locator("td").nth(3)).toHaveAttribute(
      "title",
      "Input / cached / output / total LLM tokens",
    );

    const ok = row.locator("td").nth(4);
    await expect(ok).toHaveText("ok");
    await expect(ok.locator("span").last()).toHaveCSS(
      "color",
      "rgb(34, 197, 94)",
    );
  });

  test("phases summarise to one line and the full order is reachable", async ({
    page,
  }) => {
    await openTraces(page, 1280);
    const rows = page.locator("table tbody tr");
    const cell = rows.nth(1).locator("td").nth(6);

    // One line, not a stack of chips: the cell's content is a single row-tall
    // run of text (a td always stretches to the row, so measure the content).
    const toggle = cell.getByRole("button");
    const contentHeight = await toggle.evaluate(
      (el) => el.getBoundingClientRect().height,
    );
    expect(contentHeight).toBeLessThanOrEqual(PHASES_CELL_MAX_PX);

    // The summary is a real disclosure, and its title carries the whole list
    // in pipeline order, whatever phases the payload happened to carry.
    await expect(toggle).toHaveAttribute("aria-expanded", "false");
    // The whole ordered list, including the phase the newest release added.
    expect(await toggle.getAttribute("title")).toBe(
      `All phases in pipeline order: ${expectedPhases(1)}`,
    );
    // Dominant phases first (the ones a clipped cell must keep), with the
    // count of the ones that are only in the title and the detail.
    await expect(toggle).toContainText(
      "total_ms 15735ms · upstream_ttfb_ms 8121ms",
    );
    await expect(toggle).toContainText("+4");

    // Keyboard reachable: focus, Enter, read the ordered detail.
    await toggle.focus();
    await toggle.press("Enter");
    await expect(toggle).toHaveAttribute("aria-expanded", "true");
    await expect(rows).toHaveCount(201);
    const detail = rows.nth(2);
    await expect(detail).toContainText(
      `Phases (pipeline order): ${expectedPhases(1)}`,
    );
    await expect(detail).toContainText("Tokens: 276,467 in");

    await toggle.press("Enter");
    await expect(toggle).toHaveAttribute("aria-expanded", "false");
    await expect(rows).toHaveCount(200);
  });

  test("a clean row reserves nothing for an error, an error row carries it", async ({
    page,
  }) => {
    await openTraces(page, 1280);
    const rows = page.locator("table tbody tr");

    // One cell per visible column: the error rides the status cell instead of
    // holding a column of its own open for every row.
    await expect(rows.nth(CLEAN_ROW).locator("td")).toHaveCount(8);
    await expect(rows.nth(CLEAN_ROW).locator("td").nth(4)).toHaveText("ok");

    const errorRow = rows.nth(ERROR_ROW);
    await expect(errorRow.locator("td")).toHaveCount(8);
    await expect(errorRow.locator("td").nth(4)).toContainText("error");
    await expect(errorRow.locator("td").nth(4)).toContainText(
      "upstream timeout",
    );
    await expect(
      errorRow.locator("td").nth(4).locator('[title="upstream timeout"]'),
    ).toHaveText("upstream timeout");
  });

  test("the stacked cards below lg carry the same fields, compacted", async ({
    page,
  }) => {
    await openTraces(page, 390);
    const cards = page.getByLabel("Chat traces").locator("li");
    await expect(cards).toHaveCount(200);

    const card = cards.nth(1);
    // Same fields as the table row: time, account, model, status, tokens,
    // latency, phases and the logs control.
    await expect(card).toContainText(/\d{2}:\d{2}:\d{2}/);
    await expect(card).toContainText("#1");
    await expect(card).toContainText("openai/gpt-5.6-luna");
    await expect(card).toContainText("ok");
    await expect(card.getByRole("button", { name: "Logs" })).toBeVisible();

    // Tokens render a short human form; the full breakdown stays in the
    // title and in the card's detail, not in the visible cell.
    const tokens = card.locator("span.fp-num").first();
    await expect(tokens).toContainText("277.9k");
    await expect(tokens).toHaveAttribute(
      "title",
      "Input / cached / output / total LLM tokens",
    );

    // Phases summarised on one line, full pipeline order on the summary's
    // title and in the card's own detail.
    const phases = card.locator('span[title*="acquire_ms"]').first();
    expect(await phases.getAttribute("title")).toBe(expectedPhases(1));

    // Every card stays a few slim lines: the tallest of the 200 sets the
    // bound, so a single verbose row cannot quietly re-inflate the list.
    const tallestCard = await cards.evaluateAll((els) =>
      Math.round(
        Math.max(...els.map((el) => el.getBoundingClientRect().height)),
      ),
    );
    expect(tallestCard).toBeLessThanOrEqual(CARD_MAX_PX);

    const disclosure = card.getByRole("button", {
      name: "Show trace detail",
    });
    await expect(disclosure).toHaveAttribute("aria-expanded", "false");
    await disclosure.press("Enter");
    await expect(disclosure).toHaveAttribute("aria-expanded", "true");
    await expect(card).toContainText("Tokens: 276,467 in");
    await expect(card).toContainText("req_id: req-0001");
  });
});
