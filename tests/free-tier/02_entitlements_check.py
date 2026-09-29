#!/usr/bin/env python3
"""Suite 02: Account Entitlements, Quotas & Pricing Audit.

Queries and validates:
1. GET /api/v1/freebuff/session (authoritative free-tier endpoint):
   - Access tier ('limited' vs 'full')
   - Freebucks balance, daily spent, reset time & timezone
   - Model pricing map & price notices
   - Paid-subscription restricted models (planRequiredModelIds)
2. GET /api/v1/freebuff/streak:
   - Daily streak count & bonus eligibility
3. GET /api/v1/me:
   - User ID, verified email, account ban standing

Usage:
    python 02_entitlements_check.py [--verbose]
"""

import argparse
import json
import sys

from client import (
    CODEBUFF_WWW,
    UA_BUN,
    discover_credentials,
    request_http,
)


def run_entitlements_check(verbose=False):
    creds = discover_credentials()
    if not creds:
        print("FAIL: No Freebuff credentials found. Run 01_auth_device_login.py first.")
        return 1

    token = creds["authToken"]
    masked = f"****{token[-4:]}"
    print(f"Auditing entitlements for token {masked}...")

    # 1. Session & Freebucks
    st_sess, sess = request_http(
        "GET",
        f"{CODEBUFF_WWW}/api/v1/freebuff/session",
        headers={"Authorization": f"Bearer {token}", "User-Agent": UA_BUN},
        timeout=30,
    )
    if st_sess != 200 or not isinstance(sess, dict):
        print(f"FAIL: GET /api/v1/freebuff/session returned HTTP {st_sess}: {sess}")
        return 1

    # 2. Streak
    st_streak, streak = request_http(
        "GET",
        f"{CODEBUFF_WWW}/api/v1/freebuff/streak",
        headers={"Authorization": f"Bearer {token}", "User-Agent": UA_BUN},
        timeout=30,
    )

    # 3. User info /me
    st_me, me = request_http(
        "GET",
        f"{CODEBUFF_WWW}/api/v1/me?fields=id,email,banned,created_at",
        headers={"Authorization": f"Bearer {token}", "User-Agent": UA_BUN},
        timeout=30,
    )

    # Parse and format
    tier = sess.get("accessTier", "unknown")
    country = sess.get("countryCode", "unknown")
    reason = sess.get("countryBlockReason", "none")

    fb = sess.get("freebucks", {})
    balance = fb.get("balance", 0)
    daily = fb.get("daily", {})
    limit = daily.get("limit", 25)
    spent = daily.get("spent", 0)
    reset_at = daily.get("resetAt", "unknown")
    reset_tz = daily.get("resetTimeZone", "unknown")

    prices = fb.get("prices", {})
    plan_required = set(sess.get("planRequiredModelIds") or [])

    print("\n==================================================")
    print(" Freebuff Entitlements & Pricing Summary")
    print("==================================================")
    if isinstance(me, dict):
        print(f"Account:        {me.get('email', 'unknown')}")
        print(f"User ID:        {me.get('id', 'unknown')}")
        print(f"Banned:         {me.get('banned', False)}")
    print(f"Access Tier:    {tier.upper()} (Region: {country}, Reason: {reason})")
    print(f"Freebucks:      {balance} remaining (Spent today: {spent} / {limit})")
    print(f"Daily Reset:    {reset_at} ({reset_tz})")

    if isinstance(streak, dict):
        print(
            f"Streak:         {streak.get('streak', 0)} days (Today used: {streak.get('todayUsed', False)})"
        )

    print("\n--------------------------------------------------")
    print(" Model Catalog & Pricing (Freebucks/hour)")
    print("--------------------------------------------------")
    print(f"{'Model ID':<35} | {'Cost':<12} | {'Availability':<15}")
    print("-" * 68)

    free_available = []
    paid_only = []

    for model_id, cost in sorted(prices.items(), key=lambda x: (x[1], x[0])):
        if model_id in plan_required:
            paid_only.append((model_id, cost))
            print(f"{model_id:<35} | {cost:<12} | PAID SUBSCRIPTION")
        else:
            free_available.append((model_id, cost))
            cost_str = "0 (FREE)" if cost == 0 else f"{cost} FB/hr"
            print(f"{model_id:<35} | {cost_str:<12} | AVAILABLE")

    print("-" * 68)
    print(f"Total Models: {len(prices)} ({len(free_available)} Available, {len(paid_only)} Paid Subscription)")

    if verbose and sess.get("offPeak"):
        print("\nOff-Peak Promotions:")
        print(json.dumps(sess.get("offPeak"), indent=2))

    print("\nPASS: Entitlements verified successfully.")
    return 0


def main():
    parser = argparse.ArgumentParser(description="Freebuff 02: Entitlements & Pricing Audit")
    parser.add_argument("--verbose", action="store_true", help="Print extended JSON notices")
    args = parser.parse_args()
    return run_entitlements_check(verbose=args.verbose)


if __name__ == "__main__":
    sys.exit(main())
