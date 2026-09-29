#!/usr/bin/env python3
"""Unified Master Test Runner for Freebuff Free-Tier Lifecycle Test Suite.

Runs and reports the complete lifecycle suites:
  01: Authentication & Auto-Detection (or device OAuth login)
  02: Account Entitlements, Model Catalog & Freebucks Quota Audit
  03: Real-Time Streaming Chat & Thinking Reasoning Tokens
  04: Two-Step Agentic Tool Calling Loop (tool_call -> tool_result -> final synthesis)
  05: Agentic Harness Tool Normalizer (OMP bash/read mapping & client name restoration)
  06: Model Lifecycle, Concurrency (slotLimit=1 409 gate) & 0-Cost Seat Resumption

Usage:
    python run_all.py [--steps 1,2,3,4,5,6] [--mitm] [--model z-ai/glm-5.3-flash] [--fresh-login]
"""

import argparse
import os
import subprocess
import sys
import time

SCRIPT_DIR = os.path.dirname(os.path.abspath(__file__))

SUITES = [
    {
        "step": 1,
        "name": "Auth & Device Login",
        "script": "01_auth_device_login.py",
        "desc": "Auto-detects token or runs device code OAuth flow",
    },
    {
        "step": 2,
        "name": "Entitlements & Quota",
        "script": "02_entitlements_check.py",
        "desc": "Audits session, streak, /me, Freebucks balance & catalog",
    },
    {
        "step": 3,
        "name": "Streaming & Reasoning",
        "script": "03_chat_and_reasoning.py",
        "desc": "Verifies live streaming chunks and reasoning tokens",
    },
    {
        "step": 4,
        "name": "Agentic Tool Loop",
        "script": "04_agentic_tool_loop.py",
        "desc": "Verifies 2-step agentic loop: tool call -> execution -> synthesis",
    },
    {
        "step": 5,
        "name": "Harness Tool Normalizer",
        "script": "05_harness_tools_normalizer.py",
        "desc": "Verifies OMP tool mapping (bash -> run_terminal_command) & client restoration",
    },
    {
        "step": 6,
        "name": "Model Concurrency & Lifecycle",
        "script": "06_model_concurrency_lifecycle.py",
        "desc": "Verifies 1h purchase seat, slotLimit=1 409 gate, 0-cost resume & release",
    },
]


def start_mitm_if_needed():
    """Starts local MITM proxy process if not already running."""
    # Check if port 8443 is listening
    import socket
    s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    try:
        s.connect(("127.0.0.1", 8443))
        s.close()
        return None  # already running
    except Exception:
        pass

    mitm_script = os.path.abspath(os.path.join(SCRIPT_DIR, "../../devtools/mitm/mitm.py"))
    if os.path.exists(mitm_script):
        proc = subprocess.Popen([sys.executable, mitm_script], stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        time.sleep(2)
        return proc
    return None


def run_suite(suite, model, mitm=False, fresh_login=False):
    script_path = os.path.join(SCRIPT_DIR, suite["script"])
    cmd = [sys.executable, script_path]

    if suite["step"] == 1 and fresh_login:
        cmd.append("--fresh")
    elif suite["step"] in (3, 4, 5, 6) and model:
        cmd.extend(["--model", model])

    if mitm and suite["step"] in (5, 6):
        cmd.append("--mitm")

    t0 = time.time()
    res = subprocess.run(cmd, cwd=SCRIPT_DIR)
    elapsed = time.time() - t0
    passed = res.returncode == 0
    return passed, elapsed


def main():
    parser = argparse.ArgumentParser(description="Freebuff Complete Free-Tier Test Suite Master Runner")
    parser.add_argument("--steps", default="1,2,3,4,5,6", help="Comma-separated step numbers to execute (default: 1,2,3,4,5,6)")
    parser.add_argument("--mitm", action="store_true", help="Enable MITM proxy routing and live wire logs validation")
    parser.add_argument("--model", default="z-ai/glm-5.3-flash", help="Model ID to evaluate")
    parser.add_argument("--fresh-login", action="store_true", help="Force fresh device code login in Step 1")
    args = parser.parse_args()

    selected_steps = set()
    for s in args.steps.split(","):
        s_clean = s.strip()
        if s_clean.isdigit():
            selected_steps.add(int(s_clean))

    print("====================================================================")
    print(" FREEBUFF FREE-TIER LIFECYCLE COMPREHENSIVE TEST SUITE")
    print("====================================================================")
    print(f"Target Model:  {args.model}")
    print(f"Active Steps:  {sorted(list(selected_steps))}")
    print(f"MITM Logging:  {'ENABLED' if args.mitm else 'DISABLED'}")
    print("====================================================================\n")

    mitm_proc = None
    if args.mitm:
        print("Ensuring MITM proxy listener on 127.0.0.1:8443...")
        mitm_proc = start_mitm_if_needed()
        if mitm_proc:
            print("Local MITM proxy started successfully.")
        else:
            print("MITM proxy already listening on 8443.")

    report = []
    overall_pass = True

    try:
        for suite in SUITES:
            step_num = suite["step"]
            if step_num not in selected_steps:
                report.append((step_num, suite["name"], "SKIPPED", 0.0, "Excluded by --steps"))
                continue

            print(f"\n>>> EXECUTING SUITE {step_num:02d}: {suite['name'].upper()} <<<")
            print(f"Description: {suite['desc']}\n")

            passed, elapsed = run_suite(suite, args.model, mitm=args.mitm, fresh_login=args.fresh_login)
            status_str = "PASS" if passed else "FAIL"
            report.append((step_num, suite["name"], status_str, elapsed, suite["desc"]))

            if not passed:
                overall_pass = False
                print(f"\n[!] Suite {step_num} FAILED. Aborting subsequent tests.")
                break

            # Polite pause between suites to prevent burst rate limiting
            time.sleep(2)

    finally:
        if mitm_proc:
            mitm_proc.terminate()

    print("\n====================================================================")
    print(" FINAL FREE-TIER TEST SUITE REPORT")
    print("====================================================================")
    print(f"{'Step':<5} | {'Suite Name':<30} | {'Status':<8} | {'Duration':<8} | {'Description'}")
    print("-" * 78)
    for step, name, status, duration, desc in report:
        print(f"{step:<5} | {name:<30} | {status:<8} | {duration:>6.2f}s | {desc}")
    print("-" * 78)

    if overall_pass and len([r for r in report if r[2] == "PASS"]) > 0:
        print("OVERALL RESULT: ALL EXECUTED SUITES PASSED (100% OPERATIONAL)")
        return 0
    else:
        print("OVERALL RESULT: ONE OR MORE SUITES FAILED")
        return 1


if __name__ == "__main__":
    sys.exit(main())
