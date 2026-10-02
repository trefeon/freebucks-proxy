import { describe, it } from "node:test";
import assert from "node:assert/strict";
import {
  parseHash,
  hashPageId,
  buildHash,
  applyHashPatch,
  readPageHash,
  writeHashUpdate,
} from "./tableUrlState.js";

describe("parseHash", () => {
  it("splits a bare page hash", () => {
    const { page, params } = parseHash("#activity");
    assert.equal(page, "activity");
    assert.equal(params.toString(), "");
  });

  it("splits page and query", () => {
    const { page, params } = parseHash("#activity?level=error&msg=foo");
    assert.equal(page, "activity");
    assert.equal(params.get("level"), "error");
    assert.equal(params.get("msg"), "foo");
  });

  it("tolerates a missing # prefix and empty input", () => {
    assert.equal(parseHash("tokens").page, "tokens");
    assert.equal(parseHash("").page, "");
    assert.equal(parseHash(null).page, "");
  });
});

describe("hashPageId", () => {
  it("strips the query so the shell still resolves the page", () => {
    assert.equal(hashPageId("#activity?view=table&page=2"), "activity");
    assert.equal(hashPageId("#tokens"), "tokens");
    assert.equal(hashPageId("#no-such-page?x=1"), "no-such-page");
  });
});

describe("buildHash", () => {
  it("renders a bare page when params are empty", () => {
    assert.equal(buildHash("tokens", new URLSearchParams()), "#tokens");
  });

  it("renders page plus query otherwise", () => {
    const p = new URLSearchParams({ view: "table", page: "2" });
    assert.equal(buildHash("activity", p), "#activity?view=table&page=2");
  });
});

describe("applyHashPatch", () => {
  it("merges keys and drops nullish/empty values", () => {
    const next = applyHashPatch("#activity?tab=live&level=error", "activity", {
      level: null,
      msg: "timeout",
    });
    assert.equal(next, "#activity?tab=live&msg=timeout");
  });

  it("removes the query entirely when the last key is dropped", () => {
    assert.equal(
      applyHashPatch("#tokens?tab=warming", "tokens", { tab: null }),
      "#tokens",
    );
  });

  it("leaves another page's hash untouched", () => {
    assert.equal(
      applyHashPatch("#tokens?tab=warming", "activity", { view: "table" }),
      "#tokens?tab=warming",
    );
  });

  it("encodes values that need it", () => {
    const next = applyHashPatch("#activity", "activity", { msg: "a b&c" });
    assert.equal(next, "#activity?msg=a+b%26c");
    assert.equal(parseHash(next).params.get("msg"), "a b&c");
  });
});

describe("readPageHash / writeHashUpdate (window seam)", () => {
  function stubWindow(hash) {
    const replaced = [];
    globalThis.window = {
      location: { hash },
      history: { replaceState: (...a) => replaced.push(a) },
    };
    return replaced;
  }

  it("reads the live hash", () => {
    stubWindow("#activity?view=table");
    const { page, params } = readPageHash();
    assert.equal(page, "activity");
    assert.equal(params.get("view"), "table");
    delete globalThis.window;
  });

  it("writes via replaceState and skips no-op writes", () => {
    const replaced = stubWindow("#activity");
    writeHashUpdate("activity", { view: "table" });
    assert.equal(replaced.length, 1);
    assert.deepEqual(replaced[0], [null, "", "#activity?view=table"]);
    // Firing the same patch again is a no-op (no history spam).
    globalThis.window.location.hash = "#activity?view=table";
    writeHashUpdate("activity", { view: "table" });
    assert.equal(replaced.length, 1);
    delete globalThis.window;
  });

  it("never rewrites another page's hash", () => {
    const replaced = stubWindow("#tokens");
    writeHashUpdate("activity", { view: "table" });
    assert.equal(replaced.length, 0);
    delete globalThis.window;
  });
});
