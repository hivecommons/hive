#!/usr/bin/env bash
# #13166: the entrypoint provisions a user, private token group and token
# cache file for every name hive_collect_agent_names prints. `replicas:`
# copies (<agent>-2 .. <agent>-N) exist only in memory, so they must be
# derived from the config here or the replica can never read its token.
# Run: bash src/deploy/test_entrypoint_replica_names.sh
set -uo pipefail

PASS=0
FAIL=0
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENTRYPOINT="$HERE/entrypoint.sh"
WORK="$HERE/.test-entrypoint-replica-names-$$"

ok() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
bad() { echo "  FAIL: $1"; [ -n "${2:-}" ] && echo "        $2"; FAIL=$((FAIL + 1)); }
cleanup() { rm -rf "$WORK"; }
trap cleanup EXIT

mkdir -p "$WORK"
HELPERS="$WORK/collect.sh"
awk '/^hive_collect_agent_names\(\) \{/{f=1} f{print} f&&/^}$/{exit}' "$ENTRYPOINT" > "$HELPERS"
if [ ! -s "$HELPERS" ]; then
  bad "hive_collect_agent_names is extractable from entrypoint.sh"
  echo "Results: $PASS passed, $FAIL failed"
  exit 1
fi
# shellcheck disable=SC1090
# Intentionally execute the extracted shipped helper.
. "$HELPERS"
unset HIVE_LEVEL

expect() {
  local desc="$1" want="$2" cfg="$WORK/hive.yaml" got
  cat > "$cfg"
  got="$(hive_collect_agent_names "$cfg" | paste -sd' ' -)"
  if [ "$got" = "$want" ]; then
    ok "$desc"
  else
    bad "$desc" "got '$got', want '$want'"
  fi
}

expect "agents without replicas are listed as-is" "quality scanner" <<'YAML'
agents:
  scanner: {backend: claude}
  quality: {backend: claude, replicas: 1}
YAML

expect "replicas: 3 adds <agent>-2 and <agent>-3" "code-reviewer code-reviewer-2 code-reviewer-3 scanner" <<'YAML'
agents:
  code-reviewer: {backend: claude, replicas: 3}
  scanner: {backend: codex}
YAML

expect "replicas: 5 (the maximum) expands fully" "a a-2 a-3 a-4 a-5" <<'YAML'
agents:
  a: {replicas: 5}
YAML

expect "out-of-range or malformed counts expand to nothing" "a b c d" <<'YAML'
agents:
  a: {replicas: 6}
  b: {replicas: -1}
  c: {replicas: many}
  d: null
YAML

expect "list-form agents expand too" "fixer fixer-2" <<'YAML'
agents:
  - name: fixer
    replicas: 2
YAML

# #13542: review_heads derive <base>-<alias>-<n>, mirroring
# config.ReviewHeadAgentName (pkg/config/review_heads_test.go uses the same cases).
expect "review_heads expand per head and replica" "code-reviewer code-reviewer-claude-sonnet-5-5-1 code-reviewer-claude-sonnet-5-5-2 code-reviewer-codex-gpt-6-sol-1 code-reviewer-sol-6-1" <<'YAML'
agents:
  code-reviewer:
    backend: claude
    review_heads:
      - {backend: claude, model: claude-sonnet-5-5, replicas: 2}
      - {backend: codex, model: gpt-6-sol}
      - {backend: codex, model: gpt-6-sol, alias: Sol_6}
YAML

expect "malformed or out-of-range heads expand to nothing" "r" <<'YAML'
agents:
  r:
    review_heads:
      - {backend: codex}
      - {backend: codex, model: m, replicas: 6}
      - not-a-head
YAML

# #14685: review.reviewers.autoscale grows the lane's base agent up to `max`
# at runtime, so the entrypoint pre-creates those names (mirrors
# config.ReviewerAutoscaleAgent and DefaultReviewerAutoscaleMax).
expect "autoscale expands the role:reviewer lane to max" "reviewer reviewer-2 reviewer-3 scanner" <<'YAML'
agents:
  scanner: {backend: codex}
  reviewer: {backend: claude, role: reviewer, replicas: 2}
review:
  reviewers:
    autoscale: {enabled: true, max: 3}
YAML

expect "autoscale default max is 4, declared replicas above it still win" "reviewer reviewer-2 reviewer-3 reviewer-4 reviewer-5" <<'YAML'
agents:
  reviewer: {backend: claude, role: reviewer, replicas: 5}
review:
  reviewers:
    autoscale: {enabled: true}
YAML

expect "autoscale honours an explicit lane agent and the replica cap" "critic critic-2 critic-3 critic-4 critic-5 reviewer" <<'YAML'
agents:
  reviewer: {backend: claude, role: reviewer}
  critic: {backend: codex}
review:
  reviewers:
    autoscale: {enabled: true, agent: critic, max: 9}
YAML

expect "autoscale falls back to review.reviewer_agents[0]" "qa qa-2 qa-3 qa-4" <<'YAML'
agents:
  qa: {backend: claude}
review:
  reviewer_agents: [qa]
  reviewers:
    autoscale: {enabled: true}
YAML

expect "autoscale off expands nothing extra" "reviewer reviewer-2" <<'YAML'
agents:
  reviewer: {backend: claude, role: reviewer, replicas: 2}
review:
  reviewers:
    autoscale: {enabled: false, max: 4}
YAML

expect "an empty config prints nothing" "" <<'YAML'
YAML

echo "Results: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
