import { createServer } from "node:http";
import { readFile } from "node:fs/promises";
import { existsSync, statSync } from "node:fs";
import { join, extname } from "node:path";
import {
  BUILD_ID_PATH,
  DIST_DIR,
  HOST,
  PORT,
  readLocalBuildId,
} from "./static-server-identity.mjs";

// Identity of the bundle this process serves: logged on listen and published on
// BUILD_ID_PATH so a client can tell this server apart from another worktree's.
const buildId = await readLocalBuildId().catch((err) => {
  console.error(`[serve-static] ${err.message}`);
  process.exit(1);
});

const mime = {
  ".html": "text/html",
  ".js": "application/javascript",
  ".css": "text/css",
  ".json": "application/json",
  ".woff": "font/woff",
  ".woff2": "font/woff2",
  ".svg": "image/svg+xml",
  ".png": "image/png",
  ".ico": "image/x-icon",
};

const server = createServer(async (req, res) => {
  try {
    const url = new URL(req.url, `http://${HOST}:${PORT}`);
    let pathname = url.pathname;
    // Identity probe: lets playwright's globalSetup tell a server started from
    // this checkout apart from one another worktree left listening on PORT.
    if (pathname === BUILD_ID_PATH) {
      res.writeHead(200, {
        "Content-Type": "application/json",
        "Cache-Control": "no-store",
      });
      res.end(JSON.stringify({ buildId, dist: DIST_DIR }));
      return;
    }
    // SPA fallback: /admin/* serves index.html
    if (
      pathname === "/admin" ||
      pathname === "/admin/" ||
      pathname.startsWith("/admin/")
    ) {
      // If it's an asset under /admin/assets/... serve that file
      if (pathname.startsWith("/admin/assets/")) {
        const filePath = join(DIST_DIR, pathname.replace("/admin/", ""));
        if (existsSync(filePath) && statSync(filePath).isFile()) {
          const ext = extname(filePath);
          res.writeHead(200, {
            "Content-Type": mime[ext] || "application/octet-stream",
          });
          res.end(await readFile(filePath));
          return;
        }
      }
      // Otherwise serve index.html
      const indexPath = join(DIST_DIR, "index.html");
      res.writeHead(200, { "Content-Type": "text/html" });
      res.end(await readFile(indexPath));
      return;
    }
    // Also handle root redirect
    if (pathname === "/" || pathname === "") {
      res.writeHead(302, { Location: "/admin/" });
      res.end();
      return;
    }
    res.writeHead(404);
    res.end("Not found");
  } catch (e) {
    res.writeHead(500);
    res.end(String(e));
  }
});

server.on("error", (err) => {
  const taken =
    err.code === "EADDRINUSE"
      ? ` — something else is already listening there (a leftover e2e run from this or another worktree); stop it before running the suite`
      : "";
  console.error(
    `[serve-static] cannot listen on http://${HOST}:${PORT}/admin/${taken}`,
  );
  console.error(err);
  process.exit(1);
});

server.listen(PORT, HOST, () => {
  console.log(
    `[serve-static] serving ${DIST_DIR} (build ${buildId ?? "unknown"}) at http://${HOST}:${PORT}/admin/`,
  );
});
