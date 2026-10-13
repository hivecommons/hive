#!/usr/bin/env bash
# nous-runner.sh — Strategist helper: invokes the real Nous framework
# Called by the strategist agent on each kick.
set -euo pipefail

NOUS_DIR="${NOUS_DIR:-/opt/nous}"
NOUS_PYTHON="${NOUS_DIR}/venv/bin/python3"
NOUS_RUN_DIR="${NOUS_RUN_DIR:-/var/run/nous}"
# Durable Nous root (#11694). HIVE_NOUS_DATA_DIR is the hive's own override
# and wins so the gate bridge publishes where the dashboard is reading.
NOUS_DATA_DIR="${HIVE_NOUS_DATA_DIR:-${NOUS_DATA_DIR:-/data/nous}}"
CAMPAIGN_PATH="${NOUS_CAMPAIGN_PATH:-/etc/hive/nous-campaign.yaml}"
HIVE_METRICS_DIR="${HIVE_METRICS_DIR:-/var/run/hive-metrics}"
GOV_STATE_DIR="${GOV_STATE_DIR:-/var/run/kick-governor}"
HIVE_CONFIG_DIR="${HIVE_CONFIG_DIR:-/etc/hive}"
OVERLAY_PATH="$HIVE_CONFIG_DIR/governor-experiment.env"
LAST_SCOPE_FILE="$NOUS_RUN_DIR/last-scope"
TIMEOUT_SEC=1800
GATE_SCRIPT="$(dirname "$0")/nous-hive-gate.py"
CAMPAIGN_DIR="$(dirname "$CAMPAIGN_PATH")"

read_yaml_field() {
  python3 -c "
import yaml, sys
with open('$1') as f:
    d = yaml.safe_load(f)
keys = '$2'.split('.')
for k in keys:
    d = (d or {}).get(k, '')
print(d or '')
"
}

MODE=$(read_yaml_field "$CAMPAIGN_PATH" "campaign.mode")
SCOPE=$(read_yaml_field "$CAMPAIGN_PATH" "campaign.scope")
SCOPE="${SCOPE:-governor}"
MODE="${MODE:-observe}"

echo "[nous-runner] scope=$SCOPE mode=$MODE"

resolve_scope() {
  local scope="$1"
  if [ "$scope" = "both" ]; then
    local last
    last=$(cat "$LAST_SCOPE_FILE" 2>/dev/null || echo "repo")
    if [ "$last" = "governor" ]; then
      echo "repo"
    else
      echo "governor"
    fi
  else
    echo "$scope"
  fi
}

EFFECTIVE_SCOPE=$(resolve_scope "$SCOPE")
echo "$EFFECTIVE_SCOPE" > "$LAST_SCOPE_FILE"
echo "[nous-runner] effective scope=$EFFECTIVE_SCOPE"

EFFECTIVE_MODE="$MODE"
if [ "$EFFECTIVE_SCOPE" = "repo" ]; then
  EFFECTIVE_MODE="suggest"
  echo "[nous-runner] repo scope forces suggest mode"
fi

WORK_DIR="$NOUS_RUN_DIR/$EFFECTIVE_SCOPE"
mkdir -p "$WORK_DIR"

build_hive_context() {
  python3 - "$WORK_DIR/hive-context.json" <<'PYEOF'
import json, sys, os, glob

out_path = sys.argv[1]
metrics_dir = os.environ.get("HIVE_METRICS_DIR", "/var/run/hive-metrics")
gov_dir = os.environ.get("GOV_STATE_DIR", "/var/run/kick-governor")
nous_dir = os.environ.get("NOUS_RUN_DIR", "/var/run/nous")
nous_data = os.environ.get("NOUS_DATA_DIR", "/data/nous")

def read_file(p):
    try:
        with open(p) as f:
            return f.read().strip()
    except Exception:
        return None

def read_json(p):
    try:
        with open(p) as f:
            return json.load(f)
    except Exception:
        return None

def read_jsonl_tail(p, n=10):
    try:
        with open(p) as f:
            lines = f.readlines()
        return [json.loads(l) for l in lines[-n:] if l.strip()]
    except Exception:
        return []

snapshots_dir = os.path.join(nous_data, "snapshots")
snapshot_files = sorted(glob.glob(os.path.join(snapshots_dir, "*.json")))[-20:]
snapshots = []
for sf in snapshot_files:
    s = read_json(sf)
    if s:
        snapshots.append(s)

ctx = {
    "governor_mode": read_file(os.path.join(gov_dir, "mode")),
    "queue_depth": read_file(os.path.join(gov_dir, "queue_depth")),
    "regime": read_file(os.path.join(gov_dir, "regime")),
    "mttr": read_json(os.path.join(metrics_dir, "issue_to_merge.json")),
    "tokens": read_json(os.path.join(metrics_dir, "tokens.json")),
    "kick_outcomes": read_jsonl_tail(os.path.join(metrics_dir, "kick-outcomes.jsonl")),
    "principles": read_json(os.path.join(nous_data, "principles.json")) or [],
    "ledger_recent": read_jsonl_tail(os.path.join(nous_data, "ledger.jsonl")),
    "recent_snapshots": snapshots,
}

with open(out_path, "w") as f:
    json.dump(ctx, f, indent=2)
print(f"[nous-runner] wrote hive context to {out_path}")
PYEOF
}

select_campaign_config() {
  local scope="$1"
  if [ "$scope" = "governor" ]; then
    echo "$CAMPAIGN_DIR/nous-governor-campaign.yaml"
  elif [ "$scope" = "repo" ]; then
    echo "$CAMPAIGN_DIR/nous-repo-campaign.yaml"
  else
    echo "$CAMPAIGN_DIR/nous-governor-campaign.yaml"
  fi
}

CAMPAIGN_CONFIG=$(select_campaign_config "$EFFECTIVE_SCOPE")
if [ ! -f "$CAMPAIGN_CONFIG" ]; then
  echo "[nous-runner] ERROR: campaign config not found: $CAMPAIGN_CONFIG"
  exit 1
fi
echo "[nous-runner] campaign config: $CAMPAIGN_CONFIG"

build_hive_context

AUTO_APPROVE="false"
if [ "$EFFECTIVE_MODE" = "evolve" ]; then
  AUTO_APPROVE="true"
elif [ "$EFFECTIVE_MODE" = "observe" ]; then
  AUTO_APPROVE="true"
fi

# Dispatcher (RFC #11585 part 5): the hive exports its resolved nous.dispatcher
# as HIVE_NOUS_DISPATCHER (inline by default; sdk is the legacy agent-SDK
# mode). A NOUS_AGENT already set in the environment wins so operators who
# pinned a mode before the hive owned the setting keep it.
NOUS_AGENT="${NOUS_AGENT:-${HIVE_NOUS_DISPATCHER:-inline}}"

APPROVE_FLAG=""
if [ "$AUTO_APPROVE" = "true" ]; then
  APPROVE_FLAG="--auto-approve"
fi

# Pinned dependencies (RFC #11585 part 7, #11700): before the engine starts,
# `hive nous-setup` fetches the campaign's declared dependencies — repos by
# commit, files by sha256 — from allowlisted hosts only (HIVE_NOUS_FETCH_ALLOWLIST,
# exported by the hive from nous.fetch_allowlist), verifies every pin and
# records the resolved versions in $NOUS_DATA_DIR/<run-id>/dependencies.json.
# Any refusal or mismatch ends the campaign here, before compute is spent.
# This is the ONLY place the runner fetches anything; do not add curl/pip/git
# fetches elsewhere in this script (pkg/nousdeps guard_test.go enforces it).
RUN_ID="$(basename "$WORK_DIR")"
HIVE_BIN="${HIVE_BIN:-hive}"
DEPS_ENV_FILE="$NOUS_DATA_DIR/$RUN_ID/dependencies.env"
if command -v "$HIVE_BIN" >/dev/null 2>&1; then
  if ! "$HIVE_BIN" nous-setup --campaign "$CAMPAIGN_CONFIG" --run-id "$RUN_ID" --root "$NOUS_DATA_DIR"; then
    echo "[nous-runner] ERROR: dependency setup refused or failed — campaign not started"
    exit 1
  fi
  if [ -f "$DEPS_ENV_FILE" ]; then
    set -a
    # shellcheck disable=SC1090
    . "$DEPS_ENV_FILE"
    set +a
  fi
else
  if [ -n "$(read_yaml_field "$CAMPAIGN_CONFIG" "campaign.dependencies")" ]; then
    echo "[nous-runner] ERROR: campaign declares dependencies but '$HIVE_BIN' is not on PATH to fetch them — refusing to start"
    exit 1
  fi
  echo "[nous-runner] WARN: '$HIVE_BIN' not found; no dependency record will be written for this run"
fi

echo "[nous-runner] invoking run_campaign.py (agent=$NOUS_AGENT, auto_approve=$AUTO_APPROVE, timeout=${TIMEOUT_SEC}s)"
NOUS_HIVE_MODE="$EFFECTIVE_MODE" \
NOUS_HIVE_SCOPE="$EFFECTIVE_SCOPE" \
NOUS_GATE_SCRIPT="$GATE_SCRIPT" \
NOUS_DATA_DIR="$NOUS_DATA_DIR" \
"$NOUS_PYTHON" "$NOUS_DIR/run_campaign.py" \
  "$CAMPAIGN_CONFIG" \
  --run-id "$RUN_ID" \
  --max-iterations 1 \
  --timeout "$TIMEOUT_SEC" \
  $APPROVE_FLAG \
  --agent "$NOUS_AGENT" \
  2>&1 || {
    echo "[nous-runner] run_campaign.py exited with $?"
  }

if [ "$EFFECTIVE_SCOPE" = "governor" ] && [ "$EFFECTIVE_MODE" = "evolve" ]; then
  if [ -f "$WORK_DIR/state.json" ]; then
    echo "[nous-runner] translating Nous output to governor overlay"
    python3 - "$WORK_DIR" "$OVERLAY_PATH" "$CAMPAIGN_PATH" <<'PYEOF'
import json, sys, os, time, yaml

work_dir = sys.argv[1]
overlay_path = sys.argv[2]
campaign_path = sys.argv[3]

state = {}
try:
    with open(os.path.join(work_dir, "state.json")) as f:
        state = json.load(f)
except Exception:
    print("[nous-runner] no state.json found, skipping overlay")
    sys.exit(0)

phase = state.get("phase", "")
if phase not in ("EXTRACTION", "ANALYSIS"):
    print(f"[nous-runner] phase is {phase}, not ready for overlay")
    sys.exit(0)

with open(campaign_path) as f:
    campaign = yaml.safe_load(f)
controllables = {c["name"] for c in (campaign.get("campaign", {}).get("controllables", []))}

plan = state.get("plan", {})
treatments = {}
for arm in plan.get("arms", []):
    for condition in arm.get("conditions", []):
        var = condition.get("variable", "")
        val = condition.get("value", "")
        if var in controllables and val:
            treatments[var] = val

if not treatments:
    print("[nous-runner] no actionable treatments found")
    sys.exit(0)

if os.path.exists(overlay_path):
    print("[nous-runner] overlay already exists, skipping")
    sys.exit(0)

exp_id = f"nous-{int(time.time())}"
now_epoch = int(time.time())
ttl = campaign.get("campaign", {}).get("schedule", {}).get("experiment_duration_hours", 4) * 3600
fast_fail = campaign.get("campaign", {}).get("fast_fail", {})

lines = [
    "# Nous experiment overlay — generated by nous-runner.sh",
    f"NOUS_EXPERIMENT_ID={exp_id}",
    f"NOUS_EXPERIMENT_START={now_epoch}",
    f"NOUS_EXPERIMENT_TTL_SEC={ttl}",
    f"NOUS_FAST_FAIL_QUEUE_MAX={fast_fail.get('queue_depth_max', 30)}",
    f"NOUS_FAST_FAIL_MTTR_MAX={fast_fail.get('mttr_max_minutes', 180)}",
]
for var, val in treatments.items():
    lines.append(f"{var}={val}")

with open(overlay_path, "w") as f:
    f.write("\n".join(lines) + "\n")
print(f"[nous-runner] wrote overlay: {overlay_path}")
print(f"[nous-runner] treatments: {treatments}")
PYEOF
  fi
fi

if [ "$EFFECTIVE_SCOPE" = "repo" ]; then
  if [ -d "$WORK_DIR/.nous-experiments" ]; then
    echo "[nous-runner] repo experiment worktree created — check dashboard for gate decision"
  fi
fi

python3 - "$EFFECTIVE_SCOPE" "$EFFECTIVE_MODE" <<'PYEOF'
import json, os, sys, time

scope = sys.argv[1]
mode = sys.argv[2]
nous_data = os.environ.get("NOUS_DATA_DIR", "/data/nous")
os.makedirs(nous_data, exist_ok=True)
entry = {
    "ts": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
    "scope": scope,
    "mode": mode,
    "type": "nous_kick",
}
try:
    with open(f"{nous_data}/ledger.jsonl", "a") as f:
        f.write(json.dumps(entry) + "\n")
except Exception:
    pass
PYEOF

echo "[nous-runner] done (scope=$EFFECTIVE_SCOPE, mode=$EFFECTIVE_MODE)"
