import { test, expect } from "@playwright/test";
import { loadFixtures, mockDashboard } from "./mocks.js";
import { adminUrl } from "./mock-data.js";

// Upstream error surfaces through the Dev Tools Model Playground (issue
// inventory: interactables gap). The playground POSTs /v1/chat/completions
// directly and renders `HTTP <status>: <body>` verbatim in a role=alert, so
// each test routes that endpoint to a terminal backend-shaped refusal and
// asserts the surfaced copy carries the distinct code (never the generic
// out_of_credits / buy-credits remediation) and that exactly one upstream
// attempt fires (terminal errors never retry-spin).
//
// Web-first assertions only: role/label locators, no sleeps, inline
// page.route mocks over the shared mockDashboard layer. This spec never
// touches fixtures/ or other specs.

test.describe("upstream error surfaces (mock backend)", () => {
  test.use({ expect: { timeout: 10_000 } });

  // The playground hides behind the DEVTOOLS_ENABLED gate (config document);
  // the shared mock serves a bare env, so each test flips the gate inline
  // (same unroute/re-route idiom as interactions.spec).
  async function enableDevTools(page: Parameters<typeof mockDashboard>[0]) {
    await page.unroute("**/admin/api/config");
    await page.route("**/admin/api/config", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          env_content: "AUTH_TOKENS=tok0\nDEVTOOLS_ENABLED=true\n",
          has_env_file: true,
        }),
      });
    });
  }

  // Routes /v1/chat/completions to one terminal refusal; returns the live
  // hit counter so tests pin the single-attempt contract.
  async function mockChatRefusal(
    page: Parameters<typeof mockDashboard>[0],
    status: number,
    body: string,
  ) {
    let hits = 0;
    await page.route("**/v1/chat/completions", async (route) => {
      hits += 1;
      await route.fulfill({ status, contentType: "application/json", body });
    });
    return () => hits;
  }

  async function openPlayground(page: Parameters<typeof mockDashboard>[0]) {
    await page.goto(adminUrl("devtools"));
    await expect(page.getByLabel("Model Playground")).toBeVisible();
    // Default prompt text is non-empty, so Send enables without typing.
    await expect(
      page.getByRole("button", { name: "Send Request" }),
    ).toBeEnabled();
  }

  async function sendOnce(page: Parameters<typeof mockDashboard>[0]) {
    await page.getByRole("button", { name: "Send Request" }).click();
  }

  test("403 free_mode_unavailable names the egress gate, not credits", async ({
    page,
  }) => {
    await mockDashboard(page, loadFixtures(), {}, { loginPage: true });
    await enableDevTools(page);
    const chatHits = await mockChatRefusal(
      page,
      403,
      JSON.stringify({
        error: {
          message:
            "Free tier unavailable for anonymous_network egress: disable VPN/proxy/Tor and retry.",
          type: "free_mode_unavailable",
          code: "free_mode_unavailable",
        },
      }),
    );
    await openPlayground(page);
    await sendOnce(page);
    const alert = page.getByRole("alert");
    await expect(alert).toContainText("HTTP 403");
    await expect(alert).toContainText("free_mode_unavailable");
    await expect(alert).toContainText("anonymous_network");
    // Distinct from the billing refusal: no credit-purchase remediation.
    await expect(alert).not.toContainText("out_of_credits");
    await expect(alert).not.toContainText(/buy credits/i);
    // Terminal gate: the single send fired exactly one upstream attempt.
    expect(chatHits()).toBe(1);
  });

  test("402 provider_usage_exhausted stays operator-side, not out_of_credits", async ({
    page,
  }) => {
    await mockDashboard(page, loadFixtures(), {}, { loginPage: true });
    await enableDevTools(page);
    const chatHits = await mockChatRefusal(
      page,
      402,
      JSON.stringify({
        error: {
          message:
            "Freebuff ran out of provider usage and needs a refill. This is on us, not your account.",
          type: "provider_usage_exhausted",
          code: "provider_usage_exhausted",
        },
      }),
    );
    await openPlayground(page);
    await sendOnce(page);
    const alert = page.getByRole("alert");
    await expect(alert).toContainText("HTTP 402");
    await expect(alert).toContainText("provider_usage_exhausted");
    await expect(alert).not.toContainText("out_of_credits");
    await expect(alert).not.toContainText(/buy credits/i);
    expect(chatHits()).toBe(1);
  });

  test("409 consent_required asks for a re-confirm, once", async ({ page }) => {
    await mockDashboard(page, loadFixtures(), {}, { loginPage: true });
    await enableDevTools(page);
    const chatHits = await mockChatRefusal(
      page,
      409,
      JSON.stringify({
        error: {
          message:
            "Your balance changed. Choose the model again to confirm 5 wallet Freebucks.",
          type: "consent_required",
          code: "consent_required",
        },
      }),
    );
    await openPlayground(page);
    await sendOnce(page);
    const alert = page.getByRole("alert");
    await expect(alert).toContainText("HTTP 409");
    await expect(alert).toContainText("consent_required");
    await expect(alert).not.toContainText("out_of_credits");
    expect(chatHits()).toBe(1);
  });

  test("409 first_tab_discount_changed carries the no-charge copy, once", async ({
    page,
  }) => {
    await mockDashboard(page, loadFixtures(), {}, { loginPage: true });
    await enableDevTools(page);
    const chatHits = await mockChatRefusal(
      page,
      409,
      JSON.stringify({
        error: {
          message:
            "Your first-tab discount changed. Review the model menu and choose again. No Freebucks were charged.",
          type: "first_tab_discount_changed",
          code: "first_tab_discount_changed",
        },
      }),
    );
    await openPlayground(page);
    await sendOnce(page);
    const alert = page.getByRole("alert");
    await expect(alert).toContainText("HTTP 409");
    await expect(alert).toContainText("first_tab_discount_changed");
    await expect(alert).toContainText("No Freebucks were charged");
    await expect(alert).not.toContainText("out_of_credits");
    expect(chatHits()).toBe(1);
  });

  test("pinned-account admit refusal surfaces inline and never reaches /v1", async ({
    page,
  }) => {
    await mockDashboard(page, loadFixtures(), {}, { loginPage: true });
    await enableDevTools(page);
    let sessionHits = 0;
    let chatHits = 0;
    // Session endpoint speaks the admin {ok,message,code} envelope, which
    // the playground prefixes with the account label.
    await page.route("**/admin/tokens/0/session", async (route) => {
      sessionHits += 1;
      await route.fulfill({
        status: 409,
        contentType: "application/json",
        body: JSON.stringify({
          ok: false,
          message:
            "upstream balance changed: re-confirm 5 wallet Freebucks to admit (consent_required)",
          code: "consent_required",
        }),
      });
    });
    await page.route("**/v1/chat/completions", async (route) => {
      chatHits += 1;
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          choices: [{ message: { content: "unreached" } }],
        }),
      });
    });
    await openPlayground(page);
    await page.locator("#dev-account").selectOption("0");
    await sendOnce(page);
    const alert = page.getByRole("alert");
    await expect(alert).toContainText("Admit on account 1 failed:");
    await expect(alert).toContainText("consent_required");
    // The admit gate stopped the send: one session attempt, zero chat sends.
    expect(sessionHits).toBe(1);
    expect(chatHits).toBe(0);
  });

  test("tool names reach the wire verbatim (no dashboard pre-mangle)", async ({
    page,
  }) => {
    await mockDashboard(page, loadFixtures(), {}, { loginPage: true });
    await enableDevTools(page);
    // The legacy search_file_content alias is renamed to its official
    // equivalent by the proxy on the way upstream (backend conformance
    // pins that); MCP (mcp_*) and locally discovered (discovered_tool_*)
    // names pass through verbatim. The dashboard layer must not pre-mangle
    // any of them, so the captured wire array equals what the page sent.
    const sentTools = [
      { type: "function", function: { name: "search_file_content" } },
      { type: "function", function: { name: "mcp_github_list_issues" } },
      { type: "function", function: { name: "discovered_tool_mycommand" } },
    ];
    let captured: Array<{ name?: string }> = [];
    await page.route("**/v1/chat/completions", async (route) => {
      const body = route.request().postDataJSON() as {
        tools?: Array<{ function?: { name?: string } }>;
      };
      captured = (body?.tools ?? []).map((t) => ({
        name: t?.function?.name,
      }));
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          choices: [{ message: { content: "ok" } }],
          usage: { total_tokens: 7 },
        }),
      });
    });
    await openPlayground(page);
    const content = await page.evaluate(async (tools) => {
      const res = await fetch("/v1/chat/completions", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          model: "deepseek/deepseek-v4-flash",
          messages: [{ role: "user", content: "hi" }],
          stream: false,
          tools,
        }),
      });
      const json = await res.json();
      return json.choices?.[0]?.message?.content ?? "";
    }, sentTools);
    expect(content).toBe("ok");
    expect(captured).toEqual([
      { name: "search_file_content" },
      { name: "mcp_github_list_issues" },
      { name: "discovered_tool_mycommand" },
    ]);
  });
});
