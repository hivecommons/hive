#!/usr/bin/env python3
"""
nous-hive-gate.py — Nous gate bridge for Hive (RFC #11585 part 3, #11696)

Implements the Nous Gate protocol so an experiment's design and findings
gates are answered by a hive owner from the Experiment Lab.

The agent cannot hold an owner credential, so it never calls the owner-only
dashboard API. It publishes the pending question as a file under the durable
Nous root and waits for the hive to record the owner's answer next to it:

  <nous_dir>/gates/pending.json    written here, read by the hive
  <nous_dir>/gates/decision.json   written by the hive, read here

The hive mirrors pending.json into GET /api/nous/gate-pending, pings the
owner once, and PUT /api/nous/gate-decision records decision.json (with the
gate id, so a stale answer can never approve a newer gate).

Modes:
  observe  → auto-approve (dry-run semantics handled upstream)
  suggest  → publish the gate, wait for the owner's decision
  evolve   → auto-approve (autonomous loop)

Repo scope always forces suggest mode regardless of setting.
"""

import json
import os
import sys
import time
import uuid

DEFAULT_NOUS_DIR = "/data/nous"
GATES_SUBDIR = "gates"
PENDING_FILE = "pending.json"
DECISION_FILE = "decision.json"

DEFAULT_GATE_TIMEOUT_SEC = 1800
DEFAULT_GATE_POLL_INTERVAL_SEC = 5

DECISIONS = ("approve", "reject", "abort")
TIMEOUT_DECISION = "reject"


def nous_dir():
    for var in ("HIVE_NOUS_DATA_DIR", "NOUS_DATA_DIR"):
        value = os.environ.get(var, "").strip()
        if value:
            return value
    return DEFAULT_NOUS_DIR


def gates_dir():
    return os.path.join(nous_dir(), GATES_SUBDIR)


def _env_number(name, default, cast=float):
    raw = os.environ.get(name, "").strip()
    if not raw:
        return default
    try:
        value = cast(raw)
    except ValueError:
        return default
    return value if value > 0 else default


def gate_timeout_sec():
    return _env_number("NOUS_GATE_TIMEOUT_SEC", DEFAULT_GATE_TIMEOUT_SEC)


def gate_poll_interval_sec():
    return _env_number("NOUS_GATE_POLL_INTERVAL_SEC", DEFAULT_GATE_POLL_INTERVAL_SEC)


def get_effective_mode():
    mode = os.environ.get("NOUS_HIVE_MODE", "observe")
    scope = os.environ.get("NOUS_HIVE_SCOPE", "governor")
    if scope == "repo":
        return "suggest"
    return mode


def _utc_now():
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())


def _write_atomic(path, data):
    tmp = path + ".tmp"
    with open(tmp, "w") as f:
        f.write(data)
    os.replace(tmp, path)


def build_request(question, gate=None, artifact_path=None, reviews=None, summary_path=None):
    gate = gate or os.environ.get("NOUS_GATE_KIND", "").strip() or None
    gate_id = f"{gate or 'gate'}-{int(time.time())}-{uuid.uuid4().hex[:8]}"
    payload = {
        "id": gate_id,
        "question": question,
        "scope": os.environ.get("NOUS_HIVE_SCOPE", "governor"),
        "requested_at": _utc_now(),
    }
    if gate:
        payload["gate"] = gate
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
    return payload


def post_pending(request):
    """Publish the gate for the hive. Returns True when the file is in place."""
    try:
        os.makedirs(gates_dir(), exist_ok=True)
        _write_atomic(os.path.join(gates_dir(), PENDING_FILE), json.dumps(request, indent=2))
        return True
    except Exception as e:
        print(f"[nous-gate] failed to publish pending gate: {e}", file=sys.stderr)
        return False


def clear_pending():
    try:
        os.remove(os.path.join(gates_dir(), PENDING_FILE))
    except FileNotFoundError:
        pass
    except Exception as e:
        print(f"[nous-gate] failed to clear pending gate: {e}", file=sys.stderr)


def read_decision(gate_id):
    """Return the owner's decision for gate_id, or None while still pending."""
    path = os.path.join(gates_dir(), DECISION_FILE)
    try:
        with open(path) as f:
            data = json.load(f)
    except FileNotFoundError:
        return None
    except Exception as e:
        print(f"[nous-gate] unreadable decision file: {e}", file=sys.stderr)
        return None
    if not isinstance(data, dict) or data.get("gate_id") != gate_id:
        return None
    decision = data.get("decision")
    return decision if decision in DECISIONS else None


def poll_response(gate_id, timeout_sec=None, poll_interval_sec=None, sleep=time.sleep, clock=time.monotonic):
    timeout_sec = gate_timeout_sec() if timeout_sec is None else timeout_sec
    poll_interval_sec = gate_poll_interval_sec() if poll_interval_sec is None else poll_interval_sec
    start = clock()
    while True:
        decision = read_decision(gate_id)
        if decision:
            return decision
        if clock() - start >= timeout_sec:
            break
        sleep(poll_interval_sec)

    print(
        f"[nous-gate] timeout waiting for operator decision — {TIMEOUT_DECISION}ing",
        file=sys.stderr,
    )
    clear_pending()
    return TIMEOUT_DECISION


def prompt(question, artifact_path=None, reviews=None, summary_path=None, gate=None):
    """Nous Gate protocol entry point: returns approve, reject or abort."""
    mode = get_effective_mode()

    if mode in ("observe", "evolve"):
        print(f"[nous-gate] auto-approving (mode={mode})")
        return "approve"

    request = build_request(question, gate=gate, artifact_path=artifact_path, reviews=reviews, summary_path=summary_path)
    print(f"[nous-gate] publishing gate {request['id']} for owner approval (mode={mode}, dir={gates_dir()})")
    if not post_pending(request):
        print("[nous-gate] failed to publish — rejecting", file=sys.stderr)
        return "reject"

    return poll_response(request["id"])


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
