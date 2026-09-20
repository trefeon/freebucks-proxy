import { test, expect } from "@playwright/test";
import { loadFixtures, mockDashboard } from "./mocks.js";
import { tokenRow, tokensPayload } from "./mock-data.js";

// Drop-session honesty regression (flipped from the false-success repro):
// the backend answers {ok:true, kept:true} when keepSession fires
// (backend/internal/pool/precious.go) and the UI must report the keep as a
// warning note — never a success toast — while the session row stays live.
//
// Faithful mock: the drop endpoint fulfills 200 ok + kept:true with the
// contract message AND the tokens payload is deliberately never mutated, so
// the next GET /admin/api/tokens (triggerAction -> refreshTokens) returns
// the same live session — exactly what a precious-kept account serves.
// Specs assert web state only, with role/label locators and no sleeps.
test("drop session on a kept session reports the keep, never success, session stays live", async ({
  page,
}) => {
  const live = tokenRow(0, {
    session_status: "active",
    session_instance: "inst-precious99-abcdefghijklmnop",
    session_model: "stealth/ox-alpha",
    session_remaining_seconds: 4620,
    session_expires_at: new Date(Date.now() + 4620_000).toISOString(),
    active_runs: 1,
  });
  await mockDashboard(page, loadFixtures(), {
    tokens: tokensPayload([live]),
  });
  // Registered after mockDashboard so this route wins for the drop POST
  // (same ordering as interactions.spec.ts). kept:true, contract-verbatim
  // message, zero state mutation: the precious-keep path.
  await page.route("**/admin/tokens/0/drop-session", async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({
        ok: true,
        kept: true,
        message: "Session kept (precious) — next request still rides it.",
      }),
    });
  });

  await page.goto("http://127.0.0.1:4173/admin/#tokens");
  const table = page.locator("table.fp-table");
  const row = table.locator("tbody tr").filter({ hasText: "Account #1" });
  // Render gate (TokenCard.svelte): active + instance + remaining>0 + model.
  await expect(
    row.getByRole("button", { name: "Drop Session" }),
    "live row offers Drop Session",
  ).toBeVisible({ timeout: 10000 });

  const dropReq = page.waitForRequest(
    (r) =>
      r.method() === "POST" && r.url().endsWith("/admin/tokens/0/drop-session"),
  );
  const refetch = page.waitForResponse(
    (r) => r.url().includes("/admin/api/tokens") && r.status() === 200,
  );
  // confirmAction() takes the native window.confirm branch under Playwright
  // (navigator.webdriver), so the suite accepts the native dialog.
  page.once("dialog", (d) => d.accept());
  await row.getByRole("button", { name: "Drop Session" }).click();
  await dropReq;
  await refetch;

  // Observation 1 — the honest note: a warning reports the keep.
  await expect(
    page.getByText("Session kept (precious) — next request still rides it."),
    "kept warning note reports the surviving session",
  ).toBeVisible();
  // Observation 2 — no lie: the old success copy never renders.
  await expect(
    page.getByText("session dropped — next request will re-admit fresh."),
    "no false success toast on the kept path",
  ).toHaveCount(0);
  // Observation 3 — the persistence: the session row is unchanged. The
  // model badge + instance id + countdown prove the upstream session is
  // still live, and the Drop Session kill switch still renders (its own
  // gate demands active + instance + remaining>0 + model).
  await expect(
    row.getByText("stealth/ox-alpha"),
    "model badge still live after the kept drop",
  ).toBeVisible();
  await expect(
    row.locator('code[title="inst-precious99-abcdefghijklmnop"]'),
    "instance id still live after the kept drop",
  ).toBeVisible();
  await expect(
    row.locator('span[aria-label^="Session time remaining"]'),
    "countdown still live after the kept drop",
  ).toBeVisible();
  await expect(
    row.getByRole("button", { name: "Drop Session" }),
    "kill switch still offered after the kept drop",
  ).toBeVisible();
});

// Parked-account honesty: a row carrying the remembered upstream-429 park
// (dashboard payload cooldown_active + cooldown_until + additive
// cooldown_kind / cooldown_resets_at — backend/internal/dashboard/
// dashboard_cards.go tokenCard, dashboard_helpers.go cardFromSnapshot; the
// payload carries no per-model detail, so the note names the pool-level
// reset) renders the parked note in its drawer instead of the bare
// no-session empty state.
test("parked account drawer names the reset instead of the bare empty state", async ({
  page,
}) => {
  const parked = tokenRow(0, {
    session_status: "idle",
    cooldown_active: true,
    cooldown_until: new Date(Date.now() + 5 * 3600_000).toISOString(),
    cooldown_kind: "freebucks_window",
    cooldown_resets_at: "2026-09-17T07:00:00Z",
  });
  await mockDashboard(page, loadFixtures(), {
    tokens: tokensPayload([parked]),
  });

  await page.goto("http://127.0.0.1:4173/admin/#tokens");
  const table = page.locator("table.fp-table");
  const row = table.locator("tbody tr").filter({ hasText: "Account #1" });
  await row.locator('button[aria-label*="Expand details"]').click();

  const note = table.getByTestId("parked-note");
  await expect(note, "parked note renders in the drawer").toBeVisible();
  await expect(note, "names the upstream 429").toContainText("upstream 429");
  await expect(note, "names the window kind").toContainText("freebucks_window");
  // The reset clock is absolute UTC on the wire and renders on the operator's
  // wall clock: local date + time plus the zone it belongs to, never "HH:MMZ".
  await expect(
    note,
    "names the pool-level reset clock in the operator's zone",
  ).toContainText(/until [A-Z][a-z]{2} \d{1,2}, \d{2}:\d{2} (AM|PM) \([^)]+\)/);
  await expect(note, "spill line stays honest").toContainText(
    "spills to next account",
  );
  await expect(
    table.getByText("No active session or run for this auth token."),
    "bare empty state yields to the parked note",
  ).toHaveCount(0);
});

// Usage Accounts probe-all: one header button posts the zero-cost
// POST /admin/tokens/test-all (pool.ProbeAllTokens claims no session),
// disables mid-flight, toasts one summary, then refetches the list.
test("usage accounts probe-all posts test-all once and toasts the summary", async ({
  page,
}) => {
  const f = loadFixtures();
  await mockDashboard(page, f, {}, { loginPage: true });
  const posts: string[] = [];
  await page.route("**/admin/tokens/test-all", async (route) => {
    posts.push(route.request().method());
    // Hold the flight so the progress/disabled state is observable even
    // under headless rAF throttling.
    const { promise: flightHold, resolve: releaseFlight } =
      Promise.withResolvers<void>();
    setTimeout(releaseFlight, 1500);
    await flightHold;
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify([
        {
          index: 0,
          status: "ok",
          spendable_freebucks: 10,
          daily_limit_freebucks: 100,
          daily_spent_freebucks: 5,
          quarantined: false,
          cooling: false,
        },
        {
          index: 1,
          status: "rate_limited",
          detail: "upstream 429",
          spendable_freebucks: 0,
          daily_limit_freebucks: 100,
          daily_spent_freebucks: 100,
          quarantined: false,
          cooling: true,
        },
        {
          index: 2,
          status: "banned",
          detail: "hard ban",
          spendable_freebucks: 0,
          daily_limit_freebucks: 0,
          daily_spent_freebucks: 0,
          quarantined: true,
          cooling: false,
        },
      ]),
    });
  });

  await page.goto("http://127.0.0.1:4173/admin/#plans");
  await page.getByRole("button", { name: "Accounts" }).click();
  // Title-anchored: the accessible name flips to "Probing…" mid-flight,
  // so a name locator would go stale exactly when we assert on it.
  const probe = page.getByTitle(
    "Zero-cost probe of every account: no session claimed",
  );
  await expect(probe, "usage accounts offers Probe all").toBeVisible();
  const refetch = page.waitForResponse(
    (r) => r.url().includes("/admin/api/tokens") && r.status() === 200,
  );
  const postReq = page.waitForRequest(
    (r) => r.method() === "POST" && r.url().endsWith("/admin/tokens/test-all"),
  );
  await probe.click();
  await postReq;
  await expect(probe, "probe button disables mid-flight").toBeDisabled();
  await refetch;
  await expect(
    page.getByText("Probed 3: 1 ok, 1 limited, 1 banned"),
    "summary toast counts every outcome",
  ).toBeVisible();
  expect(posts).toEqual(["POST"]);
});

// Real-drop leg: the gateway answers {ok:true, kept:false} with NO message
// field, the session is actually gone, and the toast copy stays
// frontend-side ("Session dropped — next request will re-admit fresh.").
test("drop session on a real drop toasts the frontend copy and retires the kill switch", async ({
  page,
}) => {
  const state = {
    tokens: [
      tokenRow(0, {
        session_status: "active",
        session_instance: "inst-dropme-abcdefghijklmnop",
        session_model: "stealth/ox-alpha",
        session_remaining_seconds: 4620,
        session_expires_at: new Date(Date.now() + 4620_000).toISOString(),
        active_runs: 1,
      }),
    ],
  };
  await mockDashboard(page, loadFixtures());
  await page.unroute("**/admin/api/tokens*");
  await page.route("**/admin/api/tokens*", async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(tokensPayload(state.tokens)),
    });
  });
  await page.route("**/admin/tokens/0/drop-session", async (route) => {
    // Messageless kept:false: the session actually ends upstream.
    state.tokens[0].session_status = "idle";
    state.tokens[0].session_instance = "";
    state.tokens[0].session_model = "";
    state.tokens[0].session_remaining_seconds = 0;
    state.tokens[0].active_runs = 0;
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ ok: true, kept: false }),
    });
  });

  await page.goto("http://127.0.0.1:4173/admin/#tokens");
  const table = page.locator("table.fp-table");
  const row = table.locator("tbody tr").filter({ hasText: "Account #1" });
  await expect(
    row.getByRole("button", { name: "Drop Session" }),
    "live row offers Drop Session",
  ).toBeVisible({ timeout: 10000 });

  const dropReq = page.waitForRequest(
    (r) =>
      r.method() === "POST" && r.url().endsWith("/admin/tokens/0/drop-session"),
  );
  const refetch = page.waitForResponse(
    (r) => r.url().includes("/admin/api/tokens") && r.status() === 200,
  );
  page.once("dialog", (d) => d.accept());
  await row.getByRole("button", { name: "Drop Session" }).click();
  await dropReq;
  await refetch;

  await expect(
    page.getByText("Session dropped — next request will re-admit fresh."),
    "real drop toasts the frontend-side success copy",
  ).toBeVisible();
  await expect(
    page.getByText("Session kept (precious)"),
    "no kept note on the real-drop leg",
  ).toHaveCount(0);
  await expect(
    row.getByRole("button", { name: "Drop Session" }),
    "kill switch retires once the session is gone",
  ).toHaveCount(0);
});
