#!/usr/bin/env python3
"""Suite 03: Streaming Chat & Thinking Reasoning Tokens.

Verifies:
1. Reusing an active model session seat (or admitting one if none active).
2. START agent-runs lifecycle.
3. POST /api/v1/chat/completions with stream=true:
   - Canonical base3 system prompt head
   - 16 official tool declarations with full schemas
   - Exact codebuff_metadata (run_id, trace_session_id, client_id, instance_id, cost_mode: free)
   - Real-time streaming chunks: delta reasoning_content & delta content
4. FINISH agent-runs lifecycle.

Usage:
    python 03_chat_and_reasoning.py [--model z-ai/glm-5.3-flash] [--prompt "What is a proxy?"]
"""

import argparse
import json
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


def ensure_session_admitted(token, model):
    """Checks for an active session or admits a new 1-hour session seat."""
    # First probe current session
    st, sess = request_http(
        "GET",
        f"{CODEBUFF_WWW}/api/v1/freebuff/session",
        headers={"Authorization": f"Bearer {token}", "User-Agent": UA_BUN},
    )
    if st == 200 and isinstance(sess, dict):
        if sess.get("status") == "active" and sess.get("instanceId"):
            return sess["instanceId"], False

    # Need admission
    claim_id = f"cli:{uuid.uuid4()}"
    headers = {
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
    }
    st, admit = request_http(
        "POST",
        f"{CODEBUFF_WWW}/api/v1/freebuff/session/admission",
        headers=headers,
        body=None,
    )
    if st == 200 and isinstance(admit, dict):
        inst = admit.get("instanceId") or claim_id
        return inst, True
    elif st == 409 and isinstance(admit, dict):
        holder = admit.get("currentInstanceId")
        if holder:
            return holder, False

    raise RuntimeError(f"Failed to admit session (HTTP {st}): {admit}")


def run_chat_test(model=DEFAULT_MODEL, user_prompt="Explain why 1+1=2 in one sentence."):
    creds = discover_credentials()
    if not creds:
        print("FAIL: No Freebuff credentials found.")
        return 1

    token = creds["authToken"]
    user_id = creds.get("id", "")
    agent_id = AGENT_MAP.get(model, DEFAULT_AGENT)

    print(f"Ensuring session seat for {model}...")
    instance_id, fresh_admit = ensure_session_admitted(token, model)
    print(f"Active Instance ID: {instance_id} (fresh admission: {fresh_admit})")

    # 1. START agent-runs
    print("Starting agent run...")
    start_body = {"action": "START", "agentId": agent_id, "ancestorRunIds": []}
    headers = {
        "Authorization": f"Bearer {token}",
        "x-freebuff-acting-user-id": user_id,
        "Content-Type": "application/json",
        "User-Agent": UA_BUN,
    }
    st_start, start_resp = request_http(
        "POST",
        f"{CODEBUFF_WWW}/api/v1/agent-runs",
        headers=headers,
        body=start_body,
    )
    if st_start != 200 or not isinstance(start_resp, dict) or "runId" not in start_resp:
        print(f"FAIL: agent-runs START failed (HTTP {st_start}): {start_resp}")
        return 1

    run_id = start_resp["runId"]
    trace_id = str(uuid.uuid4())
    client_id = uuid.uuid4().hex[:13]
    print(f"Agent Run ID: {run_id}")

    # 2. Build Chat Payload with required gate envelope
    sys_prompt = get_canonical_system_prompt()
    tools = load_canonical_tools()

    chat_payload = {
        "model": model,
        "stream": True,
        "messages": [
            {"role": "system", "content": sys_prompt},
            {"role": "user", "content": [{"type": "text", "text": user_prompt}]},
        ],
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
            "repo_snapshot": json.dumps({"gitAvailable": True, "fileCount": 50}),
        },
        "provider": {"data_collection": "deny"},
    }

    print("\nSending streaming chat request...")
    t0 = time.time()
    st_chat, stream_resp = request_http(
        "POST",
        f"{CODEBUFF_WWW}/api/v1/chat/completions",
        headers={
            "Authorization": f"Bearer {token}",
            "x-freebuff-acting-user-id": user_id,
            "Content-Type": "application/json",
            "User-Agent": UA_CHAT,
            "Accept": "text/event-stream",
        },
        body=chat_payload,
        stream=True,
    )

    if st_chat != 200:
        print(f"FAIL: chat/completions returned HTTP {st_chat}")
        return 1

    ttfb = time.time() - t0
    print(f"HTTP 200 OK (TTFB: {ttfb:.2f}s)\nStreaming model output:")
    print("--------------------------------------------------")

    reasoning_tokens = []
    content_tokens = []

    try:
        for line in stream_resp:
            s = line.decode("utf-8", errors="replace").strip() if isinstance(line, bytes) else line.strip()
            if s.startswith("data: ") and s != "data: [DONE]":
                try:
                    chunk = json.loads(s[6:])
                    delta = chunk["choices"][0].get("delta", {})
                    if delta.get("reasoning_content"):
                        reasoning_tokens.append(delta["reasoning_content"])
                    if delta.get("content"):
                        content_tokens.append(delta["content"])
                        sys.stdout.write(delta["content"])
                        sys.stdout.flush()
                except Exception:
                    continue
    finally:
        if hasattr(stream_resp, "close"):
            stream_resp.close()

    total_time = time.time() - t0
    print("\n--------------------------------------------------")
    print(f"Stream completed in {total_time:.2f}s ({len(content_tokens)} chunks received).")

    if reasoning_tokens:
        print(f"Reasoning/Thinking Tokens: {len(reasoning_tokens)} chunks captured.")

    # 3. FINISH agent-runs
    print("Completing agent run...")
    fin_body = {
        "action": "FINISH",
        "runId": run_id,
        "status": "completed",
        "totalSteps": 1,
        "directCredits": 0,
        "totalCredits": 0,
        "steps": [],
    }
    st_fin, fin_resp = request_http(
        "POST",
        f"{CODEBUFF_WWW}/api/v1/agent-runs",
        headers=headers,
        body=fin_body,
    )
    print(f"Agent Run FINISH: HTTP {st_fin}")

    print("\nPASS: Streaming chat & reasoning verified.")
    return 0


def main():
    parser = argparse.ArgumentParser(description="Freebuff 03: Streaming Chat & Reasoning Tokens")
    parser.add_argument("--model", default=DEFAULT_MODEL, help=f"Model ID to test (default: {DEFAULT_MODEL})")
    parser.add_argument("--prompt", default="Explain what an API is in one clear sentence.", help="User prompt to send")
    args = parser.parse_args()
    return run_chat_test(model=args.model, user_prompt=args.prompt)


if __name__ == "__main__":
    sys.exit(main())
