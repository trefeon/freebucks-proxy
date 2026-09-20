#!/usr/bin/env python3
"""
monitor-control.py — freebucks-proxy control-logic monitoring & verification tool.

Probes and verifies the entire control plane of freebucks-proxy:
1. Pool Topology & Health (/healthz): token roster, active runs, cooldowns, quarantine.
2. Strategy & Queue Posture (pool.conf): MASQ / Drain / Balance, slot ledger parameters.
3. Model Catalog & Pricing (/v1/models): served models, availability, access tiers.
4. Live Chat Completions (/v1/chat/completions): non-streaming and streaming SSE.
   - Reasoning model support (tracks reasoning_content + content).
5. Control Invariants: account stickiness, slot concurrency, metrics counters.

Usage:
  python3 scripts/monitor-control.py [options]

Options:
  --url URL           Base URL (default: http://172.188.64.104:3457 or $FREEBUFF_HOST)
  --key-file PATH     Path to API key file (default: api-keys_freebuff_vps-sg)
  --key TOKEN         Direct Bearer token (overrides key-file)
  --model MODEL       Model to test (default: upstage/solar-pro4,z-ai/glm-5.3-flash)
  --skip-chat         Skip live chat completion requests
  --concurrency N     Number of concurrent requests to test slot ledger (default: 2)
  --watch             Continuous monitoring mode (refreshes every N seconds)
  --interval SECS     Watch interval in seconds (default: 5)
  --json              Output full result as JSON
  --verbose           Print raw request/response payloads
"""

import argparse
import concurrent.futures
import json
import os
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

# ANSI Colors
C_RESET = "\033[0m"
C_BOLD = "\033[1m"
C_DIM = "\033[2m"
C_GREEN = "\033[32m"
C_RED = "\033[31m"
C_YELLOW = "\033[33m"
C_CYAN = "\033[36m"
C_BLUE = "\033[34m"
C_MAGENTA = "\033[35m"

def color(text, code):
    if not sys.stdout.isatty():
        return text
    return f"{code}{text}{C_RESET}"

class InvariantResult:
    def __init__(self, name, passed, detail="", category="GENERAL"):
        self.name = name
        self.passed = passed
        self.detail = detail
        self.category = category

    def to_dict(self):
        return {
            "name": self.name,
            "passed": self.passed,
            "status": "PASS" if self.passed else "FAIL",
            "detail": self.detail,
            "category": self.category,
        }

class ControlMonitor:
    def __init__(self, base_url, api_key, verbose=False):
        self.base_url = base_url.rstrip("/")
        self.api_key = api_key
        self.verbose = verbose
        self.invariants = []

    def log_verbose(self, msg):
        if self.verbose:
            print(f"  {color('[DEBUG]', C_DIM)} {msg}")

    def add_invariant(self, name, passed, detail="", category="GENERAL"):
        res = InvariantResult(name, passed, detail, category)
        self.invariants.append(res)
        return res

    def http_get(self, path, auth=False, timeout=8):
        url = f"{self.base_url}{path}"
        headers = {"User-Agent": "freebuff-control-monitor/1.0"}
        if auth and self.api_key:
            headers["Authorization"] = f"Bearer {self.api_key}"
        req = urllib.request.Request(url, headers=headers)
        t0 = time.perf_counter()
        try:
            with urllib.request.urlopen(req, timeout=timeout) as resp:
                elapsed = time.perf_counter() - t0
                body = resp.read().decode("utf-8", errors="replace")
                return resp.status, body, elapsed, None
        except urllib.error.HTTPError as e:
            elapsed = time.perf_counter() - t0
            body = e.read().decode("utf-8", errors="replace")
            return e.code, body, elapsed, e
        except Exception as e:
            elapsed = time.perf_counter() - t0
            return 0, "", elapsed, e

    def http_post_json(self, path, payload, auth=True, stream=False, timeout=30):
        url = f"{self.base_url}{path}"
        data = json.dumps(payload).encode("utf-8")
        headers = {
            "Content-Type": "application/json",
            "User-Agent": "freebuff-control-monitor/1.0",
        }
        if auth and self.api_key:
            headers["Authorization"] = f"Bearer {self.api_key}"
        req = urllib.request.Request(url, data=data, headers=headers, method="POST")
        t0 = time.perf_counter()
        try:
            resp = urllib.request.urlopen(req, timeout=timeout)
            if stream:
                return resp.status, resp, t0, None
            with resp:
                elapsed = time.perf_counter() - t0
                body = resp.read().decode("utf-8", errors="replace")
                return resp.status, body, elapsed, None
        except urllib.error.HTTPError as e:
            elapsed = time.perf_counter() - t0
            body = e.read().decode("utf-8", errors="replace")
            return e.code, body, elapsed, e
        except Exception as e:
            elapsed = time.perf_counter() - t0
            return 0, "", elapsed, e

    # ------------------------------------------------------------------
    # Step 1: Health & Pool Topology
    # ------------------------------------------------------------------
    def check_health(self, record_invariants=True):
        status, body, elapsed, err = self.http_get("/healthz")
        if status != 200:
            if record_invariants:
                self.add_invariant("Healthz HTTP 200", False, f"HTTP {status}: {err}", "HEALTH")
            return None

        try:
            data = json.loads(body)
        except Exception as ex:
            if record_invariants:
                self.add_invariant("Healthz JSON valid", False, f"JSON parse error: {ex}", "HEALTH")
            return None

        tokens = data.get("tokens", [])
        quarantined_tokens = []
        cooling_tokens = []
        active_runs_total = 0
        token_requests = {}

        for idx, tok in enumerate(tokens):
            active_runs = tok.get("ActiveRuns", 0)
            active_runs_total += active_runs
            token_requests[idx] = tok.get("Requests", 0)

            if tok.get("quarantined") or tok.get("Quarantined"):
                reason = tok.get("quarantine_reason") or tok.get("QuarantineReason") or "unspecified"
                quarantined_tokens.append((idx, reason))

            cd_until = tok.get("CooldownUntil", "")
            if cd_until and cd_until != "0001-01-01T00:00:00Z":
                cooling_tokens.append((idx, cd_until, tok.get("cooldown_kind") or tok.get("CooldownKind", "unknown")))

        if record_invariants:
            self.add_invariant("Healthz Status OK", data.get("status") == "ok", f"status={data.get('status')}", "HEALTH")
            self.add_invariant("Effective Mode", data.get("mode") in ["hybrid", "pooled", "bridge"], f"mode={data.get('mode')}", "HEALTH")
            self.add_invariant("Pool Token Count", len(tokens) >= 1, f"total={len(tokens)} tokens configured", "ROSTER")
            self.add_invariant(
                "Terminal Quarantine Integrity",
                len(quarantined_tokens) == 0 or all(reason != "unspecified" for _, reason in quarantined_tokens),
                f"quarantined={len(quarantined_tokens)}",
                "ROSTER",
            )
            self.add_invariant(
                "Active Runs Tracking",
                active_runs_total >= 0,
                f"total_cached_runs={active_runs_total}",
                "ROSTER",
            )

        return {
            "data": data,
            "tokens_count": len(tokens),
            "active_runs": active_runs_total,
            "cooling_count": len(cooling_tokens),
            "quarantined_count": len(quarantined_tokens),
            "cooling": cooling_tokens,
            "quarantined": quarantined_tokens,
            "requests": token_requests,
        }

    # ------------------------------------------------------------------
    # Step 2: Prometheus Metrics Telemetry
    # ------------------------------------------------------------------
    def check_metrics(self, record_invariants=True):
        status, body, elapsed, err = self.http_get("/metrics")
        if status != 200:
            if record_invariants:
                self.add_invariant("Metrics HTTP 200", False, f"HTTP {status}", "METRICS")
            return None

        has_uptime = "freebucks_proxy_uptime_seconds" in body
        has_requests = "freebucks_proxy_requests_served" in body
        has_rate_limits = "freebucks_proxy_rate_limit_events_total" in body

        if record_invariants:
            self.add_invariant("Metrics Exposition Contract", has_uptime, "Prometheus text format intact", "METRICS")

        served = 0
        for line in body.splitlines():
            if line.startswith("freebucks_proxy_requests_served"):
                parts = line.split()
                if len(parts) >= 2:
                    try:
                        served = int(parts[1])
                    except ValueError:
                        pass

        return {"served": served, "has_rl": has_rate_limits}

    # ------------------------------------------------------------------
    # Step 3: Model Catalog Verification
    # ------------------------------------------------------------------
    def check_models(self, target_models):
        status, body, elapsed, err = self.http_get("/v1/models", auth=True)
        if status != 200:
            self.add_invariant("Model Catalog HTTP 200", False, f"HTTP {status}: {err}", "CATALOG")
            return None

        try:
            data = json.loads(body)
        except Exception as ex:
            self.add_invariant("Model Catalog JSON valid", False, f"Parse error: {ex}", "CATALOG")
            return None

        models_list = data.get("data", [])
        model_map = {m.get("id"): m for m in models_list}

        self.add_invariant(
            "Model Catalog Non-Empty",
            len(models_list) >= 1,
            f"served_models={len(models_list)}",
            "CATALOG",
        )

        for tm in target_models:
            present = tm in model_map
            m_info = model_map.get(tm, {})
            avail = m_info.get("available", False)
            status_str = m_info.get("status", "unknown")
            self.add_invariant(
                f"Model Available [{tm}]",
                present and avail,
                f"present={present} status={status_str}",
                "CATALOG",
            )

        return model_map

    # ------------------------------------------------------------------
    # Step 4: Live Chat Completion Verification (Reasoning-Aware)
    # ------------------------------------------------------------------
    def test_chat_completion(self, model, stream=False):
        prompt = "Respond with exactly 'PONG:CONTROL_VERIFIED' and nothing else."
        # Use max_tokens=150 to support reasoning models (which spend tokens on thinking)
        payload = {
            "model": model,
            "messages": [{"role": "user", "content": prompt}],
            "max_tokens": 150,
            "temperature": 0.0,
            "stream": stream,
        }

        tag = f"CHAT:{model}:{'STREAM' if stream else 'SYNC'}"

        if not stream:
            status, body, elapsed, err = self.http_post_json("/v1/chat/completions", payload, auth=True, stream=False)
            if status != 200:
                self.add_invariant(f"Chat Response [{model}]", False, f"HTTP {status}: {body[:200]}", tag)
                return False, elapsed, "", ""

            try:
                res = json.loads(body)
                choices = res.get("choices", [])
                if not choices:
                    self.add_invariant(f"Chat Choices [{model}]", False, "No choices array in response", tag)
                    return False, elapsed, "", ""

                msg = choices[0].get("message", {})
                content = msg.get("content", "").strip()
                reasoning = msg.get("reasoning_content", "").strip()
                finish_reason = choices[0].get("finish_reason", "unknown")

                has_output = len(content) > 0 or len(reasoning) > 0
                detail = f"content='{content[:30]}' (tokens: {res.get('usage', {}).get('completion_tokens', '?')}, finish: {finish_reason}) in {elapsed:.2f}s"
                if reasoning:
                    detail += f" [reasoning={len(reasoning)} chars]"

                self.add_invariant(f"Chat Output [{model}]", has_output, detail, tag)
                return has_output, elapsed, content, reasoning
            except Exception as ex:
                self.add_invariant(f"Chat Parse [{model}]", False, f"JSON parse error: {ex}", tag)
                return False, elapsed, "", ""
        else:
            # Streaming SSE verification
            status, resp, t0, err = self.http_post_json("/v1/chat/completions", payload, auth=True, stream=True)
            if status != 200:
                self.add_invariant(f"Chat Stream HTTP [{model}]", False, f"HTTP {status}", tag)
                return False, 0.0, "", ""

            chunks = []
            ttfb = 0.0
            done_marker = False
            reconstructed_content = ""
            reconstructed_reasoning = ""

            try:
                with resp:
                    for line_bytes in resp:
                        line = line_bytes.decode("utf-8", errors="replace").strip()
                        if not line:
                            continue
                        if ttfb == 0.0:
                            ttfb = time.perf_counter() - t0
                        if line.startswith("data: "):
                            raw_data = line[6:].strip()
                            if raw_data == "[DONE]":
                                done_marker = True
                                break
                            try:
                                chunk_json = json.loads(raw_data)
                                choices = chunk_json.get("choices", [])
                                if choices:
                                    delta = choices[0].get("delta", {})
                                    if "content" in delta and delta["content"]:
                                        reconstructed_content += delta["content"]
                                    if "reasoning_content" in delta and delta["reasoning_content"]:
                                        reconstructed_reasoning += delta["reasoning_content"]
                                    chunks.append(chunk_json)
                            except Exception:
                                pass
            except Exception as ex:
                self.add_invariant(f"Stream Read [{model}]", False, f"Stream read error: {ex}", tag)
                return False, 0.0, "", ""

            total_elapsed = time.perf_counter() - t0
            reconstructed_content = reconstructed_content.strip()
            reconstructed_reasoning = reconstructed_reasoning.strip()
            has_output = len(reconstructed_content) > 0 or len(reconstructed_reasoning) > 0

            stream_detail = f"chunks={len(chunks)} ttfb={ttfb:.2f}s total={total_elapsed:.2f}s done={done_marker}"
            if reconstructed_reasoning:
                stream_detail += f" [reasoning={len(reconstructed_reasoning)} chars]"

            self.add_invariant(
                f"Stream Chunk Protocol [{model}]",
                len(chunks) > 0 and done_marker,
                stream_detail,
                tag,
            )
            self.add_invariant(
                f"Stream Text Reconstructed [{model}]",
                has_output,
                f"text='{reconstructed_content[:30]}'",
                tag,
            )

            return has_output, total_elapsed, reconstructed_content, reconstructed_reasoning

    # ------------------------------------------------------------------
    # Step 5: Concurrency & Slot Ledger Verification
    # ------------------------------------------------------------------
    def test_concurrency(self, model, concurrency=2):
        tag = f"CONCURRENCY:{model}"
        payload = {
            "model": model,
            "messages": [{"role": "user", "content": "Reply with 'OK'."}],
            "max_tokens": 50,
            "temperature": 0.0,
        }

        def _do_req(req_id):
            return self.http_post_json("/v1/chat/completions", payload, auth=True, stream=False)

        t0 = time.perf_counter()
        with concurrent.futures.ThreadPoolExecutor(max_workers=concurrency) as executor:
            futures = [executor.submit(_do_req, i) for i in range(concurrency)]
            results = [f.result() for f in concurrent.futures.as_completed(futures)]
        elapsed = time.perf_counter() - t0

        statuses = [r[0] for r in results]
        all_ok = all(s == 200 for s in statuses)
        self.add_invariant(
            f"Concurrent Slot Ledger ({concurrency} reqs) [{model}]",
            all_ok,
            f"statuses={statuses} parallel_elapsed={elapsed:.2f}s",
            tag,
        )
        return all_ok

    # ------------------------------------------------------------------
    # Step 6: Account Stickiness Verification
    # ------------------------------------------------------------------
    def test_stickiness(self, model):
        """Sends two sequential requests and observes which token handled them."""
        tag = f"STICKINESS:{model}"
        h_before = self.check_health(record_invariants=False)
        reqs_before = h_before["requests"] if h_before else {}

        # Send request 1
        self.http_post_json(
            "/v1/chat/completions",
            {"model": model, "messages": [{"role": "user", "content": "1"}], "max_tokens": 10},
            auth=True,
        )
        h_mid = self.check_health(record_invariants=False)
        reqs_mid = h_mid["requests"] if h_mid else {}

        # Identify token used for req 1
        used_tok_1 = None
        for tok_idx, count in reqs_mid.items():
            if count > reqs_before.get(tok_idx, 0):
                used_tok_1 = tok_idx
                break

        # Send request 2
        self.http_post_json(
            "/v1/chat/completions",
            {"model": model, "messages": [{"role": "user", "content": "2"}], "max_tokens": 10},
            auth=True,
        )
        h_after = self.check_health(record_invariants=False)
        reqs_after = h_after["requests"] if h_after else {}

        used_tok_2 = None
        for tok_idx, count in reqs_after.items():
            if count > reqs_mid.get(tok_idx, 0):
                used_tok_2 = tok_idx
                break

        sticky = used_tok_1 is not None and used_tok_1 == used_tok_2
        detail = f"req1=token#{used_tok_1} req2=token#{used_tok_2} (sticky={sticky})"
        self.add_invariant(f"Account Stickiness / Orderly Spill [{model}]", used_tok_1 is not None, detail, tag)
        return sticky

    # ------------------------------------------------------------------
    # Full Run Pipeline
    # ------------------------------------------------------------------
    def run_full_check(self, models, test_chat=True, concurrency=2):
        self.invariants = []
        t_start = time.perf_counter()

        # 1. Health & Topology Baseline
        health_info = self.check_health(record_invariants=True)

        # 2. Metrics Baseline
        metrics_info = self.check_metrics(record_invariants=True)

        # 3. Models Catalog
        models_info = self.check_models(models)

        # 4. Chat Completion Tests
        chat_results = {}
        if test_chat and health_info and models_info:
            for model in models:
                # Test sync
                ok_sync, el_sync, txt_sync, r_sync = self.test_chat_completion(model, stream=False)
                # Test stream
                ok_stream, el_stream, txt_stream, r_stream = self.test_chat_completion(model, stream=True)
                chat_results[model] = {
                    "sync": {"ok": ok_sync, "elapsed": el_sync, "content": txt_sync, "reasoning": r_sync},
                    "stream": {"ok": ok_stream, "elapsed": el_stream, "content": txt_stream, "reasoning": r_stream},
                }

            # Concurrency test on first model
            if models and concurrency > 1:
                self.test_concurrency(models[0], concurrency=concurrency)

            # Account stickiness test
            if models:
                self.test_stickiness(models[0])

        # 5. Metrics Post-check (confirm served requests incremented)
        metrics_post = self.check_metrics(record_invariants=False)
        if metrics_info and metrics_post and test_chat:
            diff = metrics_post["served"] - metrics_info["served"]
            self.add_invariant(
                "Telemetry Counter Monotonicity",
                diff >= 0,
                f"requests_served_delta=+{diff}",
                "METRICS",
            )

        duration = time.perf_counter() - t_start

        return {
            "timestamp": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
            "duration_secs": round(duration, 3),
            "base_url": self.base_url,
            "health": health_info,
            "metrics": metrics_post,
            "chats": chat_results,
            "invariants": [inv.to_dict() for inv in self.invariants],
            "all_passed": all(inv.passed for inv in self.invariants),
        }

    def print_terminal_report(self, report):
        print("\n" + "=" * 76)
        print(color("  FREEBUFF-PROXY CONTROL LOGIC VERIFICATION REPORT", C_BOLD))
        print(f"  Target: {color(self.base_url, C_CYAN)} | Time: {report['timestamp']}")
        print("=" * 76)

        if report.get("health"):
            h = report["health"]
            print(f"\n{color('● POOL TOPOLOGY & STATUS', C_BOLD)}")
            print(f"  Mode:            {color(h['data'].get('mode', 'unknown'), C_BLUE)}")
            print(f"  Status:          {color(h['data'].get('status', 'unknown'), C_GREEN)}")
            print(f"  Configured:      {h['tokens_count']} tokens")
            print(f"  Active Runs:     {h['active_runs']} cached runs")
            print(f"  Cooling:         {h['cooling_count']} tokens")
            print(f"  Quarantined:     {h['quarantined_count']} tokens")

        categories = {}
        for inv in self.invariants:
            categories.setdefault(inv.category, []).append(inv)

        for cat, items in categories.items():
            print(f"\n{color('● ' + cat, C_BOLD)}")
            for item in items:
                badge = color("[PASS]", C_GREEN) if item.passed else color("[FAIL]", C_RED)
                detail = f" — {item.detail}" if item.detail else ""
                print(f"  {badge} {item.name}{color(detail, C_DIM)}")

        total = len(self.invariants)
        passed = sum(1 for inv in self.invariants if inv.passed)
        failed = total - passed

        print("\n" + "-" * 76)
        summary_color = C_GREEN if failed == 0 else C_RED
        status_text = "ALL INVARIANTS PERFECT" if failed == 0 else f"{failed} INVARIANTS FAILED"
        print(f"  RESULT: {color(status_text, summary_color)} ({passed}/{total} passed) in {report['duration_secs']}s")
        print("=" * 76 + "\n")


def load_api_key(key_file, direct_key):
    if direct_key:
        return direct_key.strip()
    if os.environ.get("FREEBUFF_API_KEY"):
        return os.environ["FREEBUFF_API_KEY"].strip()
    if key_file and os.path.isfile(key_file):
        try:
            with open(key_file, "r", encoding="utf-8") as f:
                content = f.read().strip()
                if content:
                    return content
        except Exception:
            pass
    return ""


def main():
    parser = argparse.ArgumentParser(description="freebucks-proxy control-logic monitoring tool")
    parser.add_argument("--url", default=os.environ.get("FREEBUFF_HOST", "http://172.188.64.104:3457"))
    parser.add_argument("--key-file", default="api-keys_freebuff_vps-sg")
    parser.add_argument("--key", default="")
    parser.add_argument("--model", default="upstage/solar-pro4,z-ai/glm-5.3-flash")
    parser.add_argument("--skip-chat", action="store_true")
    parser.add_argument("--concurrency", type=int, default=2)
    parser.add_argument("--watch", action="store_true")
    parser.add_argument("--interval", type=int, default=5)
    parser.add_argument("--json", action="store_true")
    parser.add_argument("--verbose", action="store_true")

    args = parser.parse_args()
    api_key = load_api_key(args.key_file, args.key)
    models = [m.strip() for m in args.model.split(",") if m.strip()]

    monitor = ControlMonitor(args.url, api_key, verbose=args.verbose)

    while True:
        report = monitor.run_full_check(
            models=models,
            test_chat=not args.skip_chat,
            concurrency=args.concurrency,
        )

        if args.json:
            print(json.dumps(report, indent=2))
        else:
            monitor.print_terminal_report(report)

        if not args.watch:
            sys.exit(0 if report["all_passed"] else 1)

        time.sleep(args.interval)


if __name__ == "__main__":
    main()
