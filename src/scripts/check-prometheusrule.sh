#!/usr/bin/env bash
# check-prometheusrule.sh — validate the monitoring component's PrometheusRule.
#
# promtool reads plain Prometheus rule files, not the PrometheusRule CRD, so this
# extracts spec.groups from the CRD, runs `promtool check rules` on it, then runs
# the `promtool test rules` unit tests that force each alert to fire (#15438).
#
# Usage: src/scripts/check-prometheusrule.sh   (needs promtool and yq on PATH)
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
dir="$root/src/deploy/kustomize/components/monitoring"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

yq '{"groups": .spec.groups}' "$dir/prometheusrule.yaml" > "$work/rules.yaml"
promtool check rules "$work/rules.yaml"
cp "$dir/tests/prometheusrule_test.yaml" "$work/"
(cd "$work" && promtool test rules prometheusrule_test.yaml)
