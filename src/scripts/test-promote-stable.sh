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
    LINEAGE_AGE_SECONDS="${LINEAGE_AGE_SECONDS-}" \
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
CANDIDATE_AGE_SECONDS=3600 expect_decision hold "young build holds until its own soak completes" "build age"
CANDIDATE_AGE_SECONDS=90000 CURRENT_CANDIDATE=false expect_decision promote "superseded but soaked build remains promotable"
GREEN_EVIDENCE=false expect_decision hold "green evidence failure holds" "evidence"
BLOCKER_COUNT=1 expect_decision hold "open release blocker failure holds" "release blocker"
SMOKE_EVIDENCE= expect_decision hold "missing operator smoke signal holds" "smoke signal"
CANDIDATE_GENERATION=100 STABLE_GENERATION=200 expect_decision hold "monotonic guard prevents backwards stable moves" "monotonic guard"
CANDIDATE_DIGEST=sha256:candidate STABLE_DIGEST=mixed-or-not-promoted CANDIDATE_GENERATION=200 STABLE_GENERATION=100 expect_decision promote "partial image promotions remain retryable"
CANDIDATE_AGE_SECONDS=3600 EMERGENCY_EXCEPTION_REASON="security fix risk exceeds waiting; v2 CI and v2 Tests passed; rollback digest sha256:old" EMERGENCY_FOLLOWUP_ISSUE=5975 expect_decision promote "emergency exception bypasses soak with required evidence" "emergency exception recorded"
CANDIDATE_AGE_SECONDS=3600 EMERGENCY_EXCEPTION_REASON="security fix" EMERGENCY_FOLLOWUP_ISSUE= expect_decision hold "emergency exception requires follow-up issue" "follow-up issue"
GREEN_EVIDENCE=false EMERGENCY_EXCEPTION_REASON="security fix" EMERGENCY_FOLLOWUP_ISSUE=5975 expect_decision hold "emergency exception cannot bypass checks" "green release evidence"

# The workflow consults the hub before evaluating GHCR/GitHub evidence. The
# hub is the operator's durable play/pause switch and the source of automated
# maintained-hive smoke evidence.
# shellcheck source=/dev/null
source <(sed -n '/^stable_promotion_preflight()/,/^}/p' "$promoter")
# shellcheck source=/dev/null
source <(sed -n '/^stable_smoke_from_hub()/,/^}/p' "$promoter")

if sed -n '/^fetch_stable_promotion_state()/,/^}/p' "$promoter" | grep -q -- '--connect-timeout 5 --max-time 20 --retry 2'; then
  pass "hub state fetch has bounded curl timeouts and retries"
else
  bad "hub state fetch must bound curl with connect/max timeouts and retries"
fi

if out=$(STABLE_PROMOTION_STATE_JSON='{"auto_promote":false,"paused_by":"andy","paused_at":"2026-10-01T12:00:00Z"}' stable_promotion_preflight 2>&1); then
  bad "paused stable auto-promotion should skip before evaluation"
elif grep -q 'paused by andy' <<<"$out"; then
  pass "paused hub toggle skips stable promotion with operator attribution"
else
  bad "paused hub toggle did not report attribution (output: ${out})"
fi

if out=$(STABLE_PROMOTION_URL='http://127.0.0.1:1/nope' stable_promotion_preflight 2>&1); then
  bad "unreachable hub should fail closed and skip promotion"
elif grep -q '::warning::could not reach stable auto-promotion state' <<<"$out"; then
  pass "unreachable hub fails closed with a warning"
else
  bad "unreachable hub did not emit the fail-closed warning (output: ${out})"
fi

if out=$(STABLE_PROMOTION_STATE_JSON='not-json' stable_promotion_preflight 2>&1); then
  bad "invalid hub state should fail closed and skip promotion"
elif grep -q '::warning::could not read valid stable auto-promotion state' <<<"$out"; then
  pass "invalid hub state fails closed with a warning"
else
  bad "invalid hub state did not emit the fail-closed warning (output: ${out})"
fi

healthy_state='{"auto_promote":true,"maintained_hives":[{"id":"h-candidate","image_ref":"ghcr.io/hivecommons/hive:candidate","git_hash":"abcdef1","last_heartbeat_at":"2999-01-01T00:00:00Z","healthy":true,"crash_restarts_24h":0}]}'
if out=$(STABLE_PROMOTION_STATE_JSON="$healthy_state" stable_smoke_from_hub "abcdef1" "sha256:candidate" 24); then
  if grep -q 'maintained hive h-candidate healthy' <<<"$out"; then
    pass "healthy candidate hive synthesizes smoke evidence"
  else
    bad "healthy candidate hive evidence text is unclear (output: ${out})"
  fi
else
  bad "healthy candidate hive should satisfy automated smoke evidence"
fi

empty_state='{"auto_promote":true,"maintained_hives":[]}'
if STABLE_PROMOTION_STATE_JSON="$empty_state" stable_smoke_from_hub "abcdef1" "sha256:candidate" 24 >/dev/null; then
  bad "missing candidate hive should not synthesize smoke evidence"
else
  pass "missing candidate hive refuses automated smoke evidence"
fi



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

# Promotion must use a compare-and-set on the stable generation instead of
# requiring a quiet period where the selected build is still candidate.
promote_fn=$(sed -n '/^promote() {/,/^}/p' "$promoter")
if grep -q 'stable generation changed for' <<<"$promote_fn" && grep -q 'recheck_generation >= best_generation' <<<"$promote_fn"; then
  pass "promotion uses a stable-generation compare-and-set before publishing"
else
  bad "promotion must re-read stable generation and hold if it changed before publishing"
fi

if sed -n '/^workflow_success()/,/^}/p' "$promoter" | grep -q 'failure) return 1'; then
  pass "ancestor walk stops at a failure instead of inheriting past it"
else
  bad "ancestor walk must stop at a failure"
fi

# The scanner considers successful docker.yml builds newest to oldest and
# chooses the newest build whose own completion time has soaked, even when a
# newer build has already superseded it on candidate.
select_build="$tmp/select-build"
mkdir -p "$select_build/bin"
cat > "$select_build/bin/docker" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
if [[ $1 == buildx && $2 == imagetools && $3 == inspect ]]; then
  ref=${@: -1}
  if [[ $* == *'.Manifest.Digest'* ]]; then
    case "$*" in
      *:stable*) echo 'sha256:stable' ;;
      *:new1234*) echo 'sha256:new' ;;
      *:old1234*) echo 'sha256:old' ;;
      *:candidate*) echo 'sha256:new' ;;
      *) echo 'sha256:unknown' ;;
    esac
    exit 0
  fi
  case "$ref" in
    *:stable) gen=100; rev=stable00 ;;
    *@sha256:new|*:new1234|*:candidate) gen=300; rev=new1234 ;;
    *@sha256:old|*:old1234) gen=200; rev=old1234 ;;
    *) gen=0; rev=unknown ;;
  esac
  printf '{"config":{"Labels":{"io.kubestellar.hive.github-actions-run-number":"%s","org.opencontainers.image.revision":"%s"}}}\n' "$gen" "$rev"
  exit 0
fi
printf '%q ' "$@" >> "$MOCK_CAPTURE"
printf '\n' >> "$MOCK_CAPTURE"
MOCK
cat > "$select_build/bin/gh" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
jqexpr=""
prev=""
for a in "$@"; do
  [[ $prev == --jq ]] && jqexpr=$a
  prev=$a
done
if [[ $1 == api && $* == *'actions/workflows/docker.yml/runs'* ]]; then
  json='{"workflow_runs":[{"run_number":300,"head_sha":"new1234","updated_at":"2033-05-18T03:00:00Z","status":"completed","conclusion":"success"},{"run_number":200,"head_sha":"old1234","updated_at":"2033-05-16T00:00:00Z","status":"completed","conclusion":"success"}]}'
  jq -r "$jqexpr" <<<"$json"
  exit 0
fi
if [[ $1 == api && $* == *'head_sha='* ]]; then
  json='{"workflow_runs":[{"status":"completed","conclusion":"success"}]}'
  jq -r "$jqexpr" <<<"$json"
  exit 0
fi
if [[ $1 == issue && $2 == list ]]; then
  echo 0
  exit 0
fi
echo '[]'
MOCK
chmod +x "$select_build/bin/docker" "$select_build/bin/gh"
capture="$select_build/create"
out=$(PATH="$select_build/bin:$PATH" MOCK_CAPTURE="$capture" REPO=example/repo OWNER=example IMAGE_PREFIX=ghcr.io/example IMAGE_NAMES=hive DRY_RUN=true NOW_EPOCH=2000000000 \
  STABLE_PROMOTION_STATE_JSON='{"auto_promote":true,"maintained_hives":[{"id":"h","image_ref":"ghcr.io/example/hive:candidate","git_hash":"new1234","last_heartbeat_at":"2033-05-18T00:00:00Z","healthy":true,"crash_restarts_24h":0}]}' \
  "$promoter" promote 2>&1) && rc=0 || rc=$?
if [[ $rc -eq 0 ]] && grep -q '^decision=promote' <<<"$out" && grep -q 'generation 200 > 100' <<<"$out" && grep -q 'sha256:old' <<<"$out"; then
  pass "newest build across the 24h line is selected over a newer unsoaked candidate"
else
  bad "promote should select the newest build across the 24h line by generation (rc=${rc}; output: ${out})"
fi

# A hard-gate failure on the newest build across the 24h line holds that build;
# the scanner must not skip backward to an older build with passing evidence.
held_gate="$tmp/held-gate"
mkdir -p "$held_gate/bin"
cat > "$held_gate/bin/docker" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
if [[ $1 == buildx && $2 == imagetools && $3 == inspect ]]; then
  ref=${@: -1}
  if [[ $* == *'.Manifest.Digest'* ]]; then
    case "$*" in
      *:stable*) echo 'sha256:stable' ;;
      *:fail123*) echo 'sha256:fail' ;;
      *:old1234*) echo 'sha256:old' ;;
      *) echo 'sha256:unknown' ;;
    esac
    exit 0
  fi
  case "$ref" in
    *:stable) gen=100; rev=stable00 ;;
    *@sha256:fail|*:fail123) gen=250; rev=fail123 ;;
    *@sha256:old|*:old1234) gen=200; rev=old1234 ;;
    *) gen=0; rev=unknown ;;
  esac
  printf '{"config":{"Labels":{"io.kubestellar.hive.github-actions-run-number":"%s","org.opencontainers.image.revision":"%s"}}}\n' "$gen" "$rev"
  exit 0
fi
printf '%q ' "$@" >> "$MOCK_CAPTURE"
printf '\n' >> "$MOCK_CAPTURE"
MOCK
cat > "$held_gate/bin/gh" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
jqexpr=""
prev=""
for a in "$@"; do
  [[ $prev == --jq ]] && jqexpr=$a
  prev=$a
done
if [[ $1 == api && $* == *'actions/workflows/docker.yml/runs'* ]]; then
  json='{"workflow_runs":[{"run_number":250,"head_sha":"fail123","updated_at":"2033-05-16T12:00:00Z","status":"completed","conclusion":"success"},{"run_number":200,"head_sha":"old1234","updated_at":"2033-05-16T00:00:00Z","status":"completed","conclusion":"success"}]}'
  jq -r "$jqexpr" <<<"$json"
  exit 0
fi
if [[ $1 == api && $* == *'head_sha='* ]]; then
  json='{"workflow_runs":[{"status":"completed","conclusion":"success"}]}'
  jq -r "$jqexpr" <<<"$json"
  exit 0
fi
if [[ $1 == issue && $2 == list ]]; then
  echo 0
  exit 0
fi
echo '[]'
MOCK
chmod +x "$held_gate/bin/docker" "$held_gate/bin/gh"
capture="$held_gate/create"
out=$(PATH="$held_gate/bin:$PATH" MOCK_CAPTURE="$capture" REPO=example/repo OWNER=example IMAGE_PREFIX=ghcr.io/example IMAGE_NAMES=hive DRY_RUN=true NOW_EPOCH=2000000000 \
  STABLE_PROMOTION_STATE_JSON='{"auto_promote":true,"maintained_hives":[{"id":"old","image_ref":"ghcr.io/example/hive:old1234","git_hash":"old1234","last_heartbeat_at":"2033-05-18T00:00:00Z","healthy":true,"crash_restarts_24h":0}]}' \
  "$promoter" promote 2>&1) && rc=0 || rc=$?
if [[ $rc -eq 0 ]] && grep -q '^decision=hold' <<<"$out" && grep -q 'smoke signal gate is holding build fail123 generation 250' <<<"$out" && [[ ! -s $capture ]]; then
  pass "hard-gate failure holds the newest build across the 24h line"
else
  bad "hard-gate failure must not fall back to an older build (rc=${rc}; output: ${out})"
fi

# If there is no soaked build newer than stable, the hold reason names the
# future eligible_at for the newest unsoaked build.
no_eligible="$tmp/no-eligible"
mkdir -p "$no_eligible/bin"
cp "$select_build/bin/docker" "$no_eligible/bin/docker"
cat > "$no_eligible/bin/gh" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
jqexpr=""
prev=""
for a in "$@"; do
  [[ $prev == --jq ]] && jqexpr=$a
  prev=$a
done
if [[ $1 == api && $* == *'actions/workflows/docker.yml/runs'* ]]; then
  json='{"workflow_runs":[{"run_number":300,"head_sha":"new1234","updated_at":"2033-05-18T03:00:00Z","status":"completed","conclusion":"success"}]}'
  jq -r "$jqexpr" <<<"$json"
  exit 0
fi
if [[ $1 == issue && $2 == list ]]; then echo 0; exit 0; fi
echo '[]'
MOCK
chmod +x "$no_eligible/bin/gh"
out=$(PATH="$no_eligible/bin:$PATH" REPO=example/repo OWNER=example IMAGE_PREFIX=ghcr.io/example IMAGE_NAMES=hive DRY_RUN=true NOW_EPOCH=2000000000 \
  STABLE_PROMOTION_STATE_JSON='{"auto_promote":true}' "$promoter" promote 2>&1) && rc=0 || rc=$?
if [[ $rc -eq 0 ]] && grep -q '^decision=hold' <<<"$out" && grep -q 'eligible_at 2033-05-19T03:00:00Z' <<<"$out"; then
  pass "no eligible build holds with a reason naming eligible_at"
else
  bad "no eligible build must hold with eligible_at (rc=${rc}; output: ${out})"
fi

echo
if [[ $fail -ne 0 ]]; then
  echo "RESULT: FAIL — stable promotion gate regressed."
  exit 1
fi
echo "RESULT: PASS — stable promotion gate enforces the policy."
