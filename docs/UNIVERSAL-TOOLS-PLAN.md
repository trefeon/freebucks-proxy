# Universal tool translation — plan

Status: proposed · 2026-10-01 · supersedes nothing; extends
[`tool-name-translation.md`](decisions/tool-name-translation.md) and
[`OMP-TRANSLATION.md`](OMP-TRANSLATION.md).

Goal: any client — not just the OMP/pi family — gets its own tool names AND
its own argument shapes on both legs, without the upstream ever seeing a
foreign schema, a duplicated name, or a malformed call.

## 1. Where we actually stand (measured, 2026-10-01)

| Layer | Coverage today | Evidence |
|---|---|---|
| Name mapping (request) | **103** client names → official signature | `convert/toolmap_request.go` `clientToOfficial` |
| Name restore (response) | every mapped + passthrough name, exact client casing | `convert/toolmap_response.go`; corpus sweep |
| Researched surface | **637 names / 21 harnesses** | `convert/testdata/tool_calls_corpus.json` (regenerate: `scripts/extract-tool-calls.sh`) |
| Classification of that corpus | **36 official · 142 mapped · 459 passthrough** | measured 2026-10-01 against the live maps |
| Distinct names with NO mapping | **402** | same measurement; the Phase 0 work queue |
| Wire canonical set | 16 CLI floor defs | `upstream/testdata/cli-tools.json` |
| **Argument shape translation** | **OMP/pi only** (`floorOnly` + `tools_reshape.go`) | ADR non-goal: "No argument or schema translation" |
| Unicode/grammar legalization | all clients | `wireVirtualName`, `resolveUpstreamTool` |

So the asymmetry is precise: **names are universal; argument shapes are
OMP/pi-only.** Every other client is assumed to fill arguments in official
shape — true only while the client's own schema rides the wire unchanged
(names-only translation). That assumption breaks the moment a client's tool
has the same *intent* but a different *shape* (Codex `apply_patch` vs
`str_replace`; Claude `TodoWrite` vs `write_todos`; SWE-agent/OpenHands
trajectory tools).

## 2. Design: one registry, two legs, three tables

Replace the OMP-special-cased reshape with a single registry that every
client family reads. Nothing new on the wire; the gate constraint is
unchanged.

```
reference/ corpus ──► tools.json (generated)     ─┐
                       name table:  client → wire │
                       arg table:   wire → client │──► convert (both legs)
                       floor table: when to swap  ─┘
```

1. **Name table** (exists): `clientToOfficial` + `officialTools`.
2. **Arg table** (generalize): today's `reshapeRules` keyed by wire name,
   but selected by the *client family* rather than a `floorOnly` boolean.
   Shape per entry: `{wire, family, reshape(in) -> []out, cliKeys[]}`.
3. **Floor table** (generalize): the OMP `floorOnlyOMP` substitution becomes
   one instance of "this family's declared schemas trip the gate → substitute
   + reshape". Any future family with the same problem is a table row, not a
   new branch.

Client family = detected today for OMP by `isOMPToolsetBody` (intent-`i` prop
or 2+ signature names). Generalize to a `detectFamily(body) Family` returning
OMP / Claude-Code / Codex / generic, with the same "never floor on doubt"
rule (invalid body → generic).

## 3. Phases (each ends with an independently testable deliverable)

### Phase 0 — Baseline audit (no code)
- [ ] Produce the classification matrix for all 637 corpus names from the
      live maps (the counting script already runs: 36 official / 142 mapped /
      459 passthrough / 402 distinct unmapped).
- [ ] Triage the 402 unmapped names into: **(a) mappable** (intent matches a
      floor tool — e.g. list/`apply_layout`/`code_index`-style), **(b)
      family-private** (spawn/agent/browser/checkpoint — no official
      equivalent, correctly passthrough), **(c) UI hooks** (`ask_*`,
      `suggest*`, `canvas_ui` — the text-fallback class).
- [ ] For each of the 16 floor tools, list client tools needing **arg
      translation** (same intent, different shape).
- [x] Deliverable: [`UNIVERSAL-TOOLS-AUDIT.md`](UNIVERSAL-TOOLS-AUDIT.md) —
      created 2026-10-01 with the protocol matrix, measured buckets, the
      per-wire arg-shape table and the OpenAI/Responses/Anthropic surface
      design. Remaining: add `claude-code` + `aider` corpus rows (currently
      absent), and finish the audit-pending arg rows with
      `reference/<repo>` `file:line`.
- Verify: counts reproduce with one script run and are byte-identical on
  re-run; bucket (a) contains no name whose schema is already identical to
  its floor target (those need no rule).

### Phase 1 — Registry extraction (pure refactor, no behavior change)
- [x] One registry API in `convert/registry.go`: `NameFor`,
      `IsOfficialWireName`, `ReshapeWireNames`, `FloorRoutes`, `FamilyOf`.
      The tables stay beside their owning code (same package) and every
      access goes through these accessors, so a new family/mapping is added
      in one place. (Storage migration into the file is deferred: moving the
      literals buys nothing the accessors do not, at added churn risk.)
- [x] `floorOnly` semantics bit-identical for OMP (no rule changed).
- [x] Tests: the existing corpus sweep, `TestComprehensiveToolClassification`,
      and all reshape tests pass **unchanged** — the behavior-preservation
      proof. (An unrelated uncommitted WIP test,
      `TestSanitizeChunkMapped` in `convert_stream_test.go`, is red on its
      own; not this phase's.)
- Verify: `go test ./backend/internal/convert/ ./backend/internal/server/`
  green with zero test edits.

### Phase 2 — Arg rules for the highest-value non-OMP families
- [x] Name-map slice (not arg): Continue's shipped `BuiltInToolNames` are
      snake_case (`core/tools/builtIn.ts`), but the table only carried
      concatenated keys (`searchweb`, …) that never matched — `search_web`,
      `fetch_url_content`, `file_glob_search` were riding unrenamed. Added
      the three scalar-shaped mappings + `toolmap_continue_test.go`.
- [ ] **Shape-sensitivity finding:** a mapped wire name in
      `substitutedWireNames` gets the canonical CLI *definition*, so the model
      fills CLI args and the client receives them under its own name. Adding a
      name mapping to such a wire is only safe when the client's arg shape
      matches the canonical shape (or the family reshapes it). Non-matching
      names need a reshape rule, not just a mapping.
- [ ] For each candidate from Phase 0, add a table entry + a RED test first
      (`tools_reshape_test.go` style), then the rule.
- [ ] Order by measured frequency in the corpus, one family per commit.
- Verify: per-rule RED→GREEN; the family's end-to-end conformance test
  (`server/conformance_*_test.go`) still green.

### Phase 3 — Family detection beyond OMP
- [ ] `detectFamily` with the "never floor on doubt" rule; unknown → generic
      (today's names-only path).
- [ ] Property test: **every** corpus harness, run through the real handler,
      yields (a) wire names unique + grammar-legal, (b) no foreign schema when
      the family floors, (c) exact client-name restore on every surface, (d)
      no call the client cannot dispatch (the OMP text-fallback invariant,
      generalized).
- Verify: extend `server/floor_omp_surface_test.go` into a family-parameterized
  sweep; red on a synthetic unmapped family.

### Phase 4 — Response-leg parity
- [ ] Port the withhold/inject architecture to streaming Anthropic/Responses
      (today: chat only) so fan-out and any future reshape apply on all three
      surfaces. This is the one deliberately deferred piece (ADR §4).
- Verify: a fan-out turn on `/v1/messages` streaming produces N blocks with
  unique ids, mirroring the chat relay's pinned behavior.

## 4. Guardrails (non-negotiable)

- **Wire fidelity**: the gate keys on tool *definitions*; never let a foreign
  schema ride. Substitution, not virtualization, for a flooring family.
- **Total rules**: unknown shape / invalid JSON / unmapped name → verbatim
  passthrough, never a dropped call.
- **One mapper per request**: never rebuild the mapper from the response body
  (the #685 `mcp__execute` leak).
- **No prose matching for policy**: gate/retry decisions key on error code +
  HTTP status, except where the wire provably has one code for two meanings
  (refund vs takeover `session_superseded`), which is documented at the site.
- **Never bulk-re-pin behavior tests**: when a policy changes, rewrite the
  test with the new contract and record the supersession (see
  `TestChatSessionSupersededRefundRejoins`).

## 5. Risks

| Risk | Mitigation |
|---|---|
| Family misdetection floors a client that needed its schema | "never floor on doubt"; a flooring family must be proven under the gate |
| Arg rules drift from the client's real schema | corpus regeneration is scripted and byte-identical; each rule cites `reference/...file:line` |
| More rules → more surface for a silently dropped call | every rule is total + fan-out preserving; the sweep asserts no call vanishes |
| Registry refactor breaks OMP | Phase 1 is behavior-locked by tests that must not change |

## 6. Out of scope

- New wire tools (the gate forbids them).
- BYOK / non-free lanes (the free-tier path is the only one with a gate).
- Upstream server behavior (`reference/npm-freebuff`, `gateways/`, `routers/`
  own no tools; chained traffic inherits the client rows).
