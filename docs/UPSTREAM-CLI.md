# Upstream CLI — reference

Complete reference for the official **upstream CLI** (npm package `freebuff`), the
reference client for everything the proxy mirrors on the wire. Audience:
freebucks-proxy maintainers (session/wire parity, registry rows, error taxonomy)
and users driving the CLI through the gateway.

- **Audited pin**: `8ed5d3e5e` — the gitignored upstream vendor clone's tip, and
  the tree the citations corrected in this revision were verified against.
  Previous audit pins: `2b165f749` (npm `0.0.180`, §14) and before it
  `e2b911eca` (= npm `0.0.178`).
- **Recorded wiregen pin**: `backend/internal/wirefacts/testdata/wire/snapshots.json:2-3`
  records `upstream_sha 2b165f749…` with `vendor_version 0.0.180`, and
  `scripts/vendor-version.txt:1` reads `0.0.180`. That manifest's
  `cli/src/components/freebuff-model-selector.tsx` hash (`snapshots.json:30-31`,
  `5ecfb9ff…`) no longer matches the tip (`7fc1341d…`) — the selector is a
  wire-tracked file (`scripts/check-upstream.sh:126`), so **drift exists** and
  the manifest pin is stale by 15 commits (§14.6).
  The vendor clone *path* lives in `scripts/check-upstream.sh` (`:90-98`); that
  script holds no pin — its ref defaults to the floating `main` (`:81`) and a
  full-SHA ref is only *gated* against `snapshots.json` (`:229-244`).
- **Citations**: every `path:line` is relative to the gitignored upstream vendor
  clone at `8ed5d3e5e`.
  `freebuff/cli/release/package.json` version lags the npm tag in some
  revisions — and since `2b165f749` the npm tag no longer distinguishes
  revisions at all (the wrapper reads `0.0.180` at both the pin and the tip),
  so use the git SHA.
- **Build scope**: everything below describes the upstream build
  (`FREEBUFF_MODE=true` compile-time define → `IS_FREEBUFF`,
  `cli/src/utils/constants.ts:11`), i.e. the shipped `freebuff` binary.
  Behavior of the sibling build (the same source tree compiled without
  `FREEBUFF_MODE`) is mentioned only where it explains shared code paths.
- **Companion docs**: `CLI-Limitations.md` (behavior-by-behavior port audit vs
  the proxy), `UNIVERSAL-CLIENTS.md` (pointing other harnesses at the gateway).

| § | Section |
|---|---|
| 2 | Packaging: wrapper + binary |
| 3 | Invocation surface |
| 4 | Startup sequence |
| 5 | Login, credentials & logout |
| 6 | Session lifecycle & recovery |
| 7 | Command surface & input grammar |
| 8 | TUI surfaces, screens & shortcuts |
| 9 | Models, tiers & reasoning |
| 10 | Limits & error states (wire → UI) |
| 11 | Freebucks, quotas, peak hours & spend ceilings |
| 12 | Config, state files & environment |
| 13 | Launcher, install & self-update |
| 14 | Version delta 0.0.178 → 0.0.180 |
| 15 | Proxy cross-reference |

## 2. Packaging: wrapper + binary

The CLI ships in two layers: an npm wrapper that fetches, verifies, and launches a prebuilt Bun binary, and that binary, which holds all CLI/TUI code.

| Layer | Entry | Runtime | Role |
|---|---|---|---|
| npm wrapper | `freebuff` bin → `freebuff/cli/release/index.js:26-36` | Node ≥16 (`freebuff/cli/release/package.json:28-30`) | binary fetch + sha256 verify + install, background update, crash diagnostics |
| compiled binary | `cli/src/entry.ts:8-12` | Bun `--compile` | broker-child detection, else `await import('./index')` |

- Bin map `freebuff` → `index.js` (`freebuff/cli/release/package.json:6-8`); the wrapper prefers its packaged `launcher.js` and falls back to `cli/release-core/launcher.js` only in source checkouts (`freebuff/cli/release/index.js:6-22`).
- The wrapper spawns the binary with argv passed through unchanged and stdio inherited (stderr piped on win32 only), adding `CODEBUFF_LAUNCHER_PID` (`cli/release-core/launcher.js:1521-1533`). It parses no upstream CLI flags itself.
- **Build-time flag.** `IS_FREEBUFF = getCliEnv().FREEBUFF_MODE === 'true'` (`cli/src/utils/constants.ts:9`); the build exports `FREEBUFF_MODE: 'true'` (`freebuff/cli/build.ts:37-40`) and injects it as `process.env.FREEBUFF_MODE` in the bundler define list (`cli/scripts/build-binary.ts:165-172`). Every `IS_FREEBUFF` branch is therefore DCE'd, so "removed" in the tables below means absent from the shipped parser and registries.
- **Version source of truth.** The binary prints `loadPackageVersion()` = `CODEBUFF_CLI_VERSION` env → `../package.json` version → `'dev'` (`cli/src/cli-args.ts:23-39`). The wrapper's own version and update baseline come from its `package.json` (`freebuff/cli/release/index.js:29`), `0.0.180` at this pin (`freebuff/cli/release/package.json:3`).
- **Branding deltas vs the sibling build.** Wrapper config carries `packageName: 'freebuff'`, `displayName: 'Freebuff'`, `telemetryEvent: 'cli.update_freebuff_failed'` (`freebuff/cli/release/index.js:26-32`); archives are `freebuff-<targetKey>.tar.gz` (`cli/release-core/launcher.js:348-355`); commander registers name `freebuff`, description "Freebuff - Free AI coding assistant" (`cli/src/cli-args.ts:55-56`).
- **Wrapper config dir** is `~/.config/manicode` (`cli/release-core/launcher.js:256-259`), independent of the TUI's `FREEBUFF_CONFIG_DIR` (`cli/src/utils/config-dir.ts:16-35`). Layout: `freebuff` / `freebuff.exe` (`launcher.js:260-267`), `freebuff-metadata.json` (`:268`), `.freebuff-download-temp` (`:269`), `cpu-features.json` (`:458-460`).
- **Artifacts.** `${origin}/api/releases/download/${version}/${packageName}-${targetKey}.tar.gz` (`cli/release-core/launcher.js:1014-1018`); default origin `https://codebuff.com` (`:16`), overridable via `NEXT_PUBLIC_CODEBUFF_APP_URL` (https-only except localhost, `:47-75`). sha256 is checked against the release/NPM doc's `binaryChecksums`, fail-closed (`:91-114`); install atomically replaces binary + sibling `tree-sitter.wasm` + metadata with rollback (`:1087-1132`).
- **CPU fallback.** Baseline targets `linux-x64-baseline` / `win32-x64-baseline` (`cli/release-core/launcher.js:357-360`) are chosen pre-emptively when the cached probe says no AVX2; Windows always assumes AVX2 (`:434-446`). A confirmed SIGILL / `0xC0000409` startup crash re-downloads the baseline and respawns, and only that confirmed case writes `cpu-features.json` (`:453-459`, `:1625-1634`). An explicit `*_BINARY_TARGET` override disables auto-fallback (`:1641-1644`).
- **Crash reporting (win32 only).** stderr is piped and teed so the panic tail survives the terminal reset (`cli/release-core/launcher.js:1524-1533,1560-1600`); `printCrashDiagnostics` prints system info, honest AVX2 state, target, and binary path (`:1430-1455`), then exits with the child's code/signal (`:1690-1696`).
- **Binary-side natives.** Compiled builds self-extract bundled ripgrep next to `process.execPath` and export `getRgPath()`, which sets `CODEBUFF_RG_PATH` for the SDK (`cli/src/native/ripgrep.ts:19-63`); `cli/src/polyfills/bun-strip-ansi.ts` restores `Bun.stripANSI` removed in Bun 1.2. On Windows, child terminal commands are spawned by a detached re-exec of the same binary with `--terminal-command-broker` (`cli/src/utils/terminal-command-broker.ts:340-386`) because extra stdio pipes are fatal under Bun on win32.
- **SPEC vs code drift.** `freebuff/SPEC.md:55-56,61,66` mandates mode `'FREE'`; the shipped parser hardcodes `initialMode = 'LITE'` (`cli/src/cli-args.ts:121-124`), `AGENT_MODES` still lists DEFAULT/LITE/MAX/PLAN (`cli/src/utils/constants.ts:165-173`), and the upstream build's LITE maps to agent id `base2-free` and cost mode `free` (`:167,184`). The parser test pins `'LITE'` (`cli/src/__tests__/cli-args.test.ts:112`). Treat the SPEC mode text as stale.

## 3. Invocation surface

Flags are registered by commander inside `parseArgs()` (`cli/src/cli-args.ts:41-145`); the upstream branch is `:52-73`, the sibling branch `:74-111`.

| Flag | Upstream | Sibling | Effect / default |
|---|---|---|---|
| `-v, --version` | yes (`cli-args.ts:57`) | yes (`:79`) | prints `loadPackageVersion()` (`:23-39`) |
| `-h, --help` | yes (`:73`) | yes (`:108`) | commander auto-help; the upstream build adds no `addHelpText` |
| `--continue [conversation-id]` | yes (`:58-61`) | yes (`:88-91`) | `continue: boolean`; trimmed non-empty id ⇒ `continueId`, else `null` (`:136-140`) |
| `--cwd <directory>` | yes (`:62-65`) | yes (`:92-95`) | `process.chdir(cwd)` inside `initializeApp` (`cli/src/init/init-app.ts:9-12`); unvalidated, a missing dir throws |
| `--trust-agents` | yes (`:66-69`) | yes (`:96-99`) | loads repo `.agents`/`mcp.json` without the trust prompt (CI); defaults `false` (`:143`) |
| `login` — positional, `choices: ['login']` | yes (`:70-72`) | — | the only accepted positional; anything else is a commander "invalid choice" error |
| `[prompt...]` | **removed** | yes (`:109`) | the upstream CLI has no initial prompt: `initialPrompt` is always `null` (`:132`) |
| `--agent <agent-id>` | **removed** | yes (`:80-83`) | the upstream CLI always runs the session-selected model |
| `--clear-logs` | **removed** | yes (`:84-87`) | `clearLogs` stays `false` (`:135`), so `cli/src/index.tsx:331-333` is dead in the upstream build |
| `--lite` / `--free` / `--max` / `--plan` | **removed** | yes (`:100-103`) | the upstream build hardcodes `initialMode = 'LITE'` (`:121-124`) |

**Login is a subcommand, not a flag.** `command === 'login'` is checked before the renderer starts and short-circuits to `runPlainLogin()` (`cli/src/index.tsx:236-248`): it prints a URL, polls `LOGIN_WEBSITE_URL`, saves credentials, and exits `0` on success or `1` on timeout/abort (`cli/src/login/plain-login.ts:26-108`). `--cwd` may appear before or after `login` (`cli/src/__tests__/cli-args.test.ts:93-149`).

**Internal flags parsed before commander** (unknown flags would otherwise be rejected):

| Flag | Where | Behavior |
|---|---|---|
| `--smoke-tree-sitter` | `cli/src/index.tsx:92-167` | `Parser.init` smoke; prints `tree-sitter smoke ok`, exits 0/1 |
| `--smoke-api-url` | `cli/src/index.tsx:170-178` | prints `api-url smoke: <getWebsiteUrl()>`, exits 0 |
| `--smoke-terminal-broker <result-path> <exchange-dir>` | `cli/src/index.tsx:180-208` | native-Windows release gate; exits 2 when args are missing |
| `--terminal-command-broker` | `cli/src/entry.ts:8-12` | broker child mode; also requires `CODEBUFF_TERMINAL_COMMAND_BROKER=1` |

**Flags that do NOT exist:** `-setup`/`--setup`, `-doctor`/`--doctor`, `-refresh-token`/`refreshToken`, `--login`. There is no token-refresh CLI path at all.

**Entry-relevant environment variables** (`cli/src/utils/env.ts:73-89`, `cli/src/utils/config-dir.ts:16-35`):

- Build/runtime identity: `FREEBUFF_MODE` (build define), `CODEBUFF_IS_BINARY`, `CODEBUFF_CLI_VERSION`, `CODEBUFF_CLI_TARGET`, `CODEBUFF_RG_PATH` (set by the binary's self-extracted ripgrep).
- Config location: `FREEBUFF_CONFIG_DIR` — absolute-only, otherwise the CLI throws; defaults to `~/.config/manicode` plus `-<NEXT_PUBLIC_CB_ENVIRONMENT>` off prod.
- Trust/interactivity: `CODEBUFF_TRUST_AGENT_DIRS` (non-interactive trust opt-in), `CODEBUFF_NO_TERMINAL_WATCHDOG`, `CODEBUFF_SHIP_LOGS`, `CODEBUFF_LAUNCHER_PID` (set by the wrapper, `cli/release-core/launcher.js:1529-1533`).
- Windows broker: `CODEBUFF_TERMINAL_COMMAND_BROKER`, `CODEBUFF_TERMINAL_COMMAND_BROKER_PROTOCOL`.
- Wrapper-only: `<PACKAGE>_BINARY_TARGET` / `CODEBUFF_BINARY_TARGET` / `CLI_BINARY_TARGET` (binary target override, `cli/release-core/launcher.js:383-387`) and `NEXT_PUBLIC_CODEBUFF_APP_URL` (release-download origin override, `:1014-1017`).

## 4. Startup sequence

Ordered path for a normal `freebuff` launch; every gate below happens before the TUI renders.

1. **npm wrapper** `main()`: print `startupBanner` (empty in the upstream build) → `ensureBinaryReady()` → `spawnInstalledBinary()` → `attachExitHandler()` → `setTimeout(checkForUpdates, 100)` (`cli/release-core/launcher.js:1701-1715`).
2. `cli/src/entry.ts:8-12` — if `isTerminalCommandBrokerInvocation(process.argv)`, run `serveTerminalCommandBroker()` (child mode); otherwise `await import('./index')`.
3. `cli/src/index.tsx:8` — side-effect import of `./pre-init/tree-sitter-wasm` publishes the sibling `tree-sitter.wasm` path and bytes on `globalThis` and `CODEBUFF_TREE_SITTER_WASM_PATH` before the SDK/code-map import chain triggers `Parser.init` (`cli/src/pre-init/tree-sitter-wasm.ts:33-88`).
4. `cli/src/index.tsx:67-71` — TanStack Query `focusManager.setEventListener(...)` + `setFocused(true)` (no browser visibility API in a terminal).
5. **Smoke gates**, each before `commander.parse()`: `--smoke-tree-sitter` (`:92-167`), `--smoke-api-url` (`:170-178`), `--smoke-terminal-broker` (`:180-208`).
6. `cli/src/index.tsx:210-222` — OSC 11 theme probe, only when `process.stdin.isTTY && process.platform !== 'win32'`, run before OpenTUI attaches to stdin.
7. `cli/src/index.tsx:224-234` → `parseArgs()` (`cli/src/cli-args.ts:41-145`) returns `{initialPrompt, command, agent, clearLogs, continue, continueId, cwd, initialMode, trustAgents}`.
8. `cli/src/index.tsx:236-238` — classify `login` / `publish` / agent override.
9. `cli/src/index.tsx:240` → `initializeApp({cwd})` (`cli/src/init/init-app.ts:9-34`): `process.chdir(cwd)` → `setProjectRoot` → `initAnalytics()` (errors swallowed) → `initializeDirenv()` → `initializeThemeStore()` → `enableManualThemeRefresh()` → `initTimestampFormatter()` → background `getFingerprintId()`. Direnv is skipped entirely on win32; its `.envrc` steering denylist is listed in §12 (`cli/src/init/init-direnv.ts:51-54,124-136`).
10. `cli/src/index.tsx:243` — `setApiClientAuthToken(getAuthToken())`.
11. `cli/src/index.tsx:246-249` — `login` ⇒ `runPlainLogin()`, then return; no renderer is created.
12. `cli/src/index.tsx:251-255` — project-picker flag when cwd is the home dir **or a descendant** (`cli/src/utils/project-picker.ts:3-12`).
13. `cli/src/index.tsx:257-267` — analytics `APP_LAUNCHED` with `version/platform/arch/hasInitialPrompt/hasAgentOverride/continueChat/initialMode/isFreeBuff`.
14. `cli/src/index.tsx:268-274` — the upstream build **and** win32 ⇒ early `drainClientLogs()` so an AV/watchdog kill still leaves a launch row.
15. `cli/src/index.tsx:281-301` — trust gate: `resolveTrustedAgentDirs` over `getDefaultAgentDirs()`, `~/.agents` always trusted, `interactive` = stdin **and** stdout TTY, `trustAll` = `--trust-agents` or `CODEBUFF_TRUST_AGENT_DIRS`; skipped dirs are `logger.warn`-ed (never happen upstream, which has no `--agent`), then `initializeAgentRegistry({agentDirs})`.
16. `cli/src/index.tsx:304` — `initializeSkillRegistry()` loads `.agents/skills`.
17. `cli/src/index.tsx:307-329` — `publish` branch; **unreachable upstream** because commander accepts only `login` as a positional.
18. `cli/src/index.tsx:331-338` — `clearLogFile()` when `clearLogs` (always false here), then deferred `setTimeout(trimOversizedChatLogs, 0)`.
19. `cli/src/index.tsx:340` — `createQueryClient()`.
20. `cli/src/index.tsx:352-363` — credential gate in `AppWithAsyncAuth`: no stored token ⇒ `requireAuth = true`; token present ⇒ `hasInvalidCredentials = true`, `requireAuth = false`. The names are historical — the token is never validated at startup.
21. `cli/src/index.tsx:432-435` — early `uncaughtException`/`unhandledRejection` handlers that restore the alternate screen.
22. `cli/src/index.tsx:442` — `startTerminalWatchdog()`: detached `/bin/sh` on POSIX blocking on a stdin pipe; on Windows a PowerShell bootstrap spawning the watchdog outside Bun's kill-on-job-close job object, armed before terminal modes are enabled (`cli/src/utils/terminal-watchdog.ts:1-50`).
23. `cli/src/index.tsx:444-448` — `createCliRenderer({backgroundColor: 'transparent', exitOnCtrlC: false, screenMode: 'alternate-screen'})`.
24. `cli/src/index.tsx:452-459` — `installProcessCleanupHandlers(renderer)`, then `installTerminalProtocolController` (focus reporting `\x1b[?1004h`/`l`), then the early handlers are removed.
25. `cli/src/index.tsx:465-467` — `if (IS_FREEBUFF) startEngagementTracking()` (engaged-time heartbeat, stopped by `exitCliCleanly`).
26. `cli/src/index.tsx:469-473` — `createRoot(renderer).render(<QueryClientProvider><AppWithAsyncAuth/></QueryClientProvider>)`.

**No in-binary update check.** Version/update logic lives only in the wrapper: `ensureBinaryReady` repairs a stale cached binary synchronously when the wrapper is newer (`cli/release-core/launcher.js:1158-1201`), and `checkForUpdates` stages, swaps, and respawns when a newer release exists (`:1241-1312`).

**No local single-instance lock.** There is no pidfile or lockfile anywhere in `cli/src`; uniqueness is server-side on `/api/v1/freebuff/session` — the startup GET probe that finds a held seat shows a local-only `takeover_prompt` instead of auto-POSTing (`cli/src/hooks/use-freebuff-session.ts:783-807`), `409 session_superseded` routes to the superseded screen (`cli/src/components/freebuff-superseded-screen.tsx:10-13`, `cli/src/app.tsx:366-369`), and `takeOverFreebuffSession()` re-POSTs once (`use-freebuff-session.ts:387-403`).

**Terminal capability handling** is only `isTTY` gates (OSC probe `cli/src/index.tsx:213`; agent-dir trust prompt `:285`), the watchdog, and the focus-report controller. There is no `TERM`/`COLORTERM` capability probe in the entry path — those are read into `CliEnv` for downstream features only (`cli/src/utils/env.ts:39-43`).

## 5. Login, credentials & logout

The CLI keeps one bearer token in `<configDir>/credentials.json` and signs in through a code-in-URL plus status-poll flow; no device-code grant and no token refresh exist anywhere in the tree.

### Credential file

| Item | Value | Cite |
|---|---|---|
| Config dir | `FREEBUFF_CONFIG_DIR` when set (a **relative** value throws); otherwise `$HOME/.config/manicode` + `-<NEXT_PUBLIC_CB_ENVIRONMENT>` when that env `!== 'prod'` (`manicode-dev`, `manicode-test`) | `cli/src/utils/config-dir.ts:16-35` |
| Credentials path | `<configDir>/credentials.json` | `cli/src/utils/auth.ts:38-40` |
| File / dir mode | `0o600` (`CREDENTIALS_FILE_MODE`) / `0o700` (`CONFIG_DIR_MODE`) | `cli/src/utils/auth.ts:43-45`, `:204`, `:208-210` |
| Mode healing | Re-clamped to `0o600` on every write **and every read** when the observed mode differs; failure is logged at debug, never thrown | `cli/src/utils/auth.ts:57-70`, `:111` |
| Windows | Healing is skipped entirely — `if (process.platform === 'win32') return` | `cli/src/utils/auth.ts:58` |
| JSON shape | `{ default: {...} }` with `.catchall(z.unknown())`, so unknown top-level keys survive a rewrite | `cli/src/utils/auth.ts:26-30` |
| `default` fields | `id?`, `name` (req), `email` (req), `authToken` (req), `fingerprintId?`, `fingerprintHash?`, `credits?` | `cli/src/utils/auth.ts:14-22` |
| Write | Read-modify-write, 2-space pretty JSON; logs then rethrows on failure | `cli/src/utils/auth.ts:198-221` |
| Legacy key drop | `chatgptOAuth` is destructured out by the next write — no startup sweep | `cli/src/utils/auth.ts:182-193` |
| Clear | Removes only `default`; unlinks the file when nothing else remains | `cli/src/utils/auth.ts:227-252` |

Same dir also holds `freebuff-instance-owner.json` (`cli/src/utils/freebuff-instance-owner.ts:12-14`), `settings.json` (`cli/src/utils/settings.ts:78`), `message-history.json` (`cli/src/utils/message-history.ts:58`), `recent-projects.json` (`cli/src/utils/recent-projects.ts:18`), `analytics-id.json` (`cli/src/utils/anonymous-id.ts:26-31`), `trusted-agent-dirs.json` (mode `0o600`, `cli/src/utils/agent-dir-trust.ts:33`, `:135-137`), `projects/<basename>/` transcripts (`cli/src/project-files.ts:57-61`) and `sponsored-terminal-reports/<sha256>.json` (`cli/src/utils/sponsored-run.ts:1668-1670`).

### Login sequence

1. **Fingerprint** — hardware-derived `enhanced-<sha256(base64url)>` over machineId + system + cpu + os + shell + MACs; fallback `codebuff-cli-<8 rand>`; memoized for the process (`cli/src/utils/fingerprint.ts:113-124`, `:169-178`).
2. **`POST {LOGIN_WEBSITE_URL}/api/auth/cli/code`**, body `{fingerprintId}`, `includeAuth: false`; answers `{loginUrl, fingerprintHash, expiresAt}` (`cli/src/utils/codebuff-api.ts:516-523`, `:44-48`).
3. **Login host** — `LOGIN_WEBSITE_URL = IS_FREEBUFF ? FREEBUFF_WEB_URL : WEBSITE_URL`; `FREEBUFF_WEB_URL` is `http://localhost:3002` under `IS_DEV`, else `NEXT_PUBLIC_FREEBUFF_APP_URL ?? 'https://freebuff.com'` (`cli/src/login/constants.ts:21-27`).
4. **Browser open** — `safeOpen(loginUrl)`; the URL is also rendered with a wrap warning and a `[Copy link (c)]` affordance (`cli/src/hooks/use-fetch-login-url.ts:44-48`).
5. **`GET {LOGIN_WEBSITE_URL}/api/auth/cli/status?fingerprintId&fingerprintHash&expiresAt`**, `includeAuth: false` (`cli/src/utils/codebuff-api.ts:525-536`).
6. **Poll cadence** — `intervalMs = 5000`, `timeoutMs = 5 * 60 * 1000`, abortable via `shouldContinue`; a 401 is expected and silent, any other non-OK warns and keeps polling; success requires `data.user` to be an object (`cli/src/login/login-flow.ts:117-135`, `:168-189`). Issued code lifetime `CLI_AUTH_CODE_LIFETIME_MS = 60 * 60 * 1000` (`common/src/constants/auth.ts:18`).
7. **Persist** — modal success runs `saveUserCredentials(user)`, then validates via `getUserInfoFromApiKey(['id','email'])` and merges into the in-memory user; a validation failure still admits the raw user (`cli/src/hooks/use-auth-query.ts:186-198`).
8. **`freebuff login` (non-TUI / SSH path)** — `runPlainLogin()` makes the same code call and poll with `via: 'plain_command'`, prints the URL as plain text, then `saveUserCredentials`, `identifyUser`, `LOGIN` event, `flushAnalytics`, `process.exit(0)` (`cli/src/index.tsx:236-249`, `cli/src/login/plain-login.ts:26-107`).
9. **Instrumentation** — `LOGIN_STARTED`; `LOGIN_FAILED` with `reason: 'url_request'|'url_empty'`; `LOGIN_ABORTED`; `LOGIN_TIMEOUT`; each tagged `via: 'modal'|'plain_command'` (`cli/src/login/login-flow.ts:46`, `:64-82`, `:139-157`).

### Token resolution

| Order | Source | Cite |
|---|---|---|
| 1 | `credentials.default.authToken` → `source: 'credentials'` | `cli/src/utils/auth.ts:139-142` |
| 2 | `CODEBUFF_API_KEY` env → `source: 'environment'` | `cli/src/utils/auth.ts:144-147` |
| 3 | none → `{ source: null }`, i.e. unauthenticated | `cli/src/utils/auth.ts:149` |

### Logout

- There is **no `freebuff logout` CLI subcommand**: `parseArgs` special-cases only `login` (and `publish`) before rendering the app (`cli/src/index.tsx:236-248`). In-app `/login` (alias `signin`) only answers "You're already in the app. Use /logout to switch accounts." (`cli/src/commands/command-registry.ts:390-402`).
- `/logout` (alias `/signout`) calls `stopActiveRun('logout')`, then `logoutUser()` (`cli/src/commands/command-registry.ts:403-424`).
- `logoutUser()` best-effort sends `POST /api/auth/cli/logout` with `{userId, fingerprintId, fingerprintHash}`, logs a non-OK response at error level, then clears credentials — and **always resolves `true`** (`cli/src/utils/auth.ts:254-286`).
- **No refresh-token mechanism exists** at this pin: `refreshToken` occurs only inside the dropped-`chatgptOAuth` fixture (`cli/src/__tests__/integration/credentials-storage.test.ts:238`) and the prose describing that dead key (`cli/src/utils/auth.ts:176`); a case-insensitive repo search for `refresh[-_ ]?token` finds nothing else in `cli/src`, `common/src` or `sdk/src`. Re-authentication means running the login flow again.

## 6. Session lifecycle & recovery

The CLI holds at most one free seat per account: POST acquires, GET refreshes, DELETE releases — all against the **codebuff** app host even in the upstream build (`NEXT_PUBLIC_CODEBUFF_APP_URL || 'https://codebuff.com'`, trailing slash stripped) (`cli/src/utils/freebuff-session-api.ts:98-107`).

| Method | Path | Extra headers | Body |
|---|---|---|---|
| POST | `/api/v1/freebuff/session/admission` | `x-freebuff-model` (only when a model is selected), `x-freebuff-wallet-spend-limit: String(limit ?? 0)` | none |
| GET | `/api/v1/freebuff/session` | `x-freebuff-instance-id` (when held), `x-freebuff-compact-session: 1` (when compact) | none |
| DELETE | `/api/v1/freebuff/session` | `x-freebuff-instance-id` (**required**) | none |

Every call also sends `Authorization: Bearer <token>`, `x-fb-timezone` (recomputed per request, omitted when `Intl` throws) and `x-freebuff-first-tab-discount: '1'|'0'` — always present, `'0'` when the server never offered the discount (`cli/src/utils/freebuff-session-api.ts:163-179`; names `common/src/constants/freebuff-models.ts:2609-2614`; `common/src/util/freebucks-timezone.ts:17-25`; `common/src/util/freebuff-first-tab-discount.ts:4`). Per-request timeout 20 s via `AbortSignal.any([caller, AbortSignal.timeout(20_000)])` (`cli/src/utils/freebuff-session-api.ts:90-96`).

### Response decoding

Cites in this table are line refs into `cli/src/utils/freebuff-session-api.ts`.

| Condition | Handling | Cite |
|---|---|---|
| POST 404/405 | throws `FreebuffSessionRequestError` with code `session_admission_unsupported` — a server too old to admit safely | `:187-194` |
| any 404 | `{ status: 'none' }` (no row) | `:195-197` |
| 403 + body `country_blocked` \| `banned` | body returned as the state; any other, unparseable 403 falls through to the generic throw | `:199-209`, `:241-256` |
| POST 409 + `model_locked` \| `model_unavailable` \| `first_tab_discount_changed` \| `consent_required` | body returned | `:211-225` |
| POST 429 + `rate_limited` \| `spend_limited` \| `ip_capped` | body returned | `:227-239` |
| other `!ok` | throws with `Retry-After` parsed (seconds or HTTP-date → ms) and the body `error` string as `errorCode` | `:241-256`, `:75-87` |

### Statuses and what each does to the UI

Union at `common/src/types/freebuff-session.ts:809-1153`; the line refs in this table are lines of that same file. Routing rule: every non-admitted status renders the pre-chat landing screen except `superseded` (own screen) and `ended` (falls through to `<Chat>`) (`cli/src/app.tsx:364-405`).

| Status | Wire facts | UI effect |
|---|---|---|
| `none` | no row (`:824`) | landing / model picker |
| `active` | `instanceId`, `model` **immutable mid-session**, `admittedAt`, `expiresAt`, `remainingMs`, optional `rateLimit`, `rateLimitsByModel`, `referral`, `subscription`, `freeWindows`, `freebucks` (`:865-894`) | `<Chat>`; poll continues at active cadence |
| `ended` | optional `instanceId` while inside the server grace window; `freebucksRefund`, `freebucksRefundPending` (`:898-930`) | keeps `<Chat>` mounted with the session-ended banner; polls only while `instanceId` is present |
| `takeover_prompt` | synthesized locally when the first probe finds a seat owned by a live pid | landing screen plus a take-over affordance |
| `superseded` | 409 `session_superseded` from the chat gate; polling stops (`:1150-1153`) | dedicated `FreebuffSupersededScreen` |
| `country_blocked` | terminal; `countryCode`, `countryBlockReason`, `ipPrivacySignals` (`:932-937`) | landing screen, terminal region message |
| `banned` | terminal (`:1027`) | landing screen, banned message |
| `model_locked` | POST 409; `currentModel`, `requestedModel` (`:944-948`) | recovery, see below |
| `model_unavailable` | POST 409; `requestedModel`, `availableHours`, `availableAt?`, `requiresSubscription?`, `withdrawn?`, `limitedOfferReason?` (`:951-1026`) | recovery, see below |
| `first_tab_discount_changed` | POST 409; `freebucks` (`:810-813`) | applies `{status:'none', freebucks, accessTier}` + failure banner; no auto-retry |
| `consent_required` | POST 409; `walletConsent` (`:815-822`) | same landing reset + banner; consent re-sent only for the same model+token inside a 120 s window |
| `rate_limited` | POST 429; optional `upgrade` hint (`:1050-1087`) | landing screen; terminal for this poll run |
| `spend_limited` | POST 429; `freebucks` carried client-side, never sent (`:1089-1099`) | landing screen; return after the daily reset |
| `ip_capped` | POST 429; `model`, distinct-user count (`:1036-1048`) | landing screen, "too many distinct users on this IP" |
| `premium_slot_taken` | `requestedModel`, `currentModel`, `currentInstanceId` (`:1122-1140`) | landing screen |
| `purchase_claim_released` / `purchase_in_use` / `purchase_capacity` | Desktop purchase refusals (`:1101-1120`) | landing screen |

### Poll loop and release

- Start is a bare GET probe (no auto-join); `nextMethod` flips to POST only for an explicit join/rejoin, and back to GET after any successful call (`cli/src/hooks/use-freebuff-session.ts:650-655`, `:783-807`).
- Active cadence `POLL_INTERVAL_ACTIVE_MS = 30_000` with ±20 % symmetric jitter, clamped to `max(1_000, min(jittered, remainingMs + 1_000))` so expiry shows promptly (`cli/src/hooks/use-freebuff-session.ts:63`, `:76-88`; `cli/src/utils/polling-backoff.ts:46-59`).
- `ended` keeps polling only while `instanceId` is present; every other status returns `null` and stops (`cli/src/hooks/use-freebuff-session.ts:89-110`).
- Compact GET while `previousStatus === 'active'` is merged with the held snapshot (`mergeCompactActiveSession`, so quota/subscription/freebucks survive the sparse response) (`cli/src/utils/freebuff-session-api.ts:261-291`).
- Failures: POST retries only 408/429/503 (a bodyless POST may already have rotated the instance), GET retries 408/429/5xx (`cli/src/utils/freebuff-session-api.ts:48-73`); backoff is 20 s doubling to a 300 s cap with equal jitter, raised to `Retry-After × (1..1.2)` (`cli/src/utils/polling-backoff.ts:3-43`).
- Release requires `holdsLiveFreebuffSlot` = `active`, or `ended` **with** an `instanceId` (`cli/src/utils/freebuff-session-api.ts:293-301`); `releaseSlot` needs both token and instance id, concurrent callers for the same `[token, instanceId]` share one in-flight DELETE via a `Map`, and the DELETE must answer `status === 'ended'` or it throws (`cli/src/state/freebuff-session-store.ts:90-120`). A `freebucksRefundPending` receipt makes the hook re-DELETE the same instance every 3 s until confirmation (`cli/src/hooks/use-freebuff-session.ts:480-497`; `cli/src/state/freebuff-session-store.ts:76-89`).

### Recovery flows

| Trigger | Behavior | Cite |
|---|---|---|
| `model_locked` after a deliberate pick | GET without instance id → if `active` on `currentModel`, DELETE via `releaseSlot` → re-POST on the requested model, plus the chat note "Ended your previous session on X and switched to Y"; a failed DELETE reverts the selection with "/end-session" advice; the marker is consume-once | `cli/src/hooks/use-freebuff-session.ts:659-733` |
| `model_locked` on a background rejoin | silent revert of the local selection to `currentModel` (the comment at `:670-675` documents the 2026-07-30 bug this fixes) | `cli/src/hooks/use-freebuff-session.ts:730-732` |
| `model_unavailable` | message for `withdrawn` / limited offer, select `FALLBACK_FREEBUFF_MODEL_ID` in memory only, re-POST | `cli/src/hooks/use-freebuff-session.ts:734-781` |
| `first_tab_discount_changed` / `consent_required` | `{status:'none', …}` + failure banner; wallet consent only for the same model+token inside a 120 s window | `cli/src/hooks/use-freebuff-session.ts:630-651`, `:139-146` |
| country block caught by the **chat gate** | `markFreebuffSessionCountryBlocked()` aborts the loop, applies terminal `country_blocked`, then best-effort DELETE | `cli/src/hooks/use-freebuff-session.ts:417-431`; `cli/src/hooks/helpers/send-message.ts:420-429`, `:544-552` |
| `session_superseded` (409) | `markFreebuffSessionSuperseded()` → `{status:'superseded'}` → superseded screen; polling stops | `cli/src/hooks/use-freebuff-session.ts:405-409`; `cli/src/hooks/helpers/send-message.ts:620-627` |
| `session_expired` (410) / `waiting_room_required` (428) / `session_model_mismatch` (409) | finalize the in-flight message, flip to local `ended` (Chat stays mounted with the rejoin banner) | `cli/src/hooks/helpers/send-message.ts:585-609`; gate table `common/src/types/freebuff-session.ts:1200-1224` |
| Seat lost locally | `markFreebuffSessionEnded()` when the gate missed and the message is re-queued at the front; a poll returning `none` while holding `active`/`ended` synthesizes `ended` without an instance id, carrying `rateLimitsByModel`/`subscription`/`freebucks` forward | `cli/src/hooks/use-send-message.ts:303-319`; `cli/src/hooks/use-freebuff-session.ts:816-848` |
| Startup finds an existing seat | first GET (`previousStatus === null`) + `active` → auto-POST if the recorded owner pid is dead, else apply `takeover_prompt` and stop | `cli/src/hooks/use-freebuff-session.ts:791-807` |
| Take-over accepted | `takeOverFreebuffSession()` pins the selected model to the server's `active` model and re-POSTs (queue position preserved); single-flight | `cli/src/hooks/use-freebuff-session.ts:389-403` |
| BYOK connection selected | every upstream session path short-circuits: no admission, no gate, `costMode: 'normal'` | `cli/src/app.tsx:358-360`, `:386-396` |

### Instance ownership (pid logic)

- `freebuff-instance-owner.json` holds `{instanceId, pid}`; written when a poll lands on `active` (`cli/src/utils/freebuff-instance-owner.ts:45-58`; call site `cli/src/hooks/use-freebuff-session.ts:549-551`).
- Liveness is `process.kill(pid, 0)`; `EPERM` counts as running, non-integer or `<= 0` pids are dead (`cli/src/utils/freebuff-instance-owner.ts:35-43`).
- `isFreebuffInstanceOwnedByDeadLocalProcess(instanceId)` returns true only when the recorded `instanceId` matches **and** that pid is gone (`cli/src/utils/freebuff-instance-owner.ts:60-66`) — the silent-takeover trigger; a live pid yields `takeover_prompt` instead, so opening a second CLI never supersedes the first silently.

### Exit path

- `/end-session` (alias `/model`) → `returnToFreebuffLanding({resetChat:true})` → `releaseSlot: true` DELETE, then the picker with `status:'none'` (`cli/src/commands/command-registry.ts:765-781`; `cli/src/hooks/use-freebuff-session.ts:295-302`); message `END_SESSION_MESSAGE` = "Ending session and returning to the model picker…" (`cli/src/utils/constants.ts:12-13`).
- Unmount / HMR fires a best-effort DELETE only when `holdsLiveFreebuffSlot` is true (`cli/src/hooks/use-freebuff-session.ts:1008-1022`).
- Cleanup ordering (`cli/src/utils/exit-cleanly.ts:55-121`): sponsored-run settlement **started first, awaited last**; then `cleanupLocal()`; then `stopEngagementTracking()`; then the remote tasks `flushAnalytics`, `drainClientLogs`, the sponsored notice and — upstream only — `endFreebuffSession` (= `releaseSlot()`) under `Promise.allSettled` with a 1000 ms `EXIT_CLEANUP_TIMEOUT_MS`; then `process.exit(code)`. A single-flight `exitPromise` makes competing triggers idempotent.
- Sponsored-run settlement: `settleInterruptedSponsoredRun()` → `run.interrupt('signal')` → abort the turn, report `failed` with a diagnostic reason (one attempt; no sweep exists for local rows), **keep the worktree**, and return a notice naming path, branch and `/ads:remove-worktree` (`cli/src/utils/sponsored-run-exit.ts:40-46`; `cli/src/utils/sponsored-run.ts:882-913`).
- Ctrl+C: stdin is raw in the TUI so SIGINT never fires — the key is routed from OpenTUI to `exitCliCleanly()`; the non-fullscreen handler needs a double Ctrl+C within 2 s (`cli/src/hooks/use-freebuff-ctrl-c-exit.ts:8-23`; `cli/src/hooks/use-exit-handler.ts:52-59`).
- Only `SIGTERM`/`SIGHUP`/`SIGINT` are routed to `exitCliCleanly()` (one shared handler); `beforeExit` calls the renderer `cleanup()` directly (`:210-212`), `exit` calls `cleanup()` and then `stopTerminalWatchdog()` when it succeeded (`:215-222`), and `uncaughtException`/`unhandledRejection` go to `exitCliWithFatalError()` (`:225-227`, `:229-232`). With `CODEBUFF_LAUNCHER_PID` set and differing from our pid, a 500 ms poll (`tasklist /FI PID` on win32, `kill(pid, 0)` elsewhere) calls the same exit handler when the launcher dies (`cli/src/utils/renderer-cleanup.ts:173-233`).
- A resume hint is printed from a synchronous `process.on('exit')` handler: `freebuff --continue <chatId>` (`cli/src/hooks/use-exit-handler.ts:19-34`).
- Chat requests carry the session identity as run metadata, not a header: `extraCodebuffMetadata = { freebuff_instance_id, freebuff_reasoning_effort? }`, set only when `IS_FREEBUFF && !byok && instanceId` (`cli/src/hooks/use-send-message.ts:664-676`).

## 7. Command surface & input grammar

The upstream CLI carries **two independently filtered command registries** — the `/` menu list and the executable lookup — so the same build can render a command it cannot run (none) or run one it never shows (`login`, `init`, the 8 `ads:*` controls).

**(a) Two registries and why they diverge.**

- **Menu list** `SLASH_COMMANDS` (`cli/src/data/slash-commands.ts:230-236`), extended with one dynamic row per loaded skill by `getSlashCommandsWithSkills()` (`cli/src/data/slash-commands.ts:258-269`); drives the `/` autocomplete.
- **Executable registry** `COMMAND_REGISTRY` (`cli/src/commands/command-registry.ts:784-786`), resolved by `findCommand(cmd)`: lowercase match on `name` **or** `aliases`, then a dynamic `skill:<name>` lookup (`cli/src/commands/command-registry.ts:788-809`).
- Divergence is deliberate: the registry keeps `login`, `init` and the 8 `ads:*` proposal commands that the menu never renders, while the menu's `dashboard` row carries the `usage` alias so `/usage` still lands somewhere after the `usage` command was removed (`cli/src/commands/command-registry.ts:522-531`).
- Upstream counts: **19** static menu entries + N skill rows; **29** static registry entries.
- Invariant pinned by test: every menu command lacking `insertText` must exist in the registry (`cli/src/commands/__tests__/router-input.test.ts:264-270`).

**(b) Command table (upstream build).** `M` = in `/` menu, `R` = resolvable by typing; `Y` present, `–` absent.

| # | Command | Aliases | M | R | Behavior | Source |
|---|---|---|---|---|---|---|
| 1 | `help` | `h`, `?`; slashless `help` | Y | Y | Sets input mode `help` → HelpBanner, auto-hides after 60 s. | `cli/src/commands/help.ts:9`; `cli/src/components/help-banner.tsx:9,42-47`; `cli/src/commands/command-registry.ts:318-327`; `cli/src/data/slash-commands.ts:53-59` |
| 2 | `diagnostics` | `diag`, `processes` | Y | Y | System message: local CLI CPU/mem + terminal-tool PIDs; "Command lines and environment variables are omitted for safety." | `cli/src/commands/command-registry.ts:328-337`; `cli/src/commands/process-diagnostics.ts:94-95`; `cli/src/data/slash-commands.ts:60-65` |
| 3 | `interview` | — | Y | Y | Args → `buildInterviewPrompt(args)`; bare → `interview` mode (label `Interview`). | `cli/src/commands/command-registry.ts:639-662`; `cli/src/utils/input-modes.ts:84-93`; `cli/src/data/slash-commands.ts:104-108` |
| 4 | `plan` | — | Y | Y | Args → `buildPlanPrompt(args)`; bare → `plan` mode. **Upstream-only.** | `cli/src/commands/command-registry.ts:663-687,199-209`; `cli/src/data/slash-commands.ts:109-113` |
| 5 | `review` | — | Y | Y | Args → `buildReviewPromptFromArgs(args)`; bare → `openReviewScreen` selection UI, then `review` mode. Present, not removed. | `cli/src/commands/command-registry.ts:688-712`; `cli/src/chat.tsx:1055-1057`; `cli/src/data/slash-commands.ts:114-118` |
| 6 | `queue` | `queued` | Y | Y | Opens the queue editor when `queuedCount > 0`, else system message `Nothing queued.` No `/q` alias: `/q` quits. | `cli/src/commands/command-registry.ts:713-723`; `cli/src/chat.tsx:1059-1064`; `cli/src/data/slash-commands.ts:119-124` |
| 7 | `new` | `n`, `clear`, `c`, `reset`; slashless `new` | Y | Y | Aborts the run (`stopActiveRun('new-chat')`), clears messages, `startNewChat()`; args become the new chat's first message. | `cli/src/commands/command-registry.ts:436-471`; `cli/src/data/slash-commands.ts:125-131` |
| 8 | `history` | `chats` | Y | Y | Opens the chat-history browser (`openChatHistory`). | `cli/src/commands/command-registry.ts:630-638`; `cli/src/data/slash-commands.ts:132-137` |
| 9 | `copy` | `copy-chat` | Y | Y | Renders the full transcript to markdown and copies it; over SSH uses OSC 52 and drops large tool bodies; `Copied conversation · N (… to fit clipboard)`. | `cli/src/commands/command-registry.ts:338-344`; `cli/src/commands/copy-conversation.ts:276-277`; `cli/src/data/slash-commands.ts:138-143` |
| 10 | `export` | `export-chat` | Y | Y | Writes the transcript to a file; plain arg = filename (default `freebuff-chat-<chatId>.md`), `.json` = raw messages; refuses paths outside the project root and refuses overwrite. | `cli/src/commands/command-registry.ts:345-351`; `cli/src/commands/export-conversation.ts:57-112`; `cli/src/data/slash-commands.ts:144-149` |
| 11 | `feedback` | registry `bug`, `report`; menu entry has none | Y | Y | Opens the feedback form; args prefill the text box and cursor. | `cli/src/commands/command-registry.ts:352-368`; `cli/src/data/slash-commands.ts:162-166` |
| 12 | `bash` | `!` | Y | Y | Args → runs `runBashCommand(args)`; bare → `bash` input mode. | `cli/src/commands/command-registry.ts:369-389`; `cli/src/data/slash-commands.ts:167-172` |
| 13 | `theme:toggle` | — | Y | Y | Swaps light/dark; system message `Switched to <theme> theme.` | `cli/src/commands/command-registry.ts:724-737`; `cli/src/data/slash-commands.ts:185-189` |
| 14 | `byok` | `provider` | Y | Y | **Upstream-only.** Subcommands `list`/`add`/`update`/`validate`/`select`/`remove`/`off`/`help`; keys referenced by env-var NAME only, never stored; credential-shaped history redacted. | `cli/src/commands/command-registry.ts:742-746,199-209`; `cli/src/commands/byok.ts:15-41,161-320`; `cli/src/data/slash-commands.ts:190-195` |
| 15 | `reasoning` | `effort`, `think` | Y | Y | **Upstream-only.** Reads/sets thinking level for the selected model; clear words `default|auto|reset|clear|none`; takes effect on the next message. | `cli/src/commands/command-registry.ts:747-760`; `cli/src/commands/reasoning.ts:18,36-82`; `cli/src/data/slash-commands.ts:196-201` |
| 16 | `end-session` | `model` | Y | Y | **Upstream-only.** Posts `END_SESSION_MESSAGE` (`Ending session and returning to the model picker…`) then `returnToFreebuffLanding({ resetChat: true })`. `/model` alias pinned by test. | `cli/src/commands/command-registry.ts:765-780`; `cli/src/utils/constants.ts:12-13`; `cli/src/commands/__tests__/freebuff-command-aliases.test.ts:4-32`; `cli/src/data/slash-commands.ts:202-207` |
| 17 | `dashboard` | `usage`, `stats`, `streak` | Y | Y | **Upstream-only.** Posts the `/account` URL plus a streak/activity/tokens/system line and best-effort `safeOpen(url)`; the `usage` alias is why `/usage` still resolves. | `cli/src/commands/command-registry.ts:522-548`; `cli/src/data/slash-commands.ts:208-213` |
| 18 | `logout` | `signout`; slashless `logout` | Y | Y | Stops the active run, runs the logout mutation, posts `Logged out.`, then unmounts the authenticated runtime. | `cli/src/commands/command-registry.ts:403-428`; `cli/src/data/slash-commands.ts:214-220` |
| 19 | `exit` | `quit`, `q`; slashless `exit` | Y | Y | `exitCliCleanly()`. | `cli/src/commands/command-registry.ts:429-435`; `cli/src/data/slash-commands.ts:221-227` |
| 20 | `login` | `signin` | – | Y | Hidden: posts `You're already in the app. Use /logout to switch accounts.` | `cli/src/commands/command-registry.ts:390-402` |
| 21 | `init` | — | – | Y | Hidden from the menu; runs the local knowledge-file init flow and sends the first message (queues it if a turn is live). Not in the registry removal set. | `cli/src/commands/command-registry.ts:472-503,189-197`; `cli/src/data/slash-commands.ts:76-81` |
| 22 | `ads:proposal` | — | – | Y | Opens the sponsored-proposal menu on the live card; the only route in. | `cli/src/commands/command-registry.ts:240-250` |
| 23 | `ads:dismiss-proposal` | — | – | Y | Declines the proposal; the only route to the decline. | `cli/src/commands/command-registry.ts:251-259` |
| 24 | `ads:accept-proposal` | — | – | Y | Opens the consent screen; starts nothing. | `cli/src/commands/command-registry.ts:263-272` |
| 25 | `ads:pull-request` | — | – | Y | Opens a PR from the proposal worktree. | `cli/src/commands/command-registry.ts:273-281` |
| 26 | `ads:remove-worktree` | — | – | Y | Removes the proposal worktree. | `cli/src/commands/command-registry.ts:282-290` |
| 27 | `ads:report-proposal` | — | – | Y | Reports the proposal. | `cli/src/commands/command-registry.ts:291-299` |
| 28 | `ads:never-advertiser` | — | – | Y | Opts out of the advertiser permanently. | `cli/src/commands/command-registry.ts:300-308` |
| 29 | `ads:proposals-off` | — | – | Y | Turns the sponsored-proposal channel off. | `cli/src/commands/command-registry.ts:309-317` |
| 30 | `skill:<name>` | — | Y (appended) | Y (dynamic) | One row per loaded skill; menu row sets `insertText: '/skill:<name> '` and truncates the description to 49 chars + `…`. With args sends the skill prompt; bare enters `skill` mode (empty Enter still runs it). | `cli/src/data/slash-commands.ts:258-269`; `cli/src/commands/command-registry.ts:800-806,815-849` |

**(c) Removed commands.**

- Menu removal set `FREEBUFF_REMOVED_COMMAND_IDS` (`cli/src/data/slash-commands.ts:33-42`): `ads:enable`, `ads:disable`, `usage`, `subscribe`, `agent:gpt-5`, `image`, `publish`, `init`.
- Registry removal set `FREEBUFF_REMOVED_COMMANDS` (`cli/src/commands/command-registry.ts:189-197`): `ads:enable`, `ads:disable`, `usage`, `subscribe`, `image`, `publish`, `gpt-5-agent`.
- Effect per command: `ads:enable`/`ads:disable` gone from both surfaces (ads are always on upstream) — `cli/src/data/slash-commands.ts:34-35`, `cli/src/commands/command-registry.ts:190-191`; `usage` (+`credits`) gone, `/usage` resolves to `dashboard` via alias while `/credits` has no owner — `cli/src/commands/command-registry.ts:192,506,531`; `subscribe` (+`strong`,`sub`,`buy-credits`) gone from both — `cli/src/commands/command-registry.ts:193,515-516`; `agent:gpt-5`/`gpt-5-agent` gone with its `insertText: '@GPT-5 Agent '` shortcut — `cli/src/data/slash-commands.ts:38,151-155`, `cli/src/commands/command-registry.ts:196,617-628`; `image` (+`img`,`attach`) gone, leaving the `image` input mode unreachable (only `Ctrl+V` paste attaches) — `cli/src/commands/command-registry.ts:194,549-568`; `publish` gone (its menu entry was already commented out) — `cli/src/data/slash-commands.ts:40,180-184`.
- `init` is the asymmetry: filtered from the menu only, still executable (`cli/src/data/slash-commands.ts:41`); absent from `cli/src/commands/command-registry.ts:189-197`.
- `mode:*` is excluded by construction, not by the removal set: `MODE_COMMANDS = IS_FREEBUFF ? [] : …` and `...(IS_FREEBUFF ? [] : AGENT_MODES)` — `cli/src/data/slash-commands.ts:24-31,179`, `cli/src/commands/command-registry.ts:569-598`.
- `/connect:claude` (+`/claude`) and `/refer-friends` (+`/referral`,`/redeem`) are **not defined anywhere** in this revision, so "removed" does not apply.

**(d) Upstream-only commands.** `FREEBUFF_ONLY_COMMAND_IDS` (`cli/src/data/slash-commands.ts:44-50`) and `FREEBUFF_ONLY_COMMANDS` (`cli/src/commands/command-registry.ts:199-209`) list the same five: `byok`, `plan`, `end-session`, `dashboard`, `reasoning`. The registry comment records why: the reasoning ladder and the metadata it sets are upstream-catalog/free-mode only (`cli/src/commands/command-registry.ts:204-208`), and the dashboard hub is an upstream web surface (`cli/src/commands/command-registry.ts:524-530`).

**(e) Input grammar.**

- **Slash parsing.** `/name args`; the first whitespace-delimited token is lowercased, args are the remainder (`cli/src/commands/router-utils.ts:26-34,59-68`). Matching is case-insensitive against name or alias (`cli/src/commands/command-registry.ts:789-794`).
- **`/` menu activation.** Only when the current line matches `/^(\s*)\/([^\s]*)$/` **and** starts at index 0 — first composer line, no spaces in the query (`cli/src/hooks/use-suggestion-engine.ts:52-65`). Filtering is ordered and dedup'd by id: prefix-of-id/alias → substring-of-id/alias → substring-of-description; slash commands are **not** fuzzy (fuzzy matching applies to files/agents only) (`cli/src/hooks/use-suggestion-engine.ts:174-245,291-348`).
- **Menu keys.** `↑/↓` select; `Tab`/`Shift+Tab` complete into the composer without executing; `Enter` executes the highlighted command unless it declares `insertText`, in which case the text is inserted (`cli/src/utils/keyboard-actions.ts:253-279`).
- **Slashless.** Only implicit ids surviving the upstream filter are reachable without `/`: `help`, `new`, `logout`, `exit`, and only as a single bare word (`cli/src/data/slash-commands.ts:238-242`; `cli/src/commands/router-utils.ts:70-79`). Consequence: bare `init` is now a normal message while `/init` still runs the command.
- **`!bash`.** Typing exactly `!` in `default` mode enters `bash` mode and clears the composer (`cli/src/components/chat-input-bar.tsx:211-222`); that mode shows the `!` label, placeholder `enter bash command...`, and disables slash suggestions (`cli/src/utils/input-modes.ts:64-73`). Submitting prefixes `!` into history and runs it (`cli/src/commands/router.ts:307-316`); inline `!cmd` in default mode runs immediately (`cli/src/commands/router.ts:399-406`). `/bash cmd` and `/!cmd` run immediately, bare `/bash` or `/!` enter bash mode (`cli/src/commands/command-registry.ts:369-389`). Bash output is UI-only, never sent to AI context (`cli/src/commands/router.ts:220-251`).
- **`@files` / `@agents`.** Trigger requires `@` at line start or after whitespace, not escaped (`\@`), not inside `"…"`/`` `…` ``, and not after `[a-zA-Z0-9.:]` (kills emails/URLs); the query must contain no whitespace (`cli/src/hooks/use-suggestion-engine.ts:106-143`). One combined list: **agents first, then files** (`cli/src/chat.tsx:1394-1434`). Agents match fuzzily; files match fuzzily with highlight indices and refresh from disk while a mention is active (`cli/src/hooks/use-suggestion-engine.ts:350-476,623-654`). `Tab`/`Enter` completes to `@agentId ` or `@filePath `; `Tab` with multiple matches cycles, with one match completes (`cli/src/utils/keyboard-actions.ts:294-307`).
- **No `#` or other prefix.** The engine parses only `/` and `@` (`cli/src/hooks/use-suggestion-engine.ts:45,146`).
- **Input modes.** Union: `default`, `bash`, `homeDir`, `plan`, `review`, `interview`, `skill`, `usage`, `image`, `help`, `outOfCredits`, `subscriptionLimit` (`cli/src/utils/input-modes.ts:8-20`). User-command-reachable upstream: `default`, `bash`, `plan`, `review`, `interview`, `skill`, `help` (plus automatic `homeDir`); `image` and `usage` are unreachable (their commands are removed/filtered) (`cli/src/commands/command-registry.ts:549-568`; `cli/src/data/slash-commands.ts:36`). The agent-mode toggle is force-disabled for every mode (`cli/src/utils/input-modes.ts:178-183`).
- **Queue editing.** `/queue` or **Ctrl+Q** (only when `queuedCount > 0`) opens the panel (`cli/src/utils/keyboard-actions.ts:220-224`). Inside: `q`/`Esc`/`Ctrl+C` close; `j`/`k`/`↑`/`↓` select; `Shift|Ctrl+↑↓` or `J`/`K` reorder; `t` move to top; `e`/`Enter` edit; `d`/`Delete`/`Backspace` delete (`cli/src/utils/queue-panel-actions.ts:28-70`). While editing, Enter saves, Esc cancels, emptying the prompt deletes it; footer strings verbatim: `Enter save · Esc cancel · emptying it deletes` and `click a row to edit · ⇧↑↓ reorder · d delete · esc close` (`cli/src/components/queue-panel.tsx:303-307`). A mid-turn submit **steers** the live run (plain text, no attachments, not a slash command, no queued `!` output, empty queue) or **queues** (`cli/src/commands/router.ts:465-506`).
- **Other keyboard.** `Esc` exits a non-default input mode before anything else unless the mode sets `blockKeyboardExit` (`subscriptionLimit` only) (`cli/src/utils/keyboard-actions.ts:211-215`); `Ctrl+C` clears non-empty input → interrupts a live run → clears a paused queue → warns then exits (`cli/src/utils/keyboard-actions.ts:227-237,323-325,380-385`); `Ctrl+T` collapses/expands all agents (`cli/src/utils/keyboard-actions.ts:345-350`); multi-line inserts on `Shift+Enter`, `Option/Alt+Enter`, `Ctrl+J`, and trailing `\`+Enter (`cli/src/components/multiline-input.tsx:603-621`).

**(f) Help banner — exact upstream content.** Source: `cli/src/components/help-banner.tsx:37-127`; the `Credits` block is suppressed under `IS_FREEBUFF` (`cli/src/components/help-banner.tsx:102-125`). Rendered rows, verbatim:

```
Shortcuts
  Ctrl+C / Esc   stop
  Ctrl+J / Opt+Enter   newline
  ↑↓   history
  Ctrl+T   collapse/expand agents
  Ctrl+Q   edit queued messages

Features
  /   commands
  @files   mention
  @agents   use agent
  !bash   run command
  /copy   copy chat
  /export   save chat to file

Tips
  Try workflow: /interview → /plan → implement → /review        (FreeBuff only)
  Use @ to reference agents to spawn or files to read
  Drag to select text — it copies automatically (or click ⎘ on a message)
  Esc to cancel the current response
```

Banner mechanics: `HELP_TIMEOUT = 60 * 1000` auto-hide back to `default` (`cli/src/components/help-banner.tsx:9,42-47`); the banner's X also exits (`:52`); the workflow tip is gated on `IS_FREEBUFF` (`:84-88`).

**(g) Gotchas.**

- `/model` does **not** switch models — upstream it resolves to `end-session` (alias pinned by test), i.e. it ends the session and returns to the picker (`cli/src/commands/command-registry.ts:765-767`; `cli/src/commands/__tests__/freebuff-command-aliases.test.ts:22-32`).
- `/q` is exit, never queue: `queue` deliberately has no `q` alias (`cli/src/commands/command-registry.ts:713-717`; `cli/src/data/slash-commands.ts:225`).
- Menu/registry asymmetry: `login` and `init` execute but never appear in `/`; the 8 `ads:*` controls are type-only. Conversely `dashboard`'s `usage` alias means `/usage` "works" while the `usage` command does not exist (`cli/src/data/slash-commands.ts:230-236`; `cli/src/commands/command-registry.ts:784-794`).
- `init` lost its slashless form: filtering it out of `SLASH_COMMANDS` also removes it from `SLASHLESS_COMMAND_IDS` (`cli/src/data/slash-commands.ts:238-242`), so bare `init` becomes a normal agent message.
- `/help` shows the banner, not the CLI help: `--help` (commander) is a separate surface (`cli/src/commands/help.ts:5-12`; `cli/src/cli-args.ts:52-73`).
- The spec's "`/review` is removed" claim is wrong for this revision — `review` is in both surfaces (`cli/src/data/slash-commands.ts:114-118`; `cli/src/commands/command-registry.ts:688-712`).
- `freebuff/e2e/tests/slash-commands.e2e.test.ts:40` is `describe.skip`, so it asserts nothing at runtime; its removed-command list is stale and its substring assertions can be defeated by a kept row's description.

## 8. TUI surfaces, screens & shortcuts

The TUI is a second rendering surface on top of the session wire: model names, taglines and warnings are hardcoded in `common/src/constants/freebuff-models.ts`, while prices, balances, pool quotas, offers and notices all arrive in the session payload (`cli/src/components/freebuff-model-selector.tsx:297-390`).

### 8.1 Landing / hero screen ("waiting room")

`cli/src/components/freebuff-landing-screen.tsx`; routed for statuses `null | none | country_blocked | banned | rate_limited | spend_limited | ip_capped | takeover_prompt` (`cli/src/app.tsx:385-405`).

- Heading `Start coding for free` (`:81`), rendered bold, plus a top-right `✕` mouse exit affordance for Ctrl+C (`:661-688`).
- 6-line ASCII logo (`cli/src/hooks/use-logo.tsx`, `LOGO` in `cli/src/login/constants.ts:64`). Drawn when `terminalHeight >= 40`, or collapsed with no referral card and height `>= COLLAPSED_LOGO_MIN_HEIGHT` = 26 (`:82`).
- Progressive disclosure by height: below 22 rows the layout is `compact` and notices are dropped (`:418`); below 18 rows the ad banner is dropped (`:419`).
- Height budget (`:582-636`): `selectorMaxHeight` = terminal rows − `reservedChrome`, where chrome = top bar 2 rows, ad row `AD_CARD_HEIGHT` (5) only when shown, main-box bottom padding 1, and the logo block (lines + `marginBottom` 1). Rows are computed from real strings against `contentMaxWidth`, not reserved blindly.
- Pre-session status lines: `⚠ {getLandingFailureMessage}` (`:717-721`), refund-pending line (`:723-728`), `N Freebuck(s) returned to your wallet.` (`:729-734`), `Connecting…` shimmer (`:736-740`).
- Unreachable-host line: the `⚠ {getLandingFailureMessage}` line renders the network-failure copy built by `freebuffSessionUnreachableMessage()` — exact text in §10 row 29 (`cli/src/utils/freebuff-session-api.ts:143-148`).
- Session counter rides `belowToggle` inside the selector, only when used > 0 and the account is not metered: `{used} of {limit} {sessions|premium sessions} used, resets in {countdown}` (`:769-788`; label at `:569-570`).
- Below-picker notices, muted and never amber (`:790-799`): limited tier → `getFreebuffModelAvailabilityNotice` (`common/src/util/freebuff-model-availability.ts:81`); full access → `FREEBUFF_TIER_CHANGE_NOTICE` (`:26-27`). Metered accounts see neither.
- Streak bonus note (`🎁 …`) under the picker only when the bonus is earned (`:800-806`).
- One-time Freebucks intro card above the picker when metered and height `>= 30` (`:495-497`, `:750-755`): `FREEBUCKS_INTRO` = title `Meet Freebucks`, a lead, 3 points, dismiss `Shown once. Press any key to continue.` (`cli/src/utils/freebucks.ts:175-184`). The card owns the keyboard; the picker is suspended while visible (`keyboardSuspended`, `:766-768`).

### 8.2 Streak line

- `getFreebuffStreakLine` returns null for `streak <= 0`, so the row is hidden entirely (`common/src/util/freebuff-streak-line.ts:41-45`).
- Label `{N} day streak`; dots fill to 7 then gain a trailing `+` (`●●●●●●●+`) (`:52-63`); terminal glyphs `●`/`○` (`cli/src/utils/freebuff-streak-line.ts:42`).
- Gaps: `FREEBUFF_STREAK_LABEL_GAP = 2`, `FREEBUFF_STREAK_INLINE_GAP = 3` (`:21,29`).
- Rides the heading row only if `fitsFreebuffStreakOnHeadingRow` (day-one width measured, `:56-76`), else renders on its own line (`freebuff-landing-screen.tsx:756-762`).
- Bonus note copy `🎁 {N} more days to unlock {perk}` / `🎁 Streak perk: {perk}` (`common:124-128`), gated on `streak >= 7` and `terminalHeight >= 30`; feature flag `FREEBUFF_ENABLE_STREAK_IN_UI` (`common/src/constants/freebuff-models.ts:910`).

### 8.3 Model selector

`cli/src/components/freebuff-model-selector.tsx`. Opens **collapsed** to a single hero card — no `RECOMMENDED` badge, ordering is the only steer (`:95-99`, `:707-712`); `canCollapse` requires ≥ 2 other models (`:694-697`).

- Toggle label: `↓  See all {N} models` / `↑  Show fewer` (`:1520-1522`).
- Sections (`:763-799`): expanded full access → `PREMIUM` (header carries the shared pool inline) + `UNLIMITED`; metered → one flat list; limited tier → unlabeled list; offer rows lead in both states under `LIMITED TRIAL` (`:813-826`). Empty sections are filtered out.
- **Row line 1**: `›` focus indicator, name padded to the widest `displayName`, tagline, then suffix chips appended in order — ` · Reasoning: {effort}` with `*` when user-chosen (`:895-908`), ` · Images` when `model.multimodal` (`:1345`), ` · NEW`, ` · TEST` (`:1393-1405`). Narrow terminals fall back to `name · tagline` (`:1384-1392`).
- **Row line 2**: centred, joined by `DETAIL_SEPARATOR = ' · '`, built by `rowDetails` (`:431-499`). Price leads: `{N} Freebucks/hr` (warn-coloured when balance < price; accent + bold when a first-tab list price exists — the only first-tab signal left on the row, `:453-457`); then `model.warning` (AI-training notice / `Anonymous provider retains prompts`, `:459`); `deploymentAvailabilityLabel` (`until {time}` / `opens {time}`) or the closed label `Back at {time} {zone}` (`:460-467`, `common/src/constants/freebuff-models.ts:4029-4051`); and, off the meter only, a per-row own-pool quota `{poolLabel}: {used} of {limit} used|starts` (`:479-489`, `common/src/util/freebuff-session-pools.ts:91-97`). Three chips this line drew before `8ed5d3e5e` are gone — the off-peak detail copy, `Limited-time first-tab discount` and the peak-pricing tooltip (§14.6). `taglineFor` (`:351-358`) now forces the catalog tagline for `FREEBUFF_DEEPSEEK_V4_FLASH_MODEL_ID` as well, so that row's server `priceNotices` tagline is suppressed.
- Optional third lines: the meter's ask line (paywall/confirm wording, `:625-667`), upgrade CTA `{cta} →` (`:683-689`), superseded notice (none set in the current catalog).
- Below the list (`:1599-1653`): `Freebucks balance temporarily unavailable.`; meter header `{remaining}/{limit} Freebucks daily · resets in {countdown} · {wallet} in wallet` (`cli/src/utils/freebucks.ts:122-145`, wallet segment omitted at balance 0); off-meter `FREE · {windows}` or `{TIER} PLAN · {windows}`; the blocked-limit line.
- Metered list order: cheapest first (`:306-315`). Unknown advertised model ids are dropped (`:326-332`).
- Keys: Tab / Shift+Tab / arrows only move focus; Enter or Space commits (`:1196-1258`). First Enter on a paywall/confirm row asks, a repeat commits; an unaffordable row still presses and its Enter opens `https://freebuff.com/plans` (`:1150-1167`).

### 8.4 Terminal-state banners and screens

| State | Title (verbatim) | Body / actions | Source |
| --- | --- | --- | --- |
| `country_blocked` | `⚠ Free mode isn't available in your region` | region prose (VPN/Tor variant, unknown-location variant, or detected-CC variant) + `Press Ctrl+C to exit` — exact copy: §10 rows 1-3 | `freebuff-landing-screen.tsx:817-857` |
| `banned` | `⚠ Account unavailable` | suspension prose + `Press Ctrl+C to exit` — exact copy: §10 row 4 | `:862-872` |
| `rate_limited` | `⚠ Not enough Freebucks` / `⚠ Monthly usage limit reached` / `⚠ Session limit reached` | shortfall / monthly-$ / session-count bodies, then a paywall half (`Get more sessions with a plan: https://freebuff.com/plans` or the server's `upgrade.message` + `url`) — exact copy: §10 row 6 | `:877-963` |
| `spend_limited` | `☕ Daily usage cap reached` (metered) / `☕ Daily Freebuff limit reached` | `{message}` + "resets … at midnight Pacific" + `Press Ctrl+C to exit` — exact copy: §10 row 7 | `:970-990` |
| `ip_capped` | `🚦 Too many Freebuff sessions on this network` | retry guidance until a slot frees + `Press Ctrl+C to exit` — exact copy: §10 row 5 | `:996-1012` |
| `takeover_prompt` | `Freebuff is already running` | `Only one freebuff instance is allowed at a time.`; buttons `Take over` · `Retry now` · `Try takeover again` · `Taking over...` · `Exit`; countdown `Retrying automatically in {n}s (attempt {a}).` | `:147-311` |
| `superseded` | `Another freebuff instance took over this account.` | `Only one CLI per account can be active at a time.` / `Close the other instance, then restart freebuff here.` / `Press Ctrl+C to exit.` | `cli/src/components/freebuff-superseded-screen.tsx:47-52`, routed `app.tsx:364-370` |
| session ended (chat banner) | `Session ended  ·  {balance} Freebucks left` (metered) / `Session ended  ·  {used} of {limit} {sessions\|premium sessions} used today` | `Press Enter to continue in a new session` (or `… with {fallback model}`) + `Change model`; while streaming: `Agent is wrapping up. Rejoin the wait room after it's finished.` | `cli/src/components/session-ended-banner.tsx:69-73,170,195`, shown `cli/src/chat.tsx:1971-1974` |
| out-of-hours | — (row-level only) | `opens {local time}` / `until {local time}` / `Back at {local time} {zone}`; no dedicated off-hours page | see §8.3 |

- Takeover keys: Enter confirms focus, Esc exits, ←/→/Tab switch buttons (`:181-222`); session-ended: Enter = new session, Esc = back to the picker (`session-ended-banner.tsx:130-146`).
- Statuses with **no** CLI screen: `model_locked`, `model_unavailable`, `purchase_*`, `premium_slot_taken`, `consent_required`, `first_tab_discount_changed`. No session "queued" screen exists (`committedModelId` is always null, `freebuff-model-selector.tsx:347-349`); the only queue UI is the message queue.
- Agent-mode toggle is disabled upstream (`cli/src/utils/input-modes.ts:178-183`).

### 8.5 Ads

- Landing fills a row of up to `visibleWaitingRoomPlacementIds(width)` cards, ≥ 60 cols each (`cli/src/components/ad-banner.tsx:771-824`; `common/src/ads/waiting-room-placements.ts:10-20`). Chat renders `SingleAdBanner` above the composer (`cli/src/chat.tsx:1927-1948`).
- Fixed heights: `AD_CARD_HEIGHT = 5`, `INLINE_AD_CARD_HEIGHT = 4` (`ad-banner.tsx:41-42`); the rotating dock adds a `DockDetailPanel` anchored right of the dock with `DOCK_PANEL_MAX_WIDTH = 58`, drawn only if it fits above the reserved composer (`:650-672`).
- Required disclosures: card description row `Ad` (`:257,273`), inline `INLINE_AD_DISCLOSURE = 'Ad'`; dock label row `Sponsored · {Brand}` (`DOCK_SPONSORED_LABEL`, `DOCK_LABEL_SEPARATOR` `common/src/ads/inline-ad-layout.ts:32,266-267`); detail panel prints `Sponsored` (`ad-banner.tsx:553`); CTA suffix ` ↗`; panel close `[ Close ]`.
- Always-on in free mode, with no first-message gate: `useGravityAd({ enabled: true, forceStart: true, provider: 'gravity' })`; the server tries Gravity then ZeroClick/Carbon (`freebuff-landing-screen.tsx:443-456`, surface `waiting_room`). Placeholder `────` while unfetched.
- Impression fires on mount (deduped); a click calls `recordClick` then `safeOpen(ad.clickUrl)`. Dock chord hint `⌃O details` (`cli/src/hooks/use-dock-panel.ts:32`).

### 8.6 Keyboard shortcuts

Resolution order lives in `cli/src/utils/keyboard-actions.ts:138-388`; composer editing in `cli/src/components/multiline-input.tsx:552-1014`; help card text in `cli/src/components/help-banner.tsx:56-100`.

| Keys | Action | Source |
| --- | --- | --- |
| Ctrl+C | clear input → interrupt stream → clear paused queue → warn → exit | `keyboard-actions.ts:227-385` |
| Esc | close dock panel → exit input mode → interrupt stream → unfocus agent | `:201-215,362-364` |
| Enter | submit / commit focused picker row / confirm takeover | `multiline-input.tsx:589-643`; selector `:1239-1261` |
| Space | commit focused picker row | selector `:1239` |
| Shift+Enter / Opt+Enter / Ctrl+J / `\`+Enter | insert newline in composer | `multiline-input.tsx:552-637` |
| ↑ ↓ | history (bash, slash, mention, regular) | `keyboard-actions.ts:251-343` |
| Tab / Shift+Tab | file menu, menu cycle, toggle agent mode | `:310-320,352-359` |
| Ctrl+T | collapse/expand all agents | `:345-350` |
| Ctrl+Q | open queue editor (only when messages are queued) | `:220-224` |
| Ctrl+V | paste (image/text) | `:374-377` |
| Ctrl+O | toggle sponsor dock detail panel (only when the dock is expandable) | `:198-207` |
| PageUp / PageDown | scroll the transcript | `:366-372` |
| Ctrl+A/E/B/F/H/D/K/U/W | emacs-style line/word edits | `multiline-input.tsx:660-961` |
| ← → Tab / Enter / Esc (takeover) | move button focus / confirm / exit | `freebuff-landing-screen.tsx:181-222` |
| Enter / Esc (session ended) | start a new session / back to the picker | `session-ended-banner.tsx:130-146` |

- Queue panel (`cli/src/components/queue-panel.tsx`): title `▾ Queue — {N} message(s)`, footer `click a row to edit · ⇧↑↓ reorder · d delete · esc close` (`:213,306`).
- Ctrl+C warning text: `Press Ctrl-C again to exit` in the status bar (`cli/src/components/status-bar.tsx:153`).

## 9. Models, tiers & reasoning

The CLI ships no model list and no price table of its own: `common/src/constants/freebuff-models.ts` owns which ids exist and which catalog a picker may render, and every price reaches the client only inside the session response.

### 9.1 Three catalogs, one admission union

| set | role / contents | cite |
|---|---|---|
| `FREEBUFF_MODELS` | CLI/Desktop picker — six rows in pick order: GLM 5.3 Flash, DeepSeek V4 Flash, Luna, MiMo 2.5, Solar Pro 4, Muse Spark 1.2 | `common/src/constants/freebuff-models.ts:2054-2140` |
| `FREEBUFF_WEB_MODELS` | Web picker: Web-only rows + `...FREEBUFF_MODELS`. Gemini 3.8 Flash is listed here and in no other catalog, because Pro is enforced on Web alone | `:2445-2475` |
| `FREEBUFF_WEB_ALL_MODELS` | `FREEBUFF_WEB_GOD_ONLY_MODELS` (Kimi K3 Eco, GPT-5.6 Luna-ES) + `FREEBUFF_WEB_MODELS` | `:2477-2485` |
| `SUPPORTED_FREEBUFF_MODELS` | 13 recognised rows — the picker rows **plus** paused/withdrawn ids, kept so released binaries hold ids the server can coerce rather than refuse | `:2011-2025` |
| admission | `isFreebuffSessionModelId` = `SUPPORTED` ∪ Web ids (god-only included); no picker reads this union directly | `:3306-3318` |

- Nesting on the client is `FREEBUFF_MODELS` ⊂ `FREEBUFF_WEB_MODELS` ⊂ `FREEBUFF_WEB_ALL_MODELS`; `SUPPORTED_FREEBUFF_MODELS` is a sibling superset used only for recognition and coercion.
- MiMo's row is compiled in unconditionally: `FREEBUFF_ENABLE_MIMO_MODELS_IN_UI = true` (`:908`, spread at `:2100`). The switch is UI-only — backend support and allowlists stay wired when a model is hidden.
- `FreebuffModelOption` (`:50-149`) carries id/displayName/tagline/taglineTooltip/availability/unavailableFallback/warning/dataUse/premium/multimodal/reasoningEffort/efforts/defaultEffort/experimental/isNew/supersededBy. **No price field exists.**
- `supersededBy` is still declared on the interface and read by the picker nudge (`:3999-4012`), but **no row sets it** — the last notice went 2026-08-21 and each row's docblock says so (`:1602-1605`, `:1791-1793`).

### 9.2 CLI/Desktop catalog rows (section 9.1 order)

| wire id | route / provider | section, tier flags | efforts → wire default | Freebucks/hr | images | badges / notes |
|---|---|---|---|---|---|---|
| `z-ai/glm-5.3-flash` | OpenRouter (Merge Gateway lane); `provider.max_price` ceiling `$0.14` in / `$0.45` out per M | UNLIMITED, `premium:false` | `low/high/max` → **max** (both `reasoningEffort` and `defaultEffort`) | **5 on every tier** | yes, text+image+video | `FREEBUFF_MODELS[0]` = `DEFAULT_FREEBUFF_MODEL_ID`; limited-tier hero; `isNew`; `dataUse:'service'`; unmetered (`:1719-1795`, `:2789-2790`, `:2934-2935`, `:2928`, `:215`, `:258-261`, `:828`) |
| `deepseek/deepseek-v4-flash` | DeepSeek direct; legacy alias `fireworks/deepseek-v4-flash` | UNLIMITED, `premium:false` | `low/high/max` → high | 15 base, **+10 inside peak** (`common/src/util/__tests__/freebuff-peak-price.test.ts:37,42,47`), 10 off-peak (fixture) | yes (since 2026-09-10) | displayName `'DeepSeek V4.1 Flash'`; the **limited-tier coercion target**; `unavailableFallback` = Luna; `warning` = AI-training notice, `dataUse:'training'`; `isNew` (`:1329-1445`, `:2922-2923`, `:1394-1396`) |
| `openai/gpt-5.6-luna` | OpenRouter, `provider.order` = `openai`; ceiling `$0.5`/`$3.0` | PREMIUM | through-max → high | 20 (picker fixture for this row) | yes (text+image+file) | `dataUse:'service'` and no AI-training notice; draws the shared daily premium pool; per-model pool sub-cap (`:1611-1641`, `:1622-1628`, `:330`, `cli/src/components/__tests__/freebuff-model-selector.test.tsx:1253`) |
| `mimo/mimo-v2.5` | MiMo 2.5 (Xiaomi) | UNLIMITED, `premium:false` | none — provider exposes only disabled/high, no ladder | 10 (fixture) | yes | `FALLBACK_FREEBUFF_MODEL_ID`, the always-joinable step-down; no `supersededBy` on purpose (`:1299-1327`, `:2901-2902`, `cli/src/utils/__tests__/freebucks.test.ts:34`) |
| `upstage/solar-pro4` | OpenRouter, endpoint `upstage`, `allow_fallbacks:false` | UNLIMITED (`premium` comes from the entitlement = false); `limitedAccess:true` | none — the route exposes no effort parameter | 0 during promo, then 5, then 10 (schedule) | no | tagline `'Limited-time trial'`; price schedule staged in `freebuff-solar-promo.ts`, not in the catalog (`:1643-1654`, `common/src/constants/freebuff-model-entitlements.ts:5-14`, `common/src/constants/freebuff-solar-promo.ts:4-7,11-41`) |
| `meta/muse-spark-1.2-contributor` | Meta dev API (`muse-spark-1.2-contributor`) | PREMIUM, `premium:true` | through-xhigh → xhigh | n/a | no | `warning` = AI-training notice + fallback tooltip ('queues when busy, then answers on DeepSeek V4.1 Flash'); retired from the Web picker 2026-09-02, still in `FREEBUFF_MODELS` so every surface reaches it (`:1882-1902`, `:903-904`, `:2517-2525`) |

Cites for 9.2 are `common/src/constants/freebuff-models.ts` unless another path is given. Prices in the last-and-second-last columns for Luna/MiMo/Flash are test fixtures around the real per-session map; only GLM's "5 on every tier" and Solar's schedule are stated in source prose.

- Context windows drive the CLI's compaction budgets (`FREEBUFF_MODEL_CONTEXT_WINDOWS`, `:1155-1199`): DeepSeek V4 Flash/Pro 1,048,576 · GLM 5.3 Flash 1,000,000 · Luna 1,000,000 (Luna-ES 372,000) · Muse Spark 1.2 1,000,000 · Ox Alpha 1,000,000 · Solar Pro 4 500,000 · MiniMax M3 524,288; every other id (MiMo included) falls back to `FREEBUFF_DEFAULT_CONTEXT_WINDOW = 131,072` (`:1203`). Published limits are entered deliberately low; only Flash/Pro were read off a provider rejection.

### 9.3 Paused, retired, withdrawn

Cites in this subsection are `common/src/constants/freebuff-models.ts`.

- `FREEBUFF_PAUSED_FREE_MODEL_IDS` (`:2220-2324`), in order: Muse Spark 1.3 (2026-09-07, 404 on every key), MiniMax M3 (08-20, largest single bill line), DeepSeek V4 Pro (08-26, cost), Ox Alpha (08-27, host ended the promo), GLM 5.2 (08-31, reward moved).
- A paused id is out of **every** picker and quota list, still *recognised*, and coerced to the tier's default at admission and at the session gate. The pause branch is checked first, ahead of all other admission logic (`:3340-3344`); the ordering exists because #1801 (limited tier, 2026-08-18) reached 2.5x admissions and 91% of sessions at the 0.1-unit floor when an unrecognised id could only be refused (`:2196-2219`).
- Retired-picker ids: `FREEBUFF_WEB_RETIRED_PICKER_MODEL_IDS` (`:2517-2525`) currently holds only Muse Spark 1.2, self-described as drain-only; both former occupants (CrofAI GLM 5.2, HY3) were deleted outright on 2026-08-04 after proving a picker filter is not a gate (`:2503-2510`).
- `FREEBUFF_SERVICE_ONLY_MODEL_IDS` is **empty** (`:3733-3734`), emptied 2026-09-04 when Muse Spark shipped to CLI/Desktop; the predicate still runs (`:3756-3758`).
- Withdrawn ids also keep their agent-root and allowlist entries so pre-deploy sessions drain mid-turn instead of failing (`:2247-2249`, `:2269-2273`). `freebuffWithdrawnModelMessage` names the asked-for model and its replacement (`:2333-2340`).

### 9.4 Agent-id mapping and cost mode

- base2 root per model — `FREEBUFF_ROOT_AGENT_ID_BY_MODEL`, 22 entries (`common/src/constants/free-agents.ts:418-447`): Flash → `base2-free-deepseek-flash`, GLM 5.3 Flash → `base2-free-glm-5-3-flash`, Luna → `base2-free-luna`, MiMo → `base2-free-mimo`, Solar → `base2-free-solar-pro4`, Muse Spark 1.2 → `base2-free-muse-spark`, Fable 5.1 → `base2-free-fable`; unknown models fall back to `base2-free`.
- base3 (single-loop harness) has two per-surface maps whose ids are deliberately shared: Web 14 entries (`:129-146`) and CLI 12 entries (`:166-185`). `getFreebuffBase3RootAgentIdForModel` falls back to the model's **base2** root, never another model's base3 root (`:530-534`).
- Reviewer per model: `FREEBUFF_REVIEWER_AGENT_ID_BY_MODEL` (`:463-486`); every entry must run the same model as its key, because the chat-completions session gate 403s `session_model_mismatch` on a cross-model reviewer (`:449-458`).
- Cost mode: `isFreeMode(costMode) === 'free'` (`:796-798`). Gating predicates are publisher-spoof-safe and exact-model, tolerating only date-like suffixes `^\d{6,8}(?:$|[-:])` (`:903-938`).
- Provisioned tiers and internal-eval rows (`FREEBUFF_PROVISIONED_MODELS`, `FREEBUFF_INTERNAL_EVAL_MODELS`, `common/src/constants/freebuff-models.ts:1539-1582`) are picker-invisible and **base2-only** — no base3 twin exists for any of them (`common/src/constants/free-agents.ts:434-446`).

### 9.5 Entitlement rules

Cites in this subsection are `common/src/constants/freebuff-models.ts`.

| who | may pick / be admitted |
|---|---|
| plain full-access free user | `FREEBUFF_MODELS`; the premium rows (Luna, Muse Spark 1.2) draw the shared daily pool — `FREEBUFF_PREMIUM_SESSION_LIMIT = 5`, documented as the rollback-safety value rather than the live limit (`:933-934`); the non-premium rows do not draw that pool (GLM 5.3 Flash and MiMo are documented unmetered, `:1742-1752`) |
| limited tier | `LIMITED_FREEBUFF_MODELS` = GLM 5.3 Flash (hero), Flash, MiMo, Solar (`:2959-2972`); hero is `LIMITED_FREEBUFF_HERO_MODEL_ID` = GLM 5.3 Flash (`:2934-2935`); the **coercion target** is a different row, `LIMITED_FREEBUFF_MODEL_ID` = Flash, chosen because it is joinable with no meter, no grant and no plan (`:2904-2923`) |
| reward / referral | reward model = GLM 5.3 Flash (`FREEBUFF_REWARD_MODEL_IDS`, `:2594-2595`), survives the coercion at limited tier (`:3471-3476`); `FREEBUFF_REWARD_MAX_DAILY_SESSIONS = 1` (`:1024`); full-access referrals grant +1 premium session/day instead |
| paid plan | widens *what* may be picked at limited access, never *how much* (`:3214-3222`); plan-metered ids are GLM 5.3 Flash, Luna, Flash, Kimi K3 Eco, Gemini 3.8 Flash (`:3388-3395`), with Gemini 3.8 Flash Pro-only globally (`:3107-3108`) |
| god-only | Kimi K3 Eco, GPT-5.6 Luna-ES — required for `/api/live`, latency and picker (`:2477-2494`) |
| limited-offer campaign | Fable 5.1 (`FREEBUFF_LIMITED_OFFER_MODEL_IDS`), admitted on **both** tiers; one admission per user per campaign, hard cap 500 (`:2419-2441`), returned before the limited-tier branch so a limited pick survives (`:3466-3469`) |
| paused | no tier at all (`:3340-3344`) |
| provisioned / internal eval | not entitlement-gated by any list: the id is absent from every catalog and quota list, the account carries the wire id, and the session resolves to that tier's own single-model root (`:1453-1533`, `common/src/constants/free-agents.ts:434-446`) |

### 9.6 Reasoning effort

- The CLI's `/reasoning` command is the counterpart to Desktop's effort picker (`cli/src/commands/reasoning.ts:36`); both write the same metadata key. With no argument it reports the catalog default and sets nothing; `default`/`reset` clears the override rather than storing the default; overrides are per model and do not carry across a model switch (`cli/src/__tests__/unit/freebuff-reasoning.test.ts:17-20,72-79,102-107,123-127`).
- Reporting to upstream: the override is sent verbatim as `extraCodebuffMetadata.freebuff_reasoning_effort` (`cli/src/hooks/use-send-message.ts:671-673`), and `null` means "send nothing" (`cli/src/state/freebuff-model-store.ts:107-111`). The server treats it as a **request it re-clamps**, so the client's only job is to send a rung the selected model actually offers.
- Ladders, all `as const` in `common/src/constants/freebuff-models.ts`: DeepSeek V4 `['low','high','max']` (`:743`), GLM 5.3 Flash `['low','high','max']` (`:828`), Luna through-max (`:719-724`), Muse Spark through-xhigh (`:712-717`), Ox Alpha `['low','high','max']` (`:748`). MiMo, Solar Pro 4 and MiniMax M3 carry no ladder because their routes expose no native effort parameter.
- Wire defaults are `reasoningEffort`/`defaultEffort` on the row: GLM 5.3 Flash both `'max'` (`:1788-1789`), Flash `'high'` + `defaultEffort:'high'` (`:1432,1443`), Luna `'high'` (`:330`, `:1625-1628`), Muse Spark `'xhigh'` (`:640`, `:1894-1896`). V4.1 now validates the parameter against `none|minimal|low|medium|high|xhigh|max`, while `toDeepSeekReasoningEffort` still collapses onto `low|high|max` (`:1436-1441`).
- Two stale docblocks contradict the code and are **not** to be trusted: the catalog note claiming GLM 5.3 Flash is "pinned to `reasoningEffort: 'high'` and `max` is off its ladder" (`:2082-2083`, and the same claim in the `DEFAULT_FREEBUFF_MODEL_ID` docblock) while the row declares `max`; and the `getRecommendedFreebuffModelId` docblock still naming Luna/MiMo as the heroes (`:3227-3231`).

### 9.7 Quota labels and where prices come from

- Row-level pool chip: `formatFreebuffRowQuota` renders `poolLabel: N of M used` — or `N of M starts` when the pool counts admissions — e.g. `DeepSeek: 1 of 1 used`, `Frontier: 2 of 2 used` (`common/src/util/freebuff-session-pools.ts:91-97`, `cli/src/components/__tests__/deepseek-quota-row.test.tsx:76-77,135-136`). The CLI only draws it for rows carrying a stricter pool than their section (`cli/src/components/freebuff-model-selector.tsx:468-489`).
- Section header: `getFreebuffSectionQuotas(...).header` supplies the shared count, server-sent and never a locally guessed denominator (`cli/src/components/freebuff-model-selector.tsx:389-403`); the session-ended banner reuses it as `N of M used today` (`cli/src/components/session-ended-banner.tsx:71-73`).
- First-tab discount: applied client-side over the quote, not a new price — `applyFirstTabDiscount` / `firstTabListPriceFor` keep `listPrices` beside `prices` (`common/src/util/freebuff-first-tab-discount.ts:14,40`); the CLI draws the moved price in the accent colour, and since `8ed5d3e5e` that accent is the row's **only** first-tab signal — the adjacent `'Limited-time first-tab discount'` chip was deleted (§14.6) (`cli/src/components/freebuff-model-selector.tsx:453-457`).
- Price sourcing: no catalog price, so `freebucksPriceFor(freebucks, modelId)` reads `freebucks.prices[modelId]` off the session, and that map **is** the allowlist — an absent row falls through to whatever metered it before (`cli/src/utils/freebucks.ts:59-72`). Prices present as `N/hr` (`cli/src/components/freebuff-model-selector.tsx:448-451`); the shared off-peak helper now emits only `Off-peak: {price} Freebucks/hour, daily {hours}.` (`common/src/util/freebuff-off-peak-price.ts:39-43`) and since `8ed5d3e5e` no CLI surface renders it — the picker's off-peak detail chip was deleted, leaving the helper with no CLI caller (§14.6).
- The authoritative price table lives in `common/src/constants/freebuff-freebucks.ts`, which is **deleted from the public export** (`scripts/public-export-manifest.txt` carries `!common/src/constants/freebuff-freebucks.ts`) because it records measured per-session provider costs; `cli/` *is* exported, so the CLI cannot import it and takes the currency label as a literal instead (`cli/src/utils/freebucks.ts:1-12`, `common/src/constants/freebuff-earn.ts:8-13`, `common/src/util/freebuff-peak-price.ts:4-8`). The wire carries upgrade copy for the same reason (`common/src/types/freebuff-session.ts:312-316`). Per-model prices therefore cannot be enumerated from the public clone beyond the fixtures cited above.

## 10. Limits & error states (wire → UI)

Every limit is a wire status/code the CLI matches, and the chat path evaluates them in a fixed order — **provider-usage → out-of-credits → free-mode-unavailable → session gate → 429 rate limit → generic** — mirroring the same ladder for a finished run whose `output.type === 'error'` (`cli/src/hooks/helpers/send-message.ts:403-453,530-572`). Gate codes require **both** code and HTTP status to match, via `Object.hasOwn` (`common/src/types/freebuff-session.ts:1236-1244`), so a 5xx echoing e.g. `session_superseded` stays a generic error.

`Class` legend: **T-chat** = terminal, chat unusable until re-admit/restart · **T-run** = terminal for this run only · **T-turn** = terminal for the turn, session survives · **T-admit** = blocks fresh admission only · **N** = non-terminal, chat kept · **R** = retryable.

| # | Status | Marker | Class | Exact user-facing message | Recovery / behavior | Cite |
|---|---|---|---|---|---|---|
| 1 | 403 | `free_mode_unavailable` | T-chat | body `message` ?? `Freebuff is not available in your country.` | flips session to `country_blocked`, aborts polling, landing shows region screen + `Press Ctrl+C to exit` | `cli/src/utils/error-handling.ts:59-65,200-211,248-250`; `send-message.ts:542-552`; `cli/src/components/freebuff-landing-screen.tsx:817-857` |
| 2 | 403 | `free_mode_unavailable` + `countryBlockReason:'anonymous_network'` | T-chat | `Freebuff cannot be used from {signals} traffic. Please disable it and try again.` — signals `VPN, proxy, or Tor` when unrecognized, 2 signals join as `A or B` | same as #1 | `error-handling.ts:204-208`; `common/src/util/freebuff-privacy.ts:3-24,59-74` |
| 3 | 403 | body `status:'country_blocked'` | T-chat | Landing `⚠ Free mode isn't available in your region` + per-reason prose (`UNKNOWN` → "We couldn't verify an eligible location…", else "We detected your location as {CC}, which is outside the countries…") | terminal, polling stopped | `cli/src/utils/freebuff-session-api.ts:200-208`; `landing:817-857`; `cli/src/hooks/use-freebuff-session.ts:98` |
| 4 | 403 | body `status:'banned'` | T-chat | `⚠ Account unavailable` / `This account has been suspended and can't use freebuff. If you think this is a mistake, contact support@codebuff.com. Press Ctrl+C to exit.` | terminal | `freebuff-session-api.ts:204-207`; `landing:862-873` |
| 5 | 429 | body `status:'ip_capped'` (`activeUsersForIp`,`limit`,`retryAfterMs`) | T-chat | `🚦 Too many Freebuff sessions on this network` / `{N} other people are already using Freebuff from your network, which is the most we allow at once. Try again in {retry} — a slot opens as soon as one of them finishes. Press Ctrl+C to exit.` | terminal; no reset clock (slot frees when a peer ends) | `freebuff-session-api.ts:229-234`; `landing:996-1012` |
| 6 | 429 | body `status:'rate_limited'` (`period`,`limit`,`recentCount`,`retryAfterMs`, optional `freebucksShortfall`, `upgrade`) | T-chat | Title: `⚠ Not enough Freebucks` (shortfall) / `⚠ Monthly usage limit reached` (metered + `pacific_month`) / `⚠ Session limit reached`. Bodies — shortfall: `This model costs {price}, and you have {balance} left. More in {retry}, or pick a cheaper model. Turn on auto top-up to keep going: https://freebuff.com/freebucks. Press Ctrl+C to exit.`; monthly: `You've used {$X} of {$Y} monthly usage. It resets in {retry}. Press Ctrl+C to exit.`; else: `You've used {n} of {m} sessions today\|this week\|this month. Try again in {retry}. Press Ctrl+C to exit.` Upgrade line: `{upgrade.message} {upgrade.url}` else `Get more Freebucks with a plan: https://freebuff.com/plans` / `Get more sessions with a plan: https://freebuff.com/plans` | terminal for the run | `landing:877-963`; `common/src/types/freebuff-session.ts:1069-1084` |
| 7 | 429 | body `status:'spend_limited'` (`message`,`resetAt`,`retryAfterMs`,`upgrade?`) | T-admit | Title `☕ Daily usage cap reached` (metered) / `☕ Daily Freebuff limit reached`; body `{session.message} It resets in {retry}, at midnight Pacific. Press Ctrl+C to exit.` | fresh admission blocked; live/reconnect sessions continue | `landing:970-989`; `types:1085-1098` |
| 8 | 409 | gate `session_superseded` | T-chat | `Another freebuff CLI took over this account. Close the other instance, then restart.` | `markFreebuffSessionSuperseded()` → superseded screen: `Another freebuff instance took over this account.` / `Only one CLI per account can be active at a time.` / `Close the other instance, then restart freebuff here.` | `send-message.ts:620-627`; `types:1203`; `cli/src/components/freebuff-superseded-screen.tsx:33-44`; `cli/src/app.tsx:367-369` |
| 9 | 409 | GET body `status:'superseded'` | T-chat | same superseded screen | terminal, polling stopped | `use-freebuff-session.ts:405-409,96` |
| 10 | 410 / 428 / 409 | gates `session_expired` / `waiting_room_required` / `session_model_mismatch` | T-chat | if the run produced no content: `Your free session ended before this message was processed. Send it again after starting a new session.` Otherwise banner only | `markComplete()` + `markFreebuffSessionEnded()`; chat stays mounted, `SessionEndedBanner` Enter re-joins | `send-message.ts:586-609`; `types:1201-1204` |
| 11 | 429 | gate `waiting_room_queued` | N | `Your free session is still being set up. Try again in a moment.` | `refreshFreebuffSession()` (re-POST), chat kept | `send-message.ts:610-619`; `types:1208` |
| 12 | 409 | gate `session_limit_reached` | N | **No CLI message** — deliberately excluded (Desktop concurrent-tab cap) | none (CLI runs one session/user) | `error-handling.ts:216-241`; `types:1205-1206` |
| 13 | 410 | gate `model_unavailable` | N | **No gate-handler case** (falls through `default: return`) — gate prose never printed here | window kept; user re-picks | `error-handling.ts:227-241`; `send-message.ts:628-629`; `types:1209-1223` |
| 14 | 409 | POST body `status:'model_unavailable'` (`availableHours`,`availableAt?`,`withdrawn?`,`requiresSubscription?`,`limitedOfferReason?`) | N | withdrawn → `{Name} is no longer available in Freebuff. We recommend using {default} instead.`; limited offer → `You've used your one {name} trial session. Switching to another model.` or `{name}'s trial is currently unavailable. Switching to another model.`; otherwise silent | auto-flip to `FALLBACK_FREEBUFF_MODEL_ID` (in-memory; saved preference kept), re-POST | `use-freebuff-session.ts:734-780`; `common/src/constants/freebuff-models.ts:2333-2340` |
| 15 | 409 | POST body `status:'model_locked'` (`currentModel`,`requestedModel`) | N | deliberate pick + DELETE ok → `Ended your previous session on {current} and switched to {requested}.`; DELETE failed → `You're already in an active session on {current}, and ending it failed, so the switch to {requested} was not applied. Run /end-session, then pick {requested}. (Sessions end on their own after 1 hour.)`; background rejoin → silent revert | end + re-POST, or revert selection | `use-freebuff-session.ts:662-733` |
| 16 | 429 | `turn_spend_limit` (+ `message`) | T-turn | body `message` ?? `This turn reached its model usage limit. Your session is still available — send a new message to continue from here.` | SDK throws non-retryable `APICallError`: no retry loop, no cooldown | `error-handling.ts:135-139`; `common/src/constants/freebuff-errors.ts:11-14`; `sdk/src/impl/model-provider.ts:169-207` |
| 17 | 429 | `free_mode_rate_limited` | R | body `message` verbatim (already user-facing, carries a countdown) else `Freebuff is temporarily busy. Please try again in a moment.` | retry | `error-handling.ts:130-134,245-246` |
| 18 | 429 | any other 429 (relayed upstream capacity) | R | `Freebuff is temporarily busy. Please try again in a moment. ({detail})`; detail only from a server body or an agent-run `type:'error'` object; bare `too many requests` suppressed | retry | `error-handling.ts:140-155` |
| 19 | 429 | `free_mode_capacity_deferred` | R | status bar replaces retry text: `high demand — in line, starting soon...` | silent: AI SDK retries; `retryAfterSeconds` from header else **10** | `sdk/src/impl/model-provider.ts:51-92`; `cli/src/chat.tsx:550-555`; `cli/src/components/status-bar.tsx:171-174` |
| 20 | 402 or provider wording | `FREEBUFF_PROVIDER_USAGE_ERROR_PATTERN` | T-run | `Freebuff ran out of provider usage and needs a refill. This is on us, not your account.` | terminal for the run; **never** the credit-purchase flow. On `IS_FREEBUFF` any 402 matches, so #21 is unreachable for 402 | `common/src/constants/freebuff-errors.ts:2-7`; `error-handling.ts:158-171`; `send-message.ts:530-533,404-408` |
| 21 | 402 | `isOutOfCreditsError` (`statusCode === 402`, Codebuff-only path) | T-run | `Out of credits. Please add credits at {NEXT_PUBLIC_CODEBUFF_APP_URL \|\| 'https://codebuff.com'}/usage` | `setInputMode('outOfCredits')` + invalidate usage query | `error-handling.ts:20,43-53,243`; `send-message.ts:535-540` |
| 22 | 403 | `free_mode_invalid_agent_model` / `free_mode_invalid_agent_hierarchy` | R(unhandled) | no dedicated copy — surfaces raw via `updater.setError(output.message)` | server-side allowlist; withdrawn models drain rather than 403 mid-turn | `send-message.ts:571-572`; `common/src/constants/free-agents.ts:126-130` |
| 23 | 403 | `free_mode_cost_mode_required` | R(unhandled) | `Freebuff agents run in free mode. Send codebuff_metadata.cost_mode = "free", or pick a non-Freebuff agent.` | none (CLI always sends `free`) | `common/src/constants/freebuff-cost-mode.ts:63-66` |
| 24 | 403 | `free_mode_gemini_thinker_required` | R(unhandled) | no CLI copy | server-side gate (Gemini Pro → gemini-thinker subagent) | `evals/buffbench/judge.ts:158-160` |
| 25 | 404/405 (POST) | `session_admission_unsupported` (client-synthesized) | T-chat | `This server cannot safely start or resume your session yet. Reload or update Freebuff and try again shortly. No purchase was made.` | POST 4xx → disposition `stop` (no repeat; a POST may have rotated the instance) | `cli/src/utils/freebuff-session-api.ts:56-70,186-192`; `freebuff-models.ts:2619-2620` |
| 26 | 200 | POST body `status:'consent_required'` | N | `Your balance changed. Choose the model again to confirm {walletSpend} wallet Freebucks.` | drops to landing (`status:'none'`), user re-picks | `use-freebuff-session.ts:630-651`; `types:814-819` |
| 27 | 200 | POST body `status:'first_tab_discount_changed'` | N | `Your first-tab discount changed. Review the model menu and choose again. No Freebucks were charged.` | drops to landing | `use-freebuff-session.ts:634-650`; `common/src/util/freebuff-first-tab-discount.ts:5-6` |
| 28 | 200 | Desktop-only `premium_slot_taken`, `purchase_in_use`, `purchase_capacity`, `purchase_claim_released`, `purchase_*` | T-chat | no dedicated CLI screen; treated as terminal (no further polling) | Desktop-only | `use-freebuff-session.ts:105-109`; `types:1099-1131` |
| 29 | n/a | network failure (no HTTP answer) | R | `Couldn't get a response from {host}. If your browser can open freebuff.com, this network isn't routing to that host: try mobile data or another ISP, or use freebuff.com/web meanwhile.` + ` Retrying automatically.` when a retry is armed | GET retries 408/429/5xx; POST retries only 408/429/503, other 4xx `stop`, else `unknown` | `freebuff-session-api.ts:170-176,56-77`; `landing:114-126`; `use-freebuff-session.ts:872-923` |
| 30 | 503 (takeover) | — | N | `Freebuff is busy and couldn't complete the takeover yet.`; outcome-unknown: `Freebuff couldn't confirm whether the takeover succeeded. Check the warning, then retry if you still want to take over.` | prompt retry/exit | `landing:128-145` |
| 31 | n/a | `/end-session` DELETE not confirmed | N | `Could not confirm the session ended. Retry /end-session. {msg}`; plus system line `Ending session and returning to the model picker…` | held instance kept so the refund receipt can be recovered | `use-freebuff-session.ts:255-264`; `cli/src/utils/constants.ts:11-13` |

Gate wire contract (`common/src/types/freebuff-session.ts:1200-1224`): `waiting_room_required` 428 `endsTheSession:true`, `session_expired` 410 true, `session_superseded` 409 true, `session_model_mismatch` 409 true, `session_limit_reached` 409 false, `waiting_room_queued` 429 false, `model_unavailable` 410 false.

Two cross-checks that shape the matrix: `getFreebuffRateLimitErrorMessage` returns `null` for non-429, so a 402/403/5xx can never be reframed as "temporarily busy" (`error-handling.ts:125-129`); and `nextDelayMs` enumerates every upstream-only status that must not be polled further, exhaustively over the response union (`use-freebuff-session.ts:76-111`).

Not found at this pin (verified, not assumed): `no_endpoints` (an OpenRouter body reaching generic verbatim; only server-side), `outside_hours` (the availability vocabulary is `'always' | 'off_peak_only' | 'deployment_hours'`), `fanout` (named only in a server-side comment; the CLI-visible shed path is #19), and `free_mode_cli_required` (0 hits repo-wide).

## 11. Freebucks, quotas, peak hours & spend ceilings

Freebucks is the CLI's meter for metered accounts: a per-hour price is quoted on the session response and every price/balance the CLI shows — including the header, the sort order and the disabled state — is read from that wire block, never computed locally.

### Meter semantics

- **Wire-only pricing.** The CLI hardcodes no price; `freebucksPriceFor` is literally `freebucks.prices[modelId]`, and "not metered" and "unpriced row" are deliberately the same `undefined`. The module header notes `common/src/constants/freebuff-freebucks.ts` is export-excluded, so only the label is duplicated: `FREEBUCKS_LABEL = 'Freebucks'` (`cli/src/utils/freebucks.ts:36-38,59-72`).
- **Presence is the gate.** `freebucksOf` passes the block through `firstTabQuoteForSession(..., 'single')`; wire `null` = refresh unavailable (clients must clear a stale balance), `undefined` = no meter supplied — a balance is never synthesized (`freebucks.ts:45-57`; `common/src/types/freebuff-session.ts:616-626`).
- **One price is one HOUR.** `FREEBUFF_REWARD_SESSION_LENGTH_MS = 60 * 60 * 1000` and the tier field doc says "One session is one hour"; UI copy: `Sessions end on their own after 1 hour.` (`common/src/constants/freebuff-models.ts:1031-1034`; `common/src/constants/freebuff-subscriptions.ts:167`; `cli/src/hooks/use-freebuff-session.ts:726`).
- **DELETE refund.** A session is charged once, at admission (the compact-poll merge keeps the carried Freebucks block, `cli/src/utils/freebuff-session-api.ts:mergeCompactActiveSession`). `/end-session` advertises `End session; get 90% of unspent cost back, rounded down` (`cli/src/data/slash-commands.ts:203-206`). `DELETE` must answer `status:'ended'` or the CLI throws `The server did not confirm that the session ended.`; the receipt is `freebucksRefund` (`0` is a receipt) with `freebucksRefundPending` meaning *replay the same instance* (`cli/src/state/freebuff-session-store.ts:74-135`; `types:899-902`). Pending refunds poll every 3 s; landing shows `Your refund is awaiting final usage. Once settled, it will appear in your wallet.` then `{N} Freebucks returned to your wallet.` (`use-freebuff-session.ts:480-497`; `landing:723-734`).
- **Reset boundary.** The *daily Freebucks* refill is the account's **reset timezone**, not Pacific: `'Daily Freebucks refill at midnight in your reset timezone. …'`, declared per request via header `x-fb-timezone` (`common/src/util/freebucks-reset.ts:5-8`; `common/src/util/freebucks-timezone.ts:1`; sent on every session call, `freebuff-session-api.ts:161`). The *session-pool* reset is separate and Pacific: `period: 'pacific_day' | 'pacific_week' | 'pacific_month'` (`types:1075`), the `spend_limited` screen hardcodes "at midnight Pacific" (`landing:980`), and subscriptions share that boundary (`FREEBUFF_SUBSCRIPTION_RESET_TIMEZONE = 'America/Los_Angeles'`, `freebuff-subscriptions.ts:312-317`).

### Display formats, countdown & intro

| Surface | Exact form | Cite |
|---|---|---|
| `formatFreebucks` | `max(0, round(n)).toLocaleString()` (whole units) | `freebucks.ts:41-43` |
| `freebucksPriceLabel` | `{n} Freebucks/hr` — the `/hr` is deliberate (a bare number reads as a per-message rate) | `freebucks.ts:161-168` |
| `formatAllowanceUsd` | `$N` ≥ 10, one decimal ≥ 1 (`\.0` dropped), else two decimals | `freebucks.ts:147-159` |
| Header line | `{remaining}/{limit} Freebucks daily · resets in 4h 12m · 20 in wallet` — countdown only with a clock, wallet only when `> 0`, dollar allowance omitted when the server sent none (never `$0`) | `freebucks.ts:106-145` |
| Refill / countdown | past `resetAt` → `Updating balance…` (`FREEBUCKS_REFILL_PENDING_LABEL`); else `4h 12m` / `38m` / `2d 5h`, `now` once passed | `common/src/util/freebucks-reset.ts:8,15-18`; `freebucks.ts:186-203` |
| Intro card (once) | `Meet Freebucks` / `Sessions are now bought with Freebucks instead of counted against weekly and monthly limits.` + 3 points + `Shown once. Press any key to continue.` | `freebucks.ts:170-184` |
| Unaffordable row | `Not enough Freebucks — {price}/hr against {balance} left. Enter opens plans.` | `cli/src/components/freebuff-model-selector.tsx:642-645` |

### Peak / off-peak (`common/src/constants/freebuff-peak-hours.ts`)

| Constant | Value | Note |
|---|---|---|
| `DEEPSEEK_PEAK_HOUR_RANGES_UTC` | `[[1,4],[6,10]]` (half-open, Mon–Fri Beijing) | two disjoint windows; weekends always off-peak; `:23-35,44-51` |
| `DEEPSEEK_EXPENSIVE_WINDOW_LEAD_HOURS` | `1` | an hour-long session admitted just before peak still runs into it; `:57-65` |
| `DEEPSEEK_EXPENSIVE_WINDOW_UTC` | `[0,10]` (derived `min(start)−1`, `max(end)`) | one window; swallows the 04–06 gap deliberately; `:67-84,86-93` |
| Multipliers | none encoded in public code | DeepSeek's 2× is *described*, not stored; the client-visible number is the server's `surcharge` (`types:335-342`; `common/src/util/freebuff-peak-price.ts:57-63`). Since `8ed5d3e5e` no CLI surface renders `freebucksPeakCopy` — the picker's peak-pricing chip was deleted (§14.6) — so a peaked row shows the surcharge only inside its quoted `{N} Freebucks/hr` price, with the catalog tagline beside it (`taglineFor`, `:351-358`), not an explanation. |
| `FREEBUFF_BETA_RATE_LOCK_MULTIPLIER` | `3` | beta rate-lock copy, unrelated to peak pricing; `freebuff-subscriptions.ts:514` |

Off-peak badges come from the wire block (`info.offPeak[modelId]`), e.g. tooltip `Off-peak: {price} Freebucks/hour, daily {hours}.` (`common/src/util/freebuff-off-peak-price.ts:11-44`) — since `8ed5d3e5e` that helper has **no CLI caller** (the picker's off-peak detail chip was removed, §14.6), so the copy reaches no surface and the badge/tooltip fields are dead weight in the public snapshot. Dated `priceChanges` + recurring `offPeak` apply **to new-session quotes only** — never to balances or an in-flight session's charge (`common/src/util/freebuff-price-changes.ts:31-33`). Deployment-hours rows are open while `America/New_York ≥ 09:00` **and** `America/Los_Angeles < 17:00`, label `'9am ET-5pm PT every day'`, row labels `until {t}` / `opens {t}`; `off_peak_only` rows render `Back at {t} {zone}` closed and `Open {window}` at all hours (`freebuff-models.ts:1095-1096,4029-4077,4151-4171`).

### Spend ceilings & signup block

`common/src/constants/freebuff-spend-ceilings.ts` — header comment: "Compatibility notices, not spend enforcement" (`:1-4`):

| Constant | Value (verbatim) | Gates (reason) |
|---|---|---|
| `FREEBUFF_CAPACITY_NOTICE` | `Capacity is now limited per account — sustained automated abuse forced us to cap how much any one account can use.` | default / `third_party_client` |
| `FREEBUFF_RESTRICTED_NOTICE` | `This account has reduced capacity: it was flagged for VPN or proxy usage, a restricted location, or an email domain commonly used by bot farms. If you are on a VPN, connecting directly restores normal limits. If you have moved, verify your country at freebuff.com/account?tab=country.` | `privacy_egress`, `restricted_country`, `flagged_email_domain`, `unverified_egress` |
| `FREEBUFF_FREEBUCKS_CEILING_NOTICE` | `This account hit today's hard usage cap. Freebucks pay for sessions, but the compute a day can draw is capped at three times what its Freebucks are worth, to protect the service from runaway usage.` | `freebucks_plan` — hard cap = **3×** Freebucks value |
| `FREEBUFF_BUDGET_NOTICE` | `You have used all of today's free usage on this account.` | `region`, `elevated_country`, `trust_level` |

Resolution order: restricted set → budget set → `freebucks_plan` → capacity (`:30-36`). No CLI module imports this file (only its test), so these strings reach users solely through the server's verbatim `spend_limited.message` (`landing:976`).

**Signup block** (`common/src/constants/freebuff-signup-block.ts`) is web-login copy, not CLI: 9 reasons, also the `?error=` code — `captcha_missing`, `captcha_invalid`, `recaptcha_missing`, `recaptcha_invalid`, `mailbox_already_registered`, `privacy_egress`, `untrusted_client_ip`, `ip_signup_velocity`, `prefix_signup_velocity` (`:11-26`). Guard `isSignupBlockReason` (`:64-68`).

**Standing / Access Level** (`common/src/constants/freebuff-standing.ts`) — presentational half only: `FREEBUFF_TRUST_LEVELS = ['new','verified','established','core']` (index-ordered), `FREEBUFF_TRUST_MIN_LEVEL='new'`, `FREEBUFF_TRUST_FALLBACK_LEVEL='established'` (a resolver failure must NOT drop everyone to `new`), labels `Getting started | Verified | Established | Core member`; the wire `FreebuffStandingInfo` rides **only** the pre-join `status:'none'` response (`:20-26,33-46,61-76,100-123`).

### Session quotas

| Constant | Value | Cite |
|---|---|---|
| `FREEBUFF_FREE_TIER_ALLOWANCE` | `{ dailySessions: 4, weeklySessions: 14, monthlySessions: 40 }` | `freebuff-subscriptions.ts:201-205` |
| Free-tier marketed dollars | `$20` | `freebuff-subscriptions.ts:210-215` |
| `FREEBUFF_SUBSCRIPTION_TIERS` | starter 3/10/30 (`$8`, intro `$5`), plus 7/26/100 (`$25`/`$19`), pro 11/66/210 (`$60`/`$45`) — plan windows are **totals − free**; marketed totals Free 4/14/40, Starter 7/24/70, Plus 11/40/140, Pro 15/80/250 | `freebuff-subscriptions.ts:207-255,210-215` |
| `FREEBUFF_SUBSCRIPTION_FIVE_DAY_WINDOW_DAYS` | `7` (name kept for released clients) | `freebuff-subscriptions.ts:319-323` |
| `FREEBUFF_PREMIUM_SESSION_LIMIT` | `5` (base premium sessions per Pacific day, before earned; moot once an account is metered) | `freebuff-models.ts:929-934` |
| `FREEBUFF_LIMITED_SESSION_LIMIT` | `6` (limited-region base; meters MiMo, the only plan-free limited model) | `freebuff-models.ts:947` |
| `FREEBUFF_REWARD_MAX_DAILY_SESSIONS` | `1`; reward model = GLM 5.3 Flash; full-access referrals instead grant +1 daily premium session | `freebuff-models.ts:1024,2594-2595` |
| Streak rewards | interval `7` days, bonus multiplier `4`, `1` unit per tier; master switches `FREEBUFF_STREAK_REWARDS_ENABLED`/`…_BONUS_ENABLED` | `freebuff-models.ts:1060-1073` |
| `FREEBUFF_NEAR_LIMIT_FRACTION` | `0.8` (near-limit nudge vs hard wall) | `common/src/util/freebuff-limit-nudge.ts:20,43-46` |

### First-tab discount

Opted into per request via `x-freebuff-first-tab-discount`; discounts come off **list** prices so re-applying never stacks, and `listPrices` keeps the crossed-out original (`common/src/util/freebuff-first-tab-discount.ts:4,11-30`). Copy: available → `Limited-time first-tab discount: up to {amount} Freebucks off one session at a time, shared across Web, Desktop and CLI. Prices shown include the discount; the crossed-out price is the regular one.`; in use → `Your first-tab discount is in use. Parallel sessions pay the regular price. The discount becomes available when that session ends.` (`:79-87`) — since `8ed5d3e5e` that copy has **no CLI caller**: the picker's chip and the ask-line fallback that returned `firstTabDiscountCopy(freebucks)` were both deleted, so only the wire header and `firstTabListPriceFor`'s accent colour survive on this surface (§14.6). A changed offer drops to landing with `Your first-tab discount changed. Review the model menu and choose again. No Freebucks were charged.` (`:5-6`).

### Per-model / per-pool quota labels

The picker's section header takes the pool **most of its rows** belong to (ties toward the earlier row); rows on another pool carry their own inline chip (`common/src/util/freebuff-session-pools.ts:46-82`). Chip text: `{used} of {limit} starts|used`, prefixed `{poolLabel}: ` when the server sent one (`:91-97`). `getFreebuffModelMeter` returns only the applicable meter (Freebucks price/balance, else per-model quota, else legacy remaining) so label and disabled state cannot disagree — a projection of the server snapshot, never admission authority (`:102-131`). Section labels are `premium`/`unlimited` (full), `limited` (unlabelled) for limited tier, plus `offer` and a single flat `metered` list once on the Freebucks meter (`cli/src/components/freebuff-model-selector.tsx:128-131,792-841`).

### How the CLI learns prices

- **From the wire, per session**: `freebucks.prices[modelId]`, `freebucks.balance`, `freebucks.daily{remaining,limit,resetAt}`, `freebucks.wallet.balance`, `freebucks.offPeak`, `freebucks.listPrices`, `freebucks.priceChanges`, `firstTabDiscount` (`freebucks.ts:59-72`; `types:616-626`).
- **Never from the catalog**: `FreebuffModelOption` has no price field (`common/src/constants/freebuff-models.ts:50-149`); the authoritative price table lives in the export-excluded `common/src/constants/freebuff-freebucks.ts`.
- The only hardcoded token is the label string `Freebucks`; everything else — including the sort (`sortModelsByPrice`, cheapest-first, `undefined` last) — derives from the wire block (`freebucks.ts:36-38,77-104`).

## 12. Config, state files & environment

All durable CLI state lives in one config directory; the launcher keeps a separate hardcoded cache dir, and only `FREEBUFF_CONFIG_DIR` plus a few `CODEBUFF_*` runtime vars change paths or behavior after build.

### Config-dir resolution
- `getConfigDir()` returns `FREEBUFF_CONFIG_DIR` when set, else `join(os.homedir(), '.config', 'manicode' + (NEXT_PUBLIC_CB_ENVIRONMENT !== 'prod' ? '-' + env : ''))` (`cli/src/utils/config-dir.ts:16-36`).
- `FREEBUFF_CONFIG_DIR` MUST be absolute; a relative value throws `FREEBUFF_CONFIG_DIR must be an absolute path so CLI settings cannot be written relative to the current project.` (`cli/src/utils/config-dir.ts:17-25`; pinned by `cli/src/utils/__tests__/config-dir.test.ts:30-36`).
- Windows resolves identically — `%USERPROFILE%\.config\manicode` — via `os.homedir()` alone; `APPDATA`, `LOCALAPPDATA` and `XDG_CONFIG_HOME` are never part of the config-dir path (`APPDATA`/`XDG_CONFIG_HOME` are read only for editor *theme* discovery, `cli/src/utils/theme-system.ts:220-230, 255, 302-304`).
- The `-<env>` suffix is compile-time frozen (`NEXT_PUBLIC_CB_ENVIRONMENT` is a `--define`): a shipped binary is always `~/.config/manicode`, while dev runs use `manicode-dev`/`manicode-test` (`cli/src/__tests__/integration/credentials-storage.test.ts:169-203`).
- The dir is created mode `0o700` (`cli/src/utils/auth.ts:45, 204`).
- The launcher's config dir is a separate domain: `~/.config/manicode`, hardcoded, ignoring `FREEBUFF_CONFIG_DIR` (`cli/release-core/launcher.js:256-275`).

### State files in the config dir
| Path | Contents | Mode | Cite |
|---|---|---|---|
| `settings.json` | keys listed below; created as `{"mode":"DEFAULT","adsEnabled":true}` when missing | default umask | `cli/src/utils/settings.ts:25-28, 77-79, 95-103` |
| `credentials.json` | `{ default: { id?, name, email, authToken, fingerprintId?, fingerprintHash?, credits? } }`; legacy `chatgptOAuth` dropped on next rewrite | `0o600`, re-`chmod`ed on every read and write, **skipped on Windows** | `cli/src/utils/auth.ts:14-30, 42-70, 182-193, 198-221` |
| `analytics-id.json` | persistent anonymous id | umask | `cli/src/utils/anonymous-id.ts:26-31` |
| `recent-projects.json` | `[{path,lastOpened}]`, capped at 10 | umask | `cli/src/utils/recent-projects.ts:7, 17-19` |
| `message-history.json` | `string[]` of prior prompts | umask | `cli/src/utils/message-history.ts:57-59` |
| `freebuff-instance-owner.json` | `{instanceId, pid}` single-instance bookkeeping | umask | `cli/src/utils/freebuff-instance-owner.ts:12-14, 46-50` |
| `trusted-agent-dirs.json` | `{ "<abs dir>": {"trustedAt": ISO} }` for repo `.agents`/`mcp.json` | `0o600` | `cli/src/utils/agent-dir-trust.ts:33, 85-86`; `docs/agents-and-tools.md:48-51` |
| `sponsored-terminal-reports/<sha256(canonicalRoot)>.json` | sponsored-run terminal reports | `0o600`, created `flag:'wx'` | `cli/src/utils/sponsored-run.ts:1668-1670` |
| `cpu-features.json` | `{avx2:boolean}` — launcher-written AVX2 cache | umask | `cli/release-core/launcher.js:458-483` |

`settings.json` accepted keys: `mode`, `adsEnabled`, `freebuffModel`, `freebuffModelDefaultMigration`, `freebuffReasoningEfforts` (per model id), `byokConnection{id,revision,provider?,model?}`, deprecated `alwaysUseALaCarte`/`fallbackToALaCarte`, `hasSubmittedFirstPrompt`, `freebucksIntroSeenAt` (`cli/src/utils/settings.ts:35-72`). BYOK secrets never live here (asserted `cli/src/utils/__tests__/settings.test.ts:53`): the CLI stores connection metadata and reads the secret from the OS keychain or an explicit `env:NAME` reference (`cli/src/utils/byok.ts:31-43`; `sdk/src/byok.ts:8-10, 22-27, 530-563`).

### Per-chat transcript state
- Layout `<configDir>/projects/<basename(projectRoot)>/chats/<chatId>/` (`cli/src/project-files.ts:51-61, 101-106`) holding `run-state.json`, `chat-messages.json`, `chat-meta.json`, `log.jsonl` (pino, `cli/src/utils/logger.ts:27`) and `trace.jsonl` when tracing is on (`cli/src/utils/trace-writer.ts:12, 26-41`).
- `run-state.json` and `chat-messages.json` are parsed independently with atomic writes (`cli/src/utils/run-state-storage.ts:22, 163-173, 344-357, 503-509`); `chat-meta.json` is a size/mtime-validated sidecar so `/history` need not parse unbounded transcripts (`cli/src/utils/chat-meta.ts:10-11, 55-83`).
- Startup log sweep: `log.jsonl` over 10 MB **and** untouched 14+ days is deleted; chat history files are never touched (`cli/src/utils/chat-history.ts:144-183`).

### Project-local files (not the config dir)
- `<repo>/.freebuff/project-id` — UUIDv4 marker created only for git repos with no remote (`cli/src/utils/sponsored-project-identity.ts:21-22, 44-48`).
- `<repo>/.freebuff/worktrees/<runId>` (linked git worktree) and `<repo>/.freebuff/sponsored-runtime/<runId>` (`cli/src/utils/sponsored-worktree.ts:98-111`).

### Environment variables — build-time (baked by `bun build --define`, `cli/scripts/build-binary.ts:165-196`)
| Name | Effect | Cite |
|---|---|---|
| `NODE_ENV` | forced `"production"` | `:166` |
| `CODEBUFF_IS_BINARY` | `"true"` | `:167` |
| `CODEBUFF_CLI_VERSION` | the build's version argument → what `--version` prints | `:168`; `cli/src/cli-args.ts:23-39` |
| `CODEBUFF_CLI_TARGET` | platform-arch label of the build | `:169` |
| `FREEBUFF_MODE` | product selector → `IS_FREEBUFF = getCliEnv().FREEBUFF_MODE === 'true'` | `:170`; `cli/src/utils/constants.ts:9` |
| every `NEXT_PUBLIC_*` in the build env | inlined via `--define` **and** re-copied with `--env "NEXT_PUBLIC_*"` | `:161-163, 171, 195` |

Because these are inlined, `IS_FREEBUFF` and `NEXT_PUBLIC_CB_ENVIRONMENT` cannot change at runtime in a compiled binary (the bundler drops the losing branch); `freebuff/cli/build.ts:31-42` sets `FREEBUFF_MODE=true` around `build-binary.ts freebuff <version>`. The binary deliberately does not read project files at startup (`--no-compile-autoload-bunfig`, `--no-compile-autoload-dotenv`, `cli/scripts/build-binary.ts:179-187`; pinned `freebuff/e2e/tests/version.e2e.test.ts:28-49`).

### Environment variables — runtime
| Name | Effect | Cite |
|---|---|---|
| `FREEBUFF_CONFIG_DIR` | absolute config/state root override (transcripts, credentials, settings) | `cli/src/utils/config-dir.ts:17-25`; `cli/src/types/env.ts:91-92` |
| `FREEBUFF_MODE` | read at runtime only in unbundled/dev runs; inert in a compiled binary | `cli/src/utils/env.ts:87` vs `cli/scripts/build-binary.ts:170` |
| `FREEBUFF_BINARY_TARGET` | launcher: force a target key | `cli/release-core/launcher.js:382-397` |
| `FREEBUFF_BINARY` | e2e only: binary under test | `freebuff/e2e/utils/binary-helpers.ts:8-13` |
| `FREEBUFF_SMOKE_API_KEY` | live prod smoke key (falls back to `CODEBUFF_API_KEY`) | `freebuff/e2e/tests/live-turn.e2e.test.ts:26-28` |
| `FREEBUFF_GOD_QUOTA_EXEMPT`, `FREEBUFF_SHIP_LOGS`, `FREEBUFF_ADS_SLACK_WEBHOOK_URL` | test-fixture/server-side knobs (the fixture deletes the webhook) | `sdk/test/setup-env.ts:44-49, 75`; `docs/testing.md:17-22` |
| `CODEBUFF_API_KEY` | bearer-token fallback after `credentials.json` (`source:'environment'`) | `cli/src/utils/auth.ts:126-150` |
| `CODEBUFF_APP_URL` / `NEXT_PUBLIC_CODEBUFF_APP_URL` | runtime API-base override for every bearer-bearing SDK call; https anywhere, http only on loopback/`*.localhost`; `NEXT_PUBLIC_` wins precedence | `common/src/util/runtime-app-url.ts:17-46` |
| `CODEBUFF_TRUSTED_AGENT_PUBLISHERS` | comma-separated registry publishers allowed to run executable `handleSteps` | `cli/src/types/env.ts:75-77`; `docs/agents-and-tools.md:9-26` |
| `CODEBUFF_TRUST_AGENT_DIRS` | truthy → load repo `.agents`+`mcp.json` without the trust prompt (per run, writes nothing) | `cli/src/index.tsx:283-288`; `docs/agents-and-tools.md:52-55` |
| `CODEBUFF_TRACE` | `1`/`true`/`yes` → write `trace.jsonl` in prod builds (always on in dev) | `cli/src/utils/trace-writer.ts:26-31` |
| `CODEBUFF_SHIP_LOGS` | `'true'`/`'false'`; default on outside dev/test/CI | `cli/src/utils/log-shipper.ts:80-84` |
| `CODEBUFF_NO_TERMINAL_WATCHDOG` | truthy → disable the PowerShell terminal-reset watchdog | `cli/src/utils/terminal-watchdog.ts:237-239` |
| `CODEBUFF_SCROLL_MULTIPLIER` | float scroll multiplier | `cli/src/utils/chat-scroll-accel.ts:35-39` |
| `CODEBUFF_PERF_TEST`, `CODEBUFF_RG_PATH`, `CODEBUFF_WASM_DIR`, `CODEBUFF_CLI_EDITOR`/`CODEBUFF_EDITOR`, `OPEN_TUI_THEME`/`OPENTUI_THEME` | perf flag, ripgrep path, wasm dir, editor/theme prefs | `cli/src/types/env.ts:60-89` |
| `CODEBUFF_LAUNCHER_PID` | set by the launcher to its own pid on spawn | `cli/release-core/launcher.js:1529-1533` |
| `CODEBUFF_TERMINAL_COMMAND_BROKER` + `CODEBUFF_TERMINAL_COMMAND_BROKER_PROTOCOL` | internal broker marker + one-shot protocol file path (§13) | `cli/src/utils/terminal-command-broker.ts:18-20` |
| `CODEBUFF_GITHUB_ACTIONS` | `'true'` → `IS_CI` | `common/src/env.ts:20` |
| `CODEBUFF_POSTHOG_API_KEY` / `CODEBUFF_POSTHOG_HOST` | launcher-only failure telemetry (fall back to the `NEXT_PUBLIC_*` pair) | `cli/release-core/launcher.js:284-297` |
| `CODEBUFF_BINARY_TARGET`, `CLI_BINARY_TARGET` | launcher target overrides, after `FREEBUFF_BINARY_TARGET` | `cli/release-core/launcher.js:382-397` |
| `HTTP_PROXY`/`http_proxy`/`HTTPS_PROXY`/`https_proxy`, `NO_PROXY`/`no_proxy` | launcher download proxy: CONNECT tunnel for https, `Proxy-Authorization: Basic` when the URL carries credentials, `NO_PROXY` domain list | `cli/release-core/http.js:86-247` |

The upstream build reuses the `CODEBUFF_*` names for these shared knobs — there is no parallel `FREEBUFF_*` set. Terminal/IDE/OS detection (read, not configuration): `SHELL, COMSPEC, HOME, USERPROFILE, APPDATA, XDG_CONFIG_HOME, TERM, TERM_PROGRAM, TERM_BACKGROUND, TERMINAL_EMULATOR, COLORFGBG, NODE_ENV, NODE_PATH, PATH` (`common/src/env-process.ts:20-35`) plus `TMUX, STY, SSH_CLIENT, SSH_TTY, SSH_CONNECTION, CODESPACES, DISPLAY, WAYLAND_DISPLAY, KITTY_WINDOW_ID, SIXEL_SUPPORT, COLORTERM, ZED_*, VSCODE_*, CURSOR*, JETBRAINS_REMOTE_RUN, IDEA_INITIAL_DIRECTORY, IDE_CONFIG_DIR, JB_IDE_CONFIG_DIR, VISUAL, EDITOR, SystemRoot` (`cli/src/utils/env.ts:16-89`; typed `cli/src/types/env.ts:15-93`).

- `NEXT_PUBLIC_*` client schema is validated at import (invalid → throw). Required: `NEXT_PUBLIC_CB_ENVIRONMENT` (dev|test|prod), `NEXT_PUBLIC_CODEBUFF_APP_URL`, `NEXT_PUBLIC_SUPPORT_EMAIL`, `NEXT_PUBLIC_POSTHOG_API_KEY`, `NEXT_PUBLIC_POSTHOG_HOST_URL`, `NEXT_PUBLIC_STRIPE_PUBLISHABLE_KEY`, `NEXT_PUBLIC_STRIPE_CUSTOMER_PORTAL`, `NEXT_PUBLIC_WEB_PORT` (≥1000). Optional: `NEXT_PUBLIC_FREEBUFF_APP_URL`, pixel/site-verification/turnstile/recaptcha/humanbehavior keys (`common/src/env-schema.ts:5-73`; validation `common/src/env.ts:3-9`; derived `IS_DEV/IS_TEST/IS_PROD/IS_CI` `:17-20`).
- Steering-var protection: a repo `.envrc` import drops and reports `CODEBUFF_*`, `FREEBUFF_*`, `NEXT_PUBLIC_*`, `OVERRIDE_{TARGET,PLATFORM,ARCH}`, `NODE_OPTIONS`, `NODE_EXTRA_CA_CERTS`, `NODE_TLS_REJECT_UNAUTHORIZED`, `SSL_CERT_{FILE,DIR}`, `BUN_(CONFIG|OPTIONS|INSTALL)`, `LD_(PRELOAD|LIBRARY_PATH)`, `DYLD_*` (`cli/src/init/init-direnv.ts:102-141`).
- Build/CI-only: `VERBOSE`, `OVERRIDE_TARGET`, `OVERRIDE_PLATFORM`, `OVERRIDE_ARCH`, `BUN_COMPILE_EXECUTABLE_PATH` (`common/src/env-process.ts:86-91`; `cli/scripts/build-binary.ts:27-32`).

## 13. Launcher, install & self-update

The npm `freebuff` package is a thin Node wrapper (`freebuff/cli/release/index.js`) that resolves, downloads, verifies and atomically installs a platform binary, then spawns it as a child; all of that plus self-update lives in `cli/release-core/launcher.js` (copied into the package by `prepack`, `cli/release-core/prepare-package.js:10-27`). In this section, a bare `:N` line cite means that file; other files are named in full.

### Wrapper flow
- Entry: `bin.freebuff → index.js`, `createLauncher({packageName:'freebuff', displayName:'Freebuff', wrapperVersion, binaryChecksums, telemetryEvent:'cli.update_freebuff_failed'})`; no install-time lifecycle scripts (`freebuff/cli/release/index.js:26-32`; `README.md:8-11`).
- `main()`: optional startup banner (none in the upstream build) → `ensureBinaryReady()` → `spawnInstalledBinary()` → `attachExitHandler()` → `setTimeout(checkForUpdates, 100)` (`:1701-1715`).
- `ensureBinaryReady` is ready when `freebuff-metadata.json` exists, the binary exists, and the target is still allowed (`:636-671`); a wrapper newer than the installed binary repairs synchronously with no registry lookup (`:1147-1156`), otherwise it fetches `https://registry.npmjs.org/freebuff/latest` (`:550-583`). A download failure with a cached binary continues on the cache; with none, exit 1 (`:1186-1201`).
- Target key `"${process.platform}-${process.arch}"`, overridden by `FREEBUFF_BINARY_TARGET` > `CODEBUFF_BINARY_TARGET` > `CLI_BINARY_TARGET` (must be a known key): `linux-x64`, `linux-x64-baseline`, `linux-arm64`, `darwin-x64`, `darwin-arm64`, `win32-x64`, `win32-x64-baseline` (`:25-33, 378-397, 516-541`).
- Artifact URL: `GET ${resolveDownloadOrigin(NEXT_PUBLIC_CODEBUFF_APP_URL)}/api/releases/download/<version>/freebuff-<target>.tar.gz` (`:1014-1018`). The origin override applies only over `https:`, or `http:` on localhost/loopback; anything else is warned and ignored (`:37-75`). A 302 may land on the GitHub release asset (`README.md:15-16`), and redirects may only hit the original host, `codebuff.com`, `freebuff.com`, `github.com` (+`www.`), `*.githubusercontent.com`, never downgrading https→http (`cli/release-core/http.js:15-72`).
- Checksum **fails closed**: expected sha256 comes from the registry document already in hand, else the wrapper's `package.json` `binaryChecksums` when `version === wrapperVersion`, else an npm lookup of that exact version; missing/malformed → `ECHECKSUM`, `retryable:false`, install refused *before* the download (`:585-625, 995-1012`).
- Fetch into `<configDir>/.freebuff-<version>-<target>.tar.gz.part` (resume, 3 attempts, 120 s timeout, progress bar), sha256-verify, then gunzip+tar into `<configDir>/.freebuff-download-temp` accepting only root-level `freebuff`/`freebuff.exe` and `tree-sitter.wasm` (`:751-975, 841-846`).
- Atomic install with rollback: `chmod 0o755` (non-Windows), rename-with-backup binary → `<configDir>/freebuff[.exe]`, `tree-sitter.wasm` as a sibling, metadata `freebuff-metadata.json` `{version,target}`; any failure rolls the whole set back (`:1028-1136`).
- Spawn: `spawn(binaryPath, process.argv.slice(2))`, stdio inherited, env adds `CODEBUFF_LAUNCHER_PID` = launcher pid; a synchronous spawn throw prints diagnostics and exits 1 (`:1499-1543`).
- Background self-update 100 ms in, only while the child is alive: fetch npm `latest`; if newer, stage quietly, `SIGTERM` the child (`SIGKILL` after 5 s), install, print `Update available: x → y`, respawn; any failure keeps the current binary (`:1241-1312`).
- Exit handling resets terminal modes (raw mode, alt screen, mouse/focus/bracketed-paste/kitty flags, cursor) (`:152-190`) and diagnoses SIGILL / `0xC000001D`, SIGSEGV / `0xC0000005`, SIGBUS, SIGABRT / `0xC0000409`, printing platform/Node/AVX2/target/binary path plus the last 8 KiB of sanitized stderr (`:1338-1455`). Failures report PostHog `cli.update_freebuff_failed` with `distinct_id: anonymous-<homeDir>` plus stage/errorCode/target/attempts/bytes (`:284-346`).

### Windows crash tee
- Only Windows pipes stderr (`['inherit','inherit','pipe']`); elsewhere stderr stays a real tty (`:1522-1526`).
- The tee exists because the crash path resets the terminal with `\x1b[?1049l`, discarding alt-screen contents that may hold Bun's panic. Bytes are passed straight through to the wrapper's stderr, and the last `STDERR_TAIL_BYTES = 8192` are retained (`:1549-1583`).
- `drained()` waits for `close`/`end` with a 250 ms bound before reporting, since `exit` can beat the last bytes through the pipe (`:1584-1596`).

### AVX2 / baseline fallback
- Baseline targets exist only for `linux-x64` and `win32-x64` (`BASELINE_FALLBACK_TARGETS`, `:357-360`).
- Linux reads `/proc/cpuinfo` up front and treats an unreadable file as AVX2-present (`linuxCpuHasAvx2`, `:399-405`). Windows **assumes** AVX2 (the older PowerShell/C# probe tripped Defender) and self-corrects after one crash (`:416-447`); `cpu-features.json` `{avx2:false}` makes the correction permanent (`:452-483`).
- A recorded crash outranks inference (`:421-424`), and only a *confirmed* illegal instruction is written down — before the download — so the optimistic default costs at most one crash while a suspected crash records nothing (`:1625-1631`).
- Fallback triggers on confirmed SIGILL or a Windows startup abort (<10 s) with no explicit override and not already baseline: download the baseline for the same version, respawn once (`:1600-1668`).
- Bun's startup panic on a missing feature surfaces as `0xC0000409` (STATUS_STACK_BUFFER_OVERRUN), so that code counts as an illegal-instruction signal only for deaths during startup (`:192-198, 236-243`).
- `describeAvx2Support` reports `no (recorded crash)` / `yes|no` (Linux) / `not checked (assumed present)` (`:1439-1443`).

### Binary-side natives
- `tree-sitter.wasm` is installed as a sibling of the binary and copied next to it at build time because Bun asset embedding proved unreliable on Windows (`cli/scripts/build-binary.ts:123-224`).
- ripgrep self-extracts: in compiled mode (`CODEBUFF_IS_BINARY`) the CLI writes the embedded `rg`/`rg.exe` into `dirname(process.execPath)`, skipping the copy if the file already exists, `chmod +x` on Unix, falling back to the SDK's bundled ripgrep on failure (`cli/src/native/ripgrep.ts:9-72`). The path is cached in a promise and exported to the SDK as `CODEBUFF_RG_PATH` (`cli/src/utils/codebuff-client.ts:71-81`).

### Windows terminal-command broker
- The SDK owns buffering/timeouts/cancellation while the host supplies `terminalCommandBroker`; the CLI starts a detached helper via `process.execPath` with `--terminal-command-broker` and `CODEBUFF_TERMINAL_COMMAND_BROKER=1` (`cli/src/utils/terminal-command-broker.ts:17-20, 339-353`; `docs/agents-and-tools.md`).
- Request: spawn request JSON over stdin, ≤4 MiB, validated as `{executable,args,cwd,env}` before spawning (`cli/src/utils/terminal-command-broker.ts:106-121, 218-235`).
- Response: the helper writes one JSON line to `os.tmpdir()/freebuff-terminal-command-broker-<pid>-<uuid>.json` created `flag:'wx'` mode `0o600` ≤64 KiB; the parent reads it after `close`, so no extra stdio fds are needed (`cli/src/utils/terminal-command-broker.ts:141-169, 379-450`).
- The helper spawns with `stdio:['ignore','inherit','inherit']`, `windowsHide: true`, relays the child's exit code (or an error), then reaps its own process group — `taskkill.exe /pid <pid> /t /f` on Windows, `kill(-pid, SIGKILL)` elsewhere (`cli/src/utils/terminal-command-broker.ts:199-216, 238-280`).
- Failures: stage `spawn|stdio|completion`, codes `failed_to_connect|enoent|eacces|eperm|epipe|invalid_response|protocol_missing|response_too_large|unknown`, reported as `TERMINAL_BROKER_SPAWN_FAILED`; every failure message appends `Restart Freebuff and try again.` and there is **no** direct-console fallback (`cli/src/utils/terminal-command-broker.ts:27-42, 84-91, 364-373`).
- Extra fds 3/4 are avoided deliberately: on Windows Bun opens each pipe via `node:net` and can reject the handshake outside ChildProcess's error event, crashing the CLI (`cli/src/utils/terminal-command-broker.ts:388-396`).

### Release & build plumbing
- Build: `bun freebuff/cli/build.ts <version>` sets `FREEBUFF_MODE=true` and shells out to `bun cli/scripts/build-binary.ts freebuff <version>` (`freebuff/cli/build.ts:23-42`). `build-binary.ts` runs `scripts/prebuild-agents.ts` → `sdk` build → OpenTUI native bundle → `bun build src/entry.ts --compile --production --target=<bun target> --outfile=cli/bin/freebuff --sourcemap=none --define … --env "NEXT_PUBLIC_*"` → copies `tree-sitter.wasm` → `chmod 0755` (`cli/scripts/build-binary.ts:123-224`).
- Version source of truth is the published wrapper version `freebuff/cli/release/package.json:3`, passed to the build so `CODEBUFF_CLI_VERSION` matches (`freebuff/SPEC.md:244`); runtime resolution is baked `CODEBUFF_CLI_VERSION` → `../package.json` → `'dev'` (`cli/src/cli-args.ts:23-39`). Self-update compares npm `latest` against `freebuff-metadata.json`.
- Release: `bun freebuff/cli/release.ts [patch|minor|major] [--ref <sha>]` dispatches `freebuff-release.yml` in the upstream vendor repository (`freebuff/cli/release.ts:73-84`); it requires `CODEBUFF_GITHUB_TOKEN`.
- Checksum stamping: `write-binary-checksums.js write --binary-name freebuff --binaries-dir binaries --package-dir freebuff/cli/release` hashes `<name>-<target>/<name>-<target>.tar.gz` into `package.json.binaryChecksums`; `verify` fails the publish if any of the 7 targets is missing, not sha256, or unknown (`cli/release-core/write-binary-checksums.js:5-25, 82-151`). This checkout carries no `binaryChecksums` — it is stamped in the publish job.
- Public-clone CI: `.github/workflows/ci.yml:35-54` builds `bun freebuff/cli/build.ts 0.0.0-ci` on Ubuntu with placeholder `NEXT_PUBLIC_*`, then runs `cli/bin/freebuff --version` and `bun cli/scripts/smoke-binary.ts cli/bin/freebuff`. `freebuff-release.yml`, `freebuff-e2e.yml` and `prod-smoke.yml` are referenced but absent here (`freebuff/e2e/README.md:84-85, 137-161`).
- e2e/smoke expectations: `--version` prints semver, exits 0, and is unaffected by a project `bunfig.toml` preload (`version.e2e.test.ts:10-49`); `--help` contains `freebuff` and not `codebuff` (`help-command.e2e.test.ts:8-31`), and the smoke variant asserts `Usage: freebuff`, `Free AI coding assistant`, and the absence of `--free/--max/--plan/--lite` (`freebuff/cli/smoke-test.test.ts:123-150`); startup renders a boot marker and none of `Fatal error during startup`, `Internal error: tree-sitter.wasm not found`, `FATAL`, `panic`, `Segmentation fault` (`startup.e2e.test.ts:17-55`). `login` must enter `Freebuff Login` / `Generating login URL` (`smoke-test.test.ts:152-169`); the e2e binary path is `FREEBUFF_BINARY` or `cli/bin/freebuff` (`freebuff/e2e/utils/binary-helpers.ts:8-13`).

## 14. Version delta 0.0.178 → 0.0.180

43 sync commits spanning `e2b911eca` (npm `freebuff@0.0.178`, the pin `docs/CLI-Limitations.md` was written against) to `2b165f749` (npm `freebuff@0.0.180`); the delta is UI copy, registry rows and internal ad machinery — no chat wire code moved.

### 14.1 Provenance

| item | value |
|---|---|
| from | `e2b911eca` = npm `freebuff@0.0.178` |
| to | `2b165f749` = npm `freebuff@0.0.180` (vendor clone HEAD, `2026-09-19 17:58:17 +0000`) |
| commits | 43 sync commits |
| scale | 60 files, ≈ +5,641 / −1,424 |
| version file | `freebuff/cli/release/package.json:3` `0.0.177` → `0.0.180` |

- **Version-file-lags-npm caveat:** at `e2b911eca` the release file still read `0.0.177`, one behind its own npm tag `0.0.178`. Never derive the vendor revision from `freebuff/cli/release/package.json` — use the npm tag/git SHA; and since `2b165f749` the npm tag no longer distinguishes revisions at all (it reads `0.0.180` at both the pin and the clone tip), so past that point the git SHA is the only identifier.
- The proxy pin file `scripts/vendor-version.txt` reads `0.0.180` and `snapshots.json:2-3` records the same (`upstream_sha 2b165f749…`, `vendor_version 0.0.180`); the local gitignored upstream vendor clone has since moved to `8ed5d3e5e`, so this audit describes a tree 15 commits behind the checkout (§14.6).

### 14.2 File inventory (condensed by class)

Class key: **B** CLI behavior · **W** wire/registry · **A** ads/sponsored · **T** test-only · **C** build/release/comment.

| class | files (+/−) |
|---|---|
| **B** | `cli/src/utils/freebuff-session-api.ts` 50/4 · `cli/src/components/freebuff-landing-screen.tsx` 30/5 · `cli/src/components/freebuff-model-selector.tsx` 25/23 · `cli/src/index.tsx` 11/1 · `cli/src/utils/freebucks.ts` 0/4 · `common/src/util/freebuff-streak-line.ts` 5/1 · `common/src/util/freebuff-first-tab-discount.ts` 1/1 · `packages/agent-runtime/src/compact-history.ts` 54/0 · `sdk/src/compact-run-state.ts` 89/0 (new) · `sdk/src/index.ts` 2/0 |
| **W** | `common/src/constants/freebuff-models.ts` 129/7 · `common/src/constants/free-agents.ts` 40/7 · `common/src/constants/freebuff-cost-mode.ts` 97/0 (new) · `common/src/types/freebuff-session.ts` 21/2 · `common/src/util/runtime-app-url.ts` 9/0 |
| **B/C** | `cli/scripts/build-binary.ts` 8/0 |
| **A** | new: `common/src/ads/sponsored-verification.ts` 985 · `common/src/ads/sponsored-acceptance-criteria.ts` 843 · `common/src/ads/supabase-format-cpc-experiment.ts` 254 · `common/src/util/paid-social-conversions.ts` 142 · `common/src/ads/sponsored-verifier.ts` 118 · `common/src/ads/sponsored-verification-reporting.ts` 57 · `common/src/util/ad-provider-policy.ts` 6. Modified: `common/src/util/ad-experiment.ts` ≈7/99 · `common/src/ads/sponsored-proposal-view.ts` 83/1 · `common/src/ads/supabase-setup-invitation.ts` 34/1 · `common/src/ads/sponsored-proposal-conformance.ts` 3/0 · `common/src/util/axiom-only-log.ts` 21/0 · `common/src/env-schema.ts` 13/0 · `common/src/util/disposable-email.ts` (comments). Deleted: `common/src/util/imprezia-client.ts` −469 |
| **T** | 19 test files (≈7 new): `sponsored-verification.test.ts` 477, `paid-social-capi.test.ts` 386, `sponsored-acceptance-criteria.test.ts` 253, `supabase-format-cpc-experiment.test.ts` 187, `compact-run-state.test.ts` 154, `compact-history-now.test.ts` 134, `freebuff-cost-mode.test.ts` 76; 2 imprezia tests deleted (`imprezia-client-outcome`, `imprezia-sandbox-opt-in`) |
| **C** | `bun.lock` 64/56 · `freebuff/cli/release/package.json` 1/1 · `sdk/src/env.ts` 1/5 · `common/src/constants/freebuff-subscriptions.ts` 3/5 (comment) · `common/src/constants/spend-providers.ts` 2/2 (comment) · `cli/scripts/smoke-binary.ts` 216/88 (T/C) |

### 14.3 Behavior / wire notes

- **`cli/src/utils/freebuff-session-api.ts` — B, UI-only.** `sessionEndpoint` rebuilt on a new `sessionBaseUrl()` (`:98-107`); three new exports: `freebuffSessionHost()` `:110-116`, `isFreebuffSessionNetworkError()` `:123-129`, `freebuffSessionUnreachableMessage(host)` `:143-149`. Header rationale: four PLDT/PH users read the runtime's `The operation timed out.` as a broken install. **Request wire untouched** — same `FREEBUFF_SESSION_ADMISSION_PATH`, headers, POST-retry set (`classifyFreebuffSessionRequestFailure` `:48-73`, still 408/429/503 only) and classify behavior.
- **`cli/src/components/freebuff-landing-screen.tsx` — B.** New `getLandingFailureMessage()` (`:114-126`) replaces `failure.message` in the ⚠ line (`:719`): network-shaped failures now read `Couldn't get a response from <host>…` plus ` Retrying automatically.` when retrying. For a metered account `belowPickerNotices` is now empty (`:477-480`), so `FREEBUCKS_PICKER_NOTICE` is no longer rendered (tier-change/limited-mode notices only).
- **`cli/src/components/freebuff-model-selector.tsx` — B.** `RowDetail` drops `struck`, adds `highlight` (`:141-146`); a first-tab-discounted row shows **only the charged price**, accent + bold (`:462-463`, `:1445-1454`), instead of `~~15~~ 5 Freebucks/hr` — many terminals drop the strikethrough attribute, so `10 0 Freebucks/hr` read as two prices. (`common/src/util/freebuff-first-tab-discount.ts:85` still says "the crossed-out price is the regular one" — copy left behind by this change.)
- **`cli/src/index.tsx` — B.** Hidden early-exit probe before `commander.parse()`: `--smoke-api-url` prints `api-url smoke: <getWebsiteUrl()>` and exits 0 (`:170-178`); consumed by `cli/scripts/smoke-binary.ts` (`API_URL_MARKER`). New `getWebsiteUrl` import from `@codebuff/sdk` (`:16`).
- **`cli/scripts/build-binary.ts` — B/C.** Adds Bun compile flag `--no-compile-autoload-dotenv` (`:187`): a project `.env`/`.env.local` in the CWD can no longer steer `NEXT_PUBLIC_CODEBUFF_APP_URL` / `CODEBUFF_APP_URL` (and therefore every bearer-authenticated SDK call); a shell-exported var is still honoured.
- **`cli/src/utils/freebucks.ts` — B.** Removes the unused `FREEBUCKS_RESET_POLICY_COPY` import and the exported `FREEBUCKS_PICKER_NOTICE` const (the paragraph under a metered picker). No matches remain in the file.
- **`common/src/util/freebuff-streak-line.ts` — B (copy).** `+N Freebucks every day` → `+N Freebucks every Pacific day` (`:78`); streak days are Pacific while the daily pool resets on the user's clock.
- **`common/src/util/freebuff-first-tab-discount.ts` — B (copy).** `shared across Desktop and CLI` → `shared across Web, Desktop and CLI` (`:85`).
- **`common/src/constants/freebuff-models.ts` — W.** `FREEBUFF_GLM_V53_FLASH_MAX_PRICE` re-priced `$0.10/$0.30` → `$0.14/$0.45` (`:258-261`; Z.ai/Novita/GMICloud all moved above the old ceiling). New wire ids: `deepseek/deepseek-v4.1-flash` (`:486-487`), `deepseek/deepseek-v4.1-pro` (`:488`), `z-ai/glm-5.3` (`:489`), plus staff-only `anthropic/claude-fable-5.1-test` (`:501-502`) and `openai/gpt-6-astra-discount-test` (`:503-504`). `FREEBUFF_PROVISIONED_MODELS` grows to 6 rows (`:1539-1546`, new rows `:1543-1545`); new `FREEBUFF_INTERNAL_EVAL_MODELS` (`:1579-1582`). **No additions to `FREEBUFF_MODELS` / picker catalogs** — the three new model options are referenced only by the provisioned/eval lists. Tier logic: `isFreebuffSessionModelAllowedForAccessTier` admits a campaign (`FREEBUFF_LIMITED_OFFER_MODEL_IDS`) model on **either** tier (`:3361-3363`); `resolveFreebuffModelForAccessTier` hoists the limited-offer lookup above the limited-tier coercion (`:3466-3469`), so an explicit Fable pick survives at limited tier.
- **`common/src/constants/free-agents.ts` — W.** Five new roots in `FREEBUFF_ROOT_AGENT_IDS`: `base2-free-deepseek-v4-1-flash`, `-deepseek-v4-1-pro`, `-glm-5-3`, `-astra-discount-test`, `-fable-test` (`:355-359`); model→root entries for the 3 provisioned + 2 eval ids (`:439-446`); one-model pins in `FREE_MODE_AGENT_MODELS` (`:630-638`).
- **`common/src/constants/freebuff-cost-mode.ts` — W (new).** New server error code `FREEBUFF_COST_MODE_ESCALATION_ERROR = 'free_mode_cost_mode_required'` (`:63-64`) with operator copy (`:65-66`), plus `isFreebuffOnlyAgentId()` (`:13`), `isFreebuffCostModeEscalation()` (`:56`) and `parseExemptUserIds()` for `FREEBUFF_BAN_EXEMPT_USER_IDS` (`:87-97`). Closes a bypass: an upstream agent id sent with `codebuff_metadata.cost_mode != 'free'` turned off ~20 free-mode gates. The predicate alone is **not** abuse — the paid-Luna path (`codebuff/base2-free-luna` + `cost_mode: normal`) stays 200. No consumer exists in the public snapshot.
- **`common/src/types/freebuff-session.ts` — W.** `FreebuffSubscriptionTierOffer.yearlyPrepaidPurchasable?` (`:104`); `FreebuffSubscriptionInfo.source` widened to `'stripe' | 'grant' | 'prepaid'` (`:369`), prepaid ⇒ `cancelAtPeriodEnd: true` with `renewsAt` = paid-through, and `prepaidRenewableAt?: string` (`:374`). Gate-code/status literals (the `wirecodes_gen` extraction source) are **unchanged**.
- **`common/src/util/runtime-app-url.ts` — B/W.** New shared `RUNTIME_APP_URL_ENV_VARS = ['NEXT_PUBLIC_CODEBUFF_APP_URL','CODEBUFF_APP_URL']` (`:21-24`); `sdk/src/env.ts` imports it instead of a local copy (`sdk/src/env.ts:12-15,89`), and `smoke-binary.ts` uses it to strip the vars for the isolation probe.
- **`packages/agent-runtime/src/compact-history.ts` — B (SDK).** New `compactHistoryNow()` (`:1196`): the mechanical pass of `maybeCompactHistory` with the trigger decision removed; returns `null` when a pass would not shrink history, throws the runtime's user-presentable sentence when the live request alone is over budget, emits `context_compaction_completed` telemetry with `trigger_reason: 'manual'` (`:1219`).
- **`sdk/src/compact-run-state.ts` (new) + `sdk/src/index.ts` — B (SDK API).** `compactRunState({runState, maxContextLength, logger})` (`:50`) compacts a **persisted** `RunState`: clones session state, rewrites `messageHistory`, recomputes `contextTokenCount` = history + checkpointed systemPrompt/toolDefinitions, returns `{runState, previousTokens, nextTokens} | null`, never mutates input. Re-exported from `sdk/src/index.ts:39-43` (`:39-40` at this pin; the tip's `truncateRunStateAtUserTurn` addition grew the block, §14.6).
- **Comment-only / build rows:** `common/src/constants/freebuff-subscriptions.ts` and `spend-providers.ts` are comment edits; `sdk/src/env.ts` is the import refactor above; `bun.lock`/`release/package.json` are build plumbing.

### 14.4 Ads / sponsored

- **Added:** a sponsored verification pipeline (`sponsored-verification.ts`, `sponsored-verifier.ts`, `sponsored-verification-reporting.ts`, `sponsored-acceptance-criteria.ts`), a CPC format experiment (`supabase-format-cpc-experiment.ts`), and paid-social conversion reporting (`paid-social-capi.ts`, `util/paid-social-conversions.ts`, `util/ad-provider-policy.ts`); `env-schema.ts:14-16` adds optional `NEXT_PUBLIC_META_PIXEL_ID` / `NEXT_PUBLIC_X_PIXEL_ID` (absent ⇒ first-party capture off). `util/ad-experiment.ts` keeps the routing salts and defaults (all default-off) while the Imprezia experiment client was deleted (−469) with its two test files.
- **Chat wire:** untouched. No ad/sponsored file is in the request path of `v1/chat/completions`, `v1/responses` or `v1/messages`; ad *gates* already seen by the proxy (`waiting_room_required`, `classify.go` in-pin matrix) are unchanged here.
- **Overall:** *no chat-completions / responses / messages wire code changed anywhere in this batch* — `error-handling.ts`, `send-message.ts`, `use-freebuff-session.ts`, `freebuff-errors.ts`, `polling-backoff.ts` and the `freebuff-session.ts` gate codes are all untouched.

### 14.5 Effect on `docs/CLI-Limitations.md`

Invalidates:
- The vendor-pin line in `docs/CLI-Limitations.md` (`e2b911eca`, recorded there as "live npm 0.0.178, zero drift") — rewritten: "zero drift" did not hold past this batch, and by §14.6 the local clone (`8ed5d3e5e`) is 15 commits past the recorded wiregen pin `2b165f749` while the npm wrapper reads `0.0.180` at both, so the SHA — not the tag — identifies the revision.
- Row **P2-1**'s picker-notice cite (`freebucks.ts:188`) — `FREEBUCKS_PICKER_NOTICE` no longer exists, so that half of the item is moot (the cosmetic-countdown verdict stands).
- Line-number cites only (verdicts hold): row 12's `freebuff-session-api.ts:117-133` is now the network-error helper block — the `compact` / `firstTabDiscount` / `walletSpendLimit` call site moved to `:151-162`; W1's `freebuff-landing-screen.tsx:791-801` and `freebuff-model-selector.tsx` cites shift with the two edits. Row 14's `:48-73` range is unchanged.

Does not invalidate:
- **P0-1, P0-2** — `cli/src/utils/error-handling.ts` and `common/src/constants/freebuff-errors.ts` were not touched in this batch; both gaps stand.
- **P1-1…P1-5, W1…W9** — verdicts unchanged. P1-5's `sponsored-run.ts`/`exit-cleanly.ts` are untouched; W1 stays WONT (the changed files are presentation); W9 stays WONT although `build-binary.ts:187` now adds `--no-compile-autoload-dotenv`, worth one footnote since it makes the CLI env isolation explicit rather than implicit.
- Rows 13/22/23 — the gate-code and status literals in `freebuff-session.ts` and `freebuff-models.ts` are unchanged; the new wire ids/tiers need no new arm because no harness consumes them.
- Row 25 (strict gates) and the proxy-only rows — unaffected by anything in this batch.

Doc gaps to add (not invalidations): the new server error code `free_mode_cost_mode_required` (`common/src/constants/freebuff-cost-mode.ts:63-66`) appears in no limitation row, and the new prepaid subscription fields (`freebuff-session.ts:104,369,374`) are unmapped in the session-envelope rows.

### 14.6 Delta `0.0.180` → `8ed5d3e5e` (15 commits past the wiregen pin)

The citations corrected in this revision were verified against `8ed5d3e5e`, not
against the recorded pin. What changed between the pin and the tip:

| item | value |
|---|---|
| from | `2b165f749` — the recorded wiregen pin (`snapshots.json:2-3`), npm `freebuff@0.0.180`, `2026-09-19 17:58:17 +0000` |
| to | `8ed5d3e5e` — vendor clone tip, `2026-09-20 07:32:06 +0000` |
| commits | 15, every one `Sync public snapshot from freebuff-private` |
| scale | 16 files, ≈ +563 / −167 |
| version file | `freebuff/cli/release/package.json` reads `0.0.180` at **both** ends — the npm tag no longer identifies a revision, cite the SHA |
| pin / drift | `snapshots.json:30-31` pins `cli/src/components/freebuff-model-selector.tsx` at `5ecfb9ff…`; the tip hashes `7fc1341d…`. That file is wire-tracked (`scripts/check-upstream.sh:126`), so the recorded pin is **stale** — "zero drift" is false |

**File inventory** (`git diff --numstat 2b165f749..8ed5d3e5e`, class key as in §14.2):

| class | files (+/−) |
|---|---|
| **B** (picker) | `cli/src/components/freebuff-model-selector.tsx` 6/35 |
| **B** (pricing copy) | `common/src/util/freebuff-off-peak-price.ts` 2/12 |
| **B** (SDK) | `sdk/src/compact-run-state.ts` 72/0 · `sdk/src/index.ts` 4/1 |
| **A** (acquisition / CAPI) | `common/src/meta-capi.ts` 16/5 · `common/src/paid-social-capi.ts` 41/16 · `common/src/util/meta-conversions.ts` 23/0 · `common/src/util/paid-social-conversions.ts` 26/11 · `common/src/matching-hash.ts` 14/0 (new) · `common/src/util/acquisition-matching.ts` 23/0 (new) |
| **T** | `sdk/src/__tests__/truncate-run-state.test.ts` 121/0 (new) · `cli/src/components/__tests__/freebuff-model-selector.test.tsx` 78/12 · `common/src/__tests__/meta-capi.test.ts` 46/10 · `common/src/__tests__/paid-social-capi.test.ts` 37/8 · `common/src/util/__tests__/freebuff-off-peak-price.test.ts` 8/3 |
| **C** | `bun.lock` 46/54 |

**Picker chips removed (B).** `rowDetails` loses all three pricing chips — the off-peak detail copy, `'Limited-time first-tab discount'`, and the peak-pricing tooltip (with its `freebucksPeakCopy` import) — leaving the accent `highlight` on the price detail (`:453-457`) as the row's only first-tab signal. Dead exports left behind, each with no non-test CLI caller in the public snapshot:

- `freebucksOffPeakCopy` (`common/src/util/freebuff-off-peak-price.ts:11-44`) — also lost its `detail` field in the same commit, so it now returns `{active, badge, tooltip}` with the shortened tooltip.
- `freebucksPeakCopy` (`common/src/util/freebuff-peak-price.ts:44-64`) — a peaked row shows the surcharge only inside the quoted price and keeps the catalog tagline, so no surface explains the peak window any more.
- `firstTabDiscountCopy` (`common/src/util/freebuff-first-tab-discount.ts:79-87`) — its last CLI caller was the selector's ask-line fallback, which is now a literal `return undefined`.

`taglineFor` (`:351-358`) gained `FREEBUFF_DEEPSEEK_V4_FLASH_MODEL_ID`, so that row's server `priceNotices` tagline is suppressed in favour of the catalog tagline.

**Pricing / lookalike-CAPI (A), no CLI surface.** `common/src/matching-hash.ts:8-14` adds the one `hashMatchingEmail` normalizer (trim, lowercase, unsalted SHA-256) shared by Meta `em`, TikTok `email` and X `hashed_email`; `hashPaidSocialEmail` becomes an alias of it. `common/src/util/acquisition-matching.ts:8-12,20-23` adds `validHashedEmailHex` / `validMatchingIpAddress` shape checks. Meta bodies now carry `em` and `client_ip_address` and send `client_user_agent` on native events too; `meta-conversions.ts` adds `META_CLICK_COOKIE` / `validMetaClickId` / `metaClickCookieValue` (`fb.1.<ms>.<fbclid>`) so a click survives a blocked pixel. TikTok gains a per-`surface` activation page, `ttp`, hashed email and IP, with the registration/activation split now an explicit error.

**SDK additions (SDK-only).** `truncateRunStateAtUserTurn({runState, keepUserTurns})` (`sdk/src/compact-run-state.ts:127-161`) truncates a persisted `RunState` at a user-turn boundary, returning `null` when the boundary cannot be honestly placed, never mutating its input; re-exported at `sdk/src/index.ts:39-43`. **SDK-only** — no `cli/` caller exists.

**No chat-wire file changed in this delta.** `cli/src/utils/error-handling.ts`, `cli/src/hooks/helpers/send-message.ts`, `cli/src/hooks/use-freebuff-session.ts`, `common/src/constants/freebuff-errors.ts` and `cli/src/utils/polling-backoff.ts` are all untouched, so §10's limit/error matrix and the P0/P1 verdicts in `CLI-Limitations.md` stand unchanged by this batch.

**Docs corrected against this delta:** §8.3's row-detail order and line cites, §9.7's first-tab and off-peak lines, §11's peak/off-peak notes, §12's config-dir test path, and the pin blocks at the top of this document and of `CLI-Limitations.md`.

## 15. Proxy cross-reference

Where each CLI surface lands in this repo (gateway side), and which CLI facts
are load-bearing for wire parity.

| CLI area | Proxy implementation | Parity verdicts |
|---|---|---|
| Session wire: POST/GET/DELETE, model/instance/tz/first-tab headers, compact response | `backend/internal/upstream/session.go`, `session_parse.go`, `client*.go` | `CLI-Limitations.md` rows 12, 13, 19 |
| Error taxonomy (statuses/codes → envelopes) | `backend/internal/upstream/classify.go`, `backend/internal/server/errors.go` + `error_taxonomy.go` | `CLI-Limitations.md` rows 11, 13, 20-24 |
| Country/banned/ip_capped/waiting-room/spend gates | `classify.go` + `errors.go` (status passthrough) | rows 1-3, 13, 14, 23 |
| Freebucks meter (prices map, refund, reset) | `session_parse.go` freebucks block; wire `prices` is the only cost source | rows 9, 27 |
| Model registry rows (catalog, paused/retired, served ids) | `backend/internal/modelcat`, `backend/internal/registry`, pins in `backend/internal/wirefacts/testdata/wire/snapshots.json` | tracked by `scripts/check-upstream.sh` + drift PRs |
| Reasoning effort reporting | `convert_request.go` / chat path writes `codebuff_metadata.freebuff_reasoning_effort` | see §9 for the CLI-side ladders |
| Tool-name translation | proxy-side only (`backend/internal/convert/toolmap_request.go`); the CLI never sees foreign harness names | `docs/UNIVERSAL-CLIENTS.md` |
| Takeover / superseded | proxy surfaces `503 session_superseded` and never re-acquires in-request | row 5 (`GAP-P1`); §6 for the CLI side |
| Credential files (`~/.config/manicode/credentials.json`) | read path `backend/internal/cli/clicreds.go`; write path deliberately not ported | rows 16, 17, W3 |

Notes:

- The port audit (`CLI-Limitations.md`) is written against the `0.0.178`
  (`e2b911eca`) pin; §14 lists which of its audited files changed in `0.0.180`
  and §14.6 the 15 commits since, so the recorded pin no longer holds against
  the checkout (`8ed5d3e5e`, wrapper still `0.0.180`).
- Presentation surfaces (TUI screens, ads rendering, copy) are intentionally
  client-only — see the WONT rows in `CLI-Limitations.md`.
- When upstream moves: `bash scripts/check-upstream.sh` classifies wire vs
  registry vs npm drift; wire findings flow through
  `scripts/review-wire-drift.sh` and the drift PR process (`AGENTS.md` §4.6-7).
  Re-run this doc's sections only when the pinned version changes.
