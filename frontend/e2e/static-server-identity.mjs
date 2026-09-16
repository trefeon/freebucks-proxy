// Identity of the e2e harness static server (serve-static.mjs), plus the guard
// that refuses to grade somebody else's instance of it.
//
// Why: the suite is pinned to http://127.0.0.1:4173 by every spec's own origin
// literal, so the port cannot be moved here without sweeping all of them. What
// this module does instead is make a *reused* server provably ours. Each server
// publishes the content-addressed asset names of the bundle it serves on
// GET /__build-id, and globalSetup (which playwright runs after the webServer
// plugin, i.e. once the reused-or-started server is up) fails the run when the
// listening server serves anything but the bundle in this checkout. Without
// that check `reuseExistingServer` silently attaches to a server left behind by
// a parallel worktree and the suite grades the wrong bundle while passing.
//
// Note: with CI set, `reuseExistingServer` is false, so playwright refuses a
// busy port on its own. This guard covers the reuse path.

import { readFile } from "node:fs/promises";
import { join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

export const HOST = "127.0.0.1";
export const PORT = 4173;
export const SERVE_URL = `http://${HOST}:${PORT}`;
export const BUILD_ID_PATH = "/__build-id";
// dist is at ../backend/internal/dashboard/dist relative to frontend/e2e
export const DIST_DIR = resolve(
  fileURLToPath(new URL(".", import.meta.url)),
  "../../backend/internal/dashboard/dist",
);

const ASSET_REF = /\/admin\/assets\/([A-Za-z0-9._-]+\.(?:js|css))/g;

/**
 * Build id of a dashboard bundle: the names of the hashed assets its index.html
 * references. Vite names assets after their content, so two checkouts with equal
 * ids serve the same bundle. null when the html references none.
 */
export function buildIdFromIndexHtml(html) {
  const names = [...html.matchAll(ASSET_REF)].map((match) => match[1]).sort();
  return names.length ? names.join(",") : null;
}

/**
 * Build id of the bundle in this checkout. Throws when the committed bundle is
 * missing (then there is nothing to serve, and nothing to verify).
 */
export async function readLocalBuildId() {
  const indexPath = join(DIST_DIR, "index.html");
  let html;
  try {
    html = await readFile(indexPath, "utf8");
  } catch (err) {
    throw new Error(
      `[e2e] cannot read the dashboard bundle at ${indexPath} (${err.code ?? err.message}) — ` +
        `build it first: npm --prefix frontend run build`,
    );
  }
  return buildIdFromIndexHtml(html);
}

function fail(lines) {
  throw new Error(`\n${lines.join("\n")}\n`);
}

const REUSE_NOTE = [
  "Playwright reuses an already-listening server (reuseExistingServer outside",
  "CI), so this run would have graded another instance's bundle.",
];

async function get(url) {
  return fetch(url, { signal: AbortSignal.timeout(5_000) });
}

/**
 * Identity of the server listening on PORT: its own build id if it runs this
 * harness, else the assets referenced by the shell it serves for /admin/ (which
 * identifies a server started from an older checkout).
 */
async function readServedIdentity() {
  const probes = [];
  const url = `${SERVE_URL}${BUILD_ID_PATH}`;
  let identity;
  try {
    identity = await get(url);
  } catch (err) {
    fail([
      `[e2e] no answer from ${url} (${err.cause?.code ?? err.message}).`,
      `The harness static server should be listening on ${SERVE_URL} by now — it is`,
      "either started by playwright or reused as an existing server.",
    ]);
  }
  probes.push(`  GET ${BUILD_ID_PATH} -> HTTP ${identity.status}`);
  if (identity.ok) {
    const body = await identity.json().catch(() => null);
    if (typeof body?.buildId === "string") {
      return { buildId: body.buildId, dist: body.dist ?? null, probes };
    }
  }

  const shellUrl = `${SERVE_URL}/admin/`;
  const shell = await get(shellUrl).catch(() => null);
  probes.push(
    shell
      ? `  GET /admin/ -> HTTP ${shell.status}`
      : "  GET /admin/ -> no answer",
  );
  const html = shell?.ok ? await shell.text().catch(() => null) : null;
  return {
    buildId: html ? buildIdFromIndexHtml(html) : null,
    dist: null,
    probes,
  };
}

/**
 * Playwright globalSetup: the server on PORT must be serving this checkout's
 * bundle, otherwise the run is a false green and must not start.
 */
export default async function verifyStaticServerIdentity() {
  const localId = await readLocalBuildId();
  if (!localId) {
    fail([
      `[e2e] ${join(DIST_DIR, "index.html")} references no /admin/assets/index-*.{js,css}`,
      "bundle, so the served build cannot be identified. Build the dashboard bundle:",
      "  npm --prefix frontend run build",
    ]);
  }

  const served = await readServedIdentity();
  if (!served.buildId) {
    fail([
      `[e2e] the server already listening on ${SERVE_URL} cannot be identified:`,
      ...served.probes,
      ...REUSE_NOTE,
      "",
      `Stop that server (port ${PORT}) and run the suite again, or re-run it from that worktree.`,
    ]);
  }

  if (served.buildId !== localId) {
    fail([
      `[e2e] the server already listening on ${SERVE_URL} serves a different bundle than this checkout:`,
      "",
      `  serving : ${served.buildId}`,
      `            ${
        served.dist
          ? `from ${served.dist}`
          : "referenced by its /admin/ shell (it predates GET /__build-id)"
      }`,
      `  expected: ${localId}`,
      `            from ${DIST_DIR}`,
      "",
      ...REUSE_NOTE,
      "",
      `Stop that server (port ${PORT}) and run the suite again, or re-run it from that worktree.`,
    ]);
  }

  console.log(`[e2e] static server identity ok: ${localId}`);
}
