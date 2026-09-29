#!/usr/bin/env python3
"""Real-request replay: health (free) + one billed turn + release. Token from
D:/tmp/fb-device-token.json (never printed)."""
import json, sys, time, uuid, urllib.request, urllib.error

TOKEN_PATH = "D:/tmp/fb-device-token.json"
WWW = "https://www.codebuff.com"
BUN = "Bun/1.3.14"
AI_SDK_UA = ("ai-sdk/openai-compatible/0.0.0-test/codebuff "
             "ai-sdk/provider-utils/3.0.25 runtime/browser")
MODEL = "z-ai/glm-5.3-flash"
AGENT = "base3-free-glm-5-3-flash"

tok = json.load(open(TOKEN_PATH))
TOKEN = tok["authToken"]
USER_ID = tok.get("id", "")
print(f"token: ****{TOKEN[-4:]} account id present: {bool(USER_ID)}")

def call(method, path, body=None, headers=None, timeout=60):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(WWW + path, data=data,
                                 headers=headers or {}, method=method)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            raw = r.read().decode("utf-8", "replace")
            try: return r.status, json.loads(raw)
            except Exception: return r.status, raw
    except urllib.error.HTTPError as e:
        raw = e.read()[:800].decode("utf-8", "replace")
        try: return e.code, json.loads(raw)
        except Exception: return e.code, raw

def auth(extra=None):
    h = {"Authorization": "Bearer " + TOKEN, "User-Agent": BUN}
    if extra: h.update(extra)
    return h

print("== 1. session GET (reachability) ==")
st, sess = call("GET", "/api/v1/freebuff/session", headers=auth())
print("session:", st, json.dumps(sess)[:300])

print("== 2. streak GET ==")
st, streak = call("GET", "/api/v1/freebuff/streak", headers=auth())
print("streak:", st, json.dumps(streak)[:200])

print("== 3. /me GET (informational: 401 EXPECTED for free tokens) ==")
st, me = call("GET", "/api/v1/me?fields=id,email", headers=auth())
print("me:", st, json.dumps(me)[:200])

print("== 4. ads waiting_room ==")
ad = {"provider": "gravity", "surface": "waiting_room", "messages": [],
      "sessionId": str(uuid.uuid4()),
      "device": {"os": "windows", "timezone": "Asia/Jakarta", "locale": "en-US"},
      "userAgent": ("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36"
                    " (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")}
st, ads = call("POST", "/api/v1/ads", body=ad,
               headers={"Authorization": "Bearer " + TOKEN,
                        "Content-Type": "application/json",
                        "User-Agent": "Freebuff-CLI/0.1.0"})
n = len(ads.get("ads", [])) if isinstance(ads, dict) else "?"
print("ads:", st, f"count={n}")

print("== 5. session admission (BILLS THE HOUR) ==")
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
print("admission:", st, json.dumps(admit)[:400])
if st != 200:
    sys.exit("admission refused, aborting before spend")
admitted = admit.get("instanceId") or claim_id
print("admitted:", admitted, "balance:", (admit.get("freebucks") or {}).get("balance"))

print("== 6. agent-runs START ==")
st, start = call("POST", "/api/v1/agent-runs",
                 body={"action": "START", "agentId": AGENT, "ancestorRunIds": []},
                 headers=auth({"x-freebuff-acting-user-id": USER_ID,
                               "Content-Type": "application/json"}))
print("start:", st, json.dumps(start)[:200])
run_id = start["runId"]

print("== 7. chat (full gate envelope) ==")
with open("D:/github_repo/freebuff-proxy/devtools/mitm/flows.log",
          encoding="utf-8") as f:
    for i, line in enumerate(f, 1):
        if i == 176:
            exact = json.loads(json.loads(line)["body"])
            break
exact["codebuff_metadata"]["run_id"] = run_id
exact["codebuff_metadata"]["trace_session_id"] = str(uuid.uuid4())
exact["codebuff_metadata"]["freebuff_instance_id"] = admitted
exact["messages"] = [exact["messages"][0],
                     {"role": "user", "content": [{"type": "text",
                        "text": "What is 25 multiplied by 4? Answer directly."}]}]
req = urllib.request.Request(
    WWW + "/api/v1/chat/completions", data=json.dumps(exact).encode(),
    headers={"Authorization": "Bearer " + TOKEN,
             "x-freebuff-acting-user-id": USER_ID,
             "Content-Type": "application/json",
             "User-Agent": AI_SDK_UA, "Accept": "text/event-stream"},
    method="POST")
tool_calls, content = [], []
try:
    with urllib.request.urlopen(req, timeout=120) as r:
        print("chat status:", r.status)
        for line in r:
            s = line.decode("utf-8", "replace").strip()
            if s.startswith("data: ") and s != "data: [DONE]":
                try: delta = json.loads(s[6:])["choices"][0]["delta"]
                except Exception: continue
                if delta.get("tool_calls"): tool_calls.append(delta["tool_calls"])
                if delta.get("content"): content.append(delta["content"])
    print("chat stream done")
except urllib.error.HTTPError as e:
    print("chat HTTP", e.code, e.read()[:400].decode("utf-8", "replace"))
print("tool_calls:", json.dumps(tool_calls)[:300])
print("content:", "".join(content)[:300])

print("== 8. agent-runs FINISH (best effort) ==")
st, fin = call("POST", "/api/v1/agent-runs",
               body={"action": "FINISH", "runId": run_id, "status": "completed",
                     "totalSteps": 1, "directCredits": 0, "totalCredits": 0,
                     "steps": []},
               headers=auth({"x-freebuff-acting-user-id": USER_ID,
                             "Content-Type": "application/json"}))
print("finish:", st, json.dumps(fin)[:200])

print("== 9. session DELETE (release slot) ==")
st, end = call("DELETE", "/api/v1/freebuff/session/attempt",
               headers=auth({"x-freebuff-instance-id": admitted,
                             "x-freebuff-desktop-attempt-id": admitted[4:],
                             "x-fb-timezone": "Asia/Jakarta",
                             "x-freebuff-first-tab-discount": "0"}))
print("delete:", st, json.dumps(end)[:300] if not isinstance(end, str) else end[:300])
print("REPLAY COMPLETE")
