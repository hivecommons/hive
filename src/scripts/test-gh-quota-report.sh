#!/usr/bin/env bash
# test-gh-quota-report.sh — exercises gh-quota-report.sh formatting from a
# saved /api/gh-rate-limits aggregate so table math and smoke-check exits are
# covered without a Kubernetes cluster.
set -uo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
reporter="${script_dir}/gh-quota-report.sh"
tmp_root="${script_dir}/../.test-tmp"
tmp="${tmp_root}/gh-quota-report.$$"
rm -rf "$tmp"
mkdir -p "$tmp"
trap 'rm -rf "$tmp"' EXIT

fail=0
note_fail() { echo "  FAIL: $*"; fail=1; }
note_ok() { echo "  ok: $*"; }

fixture="${tmp}/aggregate.json"
cat > "$fixture" <<'JSON'
{
  "nodes": [
    {
      "name": "alpha",
      "kind": "spoke",
      "payload": {
        "core": {"limit": 5000, "remaining": 4500, "reset": "2099-01-01T00:00:00Z"},
        "etag_cache": {"hits": 75, "misses": 25, "entries": 12},
        "top_consumers": [
          {"caller": "hive", "endpoint": "/repos/{owner}/{repo}/pulls/{number}", "method": "GET", "requests": 10, "charged": 7, "not_modified": 3, "rate_limited": 0},
          {"caller": "scanner", "endpoint": "/repos/{owner}/{repo}/issues", "method": "GET", "requests": 4, "charged": 4, "not_modified": 0, "rate_limited": 0}
        ]
      }
    },
    {
      "name": "beta",
      "kind": "spoke",
      "payload": {
        "core": {"limit": 60, "remaining": 42, "reset": "2099-01-01T00:00:00Z"},
        "etag_cache": {"hits": 0, "misses": 0, "entries": 0},
        "top_consumers": [
          {"caller": "hive", "endpoint": "/repos/{owner}/{repo}/pulls/{number}", "method": "GET", "requests": 5, "charged": 5, "not_modified": 0, "rate_limited": 0}
        ]
      }
    }
  ]
}
JSON

if output=$(bash "$reporter" --from-file "$fixture" --top 1 2>&1); then
  note_ok "fixture report exits 0 when no quota smoke threshold is tripped"
else
  note_fail "fixture report should exit 0, got output:"
  printf '%s\n' "$output" | sed 's/^/      | /'
fi

if printf '%s\n' "$output" | grep -qF "alpha                    app/token  4500/5000"; then
  note_ok "app/token identity and core budget are rendered"
else
  note_fail "expected alpha app/token core row"
  printf '%s\n' "$output" | sed 's/^/      | /'
fi

if printf '%s\n' "$output" | grep -qF "beta                     anonymous  42/60"; then
  note_ok "anonymous identity is rendered for a 60/hr core limit"
else
  note_fail "expected beta anonymous core row"
fi

if printf '%s\n' "$output" | grep -qF "total charged/hr: 16"; then
  note_ok "fleet total charged/hr is summed from top consumers"
else
  note_fail "expected total charged/hr: 16"
fi

if printf '%s\n' "$output" | grep -qF "      12       15            0 hive GET /repos/{owner}/{repo}/pulls/{number}"; then
  note_ok "fleet top consumers aggregate caller+method+endpoint across hives"
else
  note_fail "expected aggregate /pulls/{number} top-consumer row"
fi

bad_fixture="${tmp}/bad.json"
python3 - "$fixture" "$bad_fixture" <<'PY'
import json
import sys
with open(sys.argv[1], encoding="utf-8") as fh:
    data = json.load(fh)
data["nodes"][0]["payload"]["core"]["remaining"] = 100
with open(sys.argv[2], "w", encoding="utf-8") as fh:
    json.dump(data, fh)
PY

if bash "$reporter" --from-file "$bad_fixture" --top 1 >/dev/null 2>&1; then
  note_fail "low remaining quota should exit non-zero"
else
  note_ok "low remaining quota trips the smoke-check exit"
fi

if [ "$fail" -ne 0 ]; then
  echo "test-gh-quota-report FAILED"
  exit 1
fi

echo "test-gh-quota-report OK"
