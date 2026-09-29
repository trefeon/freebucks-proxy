#!/usr/bin/env python3
"""Live 2-step tool loop through MITM: step 1 tool call, step 2 feeds the tool
result back so the model streams TEXT. Token never printed."""
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
print(f"token: ****{TOKEN[-4:]} via MITM", flush=True)

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

def chat(body):
    req = urllib.request.Request(
        MITM + "/api/v1/chat/completions", data=json.dumps(body).encode(),
        headers={"Authorization": "Bearer " + TOKEN,
                 "x-freebuff-acting-user-id": USER_ID,
                 "Content-Type": "application/json",
                 "User-Agent": AI_SDK_UA, "Accept": "text/event-stream",
                 "Host": "127.0.0.1:8443"}, method="POST")
    tcs, texts, finish = [], [], None
    with urllib.request.urlopen(req, timeout=120, context=CTX) as r:
        st = r.status
        for line in r:
            s = line.decode("utf-8", "replace").strip()
            if s.startswith("data: ") and s != "data: [DONE]":
                try: ch = json.loads(s[6:])["choices"][0]
                except Exception: continue
                d = ch.get("delta", {})
                if d.get("tool_calls"): tcs.append(d["tool_calls"])
                if d.get("content"):
                    texts.append(d["content"])
                    sys.stdout.write(d["content"])
                    sys.stdout.flush()
                if ch.get("finish_reason"): finish = ch["finish_reason"]
    print()
    return st, tcs, "".join(texts), finish

print("== admission (BILLS THE HOUR) ==", flush=True)
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
      (admit.get("freebucks") or {}).get("balance") if isinstance(admit, dict) else admit,
      flush=True)
if st != 200:
    sys.exit(f"admission refused: {json.dumps(admit)[:300]}")
admitted = admit.get("instanceId") or claim_id

print("== START ==", flush=True)
st, start = call("POST", "/api/v1/agent-runs",
                 body={"action": "START", "agentId": AGENT, "ancestorRunIds": []},
                 headers=auth({"x-freebuff-acting-user-id": USER_ID,
                               "Content-Type": "application/json"}))
run_id = start["runId"]
trace_id = str(uuid.uuid4())
print("run:", run_id, flush=True)

with open("D:/github_repo/freebuff-proxy/devtools/mitm/flows.log",
          encoding="utf-8") as f:
    lines = f.readlines()
exact = json.loads(json.loads(lines[175])["body"])

def base_meta(step):
    return {"run_id": run_id, "trace_session_id": trace_id,
            "client_id": uuid.uuid4().hex[:13],
            "freebuff_instance_id": admitted, "freebuff_multi_session": "1",
            "surface": "cli", "cost_mode": "free",
            "llm_step_number": str(step),
            "repo_snapshot": exact["codebuff_metadata"]["repo_snapshot"]}

sys_p = exact["messages"][0]
print("== STEP 1 chat (model calls tool) ==", flush=True)
b1 = {"model": MODEL, "stream": True, "messages": [
        sys_p, {"role": "user", "content": [{"type": "text", "text":
        "List the files in the frontend/src directory using your list_directory tool."}]}],
      "tools": exact["tools"], "tool_choice": "auto",
      "codebuff_metadata": base_meta(1), "provider": {"data_collection": "deny"}}
st1, tcs1, txt1, fin1 = chat(b1)
print(f"[step1 status={st1} finish={fin1} tool_calls={json.dumps(tcs1)[:200]}]", flush=True)
call_id = tcs1[0][0]["id"]
call_fn = tcs1[0][0]["function"]

print("== STEP 2 chat (tool result -> model TEXT, streaming below) ==", flush=True)
tool_result = json.dumps({"files": ["app.css", "App.svelte", "main.js"],
                          "dirs": ["lib"]})
b2 = {"model": MODEL, "stream": True, "messages": [
        sys_p, {"role": "user", "content": [{"type": "text", "text":
        "List the files in the frontend/src directory using your list_directory tool."}]},
        {"role": "assistant", "content": "", "tool_calls": [
            {"id": call_id, "type": "function", "function": call_fn}]},
        {"role": "tool", "tool_call_id": call_id, "content": tool_result}],
      "tools": exact["tools"], "tool_choice": "auto",
      "codebuff_metadata": base_meta(2), "provider": {"data_collection": "deny"}}
print("--- MODEL TEXT STREAM ---", flush=True)
st2, tcs2, txt2, fin2 = chat(b2)
print("--- END STREAM ---", flush=True)
print(f"[step2 status={st2} finish={fin2} text_len={len(txt2)}]", flush=True)

print("== FINISH + DELETE ==", flush=True)
call("POST", "/api/v1/agent-runs",
     body={"action": "FINISH", "runId": run_id, "status": "completed",
           "totalSteps": 2, "directCredits": 0, "totalCredits": 0, "steps": []},
     headers=auth({"x-freebuff-acting-user-id": USER_ID,
                   "Content-Type": "application/json"}))
print("delete:", call("DELETE", "/api/v1/freebuff/session",
     headers=auth({"x-freebuff-instance-id": admitted}))[0], flush=True)
print("LOOP COMPLETE", flush=True)
