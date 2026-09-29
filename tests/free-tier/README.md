# Freebuff Free-Tier Complete Lifecycle Test Suite

Automated verification harness for the official Freebuff wire protocol, covering authentication, entitlements, agentic tool calling, model concurrency boundaries, and foreign harness tool normalization (OMP, OpenCode, Claude Code).

Requires **Python 3 stdlib only** (no external pip dependencies).

---

## Suite Directory Layout

```
tests/free-tier/
├── client.py                          # Shared client, redirect handler, prompt & tool fixture loader
├── 01_auth_device_login.py            # Device-code OAuth login & automatic token discovery
├── 02_entitlements_check.py           # Quotas, Freebucks balance, daily reset & model pricing catalog
├── 03_chat_and_reasoning.py           # Real-time streaming chat, delta thinking/reasoning & content tokens
├── 04_agentic_tool_loop.py            # 2-step agentic cycle (tool_calls -> execution -> final synthesis)
├── 05_harness_tools_normalizer.py     # OMP/foreign tool translation (bash -> run_terminal_command) & restoration
├── 06_model_concurrency_lifecycle.py  # 1-hour purchase seat, slotLimit=1 409 gate, 0-cost seat resumption & DELETE
└── run_all.py                         # Master test runner with formatted reporting table
```

---

## Quick Start

### 1. Run Complete Suite (Master Runner)
```bash
python tests/free-tier/run_all.py
```

### 2. Run with Live MITM Proxy & Wire Log Validation
Routes traffic through `https://127.0.0.1:8443` and asserts wire flows in `devtools/mitm/flows.log`:
```bash
python tests/free-tier/run_all.py --mitm
```

### 3. Run Specific Suites
```bash
# Run only authentication and entitlements check
python tests/free-tier/run_all.py --steps 1,2

# Run only agentic tool loop and normalizer
python tests/free-tier/run_all.py --steps 4,5
```

---

## Detailed Suite Reference

### `01_auth_device_login.py` (Authentication & Auto-Detection)
- Auto-detects existing tokens from:
  1. `FB_AUTH_TOKEN` environment variable
  2. `D:/tmp/fb-device-token.json`
  3. `~/.config/manicode/credentials.json`
- Tests liveness with `GET /api/v1/freebuff/session`.
- If missing or dead, launches the interactive device OAuth code flow:
  1. Requests device code via `POST https://freebuff.com/api/auth/cli/code`.
  2. Outputs verbatim `https://freebuff.com/login?auth_code=...` URL (never rewrites to `/onboard`).
  3. Polls with live progress indicator `[N/60]` until approved.
  4. Saves credentials (`authToken`, `id`, `email`) locally.
- **Flags**: `--fresh` (force new login), `--timeout <seconds>`.

### `02_entitlements_check.py` (Quotas & Model Catalog)
- Queries `GET /api/v1/freebuff/session`, `GET /api/v1/freebuff/streak`, and `GET /api/v1/me`.
- Validates:
  - Account email, acting user ID, and ban standing (`banned: false`).
  - Access tier (`LIMITED` vs `FULL`).
  - Freebucks daily balance (e.g. 20/25) and exact Pacific/local reset timestamp.
  - Model pricing table: distinguishes available free models (0 to 15 FB/hr) from paid-only models (`planRequiredModelIds`).

### `03_chat_and_reasoning.py` (Streaming Chat & Thinking Tokens)
- Validates the complete chat completions turn:
  1. `POST /api/v1/agent-runs` with `action: START`.
  2. `POST /api/v1/chat/completions` with streaming:
     - Canonical base3 instructions prompt head (6 convention bullets + dynamic date).
     - 16 canonical tools with full schemas.
     - Exact `codebuff_metadata` envelope.
     - ai-sdk User-Agent.
  3. Streams and captures delta reasoning tokens (`delta.reasoning_content`) and delta text content.
  4. `POST /api/v1/agent-runs` with `action: FINISH`.
- **Flags**: `--model <id>`, `--prompt "<text>"`.

### `04_agentic_tool_loop.py` (Two-Step Agentic Tool Loop)
- Validates true multi-step agentic execution:
  - **Step 1**: User asks to inspect `backend/cmd`. Model evaluates prompt and streams `finish_reason: "tool_calls"` with `{"name": "list_directory", "arguments": "{\"path\": \"backend/cmd\"}"}`.
  - **Execution**: Tool is run locally, capturing actual directory contents.
  - **Step 2**: Replays `assistant` tool_calls message + `tool` role message with JSON result, bumps `llm_step_number` to 2. Model streams final textual synthesis with `finish_reason: "stop"`.
- **Flags**: `--model <id>`, `--target-dir <path>`.

### `05_harness_tools_normalizer.py` (OMP Tool Translation & Restoration)
- Validates foreign harness tool compatibility (OMP, OpenCode, Claude Code):
  - Injects foreign tools (e.g. `bash`, `read`, `edit`, `todo`).
  - Verifies that foreign tools translate to canonical wire signatures (`run_terminal_command`, `read_files`, `str_replace`, `write_todos`).
  - Emits request and verifies that when upstream model invokes `run_terminal_command`, the response normalizer restores the client's expected dispatch name (`bash`).

### `06_model_concurrency_lifecycle.py` (Concurrency & 1-Hour Seat Resumption)
- Validates the free tier seat lifecycle:
  1. Admission: `POST /api/v1/freebuff/session/admission` creates 1-hour active seat (`3600s`), deducting hourly Freebucks price once.
  2. Concurrency gate: Attempting to admit a *different* model while a seat is active triggers **HTTP 409 `purchase_capacity`** due to `slotLimit: 1`.
  3. Zero-cost seat resumption: Re-admitting the *same* purchased model with its `holderInstanceId` costs **0 additional Freebucks** and resumes the active seat.
  4. Release: `DELETE /api/v1/freebuff/session` ends the session cleanly.

---

## Wire Protocol Reference & Troubleshooting

| Status Code | Meaning | Cause & Recovery |
|---|---|---|
| **200 OK** | Success | Request envelope satisfied, session active or valid turn completed. |
| **401 Unauthorized** | Token rejected | Upstream token is dead or expired. Run `01_auth_device_login.py` to refresh. *(Note: `/api/v1/me` returns 401 on free tokens, but `session` returns 200 — never use `/me` as reachability gate).* |
| **403 Forbidden** | Account banned | Account suspended upstream (`{"status":"banned"}`). Rotate token with a fresh GitHub account. |
| **409 Conflict** | `purchase_capacity` | `slotLimit: 1` concurrency gate. Another model holds the 1-hour purchase seat. Either wait for `expiresAt` or resume the purchased model using its `holderInstanceId`. |
| **428 Precondition Required** | `waiting_room_required` | Session seat was ended or unadmitted. Must call admission (`POST /session/admission`) before sending chat completions. |
| **503 Unavailable** | `model_unavailable` | Envelope gate tripped. The chat request omitted the canonical base3 prompt head or 16 full-schema tools. |
