// Table overflow guard (owner rule, DESIGN.md "### Tables"): no table and no
// horizontal-scroll container may overflow at any supported width. Cell
// content stacks inside the cell instead of widening the table.
//
// One test loops every surface × width because the failure mode is a single
// geometry regression on one page; a per-width test matrix would multiply the
// page loads without adding signal. Surfaces that render no table at a given
// width (the sub-lg/md stacked-card layouts) are skipped cleanly.
import { test, expect } from "@playwright/test";
import type { Page } from "@playwright/test";
import { loadFixtures, mockDashboard } from "./mocks.js";

const WIDTHS = [1440, 1280, 1024, 900, 820, 720, 640, 480, 390];

type Surface = {
  /** label used in failure messages */
  name: string;
  hash: string;
  /** segmented-control tabs, clicked by exact accessible name */
  tabs?: string[];
  /** nested toggle that only exists on the Metrics tab (usage view) */
  nested?: { tab: string; labels: string[] };
};

const SURFACES: Surface[] = [
  { name: "#overview", hash: "#overview" },
  {
    name: "#tokens",
    hash: "#tokens",
    tabs: ["Fleet", "Allowances", "Streaks", "Strategy"],
  },
  { name: "#plans", hash: "#plans", tabs: ["Catalog", "Routing"] },
  {
    name: "#activity",
    hash: "#activity",
    tabs: ["Requests", "Metrics", "Client Keys", "Traces"],
    // The usage tables live behind Activity → Metrics; the "Usage view"
    // toggle swaps the five total cards for the per-entry table.
    nested: { tab: "Metrics", labels: ["Details", "Overview"] },
  },
  { name: "#settings", hash: "#settings" },
];

/** Wait until the page is not rendering skeletons (real content or empty state). */
async function waitForSettled(page: Page) {
  await page.waitForFunction(
    () => document.querySelectorAll(".skeleton").length === 0,
    undefined,
    { timeout: 10_000 },
  );
  // A rendered table must also have committed its rows before measurement.
  await page.waitForFunction(
    () => {
      const t = document.querySelector("table");
      if (!t) return true;
      return t.querySelector("tbody tr") !== null || t.tBodies.length === 0;
    },
    undefined,
    { timeout: 10_000 },
  );
}

type Overflow = {
  what: string;
  detail: string;
  over: number;
  clientWidth: number;
  scrollWidth: number;
};

/** Every overflowing table or scroll container on the current page. */
function findOverflows(): Overflow[] {
  const out: Overflow[] = [];
  const describe = (el: Element) => {
    const cls = (el.getAttribute("class") || "").slice(0, 60);
    const r = el.getBoundingClientRect();
    return `${el.tagName.toLowerCase()} ${Math.round(r.width)}x${Math.round(r.height)} class="${cls}"`;
  };
  for (const t of Array.from(document.querySelectorAll("table"))) {
    const over = t.scrollWidth - t.clientWidth;
    if (over > 1) {
      out.push({
        what: "table",
        detail: describe(t),
        over,
        clientWidth: t.clientWidth,
        scrollWidth: t.scrollWidth,
      });
    }
    const parent = t.parentElement;
    if (parent) {
      const pOver = parent.scrollWidth - parent.clientWidth;
      if (pOver > 1) {
        out.push({
          what: "table container",
          detail: describe(parent),
          over: pOver,
          clientWidth: parent.clientWidth,
          scrollWidth: parent.scrollWidth,
        });
      }
    }
  }
  for (const el of Array.from(document.querySelectorAll<HTMLElement>("*"))) {
    const overflowX = getComputedStyle(el).overflowX;
    if (overflowX !== "auto" && overflowX !== "scroll") continue;
    const over = el.scrollWidth - el.clientWidth;
    if (over > 1) {
      out.push({
        what: `overflow-x:${overflowX} container`,
        detail: describe(el),
        over,
        clientWidth: el.clientWidth,
        scrollWidth: el.scrollWidth,
      });
    }
  }
  // The page itself: a stacked-cell layout must never push the document wide.
  for (const el of [document.documentElement, document.body]) {
    const over = el.scrollWidth - el.clientWidth;
    if (over > 1) {
      out.push({
        what: `document (${el.tagName.toLowerCase()})`,
        detail: describe(el),
        over,
        clientWidth: el.clientWidth,
        scrollWidth: el.scrollWidth,
      });
    }
  }
  return out;
}

test("no table or scroll container overflows at any supported width", async ({
  page,
}) => {
  test.setTimeout(300_000);
  await mockDashboard(page, loadFixtures());
  // The Metrics usage block reads GET /admin/api/usage; mocks.ts predates that
  // route (usage-tabs.spec serves it inline too), so the guard serves entries
  // here — otherwise the per-request table never renders and the widest
  // surface on the page goes unmeasured. Long model ids + big counts are
  // deliberate: the table must fit with content, not just with fixtures.
  await page.route("**/admin/api/usage*", async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({
        range: "today",
        totals: {
          requests: 42,
          input: 1200,
          cached: 300,
          output: 600,
          cost: 7,
        },
        entries: [
          {
            req_id: "r1",
            ts_ms: 1785900000000,
            model: "meta/muse-spark-1.3-contributor",
            input: 12345,
            cached: 1234,
            output: 567,
            total: 14146,
          },
          {
            req_id: "r2",
            ts_ms: 1785903600000,
            model: "deepseek/deepseek-v4-flash-reasoning",
            input: 999999,
            cached: 88888,
            output: 7777,
            total: 1096664,
          },
        ],
      }),
    });
  });

  const failures: string[] = [];
  for (const width of WIDTHS) {
    await page.setViewportSize({ width, height: 900 });
    for (const surface of SURFACES) {
      await page.goto(`/admin/${surface.hash}`);
      await waitForSettled(page);

      const steps: (string | null)[] = [null, ...(surface.tabs ?? [])];
      for (const tab of steps) {
        const label = `${surface.name}${tab ? ` → ${tab}` : ""}`;
        if (tab) {
          const button = page
            .getByRole("button", { name: tab, exact: true })
            .first();
          if ((await button.count()) === 0) {
            failures.push(`${width}px ${label}: tab control not found`);
            continue;
          }
          await button.click();
          await waitForSettled(page);
        }
        const measure = async (suffix: string) => {
          const items = await page.evaluate(findOverflows);
          if (items.length > 0) {
            failures.push(
              `${width}px ${label}${suffix}:\n${items
                .map(
                  (o) =>
                    `${o.what} overflows by ${o.over}px (client ${o.clientWidth} vs scroll ${o.scrollWidth}) — ${o.detail}`,
                )
                .join("\n")}`,
            );
          }
        };
        await measure("");
        if (surface.nested && tab === surface.nested.tab) {
          for (const nested of surface.nested.labels) {
            const toggle = page
              .getByRole("button", { name: nested, exact: true })
              .first();
            if ((await toggle.count()) === 0) {
              failures.push(
                `${width}px ${label} → ${nested}: toggle not found`,
              );
              continue;
            }
            await toggle.click();
            await waitForSettled(page);
            await measure(` → ${nested}`);
          }
        }
      }
    }
  }

  expect(failures, failures.join("\n\n")).toEqual([]);
});
