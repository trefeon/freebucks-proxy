# Comprehensive Free-Tier Adaptation & Remake Plan

Detailed architectural specification and implementation roadmap for adapting the **freebucks-proxy** Go gateway backend and Svelte 5 frontend dashboard to upstream Freebuff free-tier wire protocol requirements.

---

## 1. Ground Truth & Protocol Constraints

From live empirical verification against upstream (`codebuff.com` / `freebuff.com`):

1. **Free-Tier Authentication Model**:
   - Free CLI tokens authenticate through the OAuth device code flow (`/api/auth/cli/code` + `/api/auth/cli/status`).
   - The device login URL must be opened verbatim (`https://freebuff.com/login?auth_code=...`) and must **never** be rewritten to `/onboard`.
   - The user profile endpoint `/api/v1/me` is not an authentication gate for free tokens. Free tokens may return HTTP 401 on `/api/v1/me` while returning HTTP 200 on `/api/v1/freebuff/session` and `/api/v1/freebuff/streak`.
   - `GET /api/v1/freebuff/session` is the single authoritative token health and entitlement probe.

2. **1-Hour Model Purchase Economics**:
   - Model admission (`POST /api/v1/freebuff/session/admission`) charges Freebucks once per 1-hour window (e.g. 5 Freebucks for GLM 5.3 Flash / Solar Mini 4, 15 for DeepSeek V4.1 Flash, 0 for Space Bunny Alpha).
   - When admitted, the server locks a 1-hour purchase seat (`desktopPurchases`) with `expiresAt` (3600 seconds duration).
   - Within this 1-hour window, subsequent chat turns or session re-admissions for the **same model** using the same `holderInstanceId` cost **0 additional Freebucks**.

3. **Concurrency (`slotLimit: 1`) Enforcement**:
   - In the limited access tier, concurrency is strictly 1 concurrent purchase seat.
   - Attempting to admit a *different* model while another model's 1-hour seat is active returns **HTTP 409 `purchase_capacity`** identifying the current holder instance ID and expiry time.
   - Calling `DELETE /api/v1/freebuff/session` ends the active session (`status: ended`), but upstream holds the model purchase lock until the 1-hour timestamp expires.

4. **Chat Completion Envelope Gate**:
   - Upstream enforces a fingerprint check on `POST /api/v1/chat/completions`. Omitting the official coding agent structure returns **HTTP 503 `The model is temporarily unavailable. Please try again later.`**
   - Requirements to pass:
     - Canonical base3 Buffy system prompt head (6 convention bullets + dynamic date).
     - Canonical 16 tools with full schemas (`read_files`, `str_replace`, `write_file`, `run_terminal_command`, `code_search`, `glob`, `list_directory`, `write_todos`, `web_search`, `read_url`, `ask_user`, `suggest_followups`, `gravity_index`, `render_ui`, `skill`, `report_project_profile`).
     - `codebuff_metadata` containing `run_id`, `trace_session_id`, `client_id` (13-char base36), `freebuff_instance_id`, `surface: "cli"`, `cost_mode: "free"`, `llm_step_number`.
     - `provider: {"data_collection": "deny"}` and `stream: true`.

5. **Model Catalog Segregation**:
   - Upstream advertises `planRequiredModelIds` in every session response: `openai/gpt-6-luna`, `mimo/mimo-v2.6-pro`, `google/gemini-3.8-flash`, `meta/muse-spark-1.3-contributor`.
   - Free accounts cannot use these models (rejected upstream with plan requirement error).

---

## 2. Backend Gateway Remake (`backend/internal/`)

### B1. Upstream Envelope Floor (`backend/internal/upstream/`)
- **`chat.go` (`injectEnvelope`)**:
  - Automatically guarantee the canonical base3 instructions head (`cliSystemMarkerBase3Instructions`) with dynamic `time.Now()` date format.
  - Apply `topUpCliTools` (`clitools.go`) using the canonical fixture (`testdata/cli-tools.json`) so the wire tools array always contains the 16 official declarations with full schemas.
  - Client-declared tools mapped via `convert` (`run_terminal_command <- bash`, `read_files <- read`, `str_replace <- edit`, `write_todos <- todo`) take precedence and win on collision.
  - Injected tools (such as `end_turn` and `decide`) must not duplicate or displace canonical tool declarations.

### B2. 1-Hour Seat Persistence & Concurrency Management (`backend/internal/session/` & `backend/internal/pool/`)
- **Seat Persistence (`session_manager.go` / `store.go`)**:
  - Persist `holderInstanceId`, `model`, and `expiresAt` in the SQLite database (`session_states` table).
  - On gateway boot or token reload, restore existing purchase seat metadata so restarts do not burn fresh Freebucks credits for an active hour.
- **`slotLimit: 1` Intelligent Model Routing (`acquire_route.go` / `pool.go`)**:
  - When an incoming client request matches the model currently holding the purchase seat, route directly using the persisted `holderInstanceId` (0-cost resumption).
  - When an incoming client request asks for a *different* model while `time.Now() < expiresAt`:
    - Avoid blind admission calls that trigger 409 errors.
    - Return a structured HTTP 409 error to the client with a clear message: `"Model seat currently locked to <model> until <expiresAt> (<minutes>m remaining). Re-select <model> or wait for expiration."`
- **Session Release (`DELETE`)**:
  - When an operator triggers session release via `/admin/tokens/{idx}/session`, invoke `DELETE /api/v1/freebuff/session` using the active instance ID.
  - If `freebucksRefundPending` is true, schedule bounded background retry until confirmed.

### B3. Token Health & Reachability Probing (`backend/internal/upstream/tokenhealth.go`)
- **Fix `CheckTokenHealth`**:
  - Eliminate the false-positive ban/invalid check: If `/api/v1/me` returns HTTP 401, do **not** mark `row.State = TokenInvalid`.
  - Rely on `probeSession` (`GET /api/v1/freebuff/session`) as the authoritative health determinant:
    - `session` returns 200 (`none`, `active`, `ended`, `queued`) -> `TokenOK`.
    - `session` returns 401 -> `TokenInvalid`.
    - `session` returns 403 `banned` / `account_suspended` -> `TokenBanned`.
    - `session` returns 403 `country_blocked` -> `TokenCountryBlocked`.
    - `session` returns 429 `spend_limited` / `rate_limited` -> `TokenRateLimited`.
- **Auto-Discovery Synchronization (`clicreds.go`)**:
  - Ensure `AUTO_DISCOVER_TOKEN` loads credentials from `<ConfigDir>/credentials.json`, capturing both `authToken` and `id` (`acting-user-id`).

### B4. Error Taxonomy & Classification (`backend/internal/server/error_taxonomy.go`)
- Map upstream errors to descriptive client error responses:
  - 409 `purchase_capacity` -> `"Free-tier concurrency limit reached (1 active model seat). Seat locked to <model> until <expiresAt>."`
  - 503 `model_unavailable` -> Check envelope integrity before retrying; surface hint if custom harness omitted required tool declarations.
  - 428 `waiting_room_required` -> Signal session manager to perform admission before replaying request.

---

## 3. Frontend Dashboard Remake (`frontend/src/`)

### F1. Freebucks Daily Quota & Reset Counter (`AllowancesPanel.svelte`, `FreebucksQuotaBar.svelte`)
- **Real-Time Freebucks Balance**:
  - Display exact balance from `freebucks.balance` (e.g. `20 / 25 Freebucks`).
  - Progress bar colored by remaining percentage: green (>50%), amber (15-50%), red (<15%).
- **Daily Spend & Local Reset**:
  - Show daily spent amount (e.g. `Spent Today: 5 FB`).
  - Calculate and display a real-time countdown timer to `resetAt`, localized to user's browser timezone.

### F2. Active Purchase Seat Card (`Overview.svelte`, `Tokens.svelte`)
- **Active 1-Hour Seat Status**:
  - Dedicated card displaying the currently active model seat.
  - Model badge (e.g. `GLM 5.3 Flash` or `DeepSeek V4.1 Flash`).
  - Circular countdown timer displaying remaining minutes/seconds of the 3600-second window.
  - Cost indicator: `"Hourly cost: 5 FB (0 FB for subsequent turns in this hour)"`.
- **Seat Management Actions**:
  - "Release Seat" button with confirmation modal that calls `DELETE /admin/tokens/{idx}/session`.

### F3. Model Catalog & Pricing Restrictions (`ModelsPanel.svelte`, `modelOptions.js`)
- **Subscription-Gated Model Flags**:
  - Retrieve `planRequiredModelIds` from backend API (`/v1/models` or `/admin/models`).
  - Badge paid models (`openai/gpt-6-luna`, `mimo/mimo-v2.6-pro`, `google/gemini-3.8-flash`, `meta/muse-spark-1.3-contributor`) with a `PAID SUBSCRIPTION REQUIRED` tag.
  - In model picker dropdowns, disable or visually distinguish subscription-only models.
- **Hourly Cost Tags**:
  - Display exact Freebucks price per model:
    - `Space Bunny Alpha`: `0 FB (FREE)`
    - `GLM 5.3 Flash`: `5 FB/hr`
    - `Solar Mini 4`: `5 FB/hr`
    - `Kimi K3 Eco`: `5 FB/hr`
    - `MiMo 2.5`: `10 FB/hr`
    - `Solar Pro 4`: `10 FB/hr`
    - `DeepSeek V4.1 Flash`: `15 FB/hr` (`10 FB/hr Off-Peak`)
- **Off-Peak Pricing Banners**:
  - Show active off-peak status for DeepSeek V4.1 Flash when current time is in the 22:00-06:00 UTC window.

### F4. Native Device-Code Login Wizard (`TokenTable.svelte`, `Login.svelte`)
- **Embedded Browser Auth**:
  - Add "Add Freebuff Account via Device Code" wizard.
  - Calls backend `/admin/tokens/device-login/start`, displaying verbatim link `https://freebuff.com/login?auth_code=...`.
  - One-click "Copy Login URL" button with external open helper.
  - Live animated spinner: `"Waiting for browser authorization (Attempt X/60)..."`
  - Automatically adds the fresh token and user ID to the pool upon authorization without server restarts.

---

## 4. Implementation Phasing & Milestones

```
┌──────────────────────────────────────────────────────────────┐
│ Phase 1: Gateway Core Invariants (Backend)                  │
│ - Canonical 16-tool floor fixture & full base3 prompt head   │
│ - Fix CheckTokenHealth (/api/v1/me 401 bypass)               │
│ - 1-hour purchase seat persistence & 0-cost resumption      │
│ - slotLimit: 1 409 purchase_capacity routing guard           │
└──────────────────────────────┬───────────────────────────────┘
                               │
                               ▼
┌──────────────────────────────────────────────────────────────┐
│ Phase 2: Foreign Client Tool Normalization (Backend)         │
│ - Verify OMP/OpenCode tool name mapping & restoration       │
│ - Non-colliding signature injection (end_turn / decide)      │
│ - Multi-turn agentic tool loop integration tests             │
└──────────────────────────────┬───────────────────────────────┘
                               │
                               ▼
┌──────────────────────────────────────────────────────────────┐
│ Phase 3: Dashboard Freebucks & Seat Monitoring (Frontend)   │
│ - Freebucks balance meter & local timezone reset timer       │
│ - Active 1-hour seat card with countdown and release button  │
│ - Model pricing catalog with Freebucks/hr badges             │
│ - planRequiredModelIds disabled / badged in picker           │
└──────────────────────────────┬───────────────────────────────┘
                               │
                               ▼
┌──────────────────────────────────────────────────────────────┐
│ Phase 4: Device Auth Wizard & Production Hardening           │
│ - In-dashboard device code login wizard with live polling    │
│ - End-to-end verification with tests/free-tier/run_all.py    │
│ - Production deployment and health verification              │
└──────────────────────────────────────────────────────────────┘
```
