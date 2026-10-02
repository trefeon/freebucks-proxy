import { describe, it, beforeEach } from "node:test";
import assert from "node:assert/strict";
import {
  PAGE_SIZE_KEY,
  PAGE_SIZE_OPTIONS,
  DEFAULT_PAGE_SIZE,
  loadPageSize,
  savePageSize,
} from "./pageSize.js";

function stubStorage(seed = {}) {
  const data = new Map(Object.entries(seed));
  globalThis.window = {
    localStorage: {
      getItem: (k) => (data.has(k) ? data.get(k) : null),
      setItem: (k, v) => data.set(k, String(v)),
      removeItem: (k) => data.delete(k),
    },
  };
  return data;
}

describe("pageSize global memory", () => {
  beforeEach(() => {
    delete globalThis.window;
  });

  it("defaults when nothing is stored", () => {
    stubStorage();
    assert.equal(loadPageSize(), DEFAULT_PAGE_SIZE);
  });

  it("round-trips a valid option", () => {
    const data = stubStorage();
    for (const n of PAGE_SIZE_OPTIONS) {
      savePageSize(n);
      assert.equal(data.get(PAGE_SIZE_KEY), String(n));
      assert.equal(loadPageSize(), n);
    }
  });

  it("rejects out-of-option and garbage values on both legs", () => {
    const data = stubStorage({ [PAGE_SIZE_KEY]: "25" });
    assert.equal(loadPageSize(), DEFAULT_PAGE_SIZE);
    savePageSize(25);
    savePageSize(NaN);
    savePageSize("50");
    assert.equal(
      data.get(PAGE_SIZE_KEY),
      "25",
      "invalid saves never touch the key",
    );
    data.set(PAGE_SIZE_KEY, "huge");
    assert.equal(loadPageSize(), DEFAULT_PAGE_SIZE);
  });

  it("survives missing storage (SSR/private mode)", () => {
    assert.equal(loadPageSize(), DEFAULT_PAGE_SIZE);
    savePageSize(50); // must not throw
  });
});
