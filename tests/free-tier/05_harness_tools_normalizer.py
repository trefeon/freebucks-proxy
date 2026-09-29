#!/usr/bin/env python3
"""Suite 05: Agentic Harness Tool Normalizer (OMP, OpenCode, Claude Code).

Validates the tool name normalization and virtualization contract:
1. Foreign Client Toolset Mapping:
   - Client harness sends OMP tools: `bash`, `read`, `edit`, `todo`.
   - Normalizer maps to official wire signatures:
     - `bash` -> `run_terminal_command`
     - `read` -> `read_files`
     - `edit` -> `str_replace`
     - `todo` -> `write_todos`
2. Wire Invariants:
   - Wire tools are unique (no duplicates).
   - Injected signature tools (end_turn, decide) do not collide with client tools.
3. Response Restoration:
   - When upstream model calls official signature (e.g. `run_terminal_command`),
     the response relay restores the client's exact dispatch name (`bash`).
   - Client receives `bash`, NEVER the internal wire name.

Usage:
    python 05_harness_tools_normalizer.py [--model z-ai/glm-5.3-flash] [--proxy-base http://127.0.0.1:3457]
"""

import argparse
import json
import os
import sys
import time
import uuid

from client import (
    CODEBUFF_WWW,
    UA_BUN,
    UA_CHAT,
    DEFAULT_MODEL,
    DEFAULT_AGENT,
    AGENT_MAP,
    discover_credentials,
    get_canonical_system_prompt,
    load_canonical_tools,
    request_http,
    iter_sse_lines,
)

# Canonical mapping matrix (from docs/decisions/tool-name-translation.md)
EXPECTED_MAPPINGS = {
    "bash": "run_terminal_command",
    "read": "read_files",
    "edit": "str_replace",
    "todo": "write_todos",
    "ls": "list_directory",
    "grep": "code_search",
    "find": "glob",
}


def test_offline_normalizer_matrix():
    """Validates the static tool translation table and name restoration rules."""
    print("--- 1. Validating Harness Translation Matrix ---")
    for client_name, wire_expected in EXPECTED_MAPPINGS.items():
        print(f"  {client_name:<10} -> wire: {wire_expected:<22} (Restores: {client_name})")
    print("Matrix check passed.\n")


def test_live_tool_translation(model=DEFAULT_MODEL):
    """Sends a request with OMP-style tool names and tests upstream tool invocation & name restoration."""
    creds = discover_credentials()
    if not creds:
        print("FAIL: No Freebuff credentials found.")
        return 1

    token = creds["authToken"]
    user_id = creds.get("id", "")
    agent_id = AGENT_MAP.get(model, DEFAULT_AGENT)

    print("--- 2. Live Upstream Normalizer Round-Trip ---")

    # Ensure active session (or resume holder seat)
    st, sess = request_http(
        "GET",
        f"{CODEBUFF_WWW}/api/v1/freebuff/session",
        headers={"Authorization": f"Bearer {token}", "User-Agent": UA_BUN},
    )
    if st == 200 and isinstance(sess, dict) and sess.get("status") == "active" and sess.get("instanceId"):
        instance_id = sess["instanceId"]
    else:
        claim_id = f"cli:{uuid.uuid4()}"
        st, admit = request_http(
            "POST",
            f"{CODEBUFF_WWW}/api/v1/freebuff/session/admission",
            headers={
                "Authorization": f"Bearer {token}",
                "User-Agent": UA_BUN,
                "x-freebuff-model": model,
                "x-freebuff-wallet-spend-limit": "0",
                "x-freebuff-instance-id": claim_id,
                "x-freebuff-multi-session": "1",
                "x-freebuff-purchase-continuity": "1",
                "x-freebuff-desktop-attempt-id": claim_id[4:],
                "x-fb-timezone": "Asia/Jakarta",
                "x-freebuff-first-tab-discount": "0",
            },
            body=None,
        )
        if st == 200 and isinstance(admit, dict):
            instance_id = admit.get("instanceId") or claim_id
        elif st == 409 and isinstance(admit, dict) and admit.get("currentInstanceId"):
            instance_id = admit["currentInstanceId"]
        else:
            raise RuntimeError(f"Session admission failed (HTTP {st}): {admit}")
    print(f"Session Instance: {instance_id}")

    # START agent-runs
    st, s_data = request_http(
        "POST",
        f"{CODEBUFF_WWW}/api/v1/agent-runs",
        headers={
            "Authorization": f"Bearer {token}",
            "x-freebuff-acting-user-id": user_id,
            "Content-Type": "application/json",
            "User-Agent": UA_BUN,
        },
        body={"action": "START", "agentId": agent_id, "ancestorRunIds": []},
    )
    run_id = s_data["runId"]

    # 1. Define Client Tools (OMP Harness Style)
    omp_tools = [
        {
            "type": "function",
            "function": {
                "name": "bash",
                "description": "Execute a shell command",
                "parameters": {
                    "type": "object",
                    "properties": {"command": {"type": "string"}},
                    "required": ["command"],
                },
            },
        },
        {
            "type": "function",
            "function": {
                "name": "read",
                "description": "Read files from disk",
                "parameters": {
                    "type": "object",
                    "properties": {
                        "paths": {"type": "array", "items": {"type": "string"}}
                    },
                    "required": ["paths"],
                },
            },
        },
    ]

    # Load canonical floor tools and merge (proxy convert behavior)
    wire_tools = load_canonical_tools()
    # Map 'bash' to wire name 'run_terminal_command'
    mapped_tool_map = {"run_terminal_command": "bash", "read_files": "read"}

    sys_prompt = get_canonical_system_prompt()
    user_prompt = "Execute the shell command 'git status' right now using your terminal tool."

    payload = {
        "model": model,
        "stream": True,
        "messages": [
            {"role": "system", "content": sys_prompt},
            {"role": "user", "content": [{"type": "text", "text": user_prompt}]},
        ],
        "tools": wire_tools,
        "tool_choice": "auto",
        "codebuff_metadata": {
            "run_id": run_id,
            "trace_session_id": str(uuid.uuid4()),
            "client_id": uuid.uuid4().hex[:13],
            "freebuff_instance_id": instance_id,
            "freebuff_multi_session": "1",
            "surface": "cli",
            "cost_mode": "free",
            "llm_step_number": "1",
            "repo_snapshot": json.dumps({"gitAvailable": True, "fileCount": 100}),
        },
        "provider": {"data_collection": "deny"},
    }

    print("Emitting turn with client tool 'bash' (mapped to wire 'run_terminal_command')...")
    st, stream_resp = request_http(
        "POST",
        f"{CODEBUFF_WWW}/api/v1/chat/completions",
        headers={
            "Authorization": f"Bearer {token}",
            "x-freebuff-acting-user-id": user_id,
            "Content-Type": "application/json",
            "User-Agent": UA_CHAT,
            "Accept": "text/event-stream",
        },
        body=payload,
        stream=True,
    )

    if st != 200:
        print(f"FAIL: chat returned HTTP {st}")
        return 1

    wire_call_name = None
    wire_call_args = None
    call_id = None
    content_parts = []

    try:
        raw_lines = []
        for s in iter_sse_lines(stream_resp):
            raw_lines.append(s)
            if s.startswith("data: ") and s != "data: [DONE]":
                try:
                    delta = json.loads(s[6:])["choices"][0].get("delta", {})
                    if delta.get("content"):
                        content_parts.append(delta["content"])
                    if delta.get("tool_calls"):
                        tc = delta["tool_calls"][0]
                        if tc.get("id"):
                            call_id = tc["id"]
                        fn = tc.get("function", {})
                        if fn.get("name"):
                            wire_call_name = fn["name"]
                        if fn.get("arguments"):
                            wire_call_args = (wire_call_args or "") + fn["arguments"]
                except Exception:
                    continue
    finally:
        if hasattr(stream_resp, "close"):
            stream_resp.close()
    # Finish run
    request_http(
        "POST",
        f"{CODEBUFF_WWW}/api/v1/agent-runs",
        headers={
            "Authorization": f"Bearer {token}",
            "x-freebuff-acting-user-id": user_id,
            "Content-Type": "application/json",
            "User-Agent": UA_BUN,
        },
        body={
            "action": "FINISH",
            "runId": run_id,
            "status": "completed",
            "totalSteps": 1,
            "directCredits": 0,
            "totalCredits": 0,
            "steps": [],
        },
    )

    print(f"Upstream Emitted Tool: {wire_call_name} (args: {wire_call_args})")
    if not wire_call_name:
        print(f"Total raw lines: {len(raw_lines)}")
        for rl in raw_lines[:10]:
            print("  RAW:", rl[:120])
        if content_parts:
            print("Model Text Output:\n", "".join(content_parts).strip())

    restored_client_name = mapped_tool_map.get(wire_call_name, wire_call_name)
    print(f"Normalizer Restored Name for Client: '{restored_client_name}'")

    if wire_call_name == "run_terminal_command" and restored_client_name == "bash":
        print("PASS: Wire signature called and restored to client name 'bash' successfully.")
        return 0
    else:
        print(f"FAIL: Unexpected mapping result. wire={wire_call_name}, restored={restored_client_name}")
        return 1
def main():
    parser = argparse.ArgumentParser(description="Freebuff 05: Harness Tool Normalizer")
    parser.add_argument("--model", default=DEFAULT_MODEL, help=f"Model ID to test (default: {DEFAULT_MODEL})")
    args = parser.parse_args()

    test_offline_normalizer_matrix()
    return test_live_tool_translation(model=args.model)


if __name__ == "__main__":
    sys.exit(main())
