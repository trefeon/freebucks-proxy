import { test, expect } from "@playwright/test";
import {
  MODEL_A,
  MODEL_B,
  gotoPoolTokens,
  mockPool,
  mockPoolMutations,
} from "./mock-pool.js";

// Pool + accounts + tokens flows against the 22-row mock roster.
//
// masq-mock.spec.ts pins the six MASQ states through the shared factory,
// drop-session-gate.spec.ts pins the phantom-row kill-switch gate, and
// interactions.spec.ts pins the 3-row reorder/clear/probe mechanics. This
// suite extends them at roster scale: priority order and its reorder,
// expand-details drawers, streak badges, Drop Session offered ONLY on live
// rows (active + instance + model + remaining), post-#608 expired-empty vs
// grace-keeps-live, drawer pin/unpin through the settings overlay, lock /
// remove, and the 2-slots-per-(account,model) ceiling across models.
// Web-first assertions only, role/label locators, no sleeps.
test.describe("pool accounts (mock roster)", () => {
  test.use({ expect: { timeout: 10_000 } });

  test("roster: 22 pooled accounts render in priority order", async ({
    page,
  }) => {
    await mockPool(page);
    const table = await gotoPoolTokens(page);
    await expect(table.locator("tbody tr")).toHaveCount(22);
    const first = table.locator("tbody tr").nth(0);
    const last = table.locator("tbody tr").nth(21);
    await expect(first.getByText("Account #1")).toBeVisible();
    await expect(first.getByText("acct0@example.com")).toBeVisible();
    await expect(last.getByText("Account #22")).toBeVisible();
    await expect(last.getByText("acct21@example.com")).toBeVisible();
    await expect(page.getByText("22 pooled token(s)").first()).toBeVisible();
  });

  test("reorder: move down swaps priority and posts from/to; edges hold", async ({
    page,
  }) => {
    const { state } = await mockPool(page);
    await mockPoolMutations(page, state);
    const table = await gotoPoolTokens(page);
    const rows = table.locator("tbody tr");
    await expect(
      rows.nth(0).getByRole("button", { name: "Move Up" }),
    ).toBeDisabled();
    await expect(
      rows.nth(0).getByRole("button", { name: "Move Down" }),
    ).toBeEnabled();
    await expect(
      rows.nth(21).getByRole("button", { name: "Move Down" }),
    ).toBeDisabled();
    await expect(
      rows.nth(21).getByRole("button", { name: "Move Up" }),
    ).toBeEnabled();

    const swapReq = page.waitForRequest(
      (r) => r.method() === "POST" && r.url().endsWith("/admin/tokens/swap"),
    );
    const refetch = page.waitForResponse(
      (r) => r.url().includes("/admin/api/tokens") && r.status() === 200,
    );
    await rows.nth(0).getByRole("button", { name: "Move Down" }).click();
    expect(await (await swapReq).postDataJSON()).toEqual({ from: 0, to: 1 });
    await refetch;
    await expect(page.getByText("Pool order updated.")).toBeVisible();
    // Priority flipped: the second account's email now leads the table.
    await expect(rows.nth(0).getByText("acct1@example.com")).toBeVisible();
    await expect(rows.nth(1).getByText("acct0@example.com")).toBeVisible();
  });

  test("expand: drawer shows the pin and its skip count; idle shows unlocked copy", async ({
    page,
  }) => {
    await mockPool(page);
    const table = await gotoPoolTokens(page);
    const pinned = table
      .locator("tbody tr")
      .filter({ hasText: "acct4@example.com" });
    await pinned.locator('button[aria-label*="Expand details"]').click();
    await expect(table.getByText("Pinned model").first()).toBeVisible();
    await expect(table.getByText(MODEL_B).first()).toBeVisible();
    await expect(
      table.getByText("5 request(s) routed elsewhere by this pin").first(),
    ).toBeVisible();

    // Opening a second drawer closes the first; the idle drawer carries the
    // unlocked-pin copy plus the no-session empty state.
    const idle = table
      .locator("tbody tr")
      .filter({ hasText: "acct9@example.com" });
    await idle.locator('button[aria-label*="Expand details"]').click();
    await expect(
      table
        .getByText(
          "Unlocked — serves any model. Pin one model to dedicate this account.",
        )
        .first(),
    ).toBeVisible();
    await expect(
      table.getByText("No active session or run for this auth token.").first(),
    ).toBeVisible();
  });

  test("streaks: live streak chip and dim no-streak state", async ({
    page,
  }) => {
    await mockPool(page);
    const table = await gotoPoolTokens(page);
    const hot = table
      .locator("tbody tr")
      .filter({ hasText: "acct8@example.com" });
    await expect(hot.locator('[aria-label="Streak 7 days"]')).toHaveText(
      /7d streak/,
    );
    const warm = table
      .locator("tbody tr")
      .filter({ hasText: "acct12@example.com" });
    await expect(warm.locator('[aria-label="Streak 3 days"]')).toHaveText(
      /3d streak/,
    );
    const cold = table
      .locator("tbody tr")
      .filter({ hasText: "acct9@example.com" });
    await expect(cold.locator('[aria-label="No streak"]')).toHaveText(
      /no streak/,
    );
  });

  test("drop session: only live rows offer it; killing converges to idle", async ({
    page,
  }) => {
    const { state } = await mockPool(page);
    await mockPoolMutations(page, state);
    const table = await gotoPoolTokens(page);
    // Live rows: spill heads on both models plus the precious holder.
    for (const email of [
      "acct0@example.com",
      "acct1@example.com",
      "acct3@example.com",
    ]) {
      const row = table.locator("tbody tr").filter({ hasText: email });
      const btn = row.getByRole("button", { name: "Drop Session" });
      await expect(btn).toBeVisible();
      await expect(btn).toBeEnabled();
    }
    // Idle, queued, quota-phantom (active but instanceless), expired, and
    // grace rows offer no kill switch.
    for (const email of [
      "acct2@example.com",
      "dev@example.com",
      "acct6@example.com",
      "acct7@example.com",
      "acct9@example.com",
    ]) {
      const row = table.locator("tbody tr").filter({ hasText: email });
      await expect(
        row.getByRole("button", { name: "Drop Session" }),
      ).toHaveCount(0);
    }
    await expect(table.getByText("queued", { exact: true })).toBeVisible();
    await expect(table.getByText("expired", { exact: true })).toBeVisible();
    await expect(table.getByText("grace drain", { exact: true })).toBeVisible();

    // Killing the spill head drops its session: the row goes idle and the
    // button is gone on the next fetch.
    const head = table
      .locator("tbody tr")
      .filter({ hasText: "acct0@example.com" });
    const dropReq = page.waitForRequest(
      (r) =>
        r.method() === "POST" &&
        r.url().endsWith("/admin/tokens/0/drop-session"),
    );
    const refetch = page.waitForResponse(
      (r) => r.url().includes("/admin/api/tokens") && r.status() === 200,
    );
    page.once("dialog", (d) => d.accept());
    await head.getByRole("button", { name: "Drop Session" }).click();
    await dropReq;
    await refetch;
    await expect(page.getByText("Session dropped.")).toBeVisible();
    await expect(
      head.getByRole("button", { name: "Drop Session" }),
    ).toHaveCount(0);
    await expect(head.getByText("idle", { exact: true })).toBeVisible();
  });

  test("expired rows carry no live facts; grace drain keeps them", async ({
    page,
  }) => {
    await mockPool(page);
    const table = await gotoPoolTokens(page);
    // Terminal expired row: the history (account, email) survives but the
    // Instance cell is the bare em dash — no instance, no model pill.
    const expired = table
      .locator("tbody tr")
      .filter({ hasText: "acct6@example.com" });
    await expect(expired.getByText("expired", { exact: true })).toBeVisible();
    await expect(expired.getByText("acct6@example.com")).toBeVisible();
    await expect(expired.getByText("—")).toBeVisible();
    await expect(expired.getByText(MODEL_A)).toHaveCount(0);
    await expect(expired.getByText("inst-grace07")).toHaveCount(0);
    // Grace drain: expiry crossed but in-flight runs still drain, so the
    // instance and model stay on screen.
    const grace = table
      .locator("tbody tr")
      .filter({ hasText: "acct7@example.com" });
    await expect(grace.getByText("grace drain", { exact: true })).toBeVisible();
    await expect(
      grace.getByText("inst-grace07-abcdefghijklmnop"),
    ).toBeVisible();
    await expect(grace.getByText(MODEL_A)).toBeVisible();
  });

  test("pin: drawer pin posts the overlay and refreshes; clear unpins", async ({
    page,
  }) => {
    const { posted, state } = await mockPool(page);
    const table = await gotoPoolTokens(page);
    const row = table
      .locator("tbody tr")
      .filter({ hasText: "acct8@example.com" });
    await row.locator('button[aria-label*="Expand details"]').click();
    // Scope to the desktop table: the mobile card twin renders the same
    // drawer off-screen (drop-session-gate pins the card path separately).
    // Pin a served row: withdrawn/tier-only catalog rows are listed on the
    // Models tab but never served, so the pin picker does not offer them.
    const pinSelect = table.getByLabel("Pin a model to this token");
    await pinSelect.selectOption(MODEL_A);

    // Await the POST *response*: the overlay mock records `posted` inside its
    // route handler, which runs after the request waiter fires.
    const postResp = page.waitForResponse(
      (r) =>
        r.request().method() === "POST" &&
        r.url().endsWith("/admin/api/settings") &&
        r.status() === 200,
    );
    const refetch = page.waitForResponse(
      (r) => r.url().includes("/admin/api/tokens") && r.status() === 200,
    );
    await table.getByRole("button", { name: "Pin" }).click();
    expect(await (await postResp).request().postDataJSON()).toEqual({
      key: "PIN_MODEL",
      value: `8:${MODEL_A}`,
    });
    expect(posted).toEqual([{ key: "PIN_MODEL", value: `8:${MODEL_A}` }]);
    // Converge the fake backend before the drawer's refetch lands.
    state.tokens[8].pinned_model = MODEL_A;
    await refetch;
    await expect(
      table.getByRole("button", { name: "Clear pin" }),
    ).toBeVisible();
    await expect(
      table.locator("code").filter({ hasText: MODEL_A }),
    ).toHaveCount(1);

    const unpinResp = page.waitForResponse(
      (r) =>
        r.request().method() === "POST" &&
        r.url().endsWith("/admin/api/settings") &&
        r.status() === 200,
    );
    const refetch2 = page.waitForResponse(
      (r) => r.url().includes("/admin/api/tokens") && r.status() === 200,
    );
    await table.getByRole("button", { name: "Clear pin" }).click();
    expect(await (await unpinResp).request().postDataJSON()).toEqual({
      key: "PIN_MODEL",
      value: "",
    });
    delete state.tokens[8].pinned_model;
    await refetch2;
    await expect(
      table
        .getByText(
          "Unlocked — serves any model. Pin one model to dedicate this account.",
        )
        .first(),
    ).toBeVisible();
  });

  test("lock: account leaves rotation with a locked chip; unlock rejoins", async ({
    page,
  }) => {
    const { state } = await mockPool(page);
    await mockPoolMutations(page, state);
    const table = await gotoPoolTokens(page);
    // Seeded states: the locked row offers Unlock, the cooldown row Clear.
    const seededLocked = table
      .locator("tbody tr")
      .filter({ hasText: "acct10@example.com" });
    await expect(
      seededLocked.getByText("locked", { exact: true }),
    ).toBeVisible();
    await expect(
      seededLocked.getByRole("button", { name: "Unlock" }),
    ).toBeVisible();
    const cooling = table
      .locator("tbody tr")
      .filter({ hasText: "acct11@example.com" });
    await expect(cooling.getByText("cooldown", { exact: true })).toBeVisible();
    await expect(cooling.getByRole("button", { name: "Clear" })).toBeVisible();

    const row = table
      .locator("tbody tr")
      .filter({ hasText: "acct9@example.com" });
    const lockReq = page.waitForRequest(
      (r) => r.method() === "POST" && r.url().endsWith("/admin/tokens/9/lock"),
    );
    const refetch = page.waitForResponse(
      (r) => r.url().includes("/admin/api/tokens") && r.status() === 200,
    );
    page.once("dialog", (d) => d.accept());
    await row.getByRole("button", { name: "Lock" }).click();
    await lockReq;
    await refetch;
    await expect(page.getByText("Account locked.")).toBeVisible();
    await expect(row.getByText("locked", { exact: true })).toBeVisible();
    await expect(row.getByRole("button", { name: "Unlock" })).toBeVisible();

    const unlockReq = page.waitForRequest(
      (r) =>
        r.method() === "POST" &&
        r.url().endsWith("/admin/tokens/9/unlock-lock"),
    );
    const refetch2 = page.waitForResponse(
      (r) => r.url().includes("/admin/api/tokens") && r.status() === 200,
    );
    page.once("dialog", (d) => d.accept());
    await row.getByRole("button", { name: "Unlock" }).click();
    await unlockReq;
    await refetch2;
    await expect(page.getByText("Account unlocked.")).toBeVisible();
    await expect(row.getByRole("button", { name: "Lock" })).toBeVisible();
  });

  test("remove: account leaves the pool and the roster shrinks", async ({
    page,
  }) => {
    const { state } = await mockPool(page);
    await mockPoolMutations(page, state);
    const table = await gotoPoolTokens(page);
    await expect(table.locator("tbody tr")).toHaveCount(22);
    const row = table
      .locator("tbody tr")
      .filter({ hasText: "acct21@example.com" });
    const removeReq = page.waitForRequest(
      (r) => r.method() === "POST" && r.url().endsWith("/admin/tokens/remove"),
    );
    const refetch = page.waitForResponse(
      (r) => r.url().includes("/admin/api/tokens") && r.status() === 200,
    );
    page.once("dialog", (d) => d.accept());
    await row.getByRole("button", { name: "Remove" }).click();
    expect(await (await removeReq).postDataJSON()).toEqual({ token: 21 });
    await refetch;
    await expect(page.getByText("Account removed.")).toBeVisible();
    await expect(table.locator("tbody tr")).toHaveCount(21);
    await expect(table.getByText("acct21@example.com")).toHaveCount(0);
  });

  test("ceiling: 2 slots per account-model lane across the roster", async ({
    page,
  }) => {
    await mockPool(page, { loginPage: true });
    const metaResp = page.waitForResponse(
      (r) => r.url().includes("/admin/api/config/meta") && r.status() === 200,
      { timeout: 5000 },
    );
    const table = await gotoPoolTokens(page);
    await metaResp;
    // Both lane caps live: MODEL_A head and MODEL_B head hold leased turns.
    const headA = table
      .locator("tbody tr")
      .filter({ hasText: "acct0@example.com" });
    await expect(headA.getByText("leased", { exact: true })).toBeVisible();
    await expect(headA.getByText(MODEL_A)).toBeVisible();
    const headB = table
      .locator("tbody tr")
      .filter({ hasText: "acct1@example.com" });
    await expect(headB.getByText("leased", { exact: true })).toBeVisible();
    await expect(headB.getByText(MODEL_B)).toBeVisible();
    // Honest ceiling: 2 per account x 22 accounts = 44 concurrent turns.
    await page.getByRole("button", { name: "Strategy" }).click();
    await expect(page.getByTestId("pool-ceiling")).toContainText(
      /2 per account.*22 accounts.*44 concurrent turns/,
    );
  });
});
