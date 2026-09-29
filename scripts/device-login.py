#!/usr/bin/env python3
"""Device-code login helper for the upstream CLI flow (stdlib only).

Step 1 (no auth needed) prints a browser login link:
  python3 scripts/device-login.py start
  -> open the printed URL, approve in the browser.

Step 2 polls until approval, then saves the fresh credential:
  python3 scripts/device-login.py poll
  -> writes D:/tmp/fb-device-token.json (authToken + id + email).

The saved file lives outside the repo on purpose: it holds a real
credential and must never be committed. Nothing here prints secrets.
"""
import base64
import json
import os
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

UA = "ai-sdk/openai-compatible/1.0.0/codebuff"
STATE_PATH = "D:/tmp/fb-device-pending.json"
TOKEN_PATH = "D:/tmp/fb-device-token.json"


def start():
    fp_id = "enhanced-" + base64.urlsafe_b64encode(os.urandom(32)).decode().rstrip("=")
    body = json.dumps({"fingerprintId": fp_id}).encode()
    req = urllib.request.Request(
        "https://freebuff.com/api/auth/cli/code", data=body,
        headers={"Content-Type": "application/json", "User-Agent": UA}, method="POST")
    with urllib.request.urlopen(req, timeout=30) as r:
        data = json.loads(r.read().decode())
    with open(STATE_PATH, "w") as f:
        json.dump({"fingerprintId": fp_id, "fingerprintHash": data["fingerprintHash"],
                   "expiresAt": str(data["expiresAt"])}, f)
    print("OPEN THIS IN BROWSER: " + data["loginUrl"])


def poll():
    with open(STATE_PATH) as f:
        st = json.load(f)
    url = ("https://freebuff.com/api/auth/cli/status?" + urllib.parse.urlencode(st))
    print(f"polling {url[:60]}... (60 attempts, 5s apart)", flush=True)
    for attempt in range(1, 61):
        print(f"[{attempt}/60] waiting for approval...", flush=True)
        time.sleep(5)
        req = urllib.request.Request(url, headers={"User-Agent": UA})
        try:
            with urllib.request.urlopen(req, timeout=30) as r:
                user = (json.loads(r.read().decode()).get("user") or {})
                if user.get("authToken"):
                    with open(TOKEN_PATH, "w") as f:
                        json.dump({"authToken": user["authToken"], "id": user.get("id"),
                                   "email": user.get("email"), "name": user.get("name")}, f)
                    print(f"APPROVED on attempt {attempt}, saved {TOKEN_PATH} "
                          f"(account: {user.get('email') or user.get('name') or '?'})")
                    return
                print(f"poll {attempt}: not yet approved")
        except urllib.error.HTTPError as e:
            if e.code != 401:
                print(f"poll HTTP {e.code}: {e.read().decode()[:200]}")
    print("timed out waiting for approval")


if __name__ == "__main__":
    if len(sys.argv) != 2 or sys.argv[1] not in ("start", "poll"):
        print("usage: device-login.py start|poll")
        sys.exit(1)
    (start if sys.argv[1] == "start" else poll)()
