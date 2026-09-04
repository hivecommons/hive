#!/usr/bin/env bash
# Unit tests for the stable soak promotion decision gate (#5974).
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
promoter="$script_dir/promote-stable.sh"
fail=0
pass() { echo "  ok: $*"; }
bad() { echo "  FAIL: $*"; fail=1; }

run_decide() {
  env -i PATH="$PATH" \
    CANDIDATE_DIGEST="${CANDIDATE_DIGEST-sha256:candidate}" \
    STABLE_DIGEST="${STABLE_DIGEST-sha256:stable}" \
    CANDIDATE_AGE_SECONDS="${CANDIDATE_AGE_SECONDS-90000}" \
    SOAK_HOURS="${SOAK_HOURS-24}" \
    CURRENT_CANDIDATE="${CURRENT_CANDIDATE-true}" \
    GREEN_EVIDENCE="${GREEN_EVIDENCE-true}" \
    BLOCKER_COUNT="${BLOCKER_COUNT-0}" \
    SMOKE_EVIDENCE="${SMOKE_EVIDENCE-maintained hive heartbeat healthy}" \
    CANDIDATE_GENERATION="${CANDIDATE_GENERATION-200}" \
    STABLE_GENERATION="${STABLE_GENERATION-100}" \
    EMERGENCY_EXCEPTION_REASON="${EMERGENCY_EXCEPTION_REASON-}" \
    EMERGENCY_FOLLOWUP_ISSUE="${EMERGENCY_FOLLOWUP_ISSUE-}" \
    "$promoter" decide
}

expect_decision() {
  local want=$1 desc=$2 needle=${3:-}
  local out got
  out=$(run_decide)
  got=$(awk -F= '/^decision=/{print $2}' <<<"$out")
  if [[ $got == "$want" ]] && { [[ -z $needle ]] || grep -qF "$needle" <<<"$out"; }; then
    pass "$desc"
  else
    bad "$desc (got ${got}; output: ${out})"
  fi
}

expect_decision promote "all five policy conditions promote"
CANDIDATE_AGE_SECONDS=3600 expect_decision hold "minimum soak failure holds" "candidate age"
CURRENT_CANDIDATE=false expect_decision hold "superseded candidate failure holds" "superseded"
GREEN_EVIDENCE=false expect_decision hold "green evidence failure holds" "evidence"
BLOCKER_COUNT=1 expect_decision hold "open release blocker failure holds" "release blocker"
SMOKE_EVIDENCE= expect_decision hold "missing operator smoke signal holds" "smoke signal"
CANDIDATE_GENERATION=100 STABLE_GENERATION=200 expect_decision hold "monotonic guard prevents backwards stable moves" "monotonic guard"
CANDIDATE_DIGEST=sha256:candidate STABLE_DIGEST=mixed-or-not-promoted CANDIDATE_GENERATION=200 STABLE_GENERATION=100 expect_decision promote "partial image promotions remain retryable"
CANDIDATE_AGE_SECONDS=3600 EMERGENCY_EXCEPTION_REASON="security fix risk exceeds waiting; v2 CI and v2 Tests passed; rollback digest sha256:old" EMERGENCY_FOLLOWUP_ISSUE=5975 expect_decision promote "emergency exception bypasses soak with required evidence" "emergency exception recorded"
CANDIDATE_AGE_SECONDS=3600 EMERGENCY_EXCEPTION_REASON="security fix" EMERGENCY_FOLLOWUP_ISSUE= expect_decision hold "emergency exception requires follow-up issue" "follow-up issue"
GREEN_EVIDENCE=false EMERGENCY_EXCEPTION_REASON="security fix" EMERGENCY_FOLLOWUP_ISSUE=5975 expect_decision hold "emergency exception cannot bypass checks" "green release evidence"



tmp_root="$script_dir/../.test-tmp"
mkdir -p "$tmp_root"
tmp="$tmp_root/promote-stable.$$"
mkdir -p "$tmp/bin"
trap 'rm -rf "$tmp"; rmdir "$tmp_root" 2>/dev/null || true' EXIT
cat > "$tmp/bin/docker" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
if [[ $1 == buildx && $2 == imagetools && $3 == inspect ]]; then
  ref=${@: -1}
  if [[ $ref == *':stable' && ${MOCK_STABLE_FAILURE:-0} == 1 ]]; then
    echo 'dial tcp: registry unavailable' >&2
    exit 1
  fi
  if [[ $ref == *':stable' ]]; then gen=${MOCK_STABLE_GENERATION:-100}; else gen=${MOCK_CANDIDATE_GENERATION:-200}; fi
  printf '{"config":{"Labels":{"io.kubestellar.hive.github-actions-run-number":"%s","org.opencontainers.image.revision":"abcdef"}}}\n' "$gen"
  exit 0
fi
printf '%q ' "$@" > "$MOCK_CAPTURE"
printf '\n' >> "$MOCK_CAPTURE"
MOCK
chmod +x "$tmp/bin/docker"

capture="$tmp/create-stable"
PATH="$tmp/bin:$PATH" MOCK_CAPTURE="$capture" MOCK_CANDIDATE_GENERATION=100 MOCK_STABLE_GENERATION=200 \
  "$promoter" publish-stable ghcr.io/hivecommons/hive sha256:candidate false >/dev/null
if [[ -e $capture ]]; then
  bad "publish-stable moved stable backwards despite the monotonic guard"
else
  pass "publish-stable monotonic guard leaves newer stable untouched"
fi
PATH="$tmp/bin:$PATH" MOCK_CAPTURE="$capture" MOCK_CANDIDATE_GENERATION=300 MOCK_STABLE_GENERATION=200 \
  "$promoter" publish-stable ghcr.io/hivecommons/hive sha256:candidate false >/dev/null
grep -q -- '-t ghcr.io/hivecommons/hive:stable ghcr.io/hivecommons/hive@sha256:candidate' "$capture" \
  && pass "publish-stable retags stable by digest when generation advances" \
  || bad "publish-stable did not retag stable by digest on a forward generation"
rm -f "$capture"
if PATH="$tmp/bin:$PATH" MOCK_CAPTURE="$capture" MOCK_STABLE_FAILURE=1 \
    "$promoter" publish-stable ghcr.io/hivecommons/hive sha256:candidate false >/dev/null 2>&1; then
  bad "publish-stable treated a stable inspection failure as generation zero"
elif [[ -e $capture ]]; then
  bad "publish-stable created a tag after a stable inspection failure"
else
  pass "publish-stable fails closed on stable inspection errors"
fi
if PATH="$tmp/bin:$PATH" MOCK_CAPTURE="$capture" MOCK_CANDIDATE_GENERATION=0 MOCK_STABLE_GENERATION=0 \
    "$promoter" mirror-stable ghcr.io/kubestellar/hive ghcr.io/hivecommons/hive@sha256:candidate false >/dev/null 2>&1; then
  bad "mirror-stable accepted a source digest without generation metadata"
elif [[ -e $capture ]]; then
  bad "mirror-stable created a tag from an unverified source digest"
else
  pass "mirror-stable fails closed when source digest metadata is missing"
fi

workflow="$script_dir/../../.github/workflows/docker.yml"
if grep -q 'stable,candidate\|CHANNELS: "stable,candidate"' "$workflow"; then
  bad "docker workflow still publishes stable together with candidate"
else
  pass "docker workflow leaves stable to the promotion workflow"
fi

# The Actions runs API matches head_sha EXACTLY, returning zero runs for a
# short SHA rather than an error. The candidate revision comes from an image
# label, which carries the 7-character form, so an un-expanded value read as
# "no green evidence" for every commit - and an emergency exception explicitly
# refuses to bypass missing green evidence, so the gate could never promote.
# shellcheck source=/dev/null
source <(sed -n '/^full_sha()/,/^}/p' "$promoter")

got=$(full_sha "example/repo" "526ef717269b7f73c9ccbc907ce5b94852a2c0ec")
if [[ $got == "526ef717269b7f73c9ccbc907ce5b94852a2c0ec" ]]; then
  pass "a 40-character SHA is used as-is"
else
  bad "a 40-character SHA must pass through unchanged (got ${got})"
fi

got=$(full_sha "example/definitely-not-a-real-repo-xyz" "abc1234" 2>/dev/null || true)
if [[ $got == "abc1234" ]]; then
  pass "an unresolvable short SHA falls back to the input, so the gate holds"
else
  bad "an unresolvable short SHA must fall back to the input (got ${got})"
fi

if grep -q 'resolved=$(full_sha' "$promoter"; then
  pass "the evidence check expands the revision before querying"
else
  bad "the evidence check must expand the revision before querying"
fi

# "did not run" and "ran and failed" are different answers. docker.yml
# publishes a candidate on EVERY push while v2-ci/v2-tests are path-filtered to
# src/**, so a docs-only merge yields a candidate the suites never looked at.
# Treating that as failure made such a candidate permanently unpromotable;
# treating a real failure as "did not run" would promote rejected code.
if grep -q 'workflow_ran_on()' "$promoter"; then
  pass "evidence distinguishes success, failure and did-not-run"
else
  bad "evidence must distinguish did-not-run from failure"
fi

if sed -n '/^workflow_ran_on()/,/^}/p' "$promoter" | grep -q 'branch=\${RELEASE_BRANCH'; then
  bad "workflow_ran_on must not branch-filter: it hides a failure as did-not-run"
else
  pass "workflow_ran_on matches on head_sha alone so failures stay visible"
fi

if sed -n '/^workflow_success()/,/^}/p' "$promoter" | grep -q 'failure) return 1'; then
  pass "ancestor walk stops at a failure instead of inheriting past it"
else
  bad "ancestor walk must stop at a failure"
fi

echo
if [[ $fail -ne 0 ]]; then
  echo "RESULT: FAIL — stable promotion gate regressed."
  exit 1
fi
echo "RESULT: PASS — stable promotion gate enforces the policy."
