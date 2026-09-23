# DESIGN.md — dashboard UI/UX standard (enforceable)

Source of truth is `frontend/src/app.css`; this file states the rules.
`MUST` / `MUST NOT` are gates for review. Items marked `SPEC` are documented
and queued — implement only where zero-risk single-line, else document only.
Theme law: dark-only terminal cyber ops, obsidian surfaces, hairline borders,
radius 3px buttons / 4px cards, accent `#28c244`/`#00ff66`, Geist + JetBrains
Mono, no shadows beyond the focus ring + the `.led-critical` halo + the SPEC
Toaster `shadow-lg`, no gradients/glow.

## Theme

Dark-only (`color-scheme: dark`), "terminal cyber ops" instrument feel:
obsidian surfaces, hairline enclosures, no shadows beyond the focus ring, the
`.led-critical` halo and the one queued Toaster `shadow-lg` removal.

## Tokens (`:root` in `app.css`)

| Role    | Token(s)                                            | Value(s)                          |
|---------|-----------------------------------------------------|-----------------------------------|
| bg      | `--fp-bg`                                           | `#0b0e14`                         |
| surface | `--fp-surface` / `--fp-surface-2` / `--fp-inset`    | `#131822` / `#161b26` / `#08090c` |
| input   | `--fp-input-bg`                                     | `#08090c`                         |
| border  | `--fp-border` / `--fp-border-bright`                | `#1f2633` / `#232b3b`             |
| text    | `--fp-text` / `--fp-muted` / `--fp-dim`             | `#f4f6fb` / `#94a3b8` / `#64748b` |
| accent  | `--fp-accent` / `--fp-accent-hover` / `--fp-accent-dim` | `#28c244` / `#00ff66` / `rgba(40,194,68,.12)` |
| state   | `--fp-success` / `--fp-warning` / `--fp-error` / `--fp-info` | `#22c55e` / `#f59e0b` / `#ef4444` / `#38bdf8` |
| state-deep | `--fp-error-deep`                                | `#7f1d1d`                         |
| radius  | `--fp-radius-sm` / `--fp-radius`                    | `3px` / `4px`                     |
| motion  | `--fp-ease` / `--fp-duration` / `--fp-duration-lg`  | `cubic-bezier(.23,1,.32,1)` / `150ms` / `250ms` |
| focus   | `--fp-ring`                                         | `0 0 0 2px bg, 0 0 0 4px accent`  |
| shadow  | `--shadow-soft` (declared, unused)                  | `0 4px 20px -2px rgba(0,0,0,.35)` |
| btn-h   | `--fp-btn-h-sm` / `--fp-btn-h-md` / `--fp-btn-h-lg`  | `32px` / `40px` / `48px`          |
| btn-pad | `--fp-btn-pad-sm` / `--fp-btn-pad-md` / `--fp-btn-pad-lg` | `6px 10px` / `10px 16px` / `12px 20px` |

## Type

- Sans: Geist (`400/500/600/700`). Mono: JetBrains Mono (`400/500/600`).
- Numeric data is always mono + tabular (`.fp-num`, `.tabular-nums`).
- Prose/labels are sans; values, ids, timestamps, key material are mono
  (`.fp-mono`, `.fp-num`). SPEC: audit inputs — text entry that is key
  material MUST carry mono (e.g. `DevTools.svelte:512` textarea already
  `fp-input fp-mono`; `Tokens.svelte:608-612` add-token input is text entry
  for key material and MUST gain `fp-mono`).

## Buttons (enforceable)

### Scale

`min-height` is a floor, never a fixed height. Base `.fp-btn` IS md.

| Size | Class | min-height | Padding | Font | Gap | Radius | Allowed surfaces |
|------|-------|-----------|---------|------|-----|--------|------------------|
| sm | `.fp-btn-sm` | 32px (`--fp-btn-h-sm`) | 6px 10px | 12px/500 | 5px | `3px` | dense tables, toolbars ONLY, keep ≥4px inter-target gaps |
| md (default) | `.fp-btn` | 40px (`--fp-btn-h-md`) | 10px 16px | 13px/500 | 6px | `3px` | everywhere (default workhorse) |
| lg | `.fp-btn-lg` | 48px (`--fp-btn-h-lg`) | 12px 20px | 14px/600 | 8px | `3px` | login / empty-state / hero CTA, max one per view |

Icon-only: square per size — 32 / 40 / 48px (`min-width` tracks the size
token via `.fp-copy-icon` + size class). Icon-only MUST have an action
`aria-label` (e.g. `aria-label="Copy"`), never a caption. Raw-`<button>`
exception: icon-only chrome that is NOT an action (Modal close/eye `p-1`,
AnnouncementsBanner folds, Toaster 32px dismiss) MUST keep ≥24px floor +
`aria-label`; anything that performs an action MUST be `Button.svelte`.

Touch uplift (`@media (pointer: coarse)`, in `app.css`):
`.fp-btn` → 44px, `.fp-btn-sm` → 36px, icon-only squares 44/36px.
Touch MUST never go below 36px.

### Button.svelte API

| Prop | Values (default) | Emitted class |
|------|------------------|---------------|
| `variant` | `primary\|secondary\|ghost\|danger` (`secondary`) | `fp-btn fp-btn-{variant}` |
| `size` | `sm\|md\|lg` (`md`) | `fp-btn-sm` / _(none)_ / `fp-btn-lg` |
| `loading` | `boolean` (`false`) | keeps label + `<Spinner size="sm">` + `aria-busy` |
| `disabled` / `type` / `class` | passthrough | `disabled={disabled \|\| loading}`, `{...rest}` untouched |

Geometry MUST NEVER come from Tailwind: any `h-` / `min-h-` / `p-` /
`text-` class on a `Button.svelte` call site is a defect — delete it, the
`fp-*` class already owns the geometry.

### Hierarchy (one primary per view)

- `primary` — the single intent of the view. DO: Login submit, empty-state
  CTA (`<Button variant="primary" size="lg">Sign in</Button>`). DON'T: two
  primaries side by side — cut `ApiKeysEditor`, `SessionSpawnPanel`,
  `DevTools` views to one primary each.
- `secondary` — default workhorse for all routine actions.
  (`<Button variant="secondary">Refresh</Button>`).
- `ghost` — tertiary / chrome only: dismiss, cancel, toolbar toggles.
  (`<Button variant="ghost">Cancel</Button>`).
- `danger` — destructive confirm ONLY, behind a confirm step, outline style
  (`fp-btn-danger`: transparent fill, error border). DON'T ship raw
  `bg-red-600`/`bg-amber-600` fills — `ConfirmModal.svelte:118-122` MUST
  convert to `Button.svelte` md + `danger`.

### Density

- `sm` is legal ONLY in dense tables and toolbars with ≥4px inter-target
  gaps. Per Wave-1 map: `LiveConsole` 32px `!h-8` cluster + `Activity:78` →
  `sm`; `TokenCard:212` + `TokenDetailsDrawer:164/192/202/347` 28px `!h-7`
  cluster → `sm` (coarse uplift carries them to 36px on touch).
- `sm` is ILLEGAL for form submits, modal footers, login, empty states.
  `ConfirmModal:106-122` slim footer MUST go md, not sm.
- `lg` is ILLEGAL anywhere except login / empty-state / hero CTA, max one
  per view.

### Geometry ban

Buttons are sized by `fp-*` classes + `--fp-btn-*` tokens ONLY. Tailwind
geometry utilities (`h-*`, `min-h-*` (incl. `!h-8`/`!h-7`), `p-*`, `text-*`)
on any button, link-styled-as-button, or radio/reset acting as a button are
defects. Class-only `fp-btn` sites (no `Button.svelte`) MUST convert to
`Button.svelte` or an `<a>`-styled equivalent minus geometry overrides:
`Sidebar:243`, `DurationPicker:20`, `NumberStepper:36/54`,
`StrategyPresetCard` radios/resets, `Overview:471 <a>`,
`CommandCenter:193 <a>`, `Announcements:250 <a>`,
`Review:605/674/750` bare-base. `TokenCard` double-danger rows MUST cut to
one danger.

### Contrast + focus contract

- Text contrast ≥ 4.5:1, large text / UI chrome ≥ 3:1 (sole authority:
  `better-accessibility` skill).
- Visible focus always: global `:focus-visible` 2px accent outline + 2px
  offset; never remove it. `--fp-ring` is the only shadow in active use:
  `--shadow-soft` is declared in `app.css:69-70` but referenced nowhere, and
  the Toaster's `shadow-lg` (`Toaster.svelte:60`) is the one queued removal.
- Forced-colors fallback (`@media (forced-colors: active)` in `app.css`):
  buttons drop token fills for `ButtonFace`/`ButtonText`, disabled maps to
  `GrayText`, focus ring becomes 3px `Highlight`. Any new focus treatment
  MUST add its forced-colors line in the same block.

### Motion / hover / touch

- Button `:hover` styles live ONLY inside `@media (hover: hover)` — touch
  devices MUST never see sticky hover fills.
- `:active` `translateY(1px)` is killed under
  `prefers-reduced-motion: reduce` (rule in `app.css`); the global
  reduced-motion kill (durations → 0.01ms) covers the rest.
- No shadows, gradients, or glow on buttons — ever.

### Migration checklist (grep-verifiable, run from repo root)

- [ ] `min-height: 44px` in `app.css` hits ONLY the coarse-uplift block:
  `grep -n "min-height: 44px" frontend/src/app.css`
- [ ] `.fp-btn-lg` exists in `app.css` AND `Button.svelte` accepts `lg`:
  `grep -rn "fp-btn-lg" frontend/src/app.css frontend/src/lib/components/Button.svelte`
- [ ] No Tailwind geometry on buttons: `grep -rEn "!h-[78]|min-h-\[44px\]" frontend/src/lib frontend/src/pages` → hits ONLY the documented mobile touch-target uplift (`TokenCardMobile.svelte:258`, `[&_.fp-btn]:min-h-[44px]`), zero elsewhere
- [ ] No raw destructive fills: `grep -rn "bg-red-600|bg-amber-600" frontend/src` → 0 hits (→ `fp-btn-danger`)
- [ ] One primary per view: `grep -rn 'variant="primary"' frontend/src/lib frontend/src/pages` — every file with 2+ hits MUST cut to one
- [ ] One danger per row: `grep -rn 'variant="danger"' frontend/src/lib` — `TokenCard` MUST show one

## Other families (SPEC — file:line pointers as observed on `feat/button-standard`)

### Cards

- One Card grammar: `Card.svelte:24` (`fp-card` section, radius 4px, defined
  edge, no shadow) with `header` / body / `footer` slots (`Card.svelte:26-67`).
  New panels MUST use `Card.svelte`, not hand-rolled bordered divs.
- SPEC: no `@theme` aliases — `app.css:10-17` defines `--color-surface` /
  `--color-border-subtle` / `--color-text-muted` / `--color-primary` as
  Tailwind aliases over `fp-*` tokens; `HiddenKeysCard.svelte:118,126`
  consumes them (`bg-surface border-border-subtle`, `bg-primary/10
  text-primary`). New code MUST use `bg-[var(--fp-*)]` / `border-[var(--fp-*)]`
  directly; excise aliases + the two call sites when a lane owns those files.
- SPEC: no redundant `bg-[var(--fp-surface)]` + `border-[var(--fp-border)]`
  pairs on elements already inside `fp-card` (e.g. `Modal.svelte:144-147`
  dialog card re-declares both) — inherit the card, add only what changes.

### Inputs

- `fp-input` on text/password/textarea controls; `fp-select` on ALL
  `<select>`. SPEC: the `fp-input`-classed selects in
  `BatchTestPanel.svelte:113-130`, `LiveConsole.svelte:1192-1194`,
  `LiveConsole.svelte:1402-1404`, `SessionSpawnPanel.svelte:60-62`,
  `TokenDetailsDrawer.svelte:140-143,336-339`,
  `DevTools.svelte:403-475,704-705`, `Review.svelte:722-735`,
  `LogLevelSettings.svelte:126-128` MUST migrate to `fp-select`
  (reference shape: `MaturityPanel.svelte:505-508`,
  `AdvancedSettings.svelte:278-281`); `GatewaySettings.svelte:198-200`
  already `fp-select` but re-declares bg/text/border/focus utilities the
  class owns — strip the redundancies.
- Password fields MUST carry `fp-input` (verified: `Login.svelte:110-113`,
  `DevTools.svelte:492-496`, `ChangePasswordModal.svelte:116-120,150-152`,
  `AccessSecurityCard.svelte:226-230,261-264`).
- Invalid state: `.fp-input[aria-invalid]` border rule is live in `app.css`;
  `Field.svelte:41-42` already sets/clears `aria-invalid` — no call-site
  change needed.
- SPEC: `.fp-select option` hardcodes `#141a25` / `#e9edf3` (`app.css`
  select-option rule) — move to `fp-*` tokens when a lane owns `app.css`.

### Tables

- Data tables MUST be `.fp-table` (hairline rows, left text, right `.num`
  mono numbers, `tr:last-child` no bottom border, mobile tightening in
  `app.css`). Reference: `MetricsPanel.svelte:253,435` (incl. `sr-only`
  captions), `ModelsPanel.svelte:240,296,300,304,340`,
  `TeamUsagePanel.svelte:130`, `TokenTable.svelte:154-155`,
  `DevTools.svelte:608`.
- SPEC: `Review.svelte:689` runs a bare `w-full text-left font-mono
  text-[11px]` table — move onto `.fp-table`.
- SPEC: arbitrary cell overrides (`TracesPanel.svelte:319-320`,
  `TokenTable.svelte:155` `[&_td]:!px-*`) are tolerated per-table density,
  MUST NOT leak into the shared `.fp-table` rule.
- Tables MUST NEVER produce a horizontal scrollbar at any supported width (390-1440 CSS px): every `table` and the page itself MUST satisfy `scrollWidth - clientWidth <= 1`. When cell content would stretch a column past its budget, the content stacks INSIDE the cell into a two-line composition (primary line, secondary line — `flex-col`, `truncate` + `title` on the truncating line, `min-w-0` on every flex item) instead of widening the table. `overflow-x-auto` wrappers stay as fail-safes: the rule governs behaviour (no scrollbar appears at supported widths), not the class. References: `TokenTable.svelte` Account cell (fixed 180px, truncating email line) and the `pool-table.spec.ts` geometry contract.
- Catalog reference for the same rule: `ModelsPanel.svelte` (the table renders only where it genuinely fits; narrower widths use the stacked card list). Guarded by `e2e/table-overflow.spec.ts`.

### Modals

- Single icon-badge size: badge `p-2.5` + Lucide `size={20}`
  (`ConfirmModal.svelte:89-101`); modal icons are Lucide, never emoji.
- SPEC: `ConfirmModal.svelte:89-93` badge reds/ambers
  (`bg-red-500/15 text-red-400 border-red-500/30`,
  `bg-amber-500/15 ...`) MUST move to `fp-*` state tokens; footer
  `bg-red-600`/`bg-amber-600` (`ConfirmModal.svelte:118-122`) MUST become
  `Button.svelte` md + `danger` (sister lane `MigrateButtons` owns this file).
- Loading in modals uses `Spinner` (the `Button.svelte:33-35` pattern),
  never emoji or text-only. Focus trap + restore + Escape live in
  `Modal.svelte:62-114` (restore at `:62-63`, trap at `:78-90`, Escape at
  `:70`); backdrop close button `Modal.svelte:134-140`.

### Badges

- SPEC: token-swap every `emerald-*` / raw-hex / `red-*` / `amber-*` /
  `zinc-*` badge to `fp-*` state tokens (`--fp-success`, `--fp-warning`,
  `--fp-error`, `--fp-info`, `--fp-muted`/`--fp-dim`):
  `AllowancesPanel.svelte:220,245,263` (`text-emerald-400`,
  `bg-emerald-500/10`, `bg-emerald-500` bar), `AnnouncementsBanner.svelte:147`
  (`bg-emerald-500/10 text-emerald-400`), `CommandCenterCard.svelte:131`
  (`border-emerald-500/30 bg-emerald-500/10 text-emerald-400`),
  `BridgeTokenCard.svelte:39` (`bg-[#f59e0b]/15 text-[#f59e0b]`),
  `ModelsPanel.svelte:192-194,296-299` chips, `ModelsPanel.svelte:245,322`
  (`text-emerald-400`), `FreebucksQuotaBar.svelte:27-30` (`pctColor` hex
  returns), `LiveConsole.svelte:581,589` + console stream colors
  `1051-1092` (`zinc-*`, `green-*`, `amber-*`, `red-*`).
- Single chip size: `10px` semibold uppercase tracking-wider, `px-1.5/2
  py-0.5`, `fp-radius-sm` border (reference: `CommandCenterCard.svelte:131`,
  `BridgeTokenCard.svelte:39` shape minus its hex).
- `led-pulse` (2s) is live-status ONLY — never on idle/static badges
  (`StatusBadge` `pulse` prop: `TokenCard.svelte:186` reference).

### Toggles

- Pill exception is blessed: `ToggleSwitch.svelte:61,75` (`w-11 h-6`
  rounded-full track + sliding thumb) is the ONLY rounded-full control;
  everything else stays 3px radius.
- `role="switch"` + explicit `aria-checked` contract holds
  (`ToggleSwitch.svelte:64-68`); native checkbox gives keyboard + label-click
  free — keep it.
- 24px floors: `SegmentedControl.svelte:28-32` sizeClasses (`xs` 24px / `sm`
  28px / `md` 34px); toggle-family controls MUST NOT go below 24px.
- SPEC: `SegmentedControl` uses per-option `aria-pressed`
  (`SegmentedControl.svelte:50`, same pattern `DurationPicker.svelte:25`) —
  add roving `tabindex` (active `0`, rest `-1`) + arrow-key move when a lane
  owns the file; behavior otherwise unchanged.

### Feedback

- `Alert.svelte` is the inline pattern (icon + 2px tinted left border + tone
  fill): tones `info|success|warning|error`, icons `Info|CheckCircle2|
  AlertTriangle|AlertCircle` (`Alert.svelte:2-30`) — error icon tone already
  correct, no change. Toasts (`stores/toast.js:7-10`: max 4 visible,
  10s auto-dismiss for every tone — only an explicit `sticky: true` opts out)
  mirror ONLY action receipts; contextual states (session-expired,
  default-password, DB-overlay degraded, upstream drift, maturity
  kill-switch) stay inline as `<Alert>`.
- SPEC: `Toaster.svelte:60` `shadow-lg` MUST become a hairline
  `border-[var(--fp-border)]` (already present) with no shadow — the theme
  allows no shadow beyond the focus ring and the `.led-critical` halo.
  Sister-lane file: document only.

### Layout

- Row edge padding belongs to `:first-child` / `:last-child`, never to
  every row: reference `.fp-table tbody tr:last-child td { border-bottom:
  none }` (`app.css`) — the same principle governs settings rows and log
  lists (`LiveConsole.svelte:1315` `divide-[var(--fp-border)]` stays the
  divider mechanism).
- Stable anchors: `id="setting-{entry.key}"` cross-page jump targets
  (`AdvancedSettings.svelte:235`) with `scroll-mt-24`; `SettingsRow
  first={isFirst}` (`AdvancedSettings.svelte:237`) owns edge spacing.
- Counter/id hygiene: literal `id="…"` MUST be unique across simultaneously
  mounted components — verified clean (`dev-burst-model`/`dev-burst`
  `BatchTestPanel.svelte:114,128`; `dev-model`/`dev-account`/`dev-proto`/
  `dev-stream`/`dev-reasoning`/`dev-client-key`/`dev-prompt`
  `DevTools.svelte:404-510`; `log-level`/`log-msg`/`logs-page-size`
  `LiveConsole.svelte:1193,1215,1403`; `token` `Login.svelte:110`;
  `settings-search` `Settings.svelte:123`; `add-token-input`
  `Tokens.svelte:632`). Never copy a literal `id` into a second component;
  per-row controls MUST use `Field` auto-ids or suffixed ids.

## Rules

1. Add new UI with existing `fp-*` classes; new tokens need a companion `app.css` change.
2. Buttons differ by rank (primary/secondary/ghost/danger), not hue.
3. Numbers right-aligned, mono, tabular. Status is an LED dot + label, never color alone.
4. Visible focus ring on everything (`:focus-visible`); never remove it.
5. Respect `prefers-reduced-motion`.
6. Button geometry is `fp-*` + `--fp-btn-*` tokens only — Tailwind sizing utilities on a button are defects.
7. One `primary` per view; `danger` only behind a confirm step.
8. New colors MUST be `fp-*` tokens — no `emerald-`/`zinc-`/`red-`/`amber-` scales, no raw hex in markup. (The remaining violations are enumerated as SPEC items under "Other families" — that list, not this rule, tracks what is still un-swapped.)
9. Tables MUST be `.fp-table`; selects MUST be `.fp-select`; password/key-material inputs MUST be `fp-input` (+ mono for key material). Tables never scroll horizontally — cell content stacks onto more lines instead (see "### Tables").
