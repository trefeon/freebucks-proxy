#!/usr/bin/env python3
"""Suite 01: Authentication & Device Login Flow.

Capabilities:
1. Auto-detection of existing tokens (environment, D:/tmp/fb-device-token.json, ~/.config/manicode/credentials.json).
2. Live validation check (verifying if auto-detected token is alive via session GET).
3. Interactive device login flow (requests device code, outputs verbatim loginUrl, polls live until approval).
4. Saves authenticated credentials to disk.

Usage:
    python 01_auth_device_login.py [--fresh] [--timeout 300]
"""

import argparse
import base64
import json
import os
import sys
import time
import urllib.parse

from client import (
    FREEBUFF_WWW,
    CODEBUFF_WWW,
    UA_BUN,
    UA_DEVICE,
    TOKEN_PATHS,
    discover_credentials,
    request_http,
)


def start_device_code():
    """Requests a new device OAuth login code from Freebuff."""
    fp_id = "enhanced-" + base64.urlsafe_b64encode(os.urandom(32)).decode().rstrip("=")
    body = {"fingerprintId": fp_id}
    url = f"{FREEBUFF_WWW}/api/auth/cli/code"

    st, resp = request_http(
        "POST",
        url,
        headers={"Content-Type": "application/json", "User-Agent": UA_DEVICE},
        body=body,
    )
    if st != 200 or not isinstance(resp, dict) or "loginUrl" not in resp:
        print(f"FAILED to request device login code (HTTP {st}): {resp}")
        sys.exit(1)

    return {
        "fingerprintId": fp_id,
        "fingerprintHash": resp["fingerprintHash"],
        "expiresAt": str(resp["expiresAt"]),
        "loginUrl": resp["loginUrl"],
    }


def poll_device_status(auth_state, max_attempts=60, interval=5):
    """Polls Freebuff for user OAuth authorization with live attempt counter."""
    params = {
        "fingerprintId": auth_state["fingerprintId"],
        "fingerprintHash": auth_state["fingerprintHash"],
        "expiresAt": auth_state["expiresAt"],
    }
    url = f"{FREEBUFF_WWW}/api/auth/cli/status?{urllib.parse.urlencode(params)}"

    print(f"Waiting for authorization (up to {max_attempts * interval}s)...")
    for attempt in range(1, max_attempts + 1):
        sys.stdout.write(f"\r[{attempt:02d}/{max_attempts}] polling approval...")
        sys.stdout.flush()

        st, data = request_http("GET", url, headers={"User-Agent": UA_DEVICE})
        if st == 200 and isinstance(data, dict):
            user = data.get("user")
            if user and user.get("authToken"):
                sys.stdout.write(" [APPROVED]\n")
                sys.stdout.flush()
                return user

        time.sleep(interval)

    sys.stdout.write("\n")
    return None


def save_credentials(user_info, fp_id=""):
    """Saves authenticated credentials to standard local locations."""
    payload = {
        "authToken": user_info["authToken"],
        "id": user_info.get("id", ""),
        "email": user_info.get("email", ""),
        "name": user_info.get("name", ""),
        "fingerprintId": fp_id or user_info.get("fingerprintId", ""),
    }

    # Save to primary temp file
    primary_tmp = TOKEN_PATHS[0]
    os.makedirs(os.path.dirname(primary_tmp), exist_ok=True)
    with open(primary_tmp, "w", encoding="utf-8") as f:
        json.dump(payload, f, indent=2)

    # Save to manicode credentials.json for CLI compatibility
    manicode_path = TOKEN_PATHS[1]
    os.makedirs(os.path.dirname(manicode_path), exist_ok=True)
    manicode_data = {
        "default": {
            "id": payload["id"],
            "name": payload["name"],
            "email": payload["email"],
            "authToken": payload["authToken"],
            "fingerprintId": payload["fingerprintId"],
        }
    }
    with open(manicode_path, "w", encoding="utf-8") as f:
        json.dump(manicode_data, f, indent=2)

    print(f"Credentials successfully saved to {primary_tmp} and {manicode_path}")


def verify_token_alive(token):
    """Verifies if an existing token can reach the session endpoint."""
    url = f"{CODEBUFF_WWW}/api/v1/freebuff/session"
    st, resp = request_http(
        "GET",
        url,
        headers={"Authorization": f"Bearer {token}", "User-Agent": UA_BUN},
        timeout=15,
    )
    if st == 200 and isinstance(resp, dict):
        return True, resp
    return False, resp


def main():
    parser = argparse.ArgumentParser(
        description="Freebuff 01: Authentication & Device Login Flow"
    )
    parser.add_argument(
        "--fresh",
        action="store_true",
        help="Force fresh device login even if valid token is detected",
    )
    parser.add_argument(
        "--timeout",
        type=int,
        default=300,
        help="Max timeout in seconds to wait for browser authorization (default 300)",
    )
    args = parser.parse_args()

    print("==================================================")
    print(" Freebuff Free-Tier Test: 01 Device Authentication")
    print("==================================================")

    if not args.fresh:
        creds = discover_credentials()
        if creds:
            masked = f"****{creds['authToken'][-4:]}"
            print(f"Detected existing token {masked} (source: {creds['source']})")
            print("Verifying if token is alive...")
            alive, sess_data = verify_token_alive(creds["authToken"])
            if alive:
                fb = sess_data.get("freebucks", {})
                balance = fb.get("balance", "unknown")
                spent = fb.get("daily", {}).get("spent", 0)
                print(f"TOKEN IS HEALTHY AND ACTIVE!")
                print(f"  Account Email: {creds.get('email') or 'unknown'}")
                print(f"  User ID:       {creds.get('id') or 'unknown'}")
                print(f"  Status:        {sess_data.get('status')}")
                print(f"  Freebucks:     {balance} remaining (daily spent: {spent})")
                print("PASS: Auto-detection successful.")
                return 0
            else:
                print(f"Detected token is dead or expired (status {sess_data})")
                print("Falling back to fresh device login flow...\n")

    # Interactive device login
    print("Starting fresh device login flow...")
    auth_state = start_device_code()
    print("\n--------------------------------------------------")
    print("👉 OPEN THIS IN BROWSER TO APPROVE:")
    print(auth_state["loginUrl"])
    print("--------------------------------------------------\n")

    attempts = max(1, args.timeout // 5)
    user_info = poll_device_status(auth_state, max_attempts=attempts, interval=5)
    if not user_info:
        print(f"FAIL: Timed out waiting for approval after {args.timeout}s.")
        return 1

    masked = f"****{user_info['authToken'][-4:]}"
    print(f"\nAuthentication successful!")
    print(f"  Account: {user_info.get('name')} ({user_info.get('email')})")
    print(f"  Token:   {masked}")
    print(f"  User ID: {user_info.get('id')}")

    save_credentials(user_info, auth_state["fingerprintId"])
    print("\nPASS: Fresh authentication completed.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
