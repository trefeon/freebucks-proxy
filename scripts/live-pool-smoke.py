#!/usr/bin/env python3
"""Live pooled smoke test for the freebuff-proxy gateway (stdlib only).

Exercises the real backend end to end through a client API key:
  healthz pool slots -> auth gate (bogus/no-auth must 401, no upstream cost)
  -> one chat with a foreign-named tool -> tool_calls name restored.

Usage (key stays in the environment, never in files or chat):
  FP_API_KEY=<test-key> python3 scripts/live-pool-smoke.py
  FP_BASE=http://127.0.0.1:3457 FP_MODEL=mimo/mimo-v2.5 FP_API_KEY=<k> python3 scripts/live-pool-smoke.py

Exit codes: 0 = full pass (200 + tool call restored),
            2 = transient upstream refusal (e.g. waiting room), rerun later,
            1 = real failure.
"""
import json
import os
import sys
import urllib.error
import urllib.request

BASE = os.environ.get("FP_BASE", "http://127.0.0.1:3457")
MODEL = os.environ.get("FP_MODEL", "deepseek/deepseek-v4-flash")
TOOL = os.environ.get("FP_TOOL", "demo.get-time")
KEY = os.environ.get("FP_API_KEY", "")

if not KEY:
    print("FP_API_KEY is required (pass it via the environment).")
    sys.exit(1)


def call(method, path, body=None, key=None):
    headers = {"Content-Type": "application/json"}
    if key is not None:
        headers["Authorization"] = "Bearer " + key
    req = urllib.request.Request(
        BASE + path,
        data=json.dumps(body).encode() if body is not None else None,
        headers=headers,
        method=method,
    )
    try:
        with urllib.request.urlopen(req, timeout=180) as r:
            raw = r.read().decode("utf-8", "replace")
            return r.status, json.loads(raw) if raw else None
    except urllib.error.HTTPError as e:
        return e.code, e.read()[:500].decode("utf-8", "replace")


def health():
    with urllib.request.urlopen(BASE + "/healthz", timeout=10) as r:
        return json.load(r)


fails = []

hz = health()
slots = hz.get("tokens", [])
print(f"health: {hz.get('status')} {hz.get('mode')} slots={len(slots)} "
      f"quarantined={[t.get('quarantined') for t in slots]}")
if hz.get("status") != "ok" or len(slots) == 0:
    fails.append("pool has no slots")
before = [(t.get("Requests"), t.get("SpendDay")) for t in slots]

probe = {"model": MODEL, "messages": [{"role": "user", "content": "hi"}]}
for name, k in (("bogus", "bogus-probe-key"), ("noauth", None)):
    st, _ = call("POST", "/v1/chat/completions", probe, key=k)
    print(f"gate {name}: {st}")
    if st != 401:
        fails.append(f"gate {name} = {st}, want 401")

chat = {"model": MODEL,
        "messages": [{"role": "user", "content":
                      f"You must call the {TOOL} tool with empty arguments right now. "
                      "Do not reply with text, only the tool call."}],
        "tools": [{"type": "function",
                   "function": {"name": TOOL, "description": "Returns current time",
                                "parameters": {"type": "object", "properties": {},
                                               "additionalProperties": False}}}]}
st, resp = call("POST", "/v1/chat/completions", chat, key=KEY)
print(f"chat: {st}")
if st == 503:
    print("transient upstream refusal:", resp)
elif st != 200:
    fails.append(f"chat = {st}: {resp}")
else:
    msg = resp["choices"][0].get("message", {})
    names = [c.get("function", {}).get("name") for c in (msg.get("tool_calls") or [])]
    print(f"finish={resp['choices'][0].get('finish_reason')} model={resp.get('model')} "
          f"tools={names} usage={resp.get('usage')}")
    if names != [TOOL]:
        fails.append(f"tool_calls = {names}, want [{TOOL}] (client name restored)")

after = [(t.get("Requests"), t.get("SpendDay")) for t in health().get("tokens", [])]
print(f"requests/spend {before} -> {after}")

if st == 503:
    sys.exit(2)
if fails:
    print("FAIL:")
    for f in fails:
        print(" -", f)
    sys.exit(1)
print("PASS: pooled chat + client tool-name restore through API key.")
