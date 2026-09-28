#!/usr/bin/env python3
"""Direct-vs-proxy free-tier gate probe (stdlib only).

Purpose: isolate proxy bugs from upstream free-tier refusals by replaying
the verified upstream wire chain directly (like the genuine CLI) and, in
proxy mode, the same turn through the local gateway. Compare the two legs
side by side to see which side refuses.

Token setup (never commit tokens, never print them):
  python3 scripts/device-login.py start   # open the printed URL, approve
  python3 scripts/device-login.py poll    # saves D:/tmp/fb-device-token.json
  export FB_AUTH_TOKEN=<authToken from that file>   # preferred, or:
  scripts/free-tier-gate-probe.py --mode direct --token-file D:/tmp/fb-device-token.json ...
  # --token-file must point OUTSIDE the repo. The poll-saved file also
  # carries the account "id", used as x-freebuff-acting-user-id when
  # --acting-user-id is not given.

Cost warning: direct DRY-RUN (default) spends nothing -- it only GETs
session/streak//me. Direct --spend ADMITS A REAL SESSION AND BILLS THE
HOUR on the token's account (first-tab offers aside). Pass --release to
DELETE the session afterwards (early-end refund receipt printed).

Examples:
  # dry-run reachability, no spend:
  scripts/free-tier-gate-probe.py --mode direct --model deepseek/deepseek-v4-flash
  # full billing leg against upstream (warns, bills the hour):
  scripts/free-tier-gate-probe.py --mode direct --model z-ai/glm-5.3-flash --spend --release
  # through the local gateway (needs FP_API_KEY, no upstream token):
  scripts/free-tier-gate-probe.py --mode proxy --model deepseek/deepseek-v4-flash
  # side-by-side isolation (proxy leg, then direct dry-run):
  scripts/free-tier-gate-probe.py --mode both --model deepseek/deepseek-v4-flash

Exit codes:
  0  PASS -- leg green (dry-run: session+streak 200; spend: chat 200; proxy: 200)
  1  FAIL -- unexpected status/shape on some step
  2  usage/token/API-key gate -- missing token or key, bad CLI flags, proxy 401
  3  HELD -- upstream 409: seat held (purchase_capacity / in_use); nothing spent
  4  GATE -- upstream 503 gate refusal on chat; see the printed checklist

Wire references: docs/LIVE-CAPTURE.md (per-turn chain, redirect + UA rules),
docs/MITM-CAPTURE.md (rig), docs/CLI-WIRE-TRACE.md (admission headers, chat
and agent-runs shapes), docs/UPSTREAM-CLI.md (session modes, agent maps),
docs/UPSTREAM-PORT-QUEUE.md (port status).
"""

import argparse
import datetime
import json
import os
import random
import string
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid

BUN_UA = "Bun/1.3.14"
CHAT_UA = ("ai-sdk/openai-compatible/0.0.0-test/codebuff"
           " ai-sdk/provider-utils/3.0.25 runtime/browser")
ADS_UA = "Freebuff-CLI/0.1.0"
BROWSER_UA = ("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36"
              " (KHTML, like Gecko) Chrome/151.0.0.0 Safari/537.36")
UPSTREAM_DEFAULT = "https://codebuff.com"
REDIRECT_CODES = (301, 302, 303, 307, 308)
MAX_HOPS = 3
SHAPE_LIMIT = 300
READ_LIMIT = 1 << 20

# Per-model agent-run roots (docs/LIVE-CAPTURE.md: base3-free-<model-slug>).
AGENT_FOR_MODEL = {
    "z-ai/glm-5.3-flash": "base3-free-glm-5-3-flash",
    "deepseek/deepseek-v4-flash": "base3-free-deepseek-flash",
    "upstage/solar-mini4": "base3-free-solar-mini-4",
    "stealth/space-bunny-alpha": "base3-free-space-bunny-alpha",
}

# The 16 official tool declarations the free-mode gate expects on chat.
TOOL_NAMES_16 = [
    "read_files", "str_replace", "write_file", "run_terminal_command",
    "code_search", "glob", "list_directory", "write_todos", "web_search",
    "read_url", "ask_user", "suggest_followups", "gravity_index",
    "render_ui", "skill", "report_project_profile",
]

BASE3_PROMPT_TEMPLATE = """You are Buffy, the coding agent behind Codebuff. You help users with software engineering tasks: fixing bugs, adding functionality, refactoring, and explaining code.

Current date: %s.

- Match the project's existing conventions. Verify a library is already used in the project before employing it.
- Prefer editing existing files over creating new ones. Make the fewest changes that address the request.
- Verify non-trivial changes by running the project's typecheck and relevant tests.
- Use write_todos to plan and track multi-step tasks.
- Your responses are displayed in a terminal. Keep them short and concise.
- Don't run destructive or hard-to-undo commands (git push, resets, deploys) unless the user asks for them.

# Working with the user

- **Ask about important decisions:** Use the ask_user tool to collaborate with the user on non-obvious choices — alternate implementation strategies, ambiguous requirements. Gather context first, and skip it when the answer is obvious or the detail can be changed later.
- **Suggest next steps:** At the end of your turn, use the suggest_followups tool to suggest ~3 next steps the user might want to take. Keep every suggestion short and goal-oriented: one sentence naming the outcome you want, not the steps to get there.

# Freebuff Meta-information

You are running on the %s model.

You are the AI agent behind Freebuff, a tool where users can chat with you to code with AI for free. See freebuff.com for more information about the product.

# System Info

Operating System: %s
Shell: bash
Chrome: installed

<user_shell_config_files>
</user_shell_config_files>

The following are the most recently read files according to the OS atime. This is cached from the start of this conversation:
<recently_read_file_paths_most_recent_first>
</recently_read_file_paths_most_recent_first>"""

GATE_CHECKLIST = (
    "gate checklist (docs/CLI-WIRE-TRACE.md section 4): base3 system prompt "
    "block at messages[0] + Current date line; all 16 official tool defs; "
    "repo_snapshot {gitAvailable,fileCount}; cost_mode free; 13-char "
    "client_id; x-freebuff-acting-user-id = token's own id; ai-sdk chat UA; "
    "stream true; provider data_collection deny"
)


def short(obj, limit=SHAPE_LIMIT):
    try:
        text = obj if isinstance(obj, str) else json.dumps(
            obj, separators=(",", ":"), default=str)
    except (TypeError, ValueError):
        text = str(obj)
    text = " ".join(text.split())
    if len(text) > limit:
        return text[:limit] + "..."
    return text


class NoRedirect(urllib.request.HTTPRedirectHandler):
    """Refuse auto-follow so the caller re-POSTs with auth intact."""

    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


_OPENER = urllib.request.build_opener(NoRedirect)


def raw_call(method, url, headers, body, timeout):
    """One HTTP call with manual bare=>www redirect (max 3 hops).

    urllib strips Authorization on cross-host redirects by default, so
    redirects are followed by hand, re-issuing the same method, headers
    and body at the Location (load-bearing per docs/LIVE-CAPTURE.md).
    Returns (status, headers_dict, raw_bytes, final_url).
    """
    hops = 0
    while True:
        req = urllib.request.Request(url, data=body, headers=dict(headers),
                                     method=method)
        try:
            with _OPENER.open(req, timeout=timeout) as resp:
                raw = resp.read(READ_LIMIT)
                return resp.status, dict(resp.headers), raw, url
        except urllib.error.HTTPError as exc:
            raw = exc.read(READ_LIMIT) if exc.fp is not None else b""
            if exc.code in REDIRECT_CODES and hops < MAX_HOPS:
                location = exc.headers.get("Location", "")
                if location:
                    url = urllib.parse.urljoin(url, location)
                    hops += 1
                    continue
            return exc.code, dict(exc.headers), raw, url


def decode_body(raw):
    try:
        return json.loads(raw.decode("utf-8", "replace"))
    except (ValueError, UnicodeDecodeError):
        return raw.decode("utf-8", "replace")


class Ctx:
    def __init__(self, args):
        self.args = args
        self.step_no = 0
        self.verbose = args.verbose

    def begin(self, name):
        self.step_no += 1
        print("== %d. %s ==" % (self.step_no, name), flush=True)

    def report(self, name, status, detail=""):
        line = "%s: %s %s" % (name, status, short(detail))
        print(line.strip(), flush=True)

    def debug(self, headers, body_obj):
        if not self.verbose:
            return
        print("  headers: %s" % sorted(headers.keys()), flush=True)
        if isinstance(body_obj, dict):
            print("  body keys: %s" % sorted(body_obj.keys()), flush=True)

    def mask(self, token):
        tail = token[-4:] if len(token) >= 4 else "????"
        return "****" + tail


def parse_args(argv):
    parser = argparse.ArgumentParser(
        description="Probe the upstream free-tier gate directly and/or "
                    "through the local proxy. Dry-run spends nothing; "
                    "--spend BILLS THE HOUR.")
    parser.add_argument("--mode", choices=("direct", "proxy", "both"),
                        default="direct")
    parser.add_argument("--model", default="deepseek/deepseek-v4-flash")
    parser.add_argument("--agent", default="",
                        help="agent-runs root id; default maps from --model")
    parser.add_argument("--acting-user-id", default="",
                        help="x-freebuff-acting-user-id; default from "
                             "--token-file id, then GET /me")
    parser.add_argument("--token-file", default="",
                        help="path OUTSIDE the repo to JSON with authToken "
                             "(from device-login.py poll)")
    parser.add_argument("--base", default="http://127.0.0.1:3457",
                        help="proxy base URL for --mode proxy|both")
    parser.add_argument("--api-key", default="",
                        help="proxy API key; default $FP_API_KEY")
    parser.add_argument("--upstream", default=UPSTREAM_DEFAULT,
                        help="upstream app host for --mode direct|both")
    parser.add_argument("--timezone", default="Asia/Jakarta",
                        help="x-fb-timezone declaration on session calls")
    parser.add_argument("--prompt", default="Reply with exactly: gate probe ok")
    parser.add_argument("--spend", action="store_true",
                        help="run the billing leg: admission + chat "
                             "(BILLS THE HOUR)")
    parser.add_argument("--release", action="store_true",
                        help="DELETE the admitted session afterwards")
    parser.add_argument("--timeout", type=int, default=60)
    parser.add_argument("--verbose", action="store_true",
                        help="header names + body key lists (never values)")
    return parser.parse_args(argv)


def load_token(args):
    """Token from env (preferred) or --token-file. Returns (token, user_id)."""
    token = os.environ.get("FB_AUTH_TOKEN", "").strip()
    user_id = (args.acting_user_id or "").strip()
    if args.token_file:
        try:
            with open(args.token_file, encoding="utf-8") as handle:
                saved = json.load(handle)
        except (OSError, ValueError) as exc:
            print("cannot read --token-file: %s" % exc, file=sys.stderr)
            sys.exit(2)
        if not token and isinstance(saved, dict):
            token = str(saved.get("authToken", "")).strip()
        if not user_id and isinstance(saved, dict):
            user_id = str(saved.get("id", "")).strip()
    return token, user_id


def need_token(args):
    token, user_id = load_token(args)
    if not token:
        print("usage: free-tier-gate-probe.py --mode direct|proxy|both "
              "--model <id>", file=sys.stderr)
        print("error: upstream authToken required: set FB_AUTH_TOKEN or pass "
              "--token-file <path-outside-repo> (JSON with authToken, e.g. "
              "from scripts/device-login.py poll).", file=sys.stderr)
        sys.exit(2)
    return token, user_id


def session_headers(token, timezone):
    return {
        "Authorization": "Bearer " + token,
        "User-Agent": BUN_UA,
        "x-fb-timezone": timezone,
        "x-freebuff-first-tab-discount": "0",
    }


def direct_dry_run(ctx, token, user_id):
    """Reachability without the /me-as-gate: session + streak must 200."""
    args = ctx.args
    base = args.upstream.rstrip("/")
    ok = True

    ctx.begin("free-tier session reachability (dry-run, no spend)")
    headers = session_headers(token, args.timezone)
    st, _, raw, _ = raw_call("GET", base + "/api/v1/freebuff/session",
                             headers, None, args.timeout)
    ctx.debug(headers, decode_body(raw))
    body = decode_body(raw)
    status_word = "none|active" if st == 200 else "refused"
    ctx.report("session", "%d %s %s" % (st, status_word, short(body)),
               body)
    if st != 200:
        ok = False

    st, _, raw, _ = raw_call("GET", base + "/api/v1/freebuff/streak",
                             dict(headers), None, args.timeout)
    sbody = decode_body(raw)
    ctx.debug(headers, sbody)
    ctx.report("streak", "%d %s" % (st, short(sbody)), sbody)
    if st != 200:
        ok = False

    me_headers = {"Authorization": "Bearer " + token, "User-Agent": BUN_UA}
    st, _, raw, _ = raw_call(
        "GET", base + "/api/v1/me?fields=id,email", me_headers, None,
        args.timeout)
    mbody = decode_body(raw)
    note = ("EXPECTED-401-for-free-tokens" if st == 401
            else ("id-linked" if st == 200 else "unexpected"))
    ctx.report("me", "%d %s %s" % (st, note, short(mbody)), mbody)
    if st == 200 and isinstance(mbody, dict):
        found = (mbody.get("id") or (mbody.get("user") or {}).get("id")
                 if isinstance(mbody.get("user"), dict) else mbody.get("id"))
        if found and not user_id:
            user_id = str(found)

    if ok:
        print("PASS: session+streak 200 (dry-run green; /me informational "
              "only)", flush=True)
        return 0, user_id
    print("FAIL: session or streak refused upstream", flush=True)
    return 1, user_id


def ads_waiting_room(ctx, token):
    """Waiting-room auction leg: gravity, empty messages, client-minted id."""
    args = ctx.args
    url = args.upstream.rstrip("/") + "/api/v1/ads"
    session_id = str(uuid.uuid4())
    payload = {
        "provider": "gravity",
        "messages": [],
        "sessionId": session_id,
        "device": {"os": "windows", "timezone": args.timezone,
                   "locale": "en-US"},
        "userAgent": BROWSER_UA,
        "surface": "waiting_room",
    }
    headers = {"Authorization": "Bearer " + token,
               "User-Agent": ADS_UA,
               "Content-Type": "application/json"}
    ctx.begin("ads waiting_room auction")
    st, _, raw, _ = raw_call("POST", url, headers,
                             json.dumps(payload).encode(), args.timeout)
    body = decode_body(raw)
    ctx.debug(headers, body if isinstance(body, dict) else {})
    ctx.report("ads-waiting-room", "%d %s" % (st, short(body)), body)
    return st, body


def admit(ctx, token, model):
    """Admission POST: no body, full x-freebuff-* headers."""
    args = ctx.args
    instance_id = "cli:" + str(uuid.uuid4())
    attempt_id = instance_id[len("cli:"):]
    headers = session_headers(token, args.timezone)
    headers.update({
        "x-freebuff-model": model,
        "x-freebuff-wallet-spend-limit": "0",
        "x-freebuff-instance-id": instance_id,
        "x-freebuff-multi-session": "1",
        "x-freebuff-purchase-continuity": "1",
        "x-freebuff-desktop-attempt-id": attempt_id,
    })
    ctx.begin("session admission (billing leg -- BILLS THE HOUR)")
    url = args.upstream.rstrip("/") + "/api/v1/freebuff/session/admission"
    st, _, raw, _ = raw_call("POST", url, headers, None, args.timeout)
    body = decode_body(raw)
    ctx.debug(headers, body if isinstance(body, dict) else {})
    ctx.report("admission", "%d %s" % (st, short(body)), body)
    admitted = instance_id
    if st == 200 and isinstance(body, dict):
        for key in ("instanceId", "instance_id"):
            if body.get(key):
                admitted = str(body[key])
                break
        nested = body.get("session") if isinstance(body.get("session"),
                                                   dict) else None
        if admitted == instance_id and nested:
            for key in ("instanceId", "instance_id"):
                if nested.get(key):
                    admitted = str(nested[key])
                    break
    return st, body, admitted


def agent_id_for(args):
    if args.agent:
        return args.agent
    if args.model in AGENT_FOR_MODEL:
        return AGENT_FOR_MODEL[args.model]
    slug = "".join(ch if ch.isalnum() else "-" for ch in
                   args.model.split("/", 1)[-1].lower()).strip("-")
    return "base3-free-" + slug


def start_run(ctx, token, agent, user_id):
    args = ctx.args
    headers = {"Authorization": "Bearer " + token, "User-Agent": BUN_UA,
               "Content-Type": "application/json"}
    if user_id:
        headers["x-freebuff-acting-user-id"] = user_id
    payload = {"action": "START", "agentId": agent, "ancestorRunIds": []}
    ctx.begin("agent-runs START")
    st, _, raw, _ = raw_call(
        "POST", args.upstream.rstrip("/") + "/api/v1/agent-runs", headers,
        json.dumps(payload).encode(), args.timeout)
    body = decode_body(raw)
    ctx.debug(headers, payload)
    ctx.report("agent-runs-start", "%d %s" % (st, short(body)), body)
    run_id = ""
    if isinstance(body, dict) and body.get("runId"):
        run_id = str(body["runId"])
    return st, run_id


def fresh_client_id():
    alphabet = string.ascii_lowercase + string.digits
    return "".join(random.choice(alphabet) for _ in range(13))


def chat_body(model, agent, run_id, instance_id, prompt):
    today = datetime.date.today().isoformat()
    system = BASE3_PROMPT_TEMPLATE % (today, model, os.name)
    tools = [{"type": "function",
              "function": {"name": name, "description": name,
                           "parameters": {"type": "object",
                                          "properties": {}}}}
             for name in TOOL_NAMES_16]
    return {
        "model": model,
        "codebuff_metadata": {
            "freebuff_instance_id": instance_id,
            "freebuff_multi_session": "1",
            "surface": "cli",
            "run_id": run_id,
            "trace_session_id": str(uuid.uuid4()),
            "client_id": fresh_client_id(),
            "cost_mode": "free",
            "llm_step_number": "1",
            "repo_snapshot": {"gitAvailable": True, "fileCount": 1},
        },
        "provider": {"data_collection": "deny"},
        "messages": [
            {"role": "system", "content": system},
            {"role": "user",
             "content": [{"type": "text", "text": prompt}]},
        ],
        "tools": tools,
        "tool_choice": "auto",
        "stream": True,
    }


def chat_stream(ctx, token, payload, user_id):
    """Chat POST with the full gate shape; summarize the first SSE lines."""
    args = ctx.args
    headers = {"Authorization": "Bearer " + token, "User-Agent": CHAT_UA,
               "Content-Type": "application/json",
               "Accept": "text/event-stream"}
    if user_id:
        headers["x-freebuff-acting-user-id"] = user_id
    ctx.begin("chat completions (stream)")
    body_bytes = json.dumps(payload).encode()
    url = args.upstream.rstrip("/") + "/api/v1/chat/completions"
    hops = 0
    while True:
        req = urllib.request.Request(url, data=body_bytes,
                                     headers=dict(headers), method="POST")
        try:
            resp = _OPENER.open(req, timeout=args.timeout)
            break
        except urllib.error.HTTPError as exc:
            raw = exc.read(READ_LIMIT) if exc.fp is not None else b""
            if exc.code in REDIRECT_CODES and hops < MAX_HOPS:
                location = exc.headers.get("Location", "")
                if location:
                    url = urllib.parse.urljoin(url, location)
                    hops += 1
                    continue
            ctx.debug(headers, payload)
            ctx.report("chat", "%d %s" % (exc.code, short(decode_body(raw))),
                       decode_body(raw))
            return exc.code, decode_body(raw), []
    data_lines = []
    chunks = 0
    try:
        with resp:
            if resp.status != 200:
                raw = resp.read(READ_LIMIT)
                body = decode_body(raw)
                ctx.debug(headers, payload)
                ctx.report("chat", "%d %s" % (resp.status, short(body)),
                           body)
                return resp.status, body, []
            for raw_line in resp:
                try:
                    line = raw_line.decode("utf-8", "replace").strip()
                except AttributeError:
                    line = str(raw_line).strip()
                if not line.startswith("data:"):
                    continue
                data = line[5:].strip()
                if data == "[DONE]":
                    break
                data_lines.append(data)
                chunks += 1
                if len(data_lines) >= 10 or chunks >= 200:
                    break
    except (OSError, ValueError) as exc:
        ctx.report("chat", "stream-read-error %s" % short(str(exc)), str(exc))
        return 1, str(exc), data_lines
    texts = []
    tool_names = []
    for data in data_lines:
        try:
            evt = json.loads(data)
        except ValueError:
            continue
        try:
            delta = evt["choices"][0].get("delta", {})
        except (KeyError, IndexError, TypeError, AttributeError):
            continue
        content = delta.get("content")
        if content:
            texts.append(str(content))
        for call in delta.get("tool_calls") or []:
            name = ((call.get("function") or {}).get("name")
                    if isinstance(call, dict) else "")
            if name:
                tool_names.append(str(name))
    summary = {"events": len(data_lines),
               "content": "".join(texts)[:200],
               "tool_calls": sorted(set(tool_names))}
    ctx.debug(headers, payload)
    ctx.report("chat", "200 stream %s" % short(summary), summary)
    return 200, summary, data_lines


def finish_run(ctx, token, run_id, user_id):
    args = ctx.args
    headers = {"Authorization": "Bearer " + token, "User-Agent": BUN_UA,
               "Content-Type": "application/json"}
    if user_id:
        headers["x-freebuff-acting-user-id"] = user_id
    payload = {"action": "FINISH", "runId": run_id, "status": "completed",
               "totalSteps": 2, "directCredits": 0, "totalCredits": 0,
               "steps": []}
    ctx.begin("agent-runs FINISH (best-effort)")
    st, _, raw, _ = raw_call(
        "POST", args.upstream.rstrip("/") + "/api/v1/agent-runs", headers,
        json.dumps(payload).encode(), args.timeout)
    body = decode_body(raw)
    ctx.report("agent-runs-finish", "%d %s" % (st, short(body)), body)


def release_session(ctx, token, instance_id):
    args = ctx.args
    headers = session_headers(token, args.timezone)
    if instance_id.startswith("cli:"):
        path = "/api/v1/freebuff/session/attempt"
        headers["x-freebuff-instance-id"] = instance_id
        headers["x-freebuff-multi-session"] = "1"
        headers["x-freebuff-purchase-continuity"] = "1"
        suffix = instance_id[len("cli:"):]
        if suffix:
            headers["x-freebuff-desktop-attempt-id"] = suffix
    else:
        path = "/api/v1/freebuff/session"
        if instance_id:
            headers["x-freebuff-instance-id"] = instance_id
    ctx.begin("session release")
    st, _, raw, _ = raw_call("DELETE", args.upstream.rstrip("/") + path,
                             headers, None, args.timeout)
    body = decode_body(raw)
    refund = ""
    if isinstance(body, dict):
        for key in ("freebucksRefund", "refund", "freebucksRefundPending",
                    "pendingRefund"):
            if body.get(key) is not None:
                refund = "%s=%s " % (key, body[key])
    ctx.report("release", "%d refund[%s] %s"
               % (st, refund.strip(), short(body)), body)


def direct_spend(ctx, token, user_id):
    args = ctx.args
    print("WARNING: --spend runs the billing leg and BILLS THE HOUR "
          "on this token's account.", flush=True)

    ctx.begin("free-tier streak (billing leg)")
    headers = session_headers(token, args.timezone)
    st, _, raw, _ = raw_call("GET",
                             args.upstream.rstrip("/") + "/api/v1/freebuff/streak",
                             dict(headers), None, args.timeout)
    sbody = decode_body(raw)
    ctx.report("streak", "%d %s" % (st, short(sbody)), sbody)
    if st != 200:
        print("FAIL: streak refused before any spend", flush=True)
        return 1

    ads_waiting_room(ctx, token)

    st, body, admitted = admit(ctx, token, args.model)
    if st == 409:
        print("HELD: seat held upstream -- %s" % short(body), flush=True)
        print("hint: the holder keeps the slot; re-run later or pass "
              "--release after your own active session ends.", flush=True)
        return 3
    if st != 200:
        print("FAIL: admission refused (%d)" % st, flush=True)
        return 1

    agent = agent_id_for(args)
    st, run_id = start_run(ctx, token, agent, user_id)
    if st != 200 or not run_id:
        print("FAIL: agent-runs START refused (%d)" % st, flush=True)
        if args.release:
            release_session(ctx, token, admitted)
        return 1

    payload = chat_body(args.model, agent, run_id, admitted, args.prompt)
    st, _, _ = chat_stream(ctx, token, payload, user_id)
    if st == 409:
        print("HELD: chat refused, seat held upstream", flush=True)
        return 3
    if st == 503:
        print("GATE: upstream gate refused the chat turn. " + GATE_CHECKLIST,
              flush=True)
        if args.release:
            release_session(ctx, token, admitted)
        return 4
    if st != 200:
        print("FAIL: chat refused (%s)" % st, flush=True)
        if args.release:
            release_session(ctx, token, admitted)
        return 1
    finish_run(ctx, token, run_id, user_id)
    if args.release:
        release_session(ctx, token, admitted)
    print("PASS: direct billing leg chat 200", flush=True)
    return 0


def proxy_leg(ctx):
    """Gateway leg: healthz slots, then one non-streamed chat turn."""
    args = ctx.args
    key = args.api_key or os.environ.get("FP_API_KEY", "")
    base = args.base.rstrip("/")
    if not key:
        ctx.begin("proxy gateway")
        ctx.report("proxy-auth", "missing",
                   "set FP_API_KEY or pass --api-key")
        return 2

    ctx.begin("proxy gateway")
    try:
        with urllib.request.urlopen(base + "/healthz",
                                    timeout=args.timeout) as resp:
            health = json.loads(resp.read().decode("utf-8", "replace"))
    except urllib.error.HTTPError as exc:
        ctx.report("proxy-healthz", "%d" % exc.code, exc.read(READ_LIMIT))
        return 1
    except OSError as exc:
        ctx.report("proxy-healthz", "unreachable %s" % short(str(exc)),
                   str(exc))
        return 1
    slots = health.get("tokens", []) if isinstance(health, dict) else []
    quarantined = [t.get("quarantined") for t in slots
                   if isinstance(t, dict)]
    ctx.report("proxy-healthz",
               "ok slots=%d quarantined=%s" % (len(slots), quarantined),
               health)

    payload = {"model": args.model,
               "messages": [{"role": "user", "content": args.prompt}],
               "stream": False}
    headers = {"Content-Type": "application/json",
               "Authorization": "Bearer " + key}
    st, _, raw, _ = raw_call("POST", base + "/v1/chat/completions", headers,
                             json.dumps(payload).encode(), args.timeout)
    body = decode_body(raw)
    if st == 401:
        ctx.report("proxy-chat", "401 bad-API-key gate", body)
        return 2
    if st == 409:
        ctx.report("proxy-chat",
                   "409 seat-held (upstream mapping) %s" % short(body), body)
        return 3
    if st == 503:
        ctx.report("proxy-chat",
                   "503 upstream refusal via proxy %s" % short(body), body)
        return 4
    if st != 200 or not isinstance(body, dict):
        ctx.report("proxy-chat", "%d %s" % (st, short(body)), body)
        return 1
    try:
        message = body["choices"][0].get("message", {})
    except (KeyError, IndexError, TypeError, AttributeError):
        message = {}
    calls = message.get("tool_calls") or []
    names = sorted({(c.get("function") or {}).get("name", "?")
                    for c in calls if isinstance(c, dict)})
    summary = {"content": str(message.get("content", ""))[:200],
               "tool_calls": names}
    ctx.report("proxy-chat", "200 %s" % short(summary), summary)
    print("PASS: proxy chat 200", flush=True)
    return 0


def main(argv=None):
    args = parse_args(argv if argv is not None else sys.argv[1:])
    if args.verbose:
        print("args: mode=%s model=%s base=%s upstream=%s spend=%s release=%s"
              % (args.mode, args.model, args.base, args.upstream, args.spend,
                 args.release), flush=True)
    started = time.strftime("%Y-%m-%dT%H:%M:%S")
    if args.mode == "proxy":
        ctx = Ctx(args)
        return proxy_leg(ctx)
    if args.mode == "both":
        ctx = Ctx(args)
        proxy_code = proxy_leg(ctx)
        token, user_id = need_token(args)
        if args.verbose:
            print("token: %s" % ctx.mask(token), flush=True)
        dry_code, _ = direct_dry_run(ctx, token, user_id)
        print("summary: proxy=%d direct-dry-run=%d started=%s"
              % (proxy_code, dry_code, started), flush=True)
        if proxy_code == 0 and dry_code == 0:
            print("PASS: both legs green", flush=True)
            return 0
        if proxy_code == 2 or dry_code == 2:
            return 2
        return 1
    token, user_id = need_token(args)
    ctx = Ctx(args)
    if args.verbose:
        print("token: %s" % ctx.mask(token), flush=True)
    if args.spend:
        return direct_spend(ctx, token, user_id)
    code, _ = direct_dry_run(ctx, token, user_id)
    return code


if __name__ == "__main__":
    sys.exit(main())
