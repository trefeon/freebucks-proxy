import { describe, it, beforeEach } from "node:test";
import assert from "node:assert/strict";
import { get } from "svelte/store";
import { createQueryStore } from "./query.js";
import { resetSessionState, markSessionExpired } from "./session.js";

// The race-safe lifecycle: documents are stubbed (visibility only), fetches
// are deferreds the test resolves in adversarial order.

// Minimal document stub: the store only reads hidden + listener hooks.
const listeners = {};
globalThis.document ??= {
  get hidden() {
    return false;
  },
  addEventListener: (ev, fn) => {
    listeners[ev] = fn;
  },
  removeEventListener: (ev) => {
    delete listeners[ev];
  },
};

function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

describe("createQueryStore race-safe lifecycle", () => {
  // Flush one microtask round (setTimeout 0 keeps eslint's no-undef happy;
  // setImmediate is node-only).
  const tick = () => new Promise((r) => setTimeout(r, 0));
  beforeEach(() => {
    resetSessionState();
    for (const k of Object.keys(listeners)) delete listeners[k];
  });

  it("passes an AbortSignal to fetches and aborts the stale flight on refresh", async () => {
    const seen = [];
    const gate = deferred();
    const store = createQueryStore({
      intervalMs: 60_000,
      fetchFull: (signal) => {
        seen.push(signal);
        return gate.promise;
      },
    });
    const release = store.ensure();
    assert.equal(seen.length, 1, "first poll started");
    assert.equal(seen[0] instanceof AbortSignal, true, "fetch gets a signal");
    store.refresh();
    assert.equal(seen[0].aborted, true, "refresh aborts the stale flight");
    assert.equal(seen.length, 2, "refresh starts a fresh full pass");
    gate.resolve("fresh");
    await gate.promise;
    await tick();
    assert.equal(get(store.data), "fresh", "fresh pass applies after abort");
    release();
  });

  it("a slow earlier response never regresses newer data (seq guard)", async () => {
    const first = deferred();
    const second = deferred();
    let calls = 0;
    const store = createQueryStore({
      intervalMs: 60_000,
      fetchFull: () => {
        calls += 1;
        return calls === 1 ? first.promise : second.promise;
      },
    });
    const release = store.ensure();
    store.refresh();
    second.resolve("fresh");
    await second.promise;
    await tick();
    assert.equal(get(store.data), "fresh", "newer pass applied");
    first.resolve("stale");
    await first.promise;
    await tick();
    assert.equal(get(store.data), "fresh", "stale completion dropped");
    release();
  });

  it("surfaces fetch errors and stays silent on AbortError", async () => {
    const gate = deferred();
    let calls = 0;
    const store = createQueryStore({
      intervalMs: 60_000,
      fetchFull: () => {
        calls += 1;
        return calls === 1 ? gate.promise : Promise.resolve("recovered");
      },
    });
    const release = store.ensure();
    gate.resolve("v1");
    await gate.promise;
    await tick();
    assert.equal(get(store.error), "", "no error on success");

    const fail = deferred();
    const store2 = createQueryStore({
      intervalMs: 60_000,
      fetchFull: () => fail.promise,
    });
    const release2 = store2.ensure();
    fail.reject(new Error("boom"));
    await fail.promise.catch(() => {});
    await tick();
    assert.equal(get(store2.error), "boom", "fetch error surfaced");

    const abort = deferred();
    const store3 = createQueryStore({
      intervalMs: 60_000,
      fetchFull: () => abort.promise,
    });
    const release3 = store3.ensure();
    const err = new Error("aborted");
    err.name = "AbortError";
    abort.reject(err);
    await abort.promise.catch(() => {});
    await tick();
    assert.equal(get(store3.error), "", "AbortError stays silent");
    release();
    release2();
    release3();
  });

  it("a dead session halts polling", async () => {
    let calls = 0;
    const store = createQueryStore({
      intervalMs: 10,
      fetchFull: () => {
        calls += 1;
        return Promise.resolve(calls);
      },
    });
    const release = store.ensure();
    await new Promise((r) => setTimeout(r, 30));
    assert.ok(calls >= 1, "polled while alive");
    markSessionExpired();
    const frozen = calls;
    store.refresh();
    await new Promise((r) => setTimeout(r, 30));
    assert.equal(calls, frozen, "no poll after session death");
    release();
  });

  it("seed paints cold data with a stale badge until the first poll confirms", async () => {
    const gate = deferred();
    const store = createQueryStore({
      intervalMs: 60_000,
      fetchFull: () => gate.promise,
    });
    assert.equal(get(store.stale), false, "no badge before any seed");
    assert.equal(store.seed(null), false, "null seed is a no-op");
    assert.equal(store.seed(undefined), false, "undefined seed is a no-op");
    assert.equal(
      store.seed({ tokens: [1] }),
      true,
      "empty store takes the seed",
    );
    assert.deepEqual(get(store.data), { tokens: [1] });
    assert.equal(get(store.stale), true, "seed raises the stale badge");
    assert.equal(
      store.seed({ tokens: [2] }),
      false,
      "seed never overwrites live data",
    );
    assert.deepEqual(get(store.data), { tokens: [1] });
    const release = store.ensure();
    gate.resolve({ tokens: [2] });
    await gate.promise;
    await tick();
    assert.deepEqual(get(store.data), { tokens: [2] });
    assert.equal(
      get(store.stale),
      false,
      "first confirmed poll drops the badge",
    );
    release();
  });

  it("a failed first poll keeps the stale badge up", async () => {
    const gate = deferred();
    const store = createQueryStore({
      intervalMs: 60_000,
      fetchFull: () => gate.promise,
    });
    assert.equal(store.seed({ mode: "pooled" }), true);
    const release = store.ensure();
    gate.reject(new Error("down"));
    await gate.promise.catch(() => {});
    await tick();
    assert.equal(get(store.stale), true, "badge survives poll failure");
    assert.deepEqual(get(store.data), { mode: "pooled" });
    release();
  });
});
