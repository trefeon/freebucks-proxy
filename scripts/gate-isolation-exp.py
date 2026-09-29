import json, ssl, sys, uuid, urllib.request, urllib.error

tok = json.load(open("D:/tmp/fb-device-token.json"))
TOKEN = tok["authToken"]; UID = tok.get("id", "")
CTX = ssl.create_default_context(cafile="devtools/mitm/ca-cert.pem")
UA = ("ai-sdk/openai-compatible/0.0.0-test/codebuff "
      "ai-sdk/provider-utils/3.0.25 runtime/browser")
MODEL = "z-ai/glm-5.3-flash"; AGENT = "base3-free-glm-5-3-flash"
INST = "cli:3271f7ed-2aca-4120-9803-670500a5c314"
H = {"Authorization": "Bearer " + TOKEN, "User-Agent": "Bun/1.3.14",
     "Host": "127.0.0.1:8443"}

def start():
    d = json.dumps({"action": "START", "agentId": AGENT,
                    "ancestorRunIds": []}).encode()
    r = urllib.request.Request("https://127.0.0.1:8443/api/v1/agent-runs",
        data=d, headers={**H, "x-freebuff-acting-user-id": UID,
                         "Content-Type": "application/json"}, method="POST")
    with urllib.request.urlopen(r, context=CTX) as x:
        return json.loads(x.read().decode())["runId"]

lines = open("devtools/mitm/flows.log", encoding="utf-8").readlines()
exact = json.loads(json.loads(lines[175])["body"])
FULL_TOOLS = exact["tools"]          # full CLI schemas
SNAP = exact["codebuff_metadata"]["repo_snapshot"]
SYSP_RAW = exact["messages"][0]
SYSP = SYSP_RAW["content"] if isinstance(SYSP_RAW, dict) else SYSP_RAW
BASE3 = SYSP.split("Project instructions:")[0]  # canonical head: base3 + bullets

def chat(label, tools, meta_extra, prompt):
    run = start()
    meta = {"run_id": run, "trace_session_id": str(uuid.uuid4()),
            "client_id": uuid.uuid4().hex[:13],
            "freebuff_instance_id": INST, "freebuff_multi_session": "1",
            "surface": "cli", "cost_mode": "free", "llm_step_number": "1"}
    meta.update(meta_extra)
    body = {"model": MODEL, "stream": True,
            "messages": [{"role": "system", "content": BASE3},
                         {"role": "user", "content": [{"type": "text", "text": prompt}]}],
            "tools": tools, "tool_choice": "auto",
            "codebuff_metadata": meta, "provider": {"data_collection": "deny"}}
    req = urllib.request.Request("https://127.0.0.1:8443/api/v1/chat/completions",
        data=json.dumps(body).encode(),
        headers={"Authorization": "Bearer " + TOKEN,
                 "x-freebuff-acting-user-id": UID,
                 "Content-Type": "application/json", "User-Agent": UA,
                 "Accept": "text/event-stream", "Host": "127.0.0.1:8443"},
        method="POST")
    try:
        with urllib.request.urlopen(req, timeout=90, context=CTX) as r:
            n = 0
            for line in r:
                s = line.decode("utf-8", "replace").strip()
                if s.startswith("data: ") and s != "data: [DONE]":
                    n += 1
                    if n >= 3: break
            print(f"{label}: HTTP 200 ({n}+ chunks)")
    except urllib.error.HTTPError as e:
        print(f"{label}: HTTP {e.code} {e.read()[:160].decode()}")

# Exp A: full CLI tool schemas, NO snapshot
chat("A full-schemas+no-snapshot", FULL_TOOLS, {}, "What is 9 + 9? Answer directly.")
# Exp B: full CLI tool schemas + minimal snapshot
chat("B full-schemas+snapshot  ", FULL_TOOLS, {"repo_snapshot": SNAP}, "What is 9 + 9? Answer directly.")
