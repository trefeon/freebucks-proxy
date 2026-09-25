import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { isDevToolsEnabled, isDevToolsEnabledFromConfig } from "./devtools.js";

// The unified-store gate contract: the live effective[] snapshot wins (an
// overlay save flips the gate without a restart); the .env export parse is
// only the fallback for payloads without a DEVTOOLS_ENABLED row.
describe("devtools gate (effective-first, env-export fallback)", () => {
  it("prefers the effective snapshot over the export", () => {
    const cfg = {
      env_content: "DEVTOOLS_ENABLED=true\n",
      effective: [{ key: "DEVTOOLS_ENABLED", value: "false", secret: false }],
    };
    assert.equal(isDevToolsEnabledFromConfig(cfg), false);
  });

  it("reads true/1 spellings from the snapshot", () => {
    for (const value of ["true", "TRUE", "1"]) {
      assert.equal(
        isDevToolsEnabledFromConfig({
          env_content: "",
          effective: [{ key: "DEVTOOLS_ENABLED", value, secret: false }],
        }),
        true,
        value,
      );
    }
    assert.equal(
      isDevToolsEnabledFromConfig({
        env_content: "DEVTOOLS_ENABLED=true\n",
        effective: [{ key: "DEVTOOLS_ENABLED", value: "0", secret: false }],
      }),
      false,
    );
  });

  it("falls back to the export parse without a snapshot row", () => {
    assert.equal(
      isDevToolsEnabledFromConfig({
        env_content: "DEVTOOLS_ENABLED=1\n",
        effective: [],
      }),
      true,
    );
    assert.equal(isDevToolsEnabledFromConfig({}), false);
    assert.equal(isDevToolsEnabledFromConfig(null), false);
  });

  it("keeps the legacy export predicate intact", () => {
    assert.equal(isDevToolsEnabled("DEVTOOLS_ENABLED=true\n"), true);
    assert.equal(isDevToolsEnabled("DEVTOOLS_ENABLED=0\n"), false);
    assert.equal(isDevToolsEnabled(""), false);
  });
});
