#!/usr/bin/env python3
"""ONE-SHOT 100% CLI mimic: captured-176 structure, honest content.
Order: admit (billed once) -> START -> single chat -> FINISH -> DELETE.
Token never printed."""
import json, sys, uuid, urllib.request, urllib.error

tok = json.load(open("D:/tmp/fb-device-token.json"))
TOKEN, UID = tok["authToken"], tok.get("id", "")
B = "https://www.codebuff.com"
BUN = "Bun/1.3.14"
UA = ("ai-sdk/openai-compatible/0.0.0-test/codebuff "
      "ai-sdk/provider-utils/3.0.25 runtime/browser")
MODEL, AGENT = "z-ai/glm-5.3-flash", "base3-free-glm-5-3-flash"
print(f"token ****{TOKEN[-4:]} — ONE billed admission, ONE chat", flush=True)

def call(method, path, body=None, headers=None):
    d = json.dumps(body).encode() if body is not None else None
    r = urllib.request.Request(B + path, data=d, headers=headers or {},
                               method=method)
    with urllib.request.urlopen(r, timeout=60) as x:
        return x.status, json.loads(x.read().decode())

H = {"Authorization": "Bearer " + TOKEN, "User-Agent": BUN}

print("== admit ==", flush=True)
claim = f"cli:{uuid.uuid4()}"
st, admit = call("POST", "/api/v1/freebuff/session/admission", None,
    {**H, "x-freebuff-model": MODEL, "x-freebuff-wallet-spend-limit": "0",
     "x-freebuff-instance-id": claim, "x-freebuff-multi-session": "1",
     "x-freebuff-purchase-continuity": "1",
     "x-freebuff-desktop-attempt-id": claim[4:],
     "x-fb-timezone": "Asia/Jakarta", "x-freebuff-first-tab-discount": "0"})
print("admission:", st, "balance:",
      (admit.get("freebucks") or {}).get("balance"), flush=True)
if st != 200:
    sys.exit(f"admission refused: {json.dumps(admit)[:200]}")
inst = admit.get("instanceId") or claim

print("== START ==", flush=True)
st, s = call("POST", "/api/v1/agent-runs",
    {"action": "START", "agentId": AGENT, "ancestorRunIds": []},
    {**H, "x-freebuff-acting-user-id": UID, "Content-Type": "application/json"})
run = s["runId"]
print("run:", run, flush=True)

print("== building 100%-mimic body (offline shapes) ==", flush=True)
lines = open("D:/github_repo/freebuff-proxy/devtools/mitm/flows.log",
             encoding="utf-8").readlines()
exact = json.loads(json.loads(lines[175])["body"])
# 16 tools: captured order + schemas, EXCEPT skill -> canonical empty-catalog
tools = exact["tools"]
for t in tools:
    if t["function"]["name"] == "skill":
        d = t["function"]["description"]
        i = d.find("<available_skills>")
        j = d.find("</available_skills>") + len("</available_skills>")
        t["function"]["description"] = d[:i] + d[j:]
        print("skill desc:", len(d), "->",
              len(t["function"]["description"]), flush=True)
# honest minimal repo snapshot of the real checkout
import os
fc = sum(len(f) for _, _, f in os.walk(
    "D:/github_repo/freebuff-proxy/backend/internal/upstream"))
snap = json.dumps({"gitAvailable": True, "fileCount": fc,
                   "fileCountIsLowerBound": False})
# system: full base3 head (proven minimum) — bulk session content has no
# honest equivalent in a bare probe, head is the load-bearing part
syshead = (exact["messages"][0]["content"]
           if isinstance(exact["messages"][0], dict)
           else exact["messages"][0]).split("Project instructions:")[0]
body = {"model": MODEL, "stream": True,
        "messages": [{"role": "system", "content": syshead},
                     {"role": "user", "content": [{"type": "text", "text":
                      "What is 12 + 12? Answer directly."}]}],
        "tools": tools, "tool_choice": "auto",
        "codebuff_metadata": {
            "run_id": run, "trace_session_id": str(uuid.uuid4()),
            "client_id": uuid.uuid4().hex[:13],
            "freebuff_instance_id": inst, "freebuff_multi_session": "1",
            "surface": "cli", "cost_mode": "free", "llm_step_number": "1",
            "repo_snapshot": snap},
        "provider": {"data_collection": "deny"}}
print("body tools:", len(body["tools"]), "meta keys:",
      sorted(body["codebuff_metadata"].keys()), flush=True)

print("== THE ONE CHAT SHOT ==", flush=True)
req = urllib.request.Request(
    B + "/api/v1/chat/completions", data=json.dumps(body).encode(),
    headers={"Authorization": "Bearer " + TOKEN,
             "x-freebuff-acting-user-id": UID,
             "Content-Type": "application/json", "User-Agent": UA,
             "Accept": "text/event-stream"}, method="POST")
try:
    parts = []
    with urllib.request.urlopen(req, timeout=120) as r:
        print("chat status:", r.status, flush=True)
        for line in r:
            s = line.decode("utf-8", "replace").strip()
            if s.startswith("data: ") and s != "data: [DONE]":
                try:
                    d = json.loads(s[6:])["choices"][0]["delta"]
                except Exception:
                    continue
                if d.get("content"):
                    parts.append(d["content"])
    print("RESPONSE:", "".join(parts)[:300], flush=True)
except urllib.error.HTTPError as e:
    print("chat HTTP", e.code, e.read()[:200].decode(), flush=True)

print("== FINISH + DELETE ==", flush=True)
call("POST", "/api/v1/agent-runs",
     {"action": "FINISH", "runId": run, "status": "completed",
      "totalSteps": 1, "directCredits": 0, "totalCredits": 0, "steps": []},
     {**H, "x-freebuff-acting-user-id": UID, "Content-Type": "application/json"})
st, end = call("DELETE", "/api/v1/freebuff/session",
               headers={**H, "x-freebuff-instance-id": inst})
print("delete:", st, json.dumps(end)[:150], flush=True)
print("SHOT COMPLETE", flush=True)
