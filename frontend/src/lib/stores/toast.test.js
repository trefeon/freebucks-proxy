import { describe, it, beforeEach, afterEach, mock } from "node:test";
import assert from "node:assert/strict";
import { get } from "svelte/store";
import { toasts, toastQueue, push, clear } from "./toast.js";

// The user-visible contract: every toast leaves on its own after 10s, no
// tone is exempt, and only `sticky: true` survives the window.
const WINDOW_MS = 10_000;

describe("toast lifetime (10s auto-dismiss, tone-agnostic)", () => {
  beforeEach(() => {
    mock.timers.enable({ apis: ["setTimeout"] });
    clear();
  });

  afterEach(() => {
    clear();
    mock.timers.reset();
  });

  it("dismisses every tone at 10s — warning/error are not special", () => {
    for (const tone of ["info", "success", "warning", "error"]) {
      push({ tone, title: tone });
    }
    mock.timers.tick(WINDOW_MS - 1);
    assert.equal(get(toasts).length, 4, "all four still up just before 10s");
    mock.timers.tick(1);
    assert.deepEqual(get(toasts), [], "no tone outlives the window");
  });

  it("keeps only an explicit sticky toast", () => {
    push({ tone: "error", title: "pinned", sticky: true });
    push({ tone: "success", title: "transient" });
    mock.timers.tick(WINDOW_MS * 10);
    assert.deepEqual(
      get(toasts).map((t) => t.title),
      ["pinned"],
    );
  });

  it("gives a queued toast its own full window after a slot frees", () => {
    for (let i = 0; i < 5; i++) push({ tone: "info", title: `t${i}` });
    assert.deepEqual(
      get(toasts).map((t) => t.title),
      ["t0", "t1", "t2", "t3"],
    );
    assert.deepEqual(
      get(toastQueue).map((t) => t.title),
      ["t4"],
    );
    mock.timers.tick(WINDOW_MS);
    assert.deepEqual(
      get(toasts).map((t) => t.title),
      ["t4"],
    );
    mock.timers.tick(WINDOW_MS - 1);
    assert.equal(get(toasts).length, 1, "promotion restarts the clock");
    mock.timers.tick(1);
    assert.deepEqual(get(toasts), []);
  });
});
