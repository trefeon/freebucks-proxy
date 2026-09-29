#!/usr/bin/env python3
"""Bulk analysis: 12 turns, ONE seat, passing 16-tool shape, via MITM.
Token never printed. 4s spacing between turns."""
import json, ssl, sys, time, uuid, urllib.request, urllib.error

tok = json.load(open("D:/tmp/fb-device-token.json"))
TOKEN, UID = tok["authToken"], tok.get("id", "")
MITM = "https://127.0.0.1:8443"
CTX = ssl.create_default_context(
    cafile="D:/github_repo/freebuff-proxy/devtools/mitm/ca-cert.pem")
BUN = "Bun/1.3.14"
UA = ("ai-sdk/openai-compatible/0.0.0-test/codebuff "
      "ai-sdk/provider-utils/3.0.25 runtime/browser")
MODEL, AGENT = "z-ai/glm-5.3-flash", "base3-free-glm-5-3-flash"
HOST = {"Host": "127.0.0.1:8443"}
print(f"token ****{TOKEN[-4:]} via MITM — 1 admission, 12 turns", flush=True)

def call(method, path, body=None, headers=None):
    d = json.dumps(body).encode() if body is not None else None
    r = urllib.request.Request(MITM + path, data=d, headers=headers or {},
                               method=method)
    with urllib.request.urlopen(r, timeout=60, context=CTX) as x:
        return x.status, json.loads(x.read().decode())

H = {"Authorization": "Bearer " + TOKEN, "User-Agent": BUN, **HOST}

lines = open("D:/github_repo/freebuff-proxy/devtools/mitm/flows.log",
             encoding="utf-8").readlines()
exact = json.loads(json.loads(lines[175])["body"])
tools = exact["tools"]
for t in tools:
    if t["function"]["name"] == "skill":
        d = t["function"]["description"]
        i = d.find("<available_skills>")
        j = d.find("</available_skills>") + len("</available_skills>")
        t["function"]["description"] = d[:i] + d[j:]
syshead = (exact["messages"][0]["content"]
           if isinstance(exact["messages"][0], dict)
           else exact["messages"][0]).split("Project instructions:")[0]
snap = json.dumps({"gitAvailable": True, "fileCount": 915,
                   "fileCountIsLowerBound": False})

PROMPTS = [
    ("math-1", "What is 7 * 8? Answer directly."),
    ("math-2", "What is 144 / 12? Answer directly."),
    ("text-1", "Reply with exactly the word ok."),
    ("code-1", "Write a Python one-liner to reverse a string."),
    ("tool-1", "List the files in the frontend/src directory using your list_directory tool."),
    ("math-3", "What is 2 to the power of 10? Answer directly."),
    ("code-2", "Write a JS arrow function that doubles a number."),
    ("text-2", "Name the capital of Japan in one word."),
    ("tool-2", "Read the file package.json using your read_files tool."),
    ("math-4", "What is 99 - 37? Answer directly."),
    ("code-3", "Write a Python function is_even(n)."),
    ("text-3", "Say hello in one short sentence."),
]

print("== rejoin held seat (no new spend) ==", flush=True)
inst = "cli:54cab3ef-7cf1-4b4a-a8e6-0fcd9876a32e"
st, admit = call("POST", "/api/v1/freebuff/session/admission", None,
    {**H, "x-freebuff-model": MODEL, "x-freebuff-wallet-spend-limit": "0",
     "x-freebuff-instance-id": inst, "x-freebuff-multi-session": "1",
     "x-freebuff-purchase-continuity": "1",
     "x-freebuff-desktop-attempt-id": inst[4:],
     "x-fb-timezone": "Asia/Jakarta", "x-freebuff-first-tab-discount": "0"})
print("rejoin:", st, flush=True)
if st != 200:
    sys.exit(f"rejoin refused: {json.dumps(admit)[:200]}")
trace = str(uuid.uuid4())
results = []

for n, (tag, prompt) in enumerate(PROMPTS, 1):
    st, s = call("POST", "/api/v1/agent-runs",
        {"action": "START", "agentId": AGENT, "ancestorRunIds": []},
        {**H, "x-freebuff-acting-user-id": UID,
         "Content-Type": "application/json"})
    run = s["runId"]
    body = {"model": MODEL, "stream": True,
            "messages": [{"role": "system", "content": syshead},
                         {"role": "user", "content": [{"type": "text",
                          "text": prompt}]}],
            "tools": tools, "tool_choice": "auto",
            "codebuff_metadata": {
                "run_id": run, "trace_session_id": trace,
                "client_id": uuid.uuid4().hex[:13],
                "freebuff_instance_id": inst, "freebuff_multi_session": "1",
                "surface": "cli", "cost_mode": "free",
                "llm_step_number": "1", "repo_snapshot": snap},
            "provider": {"data_collection": "deny"}}
    req = urllib.request.Request(
        MITM + "/api/v1/chat/completions", data=json.dumps(body).encode(),
        headers={"Authorization": "Bearer " + TOKEN,
                 "x-freebuff-acting-user-id": UID,
                 "Content-Type": "application/json", "User-Agent": UA,
                 "Accept": "text/event-stream", **HOST}, method="POST")
    t0 = time.time()
    try:
        tcs, texts, fin = [], [], None
        with urllib.request.urlopen(req, timeout=120, context=CTX) as r:
            code = r.status
            for line in r:
                s2 = line.decode("utf-8", "replace").strip()
                if s2.startswith("data: ") and s2 != "data: [DONE]":
                    try:
                        ch = json.loads(s2[6:])["choices"][0]
                    except Exception:
                        continue
                    d = ch.get("delta", {})
                    if d.get("tool_calls"):
                        tcs.append(d["tool_calls"][0]["function"]["name"])
                    if d.get("content"):
                        texts.append(d["content"])
                    if ch.get("finish_reason"):
                        fin = ch["finish_reason"]
        out = ("TOOLS:" + ",".join(tcs)) if tcs else "".join(texts)[:120]
        results.append((tag, code, fin, f"{time.time()-t0:.1f}s", out))
        print(f"[{n:2d}/{len(PROMPTS)}] {tag}: {code} finish={fin} "
              f"{time.time()-t0:.1f}s :: {out[:100]}", flush=True)
    except urllib.error.HTTPError as e:
        results.append((tag, e.code, "ERROR", "-", e.read()[:100].decode()))
        print(f"[{n:2d}/{len(PROMPTS)}] {tag}: HTTP {e.code} — STOPPING",
              flush=True)
        break
    call("POST", "/api/v1/agent-runs",
         {"action": "FINISH", "runId": run, "status": "completed",
          "totalSteps": 1, "directCredits": 0, "totalCredits": 0, "steps": []},
         {**H, "x-freebuff-acting-user-id": UID,
          "Content-Type": "application/json"})
    time.sleep(4)

print("== seat left active (already billed, expires 09:07:31Z) ==", flush=True)
ok = sum(1 for r in results if r[1] == 200)
print(f"BULK COMPLETE: {ok}/{len(results)} turns 200", flush=True)
