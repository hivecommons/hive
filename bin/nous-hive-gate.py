#!/usr/bin/env python3
"""
nous-hive-gate.py — Custom Nous Gate for Hive

Implements the Nous Gate protocol, bridging experiment approval decisions
to the hive dashboard.

Modes:
  observe  → auto-approve (dry-run semantics handled upstream)
  suggest  → POST to dashboard, long-poll for operator decision
  evolve   → auto-approve (autonomous loop)

Repo scope always forces suggest mode regardless of setting.
"""

import json
import os
import sys
import time
import urllib.request
import urllib.error

DASHBOARD_URL = os.environ.get("HIVE_DASHBOARD_URL", "http://localhost:3001")
GATE_TIMEOUT_SEC = 1800
GATE_POLL_INTERVAL_SEC = 5
DASHBOARD_TOKEN = os.environ.get("HIVE_DASHBOARD_TOKEN", "").strip()


def get_effective_mode():
    mode = os.environ.get("NOUS_HIVE_MODE", "observe")
    scope = os.environ.get("NOUS_HIVE_SCOPE", "governor")
    if scope == "repo":
        return "suggest"
    return mode


def post_pending(question, artifact_path=None, reviews=None, summary_path=None):
    payload = {
        "question": question,
        "scope": os.environ.get("NOUS_HIVE_SCOPE", "governor"),
        "timestamp": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
    }
    if artifact_path and os.path.exists(artifact_path):
        try:
            with open(artifact_path) as f:
                payload["artifact"] = json.load(f)
        except Exception:
            payload["artifact_path"] = artifact_path

    if reviews:
        payload["reviews"] = reviews

    if summary_path and os.path.exists(summary_path):
        try:
            with open(summary_path) as f:
                payload["summary"] = f.read()
        except Exception:
            payload["summary_path"] = summary_path

    data = json.dumps(payload).encode("utf-8")
    headers = {"Content-Type": "application/json"}
    if DASHBOARD_TOKEN:
        headers["Authorization"] = "Bearer " + DASHBOARD_TOKEN
    req = urllib.request.Request(
        f"{DASHBOARD_URL}/api/nous/gate-decision",
        data=data,
        headers=headers,
        method="PUT",
    )
    try:
        with urllib.request.urlopen(req, timeout=10) as resp:
            result = json.loads(resp.read().decode("utf-8"))
            return result.get("ok", False)
    except Exception as e:
        print(f"[nous-gate] failed to post pending decision: {e}", file=sys.stderr)
        return False


def poll_response(timeout_sec=GATE_TIMEOUT_SEC):
    start = time.time()
    while time.time() - start < timeout_sec:
        try:
            req = urllib.request.Request(
                f"{DASHBOARD_URL}/api/nous/gate-response",
                method="GET",
            )
            with urllib.request.urlopen(req, timeout=10) as resp:
                result = json.loads(resp.read().decode("utf-8"))
                decision = result.get("decision")
                if decision in ("approve", "reject", "abort"):
                    return decision
        except urllib.error.HTTPError as e:
            if e.code == 404:
                pass
            else:
                print(f"[nous-gate] poll error: {e}", file=sys.stderr)
        except Exception as e:
            print(f"[nous-gate] poll error: {e}", file=sys.stderr)

        time.sleep(GATE_POLL_INTERVAL_SEC)

    print("[nous-gate] timeout waiting for operator decision — auto-rejecting", file=sys.stderr)
    return "reject"


def prompt(question, artifact_path=None, reviews=None, summary_path=None):
    """Nous Gate protocol entry point."""
    mode = get_effective_mode()

    if mode in ("observe", "evolve"):
        print(f"[nous-gate] auto-approving (mode={mode})")
        return "approve"

    print(f"[nous-gate] posting to dashboard for approval (mode={mode})")
    posted = post_pending(question, artifact_path, reviews, summary_path)
    if not posted:
        print("[nous-gate] failed to post — rejecting", file=sys.stderr)
        return "reject"

    return poll_response()


if __name__ == "__main__":
    if len(sys.argv) < 2:
        print("Usage: nous-hive-gate.py <question> [artifact_path] [summary_path]")
        sys.exit(1)

    question = sys.argv[1]
    artifact = sys.argv[2] if len(sys.argv) > 2 else None
    summary = sys.argv[3] if len(sys.argv) > 3 else None

    decision = prompt(question, artifact_path=artifact, summary_path=summary)
    print(f"[nous-gate] decision: {decision}")
    sys.exit(0 if decision == "approve" else 1)
