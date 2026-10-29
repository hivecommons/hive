#!/usr/bin/env bash
# check-prometheusrule.sh — validate the monitoring component's PrometheusRule
# and Grafana dashboard.
#
# promtool reads plain Prometheus rule files, not the PrometheusRule CRD, so this
# extracts spec.groups from the CRD, runs `promtool check rules` on it, then runs
# the `promtool test rules` unit tests that force each alert to fire (#15438).
# It then checks hive-metrics.json is valid JSON, has a datasource variable, and
# only queries series pkg/dashboard exports (#15439).
#
# Usage: src/scripts/check-prometheusrule.sh   (needs promtool, yq and python3 on PATH)
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
dir="$root/src/deploy/kustomize/components/monitoring"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

yq '{"groups": .spec.groups}' "$dir/prometheusrule.yaml" > "$work/rules.yaml"
promtool check rules "$work/rules.yaml"
cp "$dir/tests/prometheusrule_test.yaml" "$work/"
(cd "$work" && promtool test rules prometheusrule_test.yaml)

# Grafana dashboard (#15439): valid JSON, a datasource variable, and every metric
# it queries is one the dashboard package exports.
python3 - "$dir/hive-metrics.json" "$root/src/pkg/dashboard" <<'PY'
import glob, json, re, sys

dash = json.load(open(sys.argv[1]))
exported = set()
for f in glob.glob(sys.argv[2] + "/metrics_*.go"):
    if not f.endswith("_test.go"):
        # Anchor on the string literal the emitters use, so a name that only
        # survives in a comment does not count as exported.
        exported |= set(re.findall(r'"(hive_[a-z0-9_]+)[{"]', open(f).read()))

# The stage engine defines its counters without the prefix; the dashboard
# package exports them as hive_<name> (writeStageEngineMetrics).
engine = open(sys.argv[2] + "/../stageengine/metrics.go").read()
exported |= {"hive_" + n for n in re.findall(r'Metric\w+\s*=\s*"([a-z_]+_total)"', engine)}

if not any(v["type"] == "datasource" for v in dash["templating"]["list"]):
    sys.exit("no datasource variable")


def panels(ps):
    # A collapsed Grafana row nests its panels under row["panels"].
    for p in ps:
        yield p
        yield from panels(p.get("panels", []))


used = set()
n_panels = 0
for p in panels(dash["panels"]):
    n_panels += 1
    for t in p.get("targets", []):
        used |= set(re.findall(r"\bhive_[a-z_]+\b", t["expr"]))
used -= {"hive_id"}
missing = sorted(used - exported)
if missing:
    sys.exit("dashboard queries metrics Hive does not export: " + ", ".join(missing))
print("dashboard OK: %d panels, %d metrics" % (n_panels, len(used)))
PY
