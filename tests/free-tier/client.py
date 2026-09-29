#!/usr/bin/env python3
"""Core shared client and fixture utilities for the Freebuff Free-Tier Test Suite.

Provides:
- Auto-discovery of active Freebuff authentication tokens and user credentials.
- Upstream request execution with automatic 307/301 redirect following (preserving Authorization).
- Exact wire User-Agents (Bun runtime on control plane, ai-sdk on chat completions).
- Standard canonical base3 prompt head and canonical 16-tool schema provider.
"""

import json
import os
import sys
import time
import uuid
import datetime
import urllib.request
import urllib.error
import urllib.parse
import ssl

CODEBUFF_WWW = "https://www.codebuff.com"
FREEBUFF_WWW = "https://freebuff.com"
MITM_CODEBUFF = "https://127.0.0.1:8443"
MITM_FREEBUFF = "https://127.0.0.1:8444"

MITM_CA_PATH = os.path.abspath(
    os.path.join(os.path.dirname(__file__), "../../devtools/mitm/ca-cert.pem")
)
FLOWS_LOG_PATH = os.path.abspath(
    os.path.join(os.path.dirname(__file__), "../../devtools/mitm/flows.log")
)
UA_BUN = "Bun/1.3.14"
UA_CHAT = "ai-sdk/openai-compatible/0.0.0-test/codebuff ai-sdk/provider-utils/3.0.25 runtime/browser"
UA_ADS = "Freebuff-CLI/0.1.0"
UA_DEVICE = "ai-sdk/openai-compatible/1.0.0/codebuff"

# Default models
DEFAULT_MODEL = "z-ai/glm-5.3-flash"
DEFAULT_AGENT = "base3-free-glm-5-3-flash"

AGENT_MAP = {
    "z-ai/glm-5.3-flash": "base3-free-glm-5-3-flash",
    "deepseek/deepseek-v4-flash": "base3-free-deepseek-flash",
    "upstage/solar-mini4": "base3-free-solar-mini-4",
    "upstage/solar-pro4": "base3-free-solar-pro-4",
    "mimo/mimo-v2.5": "base3-free-mimo-v2-5",
    "crof/kimi-k3-eco": "base3-free-kimi-k3-eco",
    "stealth/space-bunny-alpha": "base3-free-space-bunny-alpha",
}

TOKEN_PATHS = [
    os.path.expanduser("D:/tmp/fb-device-token.json"),
    os.path.expanduser("~/.config/manicode/credentials.json"),
    os.path.expanduser("~/.config/freebuff/credentials.json"),
]


def discover_credentials():
    """Auto-detects active Freebuff credentials from environment or disk.

    Returns:
        dict: {"authToken": str, "id": str, "email": str, "source": str}
        or None if no credentials found.
    """
    env_token = os.environ.get("FB_AUTH_TOKEN")
    if env_token:
        return {
            "authToken": env_token.strip(),
            "id": os.environ.get("FB_USER_ID", "").strip(),
            "email": os.environ.get("FB_USER_EMAIL", "").strip(),
            "source": "environment variable FB_AUTH_TOKEN",
        }

    for path in TOKEN_PATHS:
        if os.path.exists(path):
            try:
                with open(path, "r", encoding="utf-8") as f:
                    data = json.load(f)
                if "authToken" in data:
                    return {
                        "authToken": data["authToken"],
                        "id": data.get("id", ""),
                        "email": data.get("email", ""),
                        "name": data.get("name", ""),
                        "source": path,
                    }
                if "default" in data and isinstance(data["default"], dict):
                    acct = data["default"]
                    if acct.get("authToken"):
                        return {
                            "authToken": acct["authToken"],
                            "id": acct.get("id", ""),
                            "email": acct.get("email", ""),
                            "name": acct.get("name", ""),
                            "source": path,
                        }
            except Exception:
                continue

    return None


def get_canonical_system_prompt():
    """Builds the canonical base3 system instructions head required by upstream."""
    today_str = datetime.datetime.now().strftime("%B %d, %Y")
    return (
        "You are Buffy, the coding agent behind Codebuff. You help users with software "
        "engineering tasks: fixing bugs, adding functionality, refactoring, and explaining code.\n\n"
        f"Current date: {today_str}.\n\n"
        "- Match the project's existing conventions. Verify a library is already used in the project before employing it.\n"
        "- Prefer editing existing files over creating new ones. Make the fewest changes that address the request.\n"
        "- Verify non-trivial changes by running the project's typecheck and relevant tests.\n"
        "- Use write_todos to plan and track multi-step tasks.\n"
        "- Your responses are displayed in a terminal. Keep them short and concise.\n"
        "- Don't run destructive or hard-to-undo commands (git push, resets, deploys) unless the user asks for them.\n"
    )


def load_canonical_tools():
    """Loads the canonical 16 official tool schemas from the fixture file."""
    # Look for fixture in repo backend testdata
    search_paths = [
        os.path.join(
            os.path.dirname(__file__),
            "../../backend/internal/upstream/testdata/cli-tools.json",
        ),
        "D:/tmp/cli_tools.json",
    ]
    for p in search_paths:
        if os.path.exists(p):
            try:
                with open(p, "r", encoding="utf-8") as f:
                    tools = json.load(f)
                # Ensure skill tool does not carry huge session catalog
                for t in tools:
                    if t.get("function", {}).get("name") == "skill":
                        desc = t["function"].get("description", "")
                        if "<available_skills>" in desc:
                            start = desc.find("<available_skills>")
                            end = desc.find("</available_skills>") + len(
                                "</available_skills>"
                            )
                            t["function"]["description"] = desc[:start] + desc[end:]
                return tools
            except Exception:
                pass

    return []


def request_http(
    method,
    url,
    headers=None,
    body=None,
    timeout=60,
    max_redirects=3,
    stream=False,
    use_mitm=False,
):
    """Executes HTTP request, optionally routing through local MITM proxy and following 301/307 redirects."""
    cur_url = url
    cur_method = method
    cur_headers = dict(headers or {})
    payload_bytes = json.dumps(body).encode("utf-8") if body is not None else None

    is_mitm = use_mitm or os.environ.get("FREEBUFF_TEST_MITM") == "1"
    ssl_ctx = None

    if is_mitm:
        if os.path.exists(MITM_CA_PATH):
            ssl_ctx = ssl.create_default_context(cafile=MITM_CA_PATH)
        else:
            ssl_ctx = ssl._create_unverified_context()

        if cur_url.startswith("https://www.codebuff.com") or cur_url.startswith("https://codebuff.com"):
            cur_headers["Host"] = "127.0.0.1:8443"
            cur_url = cur_url.replace("https://www.codebuff.com", MITM_CODEBUFF).replace("https://codebuff.com", MITM_CODEBUFF)
        elif cur_url.startswith("https://freebuff.com"):
            cur_headers["Host"] = "freebuff.com"
            cur_url = cur_url.replace("https://freebuff.com", MITM_FREEBUFF)

    if payload_bytes and "Content-Type" not in cur_headers:
        cur_headers["Content-Type"] = "application/json"

    for _ in range(max_redirects + 1):
        req = urllib.request.Request(
            cur_url,
            data=payload_bytes if cur_method in ("POST", "PUT", "PATCH") else None,
            headers=cur_headers,
            method=cur_method,
        )

        class NoRedirectHandler(urllib.request.HTTPRedirectHandler):
            def redirect_request(self, req, fp, code, msg, headers, newurl):
                return None
        handlers = [NoRedirectHandler]
        if ssl_ctx is not None:
            handlers.append(urllib.request.HTTPSHandler(context=ssl_ctx))
        opener = urllib.request.build_opener(*handlers)
        try:
            resp = opener.open(req, timeout=timeout)
            if stream:
                return resp.status, resp
            raw = resp.read().decode("utf-8", "replace")
            try:
                data = json.loads(raw)
            except Exception:
                data = raw
            return resp.status, data
        except urllib.error.HTTPError as e:
            if e.code in (301, 302, 307, 308) and "Location" in e.headers:
                loc = e.headers["Location"]
                cur_url = urllib.parse.urljoin(cur_url, loc)
                continue
            raw = e.read().decode("utf-8", "replace")
            try:
                data = json.loads(raw)
            except Exception:
                data = raw
            return e.code, data

    return 500, {"error": "too_many_redirects"}

def iter_sse_lines(resp):
    """Yields full SSE text lines from an HTTP response stream."""
    buffer = b""
    while True:
        chunk = resp.read(4096)
        if not chunk:
            break
        buffer += chunk
        while b"\n" in buffer:
            line_bytes, buffer = buffer.split(b"\n", 1)
            line = line_bytes.decode("utf-8", errors="replace").strip()
            if line:
                yield line
    if buffer:
        tail = buffer.decode("utf-8", errors="replace").strip()
        if tail:
            yield tail


def get_flow_log_count():
    """Returns the total number of flows logged in devtools/mitm/flows.log."""
    if not os.path.exists(FLOWS_LOG_PATH):
        return 0
    try:
        with open(FLOWS_LOG_PATH, "r", encoding="utf-8") as f:
            return sum(1 for _ in f)
    except Exception:
        return 0


def get_mitm_flows(since_index=0):
    """Reads parsed flow records from flows.log starting at since_index."""
    if not os.path.exists(FLOWS_LOG_PATH):
        return []
    flows = []
    try:
        with open(FLOWS_LOG_PATH, "r", encoding="utf-8") as f:
            for i, line in enumerate(f):
                if i >= since_index:
                    line_clean = line.strip()
                    if line_clean:
                        try:
                            flows.append(json.loads(line_clean))
                        except Exception:
                            pass
    except Exception:
        pass
    return flows


def validate_mitm_flow(path_substr, method="POST", expected_status=200, since_index=0):
    """Validates that a matching flow appeared in flows.log since since_index.

    Returns:
        tuple: (found_bool, matching_flow_dict)
    """
    flows = get_mitm_flows(since_index=since_index)
    for flow in flows:
        url = flow.get("url", "")
        flow_method = flow.get("method", "")
        if path_substr in url:
            if str(expected_status) in str(flow_method) or str(flow.get("method")) == method:
                return True, flow
    return False, None
