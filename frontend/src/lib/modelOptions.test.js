import { describe, it, beforeEach } from "node:test";
import assert from "node:assert/strict";
import {
  fallbackModelOptions,
  fetchModelOptions,
  resetModelOptionsCache,
  cheapestFreeOption,
} from "./modelOptions.js";
import { touchCandidates, touchOptions } from "./utils/touchModels.js";

// Tier-catalog shaped rows: served rows pickable, withdrawn and tier-only
// rows (paid plan, limited trial) listed but never served.
const ROWS = [
  { id: "upstage/solar-pro4", agent: "a", served: true, pool: "unlimited" },
  { id: "openai/gpt-6-luna", agent: "a", served: true, pool: "premium" },
  { id: "stealth/ox-alpha", agent: "a", served: false, withdrawn: true },
  { id: "google/gemini-3.8-flash", agent: "a", served: false },
  {
    id: "anthropic/claude-fable-5.1",
    agent: "a",
    served: false,
    offer: { remaining: 3, total: 10, user_remaining: 2, joinable: true },
  },
  { id: "z-ai/glm-5.2", agent: "a", served: false, withdrawn: true },
];

describe("fallbackModelOptions (offline pickers)", () => {
  it("offers no withdrawn row", () => {
    const ids = fallbackModelOptions.map((m) => m.id);
    assert.ok(!ids.includes("z-ai/glm-5.2"));
    assert.ok(!ids.includes("stealth/ox-alpha"));
    assert.ok(!ids.includes("google/gemini-3.8-flash"));
    assert.ok(!ids.includes("anthropic/claude-fable-5.1"));
  });

  it("still defaults to a free row", () => {
    assert.equal(
      cheapestFreeOption(fallbackModelOptions),
      "upstage/solar-pro4",
    );
  });
});

describe("fetchModelOptions (live pickers)", () => {
  beforeEach(() => {
    resetModelOptionsCache();
    globalThis.fetch = async () => ({
      ok: true,
      redirected: false,
      status: 200,
      url: "http://127.0.0.1:4173/admin/api/models",
      json: async () => ({ models: ROWS }),
    });
  });

  it("drops withdrawn and tier-only rows, keeps served", async () => {
    const ids = (await fetchModelOptions()).map((m) => m.id);
    assert.deepEqual(ids, ["upstage/solar-pro4", "openai/gpt-6-luna"]);
  });
});

describe("touchCandidates (streak select)", () => {
  it("keeps served rows, premium last, drops the rest", () => {
    const ids = touchCandidates(ROWS).map((m) => m.id);
    assert.deepEqual(ids, ["upstage/solar-pro4", "openai/gpt-6-luna"]);
  });

  it("fail-open keeps the saved value when the catalog omits it", () => {
    const ids = touchOptions(ROWS, "stealth/ox-alpha").map((m) => m.id);
    assert.ok(ids.includes("stealth/ox-alpha"));
    assert.ok(!ids.includes("z-ai/glm-5.2"));
  });
});
