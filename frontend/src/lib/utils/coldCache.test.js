import { describe, it, beforeEach } from "node:test";
import assert from "node:assert/strict";
import { readColdCache, writeColdCache, clearColdCache } from "./coldCache.js";

// In-memory localStorage stub (node has none); blocked-storage variants
// throw on access like private-mode browsers do.
function stubStorage(impl) {
  Object.defineProperty(globalThis, "localStorage", {
    value: impl,
    writable: true,
    configurable: true,
  });
}

function memoryStorage() {
  const map = new Map();
  return {
    getItem: (k) => (map.has(k) ? map.get(k) : null),
    setItem: (k, v) => void map.set(k, String(v)),
    removeItem: (k) => void map.delete(k),
  };
}

describe("coldCache round-trip", () => {
  beforeEach(() => {
    stubStorage(memoryStorage());
  });

  it("writes then reads back { savedAt, value }", () => {
    assert.equal(writeColdCache("overview", { tokens: [] }), true);
    const got = readColdCache("overview");
    assert.ok(got && typeof got.savedAt === "number");
    assert.deepEqual(got.value, { tokens: [] });
  });

  it("returns null for absent keys", () => {
    assert.equal(readColdCache("nope"), null);
  });

  it("returns null for corrupt JSON instead of throwing", () => {
    globalThis.localStorage.setItem("fp-cache:bad", "{not json");
    assert.equal(readColdCache("bad"), null);
  });

  it("returns null for wrong-shape payloads", () => {
    globalThis.localStorage.setItem("fp-cache:odd", JSON.stringify([1, 2]));
    assert.equal(readColdCache("odd"), null);
    globalThis.localStorage.setItem(
      "fp-cache:odd2",
      JSON.stringify({ savedAt: "yesterday" }),
    );
    assert.equal(readColdCache("odd2"), null);
  });

  it("refuses non-object and oversized values", () => {
    assert.equal(writeColdCache("null", null), false);
    assert.equal(writeColdCache("str", "x"), false);
    assert.equal(writeColdCache("big", { blob: "x".repeat(200000) }), false);
    assert.equal(readColdCache("big"), null);
  });

  it("clear drops the entry", () => {
    writeColdCache("tokens", { tokens: [1] });
    clearColdCache("tokens");
    assert.equal(readColdCache("tokens"), null);
  });

  it("never throws when storage is blocked or missing", () => {
    stubStorage({
      getItem: () => {
        throw new Error("denied");
      },
      setItem: () => {
        throw new Error("denied");
      },
      removeItem: () => {
        throw new Error("denied");
      },
    });
    assert.equal(readColdCache("overview"), null);
    assert.equal(writeColdCache("overview", { tokens: [] }), false);
    clearColdCache("overview");
    // @ts-expect-error - simulating SSR without localStorage
    delete globalThis.localStorage;
    assert.equal(readColdCache("overview"), null);
    assert.equal(writeColdCache("overview", { tokens: [] }), false);
  });
});
