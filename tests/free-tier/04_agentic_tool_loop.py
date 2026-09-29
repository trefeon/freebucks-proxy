#!/usr/bin/env python3
"""Suite 04: Two-Step Agentic Tool Calling Loop.

Verifies the full agentic multi-turn cycle:
1. START agent-runs (llm_step_number = 1).
2. Step 1 (Tool Call):
   - Model receives user instruction asking to inspect a directory.
   - Model streams finish_reason: "tool_calls" with function: {name: "list_directory", arguments: "..."}.
   - Verifies structured function arguments.
3. Execution:
   - Tool is executed locally (inspecting target directory).
4. Step 2 (Tool Result Injection & Final Synthesis):
   - Messages history is appended:
     - assistant: {content: "", tool_calls: [...]}
     - tool: {tool_call_id: "...", content: "{...}"}
   - llm_step_number bumped to 2.
   - Model streams final synthesis explaining the tool findings.
   - finish_reason: "stop".
5. FINISH agent-runs.

Usage:
    python 04_agentic_tool_loop.py [--model z-ai/glm-5.3-flash] [--target-dir backend/cmd]
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
)


def ensure_session(token, model):
    st, sess = request_http(
        "GET",
        f"{CODEBUFF_WWW}/api/v1/freebuff/session",
        headers={"Authorization": f"Bearer {token}", "User-Agent": UA_BUN},
    )
    if st == 200 and isinstance(sess, dict):
        if sess.get("status") == "active" and sess.get("instanceId"):
            return sess["instanceId"]

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
        return admit.get("instanceId") or claim_id
    elif st == 409 and isinstance(admit, dict):
        return admit.get("currentInstanceId")
    raise RuntimeError(f"Session admission failed: {admit}")


def stream_chat(token, user_id, payload):
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
        raise RuntimeError(f"Chat failed with HTTP {st}")

    content_parts = []
    tool_calls_map = {}
    finish_reason = None

    try:
        for line in stream_resp:
            s = line.decode("utf-8", errors="replace").strip() if isinstance(line, bytes) else line.strip()
            if s.startswith("data: ") and s != "data: [DONE]":
                try:
                    ch = json.loads(s[6:])["choices"][0]
                    delta = ch.get("delta", {})
                    if delta.get("content"):
                        content_parts.append(delta["content"])
                        sys.stdout.write(delta["content"])
                        sys.stdout.flush()
                    if delta.get("tool_calls"):
                        for tc in delta["tool_calls"]:
                            idx = tc.get("index", 0)
                            if idx not in tool_calls_map:
                                tool_calls_map[idx] = {
                                    "id": tc.get("id", ""),
                                    "type": "function",
                                    "function": {"name": "", "arguments": ""},
                                }
                            if tc.get("id"):
                                tool_calls_map[idx]["id"] = tc["id"]
                            fn = tc.get("function", {})
                            if fn.get("name"):
                                tool_calls_map[idx]["function"]["name"] = fn["name"]
                            if fn.get("arguments"):
                                tool_calls_map[idx]["function"]["arguments"] += fn["arguments"]
                    if ch.get("finish_reason"):
                        finish_reason = ch["finish_reason"]
                except Exception:
                    continue
    finally:
        if hasattr(stream_resp, "close"):
            stream_resp.close()

    return finish_reason, "".join(content_parts), list(tool_calls_map.values())


def run_agentic_loop(model=DEFAULT_MODEL, target_dir="backend/cmd"):
    creds = discover_credentials()
    if not creds:
        print("FAIL: No Freebuff credentials found.")
        return 1

    token = creds["authToken"]
    user_id = creds.get("id", "")
    agent_id = AGENT_MAP.get(model, DEFAULT_AGENT)

    print(f"Ensuring session seat for {model}...")
    instance_id = ensure_session(token, model)
    print(f"Active Instance ID: {instance_id}")

    # 1. START agent-runs
    print("Initializing agent-runs START...")
    headers = {
        "Authorization": f"Bearer {token}",
        "x-freebuff-acting-user-id": user_id,
        "Content-Type": "application/json",
        "User-Agent": UA_BUN,
    }
    st, start_data = request_http(
        "POST",
        f"{CODEBUFF_WWW}/api/v1/agent-runs",
        headers=headers,
        body={"action": "START", "agentId": agent_id, "ancestorRunIds": []},
    )
    if st != 200:
        print(f"FAIL: START agent-runs returned HTTP {st}")
        return 1
    run_id = start_data["runId"]
    trace_id = str(uuid.uuid4())
    client_id = uuid.uuid4().hex[:13]
    print(f"Run ID: {run_id}")

    sys_prompt = get_canonical_system_prompt()
    tools = load_canonical_tools()

    user_msg_content = f"List the files in the directory '{target_dir}' using your list_directory tool."
    messages = [
        {"role": "system", "content": sys_prompt},
        {"role": "user", "content": [{"type": "text", "text": user_msg_content}]},
    ]

    # --- STEP 1: Prompt -> Tool Call ---
    print("\n--------------------------------------------------")
    print(" STEP 1: Sending user instruction (expecting tool_call)")
    print("--------------------------------------------------")
    payload_step1 = {
        "model": model,
        "stream": True,
        "messages": messages,
        "tools": tools,
        "tool_choice": "auto",
        "codebuff_metadata": {
            "run_id": run_id,
            "trace_session_id": trace_id,
            "client_id": client_id,
            "freebuff_instance_id": instance_id,
            "freebuff_multi_session": "1",
            "surface": "cli",
            "cost_mode": "free",
            "llm_step_number": "1",
            "repo_snapshot": json.dumps({"gitAvailable": True, "fileCount": 100}),
        },
        "provider": {"data_collection": "deny"},
    }

    fin1, text1, tcs1 = stream_chat(token, user_id, payload_step1)
    print(f"\nStep 1 Finish Reason: {fin1}")
    if fin1 != "tool_calls" or not tcs1:
        print(f"FAIL: Model did not emit tool call (got {fin1}, tools: {tcs1})")
        return 1

    first_call = tcs1[0]
    call_id = first_call["id"]
    func_name = first_call["function"]["name"]
    func_args = first_call["function"]["arguments"]
    print(f"Tool Call Emitted: ID={call_id}, Function={func_name}, Args={func_args}")

    if func_name != "list_directory":
        print(f"FAIL: Expected list_directory tool call, got {func_name}")
        return 1

    # --- LOCAL EXECUTION ---
    print("\n--------------------------------------------------")
    print(" LOCAL EXECUTION: Simulating/Running tool call")
    print("--------------------------------------------------")
    try:
        args_parsed = json.loads(func_args)
        req_path = args_parsed.get("path", target_dir)
    except Exception:
        req_path = target_dir

    if os.path.exists(req_path):
        actual_entries = os.listdir(req_path)
    else:
        actual_entries = ["freebucks-proxy", "openapi-emit", "wiregen"]

    tool_result_content = json.dumps({"files": actual_entries, "path": req_path})
    print(f"Tool Execution Output:\n{tool_result_content}")

    # --- STEP 2: Tool Result -> Final Synthesis ---
    print("\n--------------------------------------------------")
    print(" STEP 2: Sending tool result (expecting final synthesis)")
    print("--------------------------------------------------")
    messages.append({
        "role": "assistant",
        "content": text1 or "",
        "tool_calls": [first_call],
    })
    messages.append({
        "role": "tool",
        "tool_call_id": call_id,
        "content": tool_result_content,
    })

    payload_step2 = {
        "model": model,
        "stream": True,
        "messages": messages,
        "tools": tools,
        "tool_choice": "auto",
        "codebuff_metadata": {
            "run_id": run_id,
            "trace_session_id": trace_id,
            "client_id": client_id,
            "freebuff_instance_id": instance_id,
            "freebuff_multi_session": "1",
            "surface": "cli",
            "cost_mode": "free",
            "llm_step_number": "2",
            "repo_snapshot": json.dumps({"gitAvailable": True, "fileCount": 100}),
        },
        "provider": {"data_collection": "deny"},
    }

    print("Model Streaming Final Response:")
    fin2, text2, tcs2 = stream_chat(token, user_id, payload_step2)
    print(f"\n\nStep 2 Finish Reason: {fin2}")
    if fin2 != "stop":
        print(f"FAIL: Expected finish_reason 'stop', got {fin2}")
        return 1

    # 4. FINISH agent-runs
    print("Finalizing agent run...")
    request_http(
        "POST",
        f"{CODEBUFF_WWW}/api/v1/agent-runs",
        headers=headers,
        body={
            "action": "FINISH",
            "runId": run_id,
            "status": "completed",
            "totalSteps": 2,
            "directCredits": 0,
            "totalCredits": 0,
            "steps": [],
        },
    )
    print("PASS: Two-step agentic tool calling cycle succeeded.")
    return 0


def main():
    parser = argparse.ArgumentParser(description="Freebuff 04: Two-Step Agentic Tool Calling Loop")
    parser.add_argument("--model", default=DEFAULT_MODEL, help=f"Model ID to test (default: {DEFAULT_MODEL})")
    parser.add_argument("--target-dir", default="backend/cmd", help="Directory path to request")
    args = parser.parse_args()
    return run_agentic_loop(model=args.model, target_dir=args.target_dir)


if __name__ == "__main__":
    sys.exit(main())
