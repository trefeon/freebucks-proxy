import { describe, it } from "node:test";
import assert from "node:assert/strict";
import {
  distinctPinModels,
  parsePinEntries,
  predictNextAccount,
} from "./poolStrategy.js";

const row = (index, over = {}) => ({
  index,
  email: `acct${index}@example.com`,
  session_status: "idle",
  locked: false,
  cooldown_active: false,
  ...over,
});

describe("parsePinEntries", () => {
  it("parses slot:model pairs, skips malformed parts", () => {
    assert.deepEqual(parsePinEntries("0:z-ai/glm-5.2;1:deepseek/x"), {
      0: "z-ai/glm-5.2",
      1: "deepseek/x",
    });
    assert.deepEqual(parsePinEntries("banana;2:"), {});
    assert.deepEqual(parsePinEntries(""), {});
  });
});

describe("distinctPinModels", () => {
  it("dedupes in first-seen slot order", () => {
    assert.deepEqual(distinctPinModels("1:b;0:a;2:a"), ["a", "b"]);
    assert.deepEqual(distinctPinModels(""), []);
  });
});

describe("predictNextAccount", () => {
  it("names the first healthy lane in roster order", () => {
    const pick = predictNextAccount([row(1), row(0)], "");
    assert.deepEqual(pick, {
      index: 0,
      email: "acct0@example.com",
      warm: false,
      pinnedModel: "",
    });
  });

  it("skips locked, banned, cooling lanes and reports warmth", () => {
    const tokens = [
      row(0, { locked: true }),
      row(1, { ban_type: "hard" }),
      row(2, { session_status: "quarantined" }),
      row(3, { cooldown_active: true }),
      row(4, { session_status: "active" }),
    ];
    const pick = predictNextAccount(tokens, "");
    assert.equal(pick.index, 4);
    assert.equal(pick.warm, true);
  });

  it("applies the selected pin model, skips other pins", () => {
    const tokens = [
      row(0, { pinned_model: "other/model" }),
      row(1, { pinned_model: "deepseek/x" }),
      row(2),
    ];
    const pick = predictNextAccount(tokens, "deepseek/x");
    assert.equal(pick.index, 1);
    assert.equal(pick.pinnedModel, "deepseek/x");
  });

  it("any-model prediction skips reserved pinned lanes", () => {
    const pick = predictNextAccount(
      [row(0, { pinned_model: "a/b" }), row(1)],
      "",
    );
    assert.equal(pick.index, 1);
  });

  it("never guesses: ambiguous model, missing fields, or parked pool", () => {
    assert.equal(
      predictNextAccount([row(0)], null),
      null,
      "ambiguous pins name no lane",
    );
    assert.equal(predictNextAccount([], ""), null, "no rows name no lane");
    assert.equal(
      predictNextAccount([{ email: "no-index" }], ""),
      null,
      "index-less rows name no lane",
    );
    assert.equal(
      predictNextAccount([row(0, { locked: true })], ""),
      null,
      "fully parked pool names no lane",
    );
  });
});
