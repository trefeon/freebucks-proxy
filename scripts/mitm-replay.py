#!/usr/bin/env python3
"""Live MITM replay: full chain through 127.0.0.1:8443 (lab CA), prompt forces
a tool call. Token from D:/tmp/fb-device-token.json (never printed)."""
import json, ssl, sys, uuid, urllib.request, urllib.error

TOKEN_PATH = "D:/tmp/fb-device-token.json"
MITM = "https://127.0.0.1:8443"
CA = "D:/github_repo/freebuff-proxy/devtools/mitm/ca-cert.pem"
BUN = "Bun/1.3.14"
AI_SDK_UA = ("ai-sdk/openai-compatible/0.0.0-test/codebuff "
             "ai-sdk/provider-utils/3.0.25 runtime/browser")
MODEL = "z-ai/glm-5.3-flash"
AGENT = "base3-free-glm-5-3-flash"

tok = json.load(open(TOKEN_PATH))
TOKEN = tok["authToken"]
USER_ID = tok.get("id", "")
CTX = ssl.create_default_context(cafile=CA)
print(f"token: ****{TOKEN[-4:]} via MITM {MITM}")

def call(method, path, body=None, headers=None, timeout=60):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(MITM + path, data=data,
                                 headers=headers or {}, method=method)
    try:
        with urllib.request.urlopen(req, timeout=timeout, context=CTX) as r:
            raw = r.read().decode("utf-8", "replace")
            try: return r.status, json.loads(raw)
            except Exception: return r.status, raw
    except urllib.error.HTTPError as e:
        raw = e.read()[:500].decode("utf-8", "replace")
        try: return e.code, json.loads(raw)
        except Exception: return e.code, raw

def auth(extra=None):
    h = {"Authorization": "Bearer " + TOKEN, "User-Agent": BUN,
         "Host": "127.0.0.1:8443"}
    if extra: h.update(extra)
    return h

print("== streak ==")
print(call("GET", "/api/v1/freebuff/streak", headers=auth())[0])

print("== admission (BILLS THE HOUR) ==")
claim_id = f"cli:{uuid.uuid4()}"
st, admit = call("POST", "/api/v1/freebuff/session/admission", body=None,
                 headers=auth({"x-freebuff-model": MODEL,
                               "x-freebuff-wallet-spend-limit": "0",
                               "x-freebuff-instance-id": claim_id,
                               "x-freebuff-multi-session": "1",
                               "x-freebuff-purchase-continuity": "1",
                               "x-freebuff-desktop-attempt-id": claim_id[4:],
                               "x-fb-timezone": "Asia/Jakarta",
                               "x-freebuff-first-tab-discount": "0"}))
print("admission:", st, "balance:",
      (admit.get("freebucks") or {}).get("balance") if isinstance(admit, dict) else admit)
if st != 200:
    sys.exit(f"admission refused: {json.dumps(admit)[:300]}")
admitted = admit.get("instanceId") or claim_id

print("== START ==")
st, start = call("POST", "/api/v1/agent-runs",
                 body={"action": "START", "agentId": AGENT, "ancestorRunIds": []},
                 headers=auth({"x-freebuff-acting-user-id": USER_ID,
                               "Content-Type": "application/json"}))
print("start:", st)
run_id = start["runId"]

print("== chat (forces list_directory tool call) ==")
with open("D:/github_repo/freebuff-proxy/devtools/mitm/flows.log",
          encoding="utf-8") as f:
    lines = f.readlines()
exact = json.loads(json.loads(lines[175])["body"])
exact["codebuff_metadata"]["run_id"] = run_id
exact["codebuff_metadata"]["trace_session_id"] = str(uuid.uuid4())
exact["codebuff_metadata"]["freebuff_instance_id"] = admitted
exact["messages"] = [exact["messages"][0],
    {"role": "user", "content": [{"type": "text", "text":
     "List the files in the frontend/src directory using your list_directory tool."}]}]
req = urllib.request.Request(
    MITM + "/api/v1/chat/completions", data=json.dumps(exact).encode(),
    headers={"Authorization": "Bearer " + TOKEN,
             "x-freebuff-acting-user-id": USER_ID,
             "Content-Type": "application/json",
             "User-Agent": AI_SDK_UA, "Accept": "text/event-stream",
             "Host": "127.0.0.1:8443"}, method="POST")
tool_calls, content = [], []
with urllib.request.urlopen(req, timeout=120, context=CTX) as r:
    print("chat status:", r.status)
    for line in r:
        s = line.decode("utf-8", "replace").strip()
        if s.startswith("data: ") and s != "data: [DONE]":
            try: delta = json.loads(s[6:])["choices"][0]["delta"]
            except Exception: continue
            if delta.get("tool_calls"): tool_calls.append(delta["tool_calls"])
            if delta.get("content"): content.append(delta["content"])
print("TOOL_CALLS:", json.dumps(tool_calls)[:400])
print("CONTENT:", "".join(content)[:400])

print("== FINISH ==")
print(call("POST", "/api/v1/agent-runs",
           body={"action": "FINISH", "runId": run_id, "status": "completed",
                 "totalSteps": 1, "directCredits": 0, "totalCredits": 0, "steps": []},
           headers=auth({"x-freebuff-acting-user-id": USER_ID,
                         "Content-Type": "application/json"}))[0])

print("== DELETE (release slot) ==")
print(call("DELETE", "/api/v1/freebuff/session",
           headers=auth({"x-freebuff-instance-id": admitted}))[0])
print("MITM REPLAY COMPLETE")
