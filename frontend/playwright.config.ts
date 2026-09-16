import { defineConfig } from "@playwright/test";

export default defineConfig({
  testDir: "./e2e",
  timeout: 30_000,
  expect: {
    timeout: 5_000,
  },
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 2 : 0,
  workers: process.env.CI ? 1 : undefined,
  reporter: [["list"]],
  // The port below is pinned by every spec's own origin literal, so it cannot be
  // moved per-worktree. What can be checked is that the server playwright reuses
  // (reuseExistingServer, i.e. outside CI) is serving *this* checkout's bundle:
  // serve-static.mjs publishes its build id on /__build-id and this setup fails
  // the run when the listening server serves a different bundle — typically a
  // leftover `node e2e/serve-static.mjs` from a parallel worktree, which would
  // otherwise be graded silently. PORT in e2e/static-server-identity.mjs must
  // stay in sync with the three literals below.
  globalSetup: "./e2e/static-server-identity.mjs",
  use: {
    baseURL: "http://127.0.0.1:4173",
    trace: "on-first-retry",
  },
  webServer: {
    command: "node e2e/serve-static.mjs",
    url: "http://127.0.0.1:4173/admin/",
    reuseExistingServer: !process.env.CI,
    timeout: 120 * 1000,
  },
});
