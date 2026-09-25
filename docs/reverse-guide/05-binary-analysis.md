# Binary Analysis (Break-Glass)

Applicability: the freebuff vendor surface is JS/TS + Go (npm wrapper, CLI,
repo code) — there is normally NO native binary to reverse. This file applies
only when a native artifact appears: a desktop build, a bundled helper binary,
or an obfuscated native chunk inside an otherwise-JS payload. Default to the
static-first wire method ([02-static-wire-method.md](02-static-wire-method.md),
re-kit/snapshots, `scripts/repin-all.sh`); reach for this page only on that
trigger. Start at [README.md](README.md); scope rules in
[01-scope-evidence.md](01-scope-evidence.md); runtime or bundle leads route via
[03-dynamic-capture.md](03-dynamic-capture.md) and
[04-js-bundle.md](04-js-bundle.md) first.

## rev-symbol: stripped-name recovery

Trigger: stripped/so-packed function with unknown purpose.
Method (own words): match magic constants first (AES S-box, CRC32 table, zlib
headers), then paired-call idioms (alloc/free, open/close, lock/unlock), then
arg-shape/return patterns (`socket(2,1,0)`, `memcpy` dst/src/n), then trace
caller/callee xrefs against the import/export table; web-search residual
constants as fallback. Record each rename with the evidence that justified it.

## rev-struct: layout recovery

Trigger: opaque pointer passed across several unknown functions.
Method (own words): aggregate every `*(ptr+off)` access with its width across
all callers (offsets used) and callees (nested offsets), size the struct from
its `malloc` call, then fit vtable / linked-list / refcount / inline-string
idioms to the access pattern. Emit a fixed template: offset, width, inferred
type, first-seen caller.

## rev-frida: dynamic confirmation

Trigger: static reading gives two candidate behaviors and only runtime
disambiguates. Doctrine: use the modern Frida API with a load-timing helper
(hook now if loaded, else on load); restrain init-hooks (they perturb startup
timing and hide the behavior you want); prefer the constructor-dispatcher
pattern — hook constructors to find live instances, then hook methods on those.

## rev-unicorn: fragment emulation

Trigger: a single function must be executed without running the whole binary
(e.g. a decrypt/decode routine). Loop: raw-load the bytes first, simulate only
the environment the fragment touches (libc stubs, syscalls), skip calls via
the PC=LR pattern, run → crash → diagnose → fix. Stop emulating once the
fragment's input/output contract is characterized; do not build a full harness.

## rev-idapython: batch scale

Trigger: more than a handful of functions need the same treatment (rename,
decompile, xref dump). Method (own words): headless IDA batch-decompile with
multiprocess fan-out, then agent-side analysis of the exported C + strings +
imports/exports. One script, rerunnable; never hand-rename at scale.

## NOT-APPLICABLE to freebuff

- rev-dex-dumper (Android dex unpack via adb push/pidof/pull): no Android
  artifact in the vendor surface.
- rev-u3d-dump (Unity IL2CPP metadata via roytu fork): no Unity artifact.
- rev-ios-dump (iOS decrypt via frida-ios-dump + cryptid-0): no iOS artifact.

If a mobile/desktop freebuff build ever ships, re-evaluate only the matching
unpacker — the symbol/struct/Frida/unicorn methods above still apply first.

## Sources

- P4nda0s/reverse-skills: rev-symbol, rev-struct, rev-frida, rev-unicorn-debug, rev-idapython.
- Local pins: `devdocs/re-kit/`, `scripts/repin-all.sh`, `backend/internal/wirefacts/`.
