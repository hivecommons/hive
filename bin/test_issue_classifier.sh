#!/usr/bin/env bash
# Contract tests for bin/issue-classifier.sh.
# Run: bash bin/test_issue_classifier.sh
#
# issue-classifier.sh is a pre-kick pipeline stage (run-pipeline.sh, declared
# in hive-project.yaml → pipeline.stages) that enriches actionable.json
# in-place with complexity_tier, model_recommendation, is_tracker, lane,
# needs_architecture_review, cluster_key and the clusters/classification
# blocks. kick-agents.sh reads those fields verbatim into every scanner kick
# message ("[{tier}/{model}]", lane filtering, BUNDLE lines), and agent
# policies tell agents to trust them rather than classify for themselves. A
# regression here misroutes every issue in the hive — wrong model, wrong lane,
# wrong bundle — and ships green, because nothing executed this script.
#
# Like bin/test_enumerate_actionable.sh this EXECUTES the script rather than
# grepping it. The script hardcodes /var/run/hive-metrics/actionable.json and
# /var/log/kick-agents.log with no env override, so the harness runs a COPY
# with those two paths rewritten to a temp dir (and asserts the rewrite
# landed — a refactor that renames them must fail here loudly, not silently
# run the tests against the real paths). The config path IS env-overridable
# (HIVE_PROJECT_YAML), so the harness supplies its own classification rules
# and, in one case, points at a missing file to pin the built-in defaults.
#
# Doctrine (audit 6/7): every classification assertion sits next to a
# contrasting case (Simple next to Complex next to Medium; clustered next to
# unclustered), so a classifier that stamps everything with one value cannot
# pass. Hermetic: no network, never touches /var/run, /var/log or /etc/hive.
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SCRIPT="${REPO_ROOT}/bin/issue-classifier.sh"

PASS=0
FAIL=0

pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
fail() {
  echo "  FAIL: $1"
  [ $# -gt 1 ] && echo "        $2"
  FAIL=$((FAIL + 1))
}

# A missing harness dependency must be red, not a silent exit 0 (#5388
# doctrine). ubuntu-latest ships jq, python3 and PyYAML; the script itself
# needs python3+yaml and this harness asserts through jq.
for dep in jq python3; do
  if ! command -v "$dep" >/dev/null 2>&1; then
    echo "harness-error: $dep is required (script under test needs python3; assertions need jq)"
    exit 1
  fi
done
if ! python3 -c 'import yaml' 2>/dev/null; then
  echo "harness-error: python3 PyYAML is required (the script under test imports yaml)"
  exit 1
fi

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

RUN_DIR="${WORK}/run"               # stands in for /var/run/hive-metrics
LOG_FILE="${WORK}/kick-agents.log"  # stands in for /var/log/kick-agents.log
IN="${RUN_DIR}/actionable.json"
CONFIG="${WORK}/hive-project.yaml"
# The empty examples/ dir matters: when HIVE_PROJECT_YAML is missing the
# script falls back to `find <script-root>/examples -name hive-project.yaml`.
# With the dir present-but-empty that find succeeds with no output and the
# python defaults apply — the case test 8 pins. (If the dir does not exist at
# all, `set -euo pipefail` kills the script on the failing find instead; that
# latent crash is out of scope here and noted in the tracking issue.)
mkdir -p "$RUN_DIR" "${WORK}/bin" "${WORK}/examples"

# ── The path-rewritten copy ──────────────────────────────────────────────────
SCRIPT_COPY="${WORK}/bin/issue-classifier.sh"
sed \
  -e "s|/var/run/hive-metrics|${RUN_DIR}|g" \
  -e "s|/var/log/kick-agents.log|${LOG_FILE}|g" \
  "$SCRIPT" >"$SCRIPT_COPY"
if grep -qE '/var/run/hive-metrics|/var/log/kick-agents.log' "$SCRIPT_COPY" \
   || ! grep -q "$RUN_DIR" "$SCRIPT_COPY"; then
  echo "harness-error: path rewrite did not land — the script's hardcoded paths moved; update the sed above"
  exit 1
fi

# ── Classification rules under test ──────────────────────────────────────────
# Deliberately small and distinct from the shipped example so the assertions
# pin "rules come from the config", not "rules happen to match the defaults".
cat >"$CONFIG" <<'YAML'
classification:
  complexity:
    simple:
      labels: ["auto-qa"]
      title_patterns:
        - '\btypo\b'
      model: "haiku"
    complex:
      labels: ["architecture"]
      title_patterns:
        - '\brefactor\b'
      model: "opus"
    default_model: "sonnet"
  tracker_prefixes: ["[Nightly]"]
  lanes:
    architect:
      labels: ["architecture"]
      title_patterns:
        - '\brefactor\b'
    outreach:
      labels: ["outreach"]
  clustering:
    reporter_window_seconds: 1800
    failure_modes:
      tls: 'tls|certificate'
YAML

run_classifier() {
  # $1: config path (may be a missing file to exercise the defaults).
  HIVE_PROJECT_YAML="$1" bash "$SCRIPT_COPY" >"${WORK}/stdout" 2>"${WORK}/stderr"
  echo "$?"
}

jget() { jq -r "$1" "$IN"; }
# Look an issue up by number so assertions survive re-ordering.
iget() { jq -r --argjson n "$1" '.issues.items[] | select(.number == $n) | '"$2" "$IN"; }

assert_eq() {
  local name="$1" got="$2" want="$3"
  if [ "$got" = "$want" ]; then pass "$name"; else fail "$name" "got '$got', want '$want'"; fi
}

# ── Fixture: one document exercising every rule, with contrast cases ─────────
# Distinct authors everywhere except the deliberate reporter-window trio, so
# no accidental reporter cluster hides a regression in the intended ones.
write_fixture() {
  cat >"$IN" <<'JSON'
{
  "prs": {"count": 3, "sentinel": "must-survive-enrichment"},
  "issues": {
    "count": 14,
    "items": [
      {"repo": "acme/primary", "number": 1,  "title": "docs: typo in readme",             "labels": ["auto-qa"],          "author": "a1", "created_at": "2026-09-19T10:00:00Z"},
      {"repo": "acme/primary", "number": 2,  "title": "fix typo in banner",               "labels": [],                   "author": "a2", "created_at": "2026-09-19T10:01:00Z"},
      {"repo": "acme/primary", "number": 3,  "title": "rework the payment flow",          "labels": ["architecture"],     "author": "a3", "created_at": "2026-09-19T10:02:00Z"},
      {"repo": "acme/primary", "number": 4,  "title": "refactor the scheduler",           "labels": [],                   "author": "a4", "created_at": "2026-09-19T10:03:00Z"},
      {"repo": "acme/primary", "number": 5,  "title": "something entirely plain",         "labels": [],                   "author": "a5", "created_at": "2026-09-19T10:04:00Z"},
      {"repo": "acme/primary", "number": 6,  "title": "[Nightly] suite run 2026-09-19",   "labels": [],                   "author": "a6", "created_at": "2026-09-19T10:05:00Z"},
      {"repo": "acme/primary", "number": 7,  "title": "submit hive to a list",            "labels": ["outreach"],         "author": "a7", "created_at": "2026-09-19T10:06:00Z"},
      {"repo": "acme/primary", "number": 8,  "title": "docs: broken link in install",     "labels": [],                   "author": "a8", "created_at": "2026-09-19T10:07:00Z"},
      {"repo": "acme/primary", "number": 10, "title": "intermittent flake alpha",         "labels": [],                   "author": "flaky-bot", "created_at": "2026-09-19T11:00:00Z"},
      {"repo": "acme/primary", "number": 11, "title": "intermittent flake beta",          "labels": [],                   "author": "flaky-bot", "created_at": "2026-09-19T11:05:00Z"},
      {"repo": "acme/primary", "number": 12, "title": "intermittent flake gamma",         "labels": [],                   "author": "flaky-bot", "created_at": "2026-09-19T13:30:00Z"},
      {"repo": "acme/primary", "number": 13, "title": "TLS handshake fails on ingress",   "labels": [],                   "author": "a13", "created_at": "2026-09-19T10:08:00Z"},
      {"repo": "acme/primary", "number": 14, "title": "certificate expired on the edge",  "labels": [],                   "author": "a14", "created_at": "2026-09-19T10:09:00Z"},
      {"repo": "acme/primary", "number": 15, "title": "packet loss between zones",        "labels": ["area/networking"],  "author": "a15", "created_at": "2026-09-19T10:10:00Z"}
    ]
  }
}
JSON
}

# ── 1. Missing input file: a skipped stage, never a failed kick ─────────────
echo "-- missing input --"
rm -f "$IN"
rc="$(run_classifier "$CONFIG")"
assert_eq "missing actionable.json exits 0 (pipeline stage skips, kick continues)" "$rc" "0"
grep -q 'ERROR.*not found' "$LOG_FILE" 2>/dev/null && pass "the skip is logged as an ERROR" || fail "the skip is logged as an ERROR" "log: $(tail -n1 "$LOG_FILE" 2>/dev/null)"

# ── 2. Complexity tiers and model recommendations come from the config ───────
echo "-- complexity tiers --"
write_fixture
rc="$(run_classifier "$CONFIG")"
assert_eq "classifier exits 0 on a real document" "$rc" "0"
jq -e . "$IN" >/dev/null 2>&1 && pass "enriched actionable.json is valid JSON" || fail "enriched actionable.json is valid JSON"
assert_eq "simple label → Simple + configured model"        "$(iget 1 '"\(.complexity_tier)/\(.model_recommendation)"')" "Simple/haiku"
assert_eq "simple title pattern → Simple + configured model" "$(iget 2 '"\(.complexity_tier)/\(.model_recommendation)"')" "Simple/haiku"
assert_eq "complex label → Complex + configured model"       "$(iget 3 '"\(.complexity_tier)/\(.model_recommendation)"')" "Complex/opus"
assert_eq "complex title pattern → Complex + configured model" "$(iget 4 '"\(.complexity_tier)/\(.model_recommendation)"')" "Complex/opus"
assert_eq "no rule matches → Medium + default model"         "$(iget 5 '"\(.complexity_tier)/\(.model_recommendation)"')" "Medium/sonnet"
assert_eq "tier counts tally the items"                      "$(jget '"\(.classification.tier_counts.Simple)/\(.classification.tier_counts.Medium)/\(.classification.tier_counts.Complex)"')" "2/10/2"

# ── 3. Tracker detection is prefix-anchored ──────────────────────────────────
echo "-- trackers --"
assert_eq "configured tracker prefix → is_tracker"        "$(iget 6 '.is_tracker')" "true"
assert_eq "plain issue is not a tracker"                  "$(iget 5 '.is_tracker')" "false"
assert_eq "tracker_count tallies"                         "$(jget '.classification.tracker_count')" "1"

# ── 4. Lane assignment: first match wins, architect flags review ─────────────
echo "-- lanes --"
assert_eq "architecture label → architect lane + review flag" "$(iget 3 '"\(.lane)/\(.needs_architecture_review)"')" "architect/true"
assert_eq "architect title pattern → architect lane"          "$(iget 4 '"\(.lane)/\(.needs_architecture_review)"')" "architect/true"
assert_eq "outreach label → outreach lane, no review flag"    "$(iget 7 '"\(.lane)/\(.needs_architecture_review)"')" "outreach/false"
assert_eq "unmatched issue defaults to scanner lane"          "$(iget 5 '.lane')" "scanner"
assert_eq "lane counts tally"                                 "$(jget '"\(.classification.lane_counts.architect)/\(.classification.lane_counts.outreach)/\(.classification.lane_counts.scanner)"')" "2/1/11"

# ── 5. Cluster keys: title prefix, then area/kind labels, else null ──────────
echo "-- cluster keys --"
assert_eq "title prefix before ':' becomes the cluster key" "$(iget 8 '.cluster_key')" "docs"
assert_eq "area/ label is the fallback cluster key"         "$(iget 15 '.cluster_key')" "area/networking"
assert_eq "no prefix and no area/kind label → null key"     "$(iget 5 '.cluster_key')" "null"

# ── 6. Clusters need 2+ members; singletons never bundle ─────────────────────
echo "-- clusters --"
assert_eq "shared title prefix forms a cluster of 2" \
  "$(jq -r '.clusters[] | select(.key == "docs") | [.issues[].number] | sort | join(",")' "$IN")" "1,8"
assert_eq "area/networking singleton forms no cluster" \
  "$(jq -r '[.clusters[] | select(.key == "area/networking")] | length' "$IN")" "0"

# Reporter window: two issues by one author 5 minutes apart bundle; the third,
# hours later, stays out — the window anchors at its first member.
assert_eq "reporter window bundles the pair inside the window" \
  "$(jq -r '.clusters[] | select(.key | startswith("reporter-flaky-bot")) | [.issues[].number] | sort | join(",")' "$IN")" "10,11"
assert_eq "the issue outside the window keeps a null key"      "$(iget 12 '.cluster_key')" "null"
assert_eq "in-window issues carry the reporter cluster key"    "$(iget 10 '.cluster_key')" "reporter-flaky-bot-10"

# Failure-mode regex from the config bundles by title across authors.
assert_eq "failure-mode regex bundles matching titles" \
  "$(jq -r '.clusters[] | select(.key == "failure-tls") | [.issues[].number] | sort | join(",")' "$IN")" "13,14"

assert_eq "cluster_count tallies the surviving bundles" "$(jget '.classification.cluster_count')" "3"
assert_eq "no issue appears in two clusters" \
  "$(jq -r '[.clusters[].issues[] | "\(.repo)#\(.number)"] | length == (unique | length)' "$IN")" "true"

# ── 7. Enrichment is in-place: everything else survives ──────────────────────
echo "-- in-place enrichment --"
assert_eq "sibling top-level blocks survive enrichment" "$(jget '.prs.sentinel')" "must-survive-enrichment"
assert_eq "no items are dropped or invented"            "$(jget '.issues.items | length')" "14"
assert_eq "config_source records where the rules came from" "$(jget '.classification.config_source')" "$CONFIG"
grep -q 'DONE' "$LOG_FILE" && pass "completion is logged" || fail "completion is logged"

# ── 8. Missing config: built-in defaults, never a crash ──────────────────────
echo "-- missing config --"
cat >"$IN" <<'JSON'
{
  "issues": {
    "count": 3,
    "items": [
      {"repo": "acme/primary", "number": 21, "title": "coverage hole in parser",    "labels": ["auto-qa"],      "author": "b1", "created_at": "2026-09-19T10:00:00Z"},
      {"repo": "acme/primary", "number": 22, "title": "[Auto-QA] nightly findings", "labels": [],               "author": "b2", "created_at": "2026-09-19T10:01:00Z"},
      {"repo": "acme/primary", "number": 23, "title": "split the api server",       "labels": ["architecture"], "author": "b3", "created_at": "2026-09-19T10:02:00Z"}
    ]
  }
}
JSON
rc="$(run_classifier "${WORK}/no-such-config.yaml")"
assert_eq "missing config exits 0 (defaults apply)" "$rc" "0"
assert_eq "default simple rule: auto-qa label → Simple/haiku"        "$(iget 21 '"\(.complexity_tier)/\(.model_recommendation)"')" "Simple/haiku"
assert_eq "default tracker prefix [Auto-QA] is honoured"             "$(iget 22 '.is_tracker')" "true"
assert_eq "default complex rule: architecture label → Complex/opus"  "$(iget 23 '"\(.complexity_tier)/\(.model_recommendation)"')" "Complex/opus"
assert_eq "no lanes configured → everything is scanner"              "$(jget '[.issues.items[].lane] | unique | join(",")')" "scanner"

echo
echo "=== $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ] || exit 1
