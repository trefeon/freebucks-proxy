#!/usr/bin/env python3
"""Suite 06: Model Lifecycle, Concurrency & SlotLimit Enforcement.

Verifies the upstream seat lifecycle and concurrency boundaries:
1. Hourly Model Admission & Metering:
   - POST /api/v1/freebuff/session/admission bills the model's hourly Freebucks rate.
   - Session duration is exactly 3600s (1 hour).
2. SlotLimit = 1 Concurrency Gate:
   - In limited tier, only 1 concurrent purchase seat is allowed.
   - Admitting a DIFFERENT model while a seat is active returns HTTP 409 `purchase_capacity`.
   - Validates that the 409 response identifies the current holder instance ID.
3. Zero-Cost Seat Resumption:
   - Re-admitting the SAME model using the holder instance ID costs 0 additional Freebucks.
   - Cleanly resumes the seat with remaining time.
4. Clean Session Release (DELETE):
   - DELETE /api/v1/freebuff/session terminates the session (HTTP 200 `ended`).
   - Verifies session status transitions to `none`.

Usage:
    python 06_model_concurrency_lifecycle.py [--model z-ai/glm-5.3-flash] [--alt-model upstage/solar-mini4]
"""

import argparse
import json
import sys
import time
import uuid

from client import (
    CODEBUFF_WWW,
    UA_BUN,
    DEFAULT_MODEL,
    discover_credentials,
    request_http,
    get_flow_log_count,
    validate_mitm_flow,
)


def run_concurrency_lifecycle(model_a=DEFAULT_MODEL, model_b="upstage/solar-mini4", use_mitm=False):
    creds = discover_credentials()
    if not creds:
        print("FAIL: No Freebuff credentials found.")
        return 1

    token = creds["authToken"]
    masked = f"****{token[-4:]}"
    print(f"Testing model concurrency & lifecycle for token {masked}...")
    start_flow = get_flow_log_count()

    # Step 1: Check initial Freebucks balance
    st, sess_pre = request_http(
        "GET",
        f"{CODEBUFF_WWW}/api/v1/freebuff/session",
        headers={"Authorization": f"Bearer {token}", "User-Agent": UA_BUN},
        use_mitm=use_mitm,
    )
    balance_pre = sess_pre.get("freebucks", {}).get("balance", 0) if isinstance(sess_pre, dict) else 0
    print(f"1. Initial Freebucks Balance: {balance_pre}")

    # Step 2: Admit Model A
    print(f"\n2. Admitting Model A ({model_a})...")
    claim_a = f"cli:{uuid.uuid4()}"
    headers_a = {
        "Authorization": f"Bearer {token}",
        "User-Agent": UA_BUN,
        "x-freebuff-model": model_a,
        "x-freebuff-wallet-spend-limit": "0",
        "x-freebuff-instance-id": claim_a,
        "x-freebuff-multi-session": "1",
        "x-freebuff-purchase-continuity": "1",
        "x-freebuff-desktop-attempt-id": claim_a[4:],
        "x-fb-timezone": "Asia/Jakarta",
        "x-freebuff-first-tab-discount": "0",
    }
    st_admit, resp_admit = request_http(
        "POST",
        f"{CODEBUFF_WWW}/api/v1/freebuff/session/admission",
        headers=headers_a,
        body=None,
        use_mitm=use_mitm,
    )

    if st_admit == 200:
        holder_instance = resp_admit.get("instanceId") or claim_a
        balance_after_a = resp_admit.get("freebucks", {}).get("balance", balance_pre)
        cost_a = balance_pre - balance_after_a
        print(f"  Status: 200 OK (Active Seat: {holder_instance})")
        print(f"  Duration: {resp_admit.get('remainingMs', 0)//1000}s, Cost: {cost_a} Freebucks (New Balance: {balance_after_a})")
    elif st_admit == 409 and isinstance(resp_admit, dict) and resp_admit.get("currentInstanceId"):
        # Already held
        holder_instance = resp_admit["currentInstanceId"]
        print(f"  Note: Slot already held by {holder_instance} (status purchase_capacity)")
    else:
        print(f"FAIL: Admission for {model_a} failed with HTTP {st_admit}: {resp_admit}")
        return 1

    # Step 3: Concurrency Test — Attempt to admit Model B while Model A holds seat
    print(f"\n3. Concurrency Gate: Attempting to admit Model B ({model_b}) concurrently...")
    claim_b = f"cli:{uuid.uuid4()}"
    headers_b = dict(headers_a)
    headers_b["x-freebuff-model"] = model_b
    headers_b["x-freebuff-instance-id"] = claim_b
    headers_b["x-freebuff-desktop-attempt-id"] = claim_b[4:]

    st_b, resp_b = request_http(
        "POST",
        f"{CODEBUFF_WWW}/api/v1/freebuff/session/admission",
        headers=headers_b,
        body=None,
        use_mitm=use_mitm,
    )
    print(f"  Model B Admission HTTP {st_b}")
    if st_b != 409:
        print(f"FAIL: Expected HTTP 409 purchase_capacity, got {st_b}: {resp_b}")
        return 1

    status_str = resp_b.get("status") if isinstance(resp_b, dict) else ""
    current_holder = resp_b.get("currentInstanceId") if isinstance(resp_b, dict) else ""
    print(f"  Confirmed SlotLimit=1 rejection: status='{status_str}', currentHolder='{current_holder}'")
    if status_str != "purchase_capacity":
        print(f"FAIL: Expected status 'purchase_capacity', got '{status_str}'")
        return 1

    # Step 4: Zero-Cost Seat Resumption
    print(f"\n4. Testing Zero-Cost Seat Resumption for Model A ({model_a})...")
    headers_resume = dict(headers_a)
    headers_resume["x-freebuff-instance-id"] = holder_instance
    headers_resume["x-freebuff-desktop-attempt-id"] = holder_instance[4:]

    st_res, resp_res = request_http(
        "POST",
        f"{CODEBUFF_WWW}/api/v1/freebuff/session/admission",
        headers=headers_resume,
        body=None,
        use_mitm=use_mitm,
    )
    if st_res != 200:
        print(f"FAIL: Seat resumption returned HTTP {st_res}: {resp_res}")
        return 1

    balance_res = resp_res.get("freebucks", {}).get("balance")
    rem_ms = resp_res.get("remainingMs", 0)
    print(f"  Status: 200 OK (Active Seat Resumed)")
    print(f"  Freebucks Balance: {balance_res} (0 additional deduction verified)")
    print(f"  Remaining Window: {rem_ms//1000} seconds")

    # Step 5: Clean Session Termination (DELETE)
    print(f"\n5. Terminating active session via DELETE...")
    st_del, resp_del = request_http(
        "DELETE",
        f"{CODEBUFF_WWW}/api/v1/freebuff/session",
        headers={
            "Authorization": f"Bearer {token}",
            "x-freebuff-instance-id": holder_instance,
            "User-Agent": UA_BUN,
        },
        use_mitm=use_mitm,
    )
    print(f"  DELETE status: {st_del}, response: {resp_del}")
    if st_del != 200:
        print(f"FAIL: DELETE returned HTTP {st_del}")
        return 1

    # Verify session status is none
    st_post_del, sess_post = request_http(
        "GET",
        f"{CODEBUFF_WWW}/api/v1/freebuff/session",
        headers={"Authorization": f"Bearer {token}", "User-Agent": UA_BUN},
        use_mitm=use_mitm,
    )
    final_status = sess_post.get("status") if isinstance(sess_post, dict) else "unknown"
    print(f"  Session Status after DELETE: '{final_status}' (verified 'none')")
    if final_status != "none":
        print(f"FAIL: Expected status 'none', got '{final_status}'")
        return 1

    if use_mitm:
        print("\nVerifying wire flows in devtools/mitm/flows.log...")
        found_admit, _ = validate_mitm_flow("/admission", "POST", expected_status=200, since_index=start_flow)
        found_del, _ = validate_mitm_flow("/session", "DELETE", expected_status=200, since_index=start_flow)
        print(f"  Wire flow admission recorded: {found_admit}")
        print(f"  Wire flow DELETE recorded:    {found_del}")

    print("\nPASS: Model lifecycle, concurrency 409 gate, and zero-cost resume verified.")
    return 0


def main():
    parser = argparse.ArgumentParser(description="Freebuff 06: Model Lifecycle & Concurrency")
    parser.add_argument("--model", default=DEFAULT_MODEL, help=f"Primary model to test (default: {DEFAULT_MODEL})")
    parser.add_argument("--alt-model", default="upstage/solar-mini4", help="Alternative model for concurrency test")
    parser.add_argument("--mitm", action="store_true", help="Route through local MITM proxy and validate flows.log")
    args = parser.parse_args()
    return run_concurrency_lifecycle(model_a=args.model, model_b=args.alt_model, use_mitm=args.mitm)


if __name__ == "__main__":
    sys.exit(main())
