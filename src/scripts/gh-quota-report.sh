#!/usr/bin/env bash
# gh-quota-report.sh — collect /api/gh-rate-limits across hosted hives and
# summarize remaining GitHub REST quota plus the busiest one-hour consumers.
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
src_root=$(cd -- "${script_dir}/.." && pwd)

context="hive-oke"
ns_prefix="hive-hosted-"
top_n=5
json_mode=0
local_url=""
local_token=""
from_file=""

usage() {
  cat <<'USAGE'
Usage: src/scripts/gh-quota-report.sh [options]

Options:
  --context <kube-ctx>   Kubernetes context (default: hive-oke)
  --ns-prefix <prefix>   Spoke namespace prefix (default: hive-hosted-)
  --top <N>              Top consumers per hive row (default: 5)
  --json                 Print the raw aggregate JSON instead of tables
  --local <url>          Query one hive dashboard URL instead of kubectl
  --token <token>        Dashboard bearer token for --local
  --from-file <fixture>  Format a saved aggregate or single endpoint payload
  -h, --help             Show this help
USAGE
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --context)
      context=${2:?--context requires a value}; shift 2 ;;
    --ns-prefix)
      ns_prefix=${2:?--ns-prefix requires a value}; shift 2 ;;
    --top)
      top_n=${2:?--top requires a value}; shift 2 ;;
    --json)
      json_mode=1; shift ;;
    --local)
      local_url=${2:?--local requires a value}; shift 2 ;;
    --token)
      local_token=${2:?--token requires a value}; shift 2 ;;
    --from-file)
      from_file=${2:?--from-file requires a value}; shift 2 ;;
    -h|--help)
      usage; exit 0 ;;
    *)
      echo "unknown option: $1" >&2
      usage >&2
      exit 2 ;;
  esac
done

case "$top_n" in
  ''|*[!0-9]*) echo "--top must be a positive integer" >&2; exit 2 ;;
esac
if [ "$top_n" -le 0 ]; then
  echo "--top must be a positive integer" >&2
  exit 2
fi

if [ -n "$from_file" ] && { [ -n "$local_url" ] || [ -n "$local_token" ]; }; then
  echo "--from-file cannot be combined with --local/--token" >&2
  exit 2
fi
if { [ -n "$local_url" ] && [ -z "$local_token" ]; } || { [ -z "$local_url" ] && [ -n "$local_token" ]; }; then
  echo "--local and --token must be supplied together" >&2
  exit 2
fi

format_report() {
  python3 - "$top_n" "$json_mode" "$@" <<'PY'
import datetime as dt
import json
import sys
from collections import defaultdict

TOP = int(sys.argv[1])
JSON_MODE = sys.argv[2] == "1"
PATHS = sys.argv[3:]


def parse_time(value):
    if not value:
        return None
    if isinstance(value, str):
        value = value.replace("Z", "+00:00")
        try:
            return dt.datetime.fromisoformat(value)
        except ValueError:
            return None
    return None


def normalize_node(obj, default_name="local"):
    if not isinstance(obj, dict):
        return {"name": default_name, "payload": {}, "error": "invalid non-object payload"}
    if "payload" in obj or "error" in obj:
        err = obj.get("error") or ""
        if err == "unauthorized":
            err = "auth-required"
        return {
            "name": str(obj.get("name") or default_name),
            "payload": obj.get("payload") if isinstance(obj.get("payload"), dict) else None,
            "error": err,
            "kind": obj.get("kind") or "spoke",
        }
    return {"name": str(obj.get("name") or default_name), "payload": obj, "error": "", "kind": "spoke"}


def load_nodes(paths):
    if not paths:
        return []
    if len(paths) == 1:
        with open(paths[0], encoding="utf-8") as fh:
            root = json.load(fh)
        if isinstance(root, dict) and isinstance(root.get("nodes"), list):
            return [normalize_node(n) for n in root["nodes"]]
        if isinstance(root, list):
            return [normalize_node(n, f"node-{i + 1}") for i, n in enumerate(root)]
        return [normalize_node(root)]
    nodes = []
    for path in paths:
        with open(path, encoding="utf-8") as fh:
            nodes.append(normalize_node(json.load(fh), path))
    return nodes


def identity(core):
    limit = int(core.get("limit") or 0)
    if limit >= 5000:
        return "app/token"
    if limit == 60:
        return "anonymous"
    if limit > 0:
        return f"limit-{limit}"
    return "unknown"


def reset_in(core):
    reset = parse_time(core.get("reset"))
    if not reset:
        return "n/a"
    now = dt.datetime.now(reset.tzinfo or dt.timezone.utc)
    mins = max(0, int(round((reset - now).total_seconds() / 60.0)))
    return f"{mins}m"


def etag_hit_pct(payload):
    cache = payload.get("etag_cache") or {}
    hits = int(cache.get("hits") or 0)
    misses = int(cache.get("misses") or 0)
    total = hits + misses
    if total <= 0:
        return "n/a"
    return f"{(hits * 100.0 / total):.1f}%"


def top_text(payload):
    rows = payload.get("top_consumers") or []
    parts = []
    for row in rows[:TOP]:
        caller = row.get("caller") or "unknown"
        method = row.get("method") or "GET"
        endpoint = row.get("endpoint") or "unknown"
        charged = int(row.get("charged") or 0)
        requests = int(row.get("requests") or 0)
        parts.append(f"{caller} {method} {endpoint} {charged}/{requests}")
    return "; ".join(parts) if parts else "none"


def short_name(node):
    name = str(node.get("name") or "unknown")
    return name

nodes = load_nodes(PATHS)
if JSON_MODE:
    print(json.dumps({"nodes": nodes}, indent=2, sort_keys=True))

exit_bad = False
fleet_charged = 0
by_consumer = defaultdict(lambda: {"charged": 0, "requests": 0, "rate_limited": 0})

if not JSON_MODE:
    print("GitHub API quota by hive")
    print(f"{'hive':<24} {'identity':<10} {'core':<12} {'reset':<7} {'etag':<7} top consumers (charged/requests)")
    print(f"{'-' * 24} {'-' * 10} {'-' * 12} {'-' * 7} {'-' * 7} {'-' * 60}")

for node in nodes:
    payload = node.get("payload") if isinstance(node.get("payload"), dict) else {}
    error = node.get("error") or ""
    if not error and isinstance(payload, dict) and payload.get("error") and not payload.get("core"):
        error = str(payload.get("error"))
        if error == "unauthorized":
            error = "auth-required"
    name = short_name(node)
    if error or not payload:
        if not JSON_MODE:
            detail = error or "no payload"
            print(f"{name:<24} {'unavailable':<10} {'n/a':<12} {'n/a':<7} {'n/a':<7} {detail}")
        continue

    core = payload.get("core") or {}
    limit = int(core.get("limit") or 0)
    remaining = int(core.get("remaining") or 0)
    if limit > 0 and remaining * 10 < limit:
        exit_bad = True

    for row in payload.get("top_consumers") or []:
        charged = int(row.get("charged") or 0)
        requests = int(row.get("requests") or 0)
        rate_limited = int(row.get("rate_limited") or 0)
        fleet_charged += charged
        if rate_limited > 0:
            exit_bad = True
        key = (row.get("caller") or "unknown", row.get("method") or "GET", row.get("endpoint") or "unknown")
        by_consumer[key]["charged"] += charged
        by_consumer[key]["requests"] += requests
        by_consumer[key]["rate_limited"] += rate_limited

    if not JSON_MODE:
        print(
            f"{name:<24} {identity(core):<10} "
            f"{remaining}/{limit:<7} {reset_in(core):<7} {etag_hit_pct(payload):<7} {top_text(payload)}"
        )

if not JSON_MODE:
    print("")
    print("Fleet summary")
    print(f"total charged/hr: {fleet_charged}")
    print("top 10 charged consumers across fleet:")
    print(f"{'charged':>8} {'requests':>8} {'rate_limited':>12} caller method endpoint")
    for (caller, method, endpoint), stats in sorted(
        by_consumer.items(),
        key=lambda item: (-item[1]["charged"], -item[1]["requests"], item[0][0], item[0][1]),
    )[:10]:
        print(f"{stats['charged']:>8} {stats['requests']:>8} {stats['rate_limited']:>12} {caller} {method} {endpoint}")

sys.exit(1 if exit_bad else 0)
PY
}

write_envelope() {
  local name=$1
  local kind=$2
  local status=$3
  local raw=$4
  local err=$5
  local out=$6
  python3 - "$name" "$kind" "$status" "$raw" "$err" "$out" <<'PY'
import json
import sys

name, kind, status, raw_path, err_path, out_path = sys.argv[1:]
raw = ""
err = ""
try:
    with open(raw_path, encoding="utf-8") as fh:
        raw = fh.read()
except FileNotFoundError:
    pass
try:
    with open(err_path, encoding="utf-8") as fh:
        err = fh.read().strip()
except FileNotFoundError:
    pass
node = {"name": name, "kind": kind}
try:
    node["payload"] = json.loads(raw)
    node["error"] = "" if status == "ok" else (err or "command failed")
except json.JSONDecodeError:
    snippet = (raw or err or "empty response").strip().replace("\n", " ")[:240]
    node["payload"] = None
    node["error"] = snippet or "empty response"
with open(out_path, "w", encoding="utf-8") as fh:
    json.dump(node, fh, sort_keys=True)
    fh.write("\n")
PY
}

fetch_local() {
  local work_dir=$1
  local raw="${work_dir}/local.raw"
  local err="${work_dir}/local.err"
  local out="${work_dir}/local.json"
  local url=${local_url%/}/api/gh-rate-limits
  local status=ok
  if ! curl -sS -H "Authorization: Bearer ${local_token}" -H "X-Hive-Internal: ${local_token}" "$url" >"$raw" 2>"$err"; then
    status=error
  fi
  write_envelope "local" "local" "$status" "$raw" "$err" "$out"
}

fetch_kube_node() {
  local context_arg=$1
  local namespace=$2
  local short=$3
  local deploy=$4
  local container=$5
  local kind=$6
  local work_dir=$7
  local raw="${work_dir}/${short}.raw"
  local err="${work_dir}/${short}.err"
  local out="${work_dir}/${short}.json"
  local status=ok
  local remote_cmd
  if [ "$kind" = "hub" ]; then
    # shellcheck disable=SC2016 # This command is evaluated inside the hub pod.
    remote_cmd='P=${HIVE_HUB_PORT:-80}; code=$(curl -s -o /dev/null -w "%{http_code}" "localhost:${P}/api/gh-rate-limits" || true); case "$code" in 404) printf "%s\n" "{\"error\":\"endpoint-missing\"}" ;; 000) exit 7 ;; *) curl -s "localhost:${P}/api/gh-rate-limits" ;; esac'
  else
    # shellcheck disable=SC2016 # This command is evaluated inside the hive pod.
    remote_cmd='P=${DASHBOARD_PORT:-${HIVE_DASHBOARD_PORT:-}}; if [ -z "$P" ] && [ -r /etc/hive/hive.yaml ]; then P=$(awk "/^[[:space:]]*dashboard:/{f=1; next} f&&/^[^[:space:]][^:]*:/{exit} f&&/^[[:space:]]*port:/{print \$2; exit}" /etc/hive/hive.yaml); fi; P=${P:-3002}; C=""; if [ -r /etc/hive/hive.yaml ]; then C=$(awk "/^[[:space:]]*dashboard:/{f=1; next} f&&/^[^[:space:]][^:]*:/{exit} f&&/^[[:space:]]*auth_token:/{print \$2; exit}" /etc/hive/hive.yaml); fi; S=""; if [ -r /secrets/dashboard-token ]; then S=$(tr -d "\r\n" </secrets/dashboard-token); fi; for T in "$C" "$S" "${DASHBOARD_AUTH_TOKEN:-}" "${HIVE_DASHBOARD_TOKEN:-}"; do [ -n "$T" ] || continue; code=$(curl -s -o /dev/null -w "%{http_code}" -H "Authorization: Bearer $T" -H "X-Hive-Internal: $T" "localhost:${P}/api/gh-rate-limits" || true); case "$code" in 200) curl -s -H "Authorization: Bearer $T" -H "X-Hive-Internal: $T" "localhost:${P}/api/gh-rate-limits"; exit 0 ;; 404) printf "%s\n" "{\"error\":\"endpoint-missing\"}"; exit 0 ;; esac; done; code=$(curl -s -o /dev/null -w "%{http_code}" "localhost:${P}/api/gh-rate-limits" || true); case "$code" in 200) curl -s "localhost:${P}/api/gh-rate-limits" ;; 404) printf "%s\n" "{\"error\":\"endpoint-missing\"}" ;; 000) exit 7 ;; *) printf "%s\n" "{\"error\":\"auth-required\"}" ;; esac'
  fi
  local container_args=()
  if [ -n "$container" ]; then
    container_args=(-c "$container")
  fi
  if ! kubectl --context "$context_arg" --request-timeout=180s exec -n "$namespace" "deploy/${deploy}" "${container_args[@]}" -- sh -c "$remote_cmd" >"$raw" 2>"$err"; then
    status=error
    if [ "$kind" = "spoke" ]; then
      sleep 2
      if kubectl --context "$context_arg" --request-timeout=180s exec -n "$namespace" "deploy/${deploy}" "${container_args[@]}" -- sh -c "$remote_cmd" >"$raw" 2>"$err"; then
        status=ok
      fi
    fi
  fi
  write_envelope "$short" "$kind" "$status" "$raw" "$err" "$out"
}

if [ -n "$from_file" ]; then
  format_report "$from_file"
  exit $?
fi

work_dir="${src_root}/.gh-quota-report.$$"
mkdir -p "$work_dir"
trap 'rm -rf "$work_dir"' EXIT

if [ -n "$local_url" ]; then
  fetch_local "$work_dir"
else
  namespace_output=$(kubectl --context "$context" --request-timeout=60s get ns -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}')
  mapfile -t namespaces < <(printf '%s\n' "$namespace_output" | awk -v p="$ns_prefix" 'index($0, p) == 1' | sort)
  for namespace in "${namespaces[@]}"; do
    short=${namespace#"$ns_prefix"}
    fetch_kube_node "$context" "$namespace" "$short" "hive" "hive" "spoke" "$work_dir" &
  done
  fetch_kube_node "$context" "hive-hub" "hub" "hive-hub" "hub" "hub" "$work_dir" &
  wait
fi

shopt -s nullglob
node_files=("$work_dir"/*.json)
if [ "${#node_files[@]}" -eq 0 ]; then
  echo "no hives found" >&2
  exit 2
fi
format_report "${node_files[@]}"
