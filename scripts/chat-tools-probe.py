#!/usr/bin/env python3
"""Proxy-side chat + tool-calling probe (stdlib only).

Purpose: with model X through the local gateway, does plain chat work,
does the model call tools, and WHAT METHOD does the call use end to end.
Proxy-side only: no upstream token is needed (needs FP_API_KEY, proxy up).
Direct-mode (upstream) legs live in scripts/free-tier-gate-probe.py.

Setup (keys stay in the environment, never in files or output):
  export FP_API_KEY=<proxy client key>
  # proxy running, e.g. ./freebucks-proxy serve (default base below)
  scripts/chat-tools-probe.py --model deepseek/deepseek-v4-flash

What METHOD means here (proxy-side observable chain):
  client OpenAI tools[] -> proxy convert rename (backend/internal/convert,
  see docs/decisions/tool-name-translation.md) -> upstream chat/completions
  tool_calls deltas -> proxy restore -> client tool_calls (client names).
  This probe can only see the two ends: the offered tool names it sends and
  the called names the proxy returns (finish_reason tool_calls vs stop,
  choices[0].message.tool_calls[]). A called name equal to the offered name
  means the convert round-trip restored OK; an official-signature name
  leaking through instead means a restore bug (cite convert.ToolMapper).

Examples:
  FP_API_KEY=$K scripts/chat-tools-probe.py --model deepseek/deepseek-v4-flash
  FP_API_KEY=$K scripts/chat-tools-probe.py --model z-ai/glm-5.3-flash --tool-set official --verbose
  FP_API_KEY=$K scripts/chat-tools-probe.py --model deepseek/deepseek-v4-flash --tool-set custom my.tool --force-tool my.tool --no-stream

Exit codes:
  0  PASS -- health ok, gates 401, chat 200, tool leg(s) returned tool_calls
            with offered names restored
  1  FAIL -- any probe step unexpected (status/shape, stop instead of
            tool_calls, name leak/mismatch)
  2  usage-or-key-gate -- bad flags or missing FP_API_KEY (usage on stderr)
"""

import argparse
import json
import os
import sys
import time
import urllib.error
import urllib.request

BASE_DEFAULT = "http://127.0.0.1:3457"
READ_LIMIT = 1 << 20


def obj_schema(properties, required=()):
    return {
        "type": "object",
        "properties": properties,
        "required": list(required),
        "additionalProperties": False,
    }


def str_prop(desc):
    return {"type": "string", "description": desc}


CLASSIC_TOOLS = [
    ("bash", "Run a shell command", obj_schema(
        {"command": str_prop("Shell command to run")}, ("command",))),
    ("read", "Read a file", obj_schema(
        {"path": str_prop("File path to read")}, ("path",))),
    ("edit", "Edit a file", obj_schema(
        {"path": str_prop("File path"),
         "old_text": str_prop("Text to replace"),
         "new_text": str_prop("Replacement text")},
        ("path", "old_text", "new_text"))),
    ("todo", "Track a task", obj_schema(
        {"task": str_prop("Task summary")}, ("task",))),
]

OFFICIAL_NAMES = [
    "read_files", "str_replace", "write_file", "run_terminal_command",
    "code_search", "glob", "list_directory", "write_todos", "web_search",
    "read_url", "ask_user", "suggest_followups", "gravity_index",
    "render_ui", "skill", "report_project_profile",
]


def official_tools():
    return [(n, "Official tool " + n,
             obj_schema({"input": str_prop("Tool input")}))
            for n in OFFICIAL_NAMES]


def custom_tools(names):
    return [(n, "Custom probe tool " + n,
             obj_schema({"input": str_prop("Tool input")}))
            for n in names]


def parse_args(argv):
    parser = argparse.ArgumentParser(
        description="Probe proxy-side chat + tool-calling method "
                    "chain (needs FP_API_KEY, proxy running).")
    parser.add_argument("--model", required=True,
                        help="proxy model id, e.g. deepseek/deepseek-v4-flash")
    parser.add_argument("--base", default=BASE_DEFAULT,
                        help="proxy base URL (default %(default)s)")
    parser.add_argument("--tool-set", nargs="+", default=["classic"],
                        metavar="SET",
                        help="classic | official | custom NAME... "
                             "(default: classic)")
    parser.add_argument("--stream", dest="stream", action="store_true",
                        default=None,
                        help="run the streaming tool leg only")
    parser.add_argument("--no-stream", dest="stream", action="store_false",
                        help="run the non-streaming tool leg only "
                             "(default: run both legs)")
    parser.add_argument("--force-tool", default="",
                        help="tool to force via prompt + tool_choice "
                             "(default: first tool of the set)")
    parser.add_argument("--timeout", type=int, default=120,
                        help="HTTP timeout in seconds (default %(default)s)")
    parser.add_argument("--verbose", action="store_true",
                        help="request bodies + raw shapes (key masked "
                             "****last4, never full)")
    return parser.parse_args(argv)


def resolve_tools(spec):
    mode = spec[0].lower() if spec else "classic"
    if mode == "classic":
        if len(spec) > 1:
            print("usage: --tool-set classic takes no names",
                  file=sys.stderr)
            sys.exit(2)
        return CLASSIC_TOOLS
    if mode == "official":
        if len(spec) > 1:
            print("usage: --tool-set official takes no names",
                  file=sys.stderr)
            sys.exit(2)
        return official_tools()
    if mode == "custom":
        if len(spec) < 2:
            print("usage: --tool-set custom NAME... needs at least one name",
                  file=sys.stderr)
            sys.exit(2)
        return custom_tools(spec[1:])
    print("usage: --tool-set must start with classic|official|custom",
          file=sys.stderr)
    sys.exit(2)


def mask(key):
    tail = key[-4:] if len(key) >= 4 else "????"
    return "****" + tail


def tool_defs(tools):
    return [{"type": "function",
             "function": {"name": name, "description": desc,
                          "parameters": schema}}
            for name, desc, schema in tools]


def force_prompt(tool):
    return ("You must call the %s tool right now with a trivial argument. "
            "Do not reply with text, only the tool call." % tool)


def post_json(base, path, body, key, timeout):
    headers = {"Content-Type": "application/json"}
    if key is not None:
        headers["Authorization"] = "Bearer " + key
    data = json.dumps(body).encode()
    req = urllib.request.Request(base + path, data=data,
                                 headers=headers, method="POST")
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            raw = resp.read(READ_LIMIT)
    except urllib.error.HTTPError as exc:
        raw = exc.read(READ_LIMIT) if exc.fp is not None else b""
        return exc.code, raw.decode("utf-8", "replace")
    return 200, raw.decode("utf-8", "replace")


def short(text, limit=300):
    flat = " ".join(str(text).split())
    return flat if len(flat) <= limit else flat[:limit] + "..."


def main(argv):
    args = parse_args(argv)
    key = os.environ.get("FP_API_KEY", "")
    if not key:
        print("usage: chat-tools-probe.py --model <id> "
              "[--base URL] [--tool-set ...]", file=sys.stderr)
        print("error: FP_API_KEY is required (proxy API key via environment).",
              file=sys.stderr)
        return 2
    tools = resolve_tools(args.tool_set)
    names = [t[0] for t in tools]
    force = args.force_tool.strip() or names[0]
    if force not in names:
        print("error: --force-tool %s not in offered set %s"
              % (force, names), file=sys.stderr)
        return 2
    base = args.base.rstrip("/")
    fails = []
    both = args.stream is None

    if args.verbose:
        print("  key: %s base: %s model: %s tools: %s force: %s"
              % (mask(key), base, args.model, names, force), flush=True)

    # 1. health.
    try:
        with urllib.request.urlopen(base + "/healthz", timeout=10) as resp:
            health = json.load(resp)
    except Exception as exc:  # noqa: BLE001 -- probe must report, not crash
        print("health: UNREACHABLE %s" % short(exc), flush=True)
        print("VERDICT: FAIL", flush=True)
        return 1
    slots = health.get("tokens", []) or []
    quar = [t.get("quarantined") for t in slots]
    print("health: %s %s slots=%d quarantined=%s"
          % (health.get("status"), health.get("mode"),
             len(slots), quar), flush=True)
    if health.get("status") != "ok" or not slots:
        fails.append("health: pool has no healthy slots")

    # 2. key-gate.
    probe = {"model": args.model,
             "messages": [{"role": "user", "content": "hi"}]}
    for label, k in (("bogus", "bogus-probe-key"), ("noauth", None)):
        status, _ = post_json(base, "/v1/chat/completions", probe, k,
                              args.timeout)
        print("gate %s: %d" % (label, status), flush=True)
        if status != 401:
            fails.append("gate %s = %d, want 401" % (label, status))

    # 3. plain chat non-stream.
    status, raw = post_json(base, "/v1/chat/completions", probe, key,
                            args.timeout)
    plain = None
    if status != 200:
        print("plain: %d %s" % (status, short(raw)), flush=True)
        fails.append("plain chat = %d" % status)
    else:
        try:
            plain = json.loads(raw)
        except ValueError:
            plain = None
        if not isinstance(plain, dict) or not plain.get("choices"):
            print("plain: 200 BAD-SHAPE %s" % short(raw), flush=True)
            fails.append("plain chat: bad shape")
            plain = None
        else:
            choice = plain["choices"][0]
            msg = choice.get("message", {})
            usage = plain.get("usage", {})
            print("plain: 200 finish_reason=%s content_len=%d "
                  "usage=%s/%s/%s model=%s"
                  % (choice.get("finish_reason"),
                     len(msg.get("content") or ""),
                     usage.get("prompt_tokens"), usage.get("completion_tokens"),
                     usage.get("total_tokens"), plain.get("model")),
                  flush=True)

    # Shared tool-call body.
    body = {"model": args.model,
            "messages": [{"role": "user", "content": force_prompt(force)}],
            "tools": tool_defs(tools)}
    if args.force_tool.strip():
        body["tool_choice"] = {"type": "function",
                               "function": {"name": force}}
    if args.verbose:
        print("  tools body keys: %s tool_choice: %s"
              % (sorted(body.keys()),
                 short(body.get("tool_choice", "auto"))), flush=True)

    def report_nonstream(tag, resp):
        msg = resp["choices"][0].get("message", {})
        calls = msg.get("tool_calls") or []
        got = [c.get("function", {}).get("name") for c in calls]
        previews = [short(c.get("function", {}).get("arguments", ""))
                    for c in calls]
        usage = resp.get("usage", {})
        print("%s: 200 finish_reason=%s calls=%s args=%s usage=%s/%s/%s" % (
            tag, resp["choices"][0].get("finish_reason"), got, previews,
            usage.get("prompt_tokens"), usage.get("completion_tokens"),
            usage.get("total_tokens")), flush=True)
        return got

    called_nonstream = None
    if both or args.stream is False:
        # 4. tools non-stream.
        status, raw = post_json(base, "/v1/chat/completions", body, key,
                                args.timeout)
        if status != 200:
            print("tools: %d %s" % (status, short(raw)), flush=True)
            fails.append("tools non-stream = %d" % status)
        else:
            try:
                resp = json.loads(raw)
            except ValueError:
                resp = None
            if not isinstance(resp, dict) or not resp.get("choices"):
                print("tools: 200 BAD-SHAPE %s" % short(raw), flush=True)
                fails.append("tools non-stream: bad shape")
            else:
                called_nonstream = report_nonstream("tools", resp)
                if resp["choices"][0].get("finish_reason") != "tool_calls":
                    fails.append("tools non-stream: finish_reason=%s, "
                                 "want tool_calls"
                                 % resp["choices"][0].get("finish_reason"))
                if force not in called_nonstream:
                    fails.append("tools non-stream: calls=%s, want %s "
                                 "(client name restored)"
                                 % (called_nonstream, force))

    called_stream = None
    stream_finish = None
    if both or args.stream is True:
        # 5. tools stream (SSE).
        sbody = dict(body, stream=True)
        headers = {"Content-Type": "application/json",
                   "Authorization": "Bearer " + key}
        req = urllib.request.Request(
            base + "/v1/chat/completions",
            data=json.dumps(sbody).encode(), headers=headers, method="POST")
        try:
            start = time.monotonic()
            with urllib.request.urlopen(req,
                                        timeout=args.timeout) as resp:
                first_ms = None
                frames = 0
                parts = {}  # index -> {id, name, args pieces}
                terminal = None
                finish = None
                buf = b""
                while True:
                    chunk = resp.read(4096)
                    if not chunk:
                        break
                    if first_ms is None:
                        first_ms = (time.monotonic() - start) * 1000.0
                    buf += chunk
                    while b"\n" in buf:
                        line, buf = buf.split(b"\n", 1)
                        line = line.decode("utf-8", "replace").strip()
                        if not line.startswith("data:"):
                            continue
                        payload = line[5:].strip()
                        if payload in ("[DONE]", "[done]"):
                            terminal = "[DONE]"
                            continue
                        frames += 1
                        try:
                            evt = json.loads(payload)
                        except ValueError:
                            continue
                        if isinstance(evt, dict) and evt.get("type") in (
                                "response.completed", "response.done"):
                            terminal = evt.get("type")
                        for ch in (evt.get("choices", [])
                                   if isinstance(evt, dict) else []):
                            delta = ch.get("delta", {}) or {}
                            if ch.get("finish_reason"):
                                finish = ch.get("finish_reason")
                            for tc in (delta.get("tool_calls") or []):
                                idx = tc.get("index", 0)
                                slot = parts.setdefault(
                                    idx, {"id": "", "name": "",
                                          "args": ""})
                                slot["id"] += str(tc.get("id") or "")
                                slot["name"] += str(
                                    (tc.get("function") or {}).get("name")
                                    or "")
                                slot["args"] += str(
                                    (tc.get("function") or {}).get(
                                        "arguments") or "")
                stream_finish = finish
        except urllib.error.HTTPError as exc:
            raw = (exc.read(READ_LIMIT) if exc.fp is not None
                   else b"").decode("utf-8", "replace")
            print("tools-stream: %d %s" % (exc.code, short(raw)), flush=True)
            fails.append("tools stream = %d" % exc.code)
        else:
            called_stream = [slot["name"] for _, slot in sorted(parts.items())
                             if slot["name"]]
            stitch = ["%s args_len=%d" % (slot["name"], len(slot["args"]))
                      for _, slot in sorted(parts.items())]
            print("tools-stream: 200 frames=%d t_first_ms=%d "
                  "finish_reason=%s terminal=%s calls=%s stitch=[%s]" % (
                      frames, round(first_ms or 0), finish, terminal,
                      called_stream, "; ".join(stitch)), flush=True)
            if finish != "tool_calls":
                fails.append("tools stream: finish_reason=%s, "
                             "want tool_calls" % finish)
            if terminal not in ("[DONE]",):
                fails.append("tools stream: terminal=%s, want [DONE]"
                             % terminal)
            if force not in called_stream:
                fails.append("tools stream: calls=%s, want %s "
                             "(client name restored)"
                             % (called_stream, force))

    # 6. name-restore matrix.
    observed = called_nonstream if called_nonstream is not None \
        else called_stream
    if observed is None:
        print("matrix: (no tool leg ran green)", flush=True)
    else:
        for offered in names:
            if offered in observed:
                print("matrix: %s=>%s RESTORED" % (offered, offered),
                      flush=True)
            elif observed and offered == force:
                print("matrix: %s=>%s LEAK (official signature name leaked "
                      "-- see convert.ToolMapper)" % (offered, observed),
                      flush=True)
        if force not in observed:
            fails.append("matrix: forced tool %s not in called %s"
                         % (force, observed))

    if args.verbose:
        print("  stream_finish=%s nonstream=%s stream=%s"
              % (stream_finish, called_nonstream, called_stream), flush=True)

    if fails:
        print("VERDICT: FAIL", flush=True)
        for item in fails:
            print(" - %s" % item, flush=True)
        return 1
    print("VERDICT: PASS", flush=True)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
