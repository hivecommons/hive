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
CANDIDATE_AGE_SECONDS=3600 expect_decision hold "young current candidate holds even when an older superseded build soaked" "candidate age"
CANDIDATE_AGE_SECONDS=90000 CURRENT_CANDIDATE=false expect_decision hold "superseded candidate failure holds after candidate soak" "superseded"
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

# Promotion must be all-or-nothing across images. Run 34360559434 re-checked
# and published one image at a time, so when hive-hub:candidate changed
# mid-loop, hive and hive-contributor were already retagged: stable ended up a
# mixed generation. Every digest must be re-verified before any publish, and
# that re-verify loop is a separate pass over every image, not interleaved
# with publishing.
promote_fn=$(sed -n '/^promote() {/,/^}/p' "$promoter")
reverify_block=$(sed -n '/candidate that is$/,/^  fi$/p' <<<"$promote_fn")
publish_block=$(sed -n '/already confirmed every image/,/^  fi$/p' <<<"$promote_fn")
if [[ -z $reverify_block || -z $publish_block ]]; then
  bad "promote() no longer has distinct re-verify and publish blocks"
elif grep -q 'publish_stable' <<<"$reverify_block"; then
  bad "promotion publishes inside the verification loop: a mid-loop candidate change leaves stable partially promoted"
elif ! grep -q 'publish_stable' <<<"$publish_block"; then
  bad "promotion block no longer publishes at all"
else
  pass "promotion verifies every candidate digest before publishing any image"
fi

# A candidate superseded between the initial read and the tag move must hold
# (like the in-flight-candidate race below), not fail the whole workflow run
# with exit 1 — a failed run pages someone about a benign, expected race.
superseded="$tmp/superseded"
mkdir -p "$superseded/bin"
cat > "$superseded/bin/docker" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
if [[ $1 == buildx && $2 == imagetools && $3 == inspect ]]; then
  if [[ $* == *'.Manifest.Digest'* ]]; then
    if [[ $* == *':stable'* ]]; then
      echo 'sha256:stable'
    else
      # Simulate a docker.yml run landing mid-promotion: the candidate digest
      # changes between the initial read (first call) and the re-verify
      # right before the tag move (second call).
      count_file="$MOCK_CALL_COUNT_FILE"
      n=$(($(cat "$count_file" 2>/dev/null || echo 0) + 1))
      echo "$n" > "$count_file"
      if [[ $n -le 1 ]]; then echo 'sha256:candidate'; else echo 'sha256:newer-candidate'; fi
    fi
    exit 0
  fi
  ref=${@: -1}
  if [[ $ref == *':stable'* ]]; then gen=100; else gen=200; fi
  printf '{"config":{"Labels":{"io.kubestellar.hive.github-actions-run-number":"%s","org.opencontainers.image.revision":"abcdef"}}}\n' "$gen"
  exit 0
fi
echo "unexpected docker invocation: $*" >&2
exit 1
MOCK
cat > "$superseded/bin/gh" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
if [[ $1 == run && $2 == list ]]; then
  echo '[{"number":200,"createdAt":"2026-09-10T00:00:00Z","status":"completed","conclusion":"success"},{"number":100,"createdAt":"2026-09-01T00:00:00Z","status":"completed","conclusion":"success"}]' \
    | jq -r "${@: -1}"
  exit 0
fi
if [[ $1 == issue && $2 == list ]]; then
  echo 0
  exit 0
fi
if [[ $1 == api ]]; then
  if [[ $* == *'head_sha='* ]]; then
    json='{"workflow_runs":[{"status":"completed","conclusion":"success"}]}'
  else
    json='{"workflow_runs":[]}'
  fi
  jqexpr=""
  prev=""
  for a in "$@"; do
    [[ $prev == --jq ]] && jqexpr=$a
    prev=$a
  done
  if [[ -n $jqexpr ]]; then
    jq -r "$jqexpr" <<<"$json"
  else
    echo "$json"
  fi
  exit 0
fi
echo '[]'
MOCK
chmod +x "$superseded/bin/docker" "$superseded/bin/gh"
out=$(PATH="$superseded/bin:$PATH" REPO=example/repo OWNER=example IMAGE_PREFIX=ghcr.io/example IMAGE_NAMES=hive DRY_RUN=true \
  MOCK_CALL_COUNT_FILE="$superseded/calls" NOW_EPOCH=2000000000 \
  STABLE_PROMOTION_STATE_JSON='{"auto_promote":true,"maintained_hives":[{"id":"h","image_ref":"ghcr.io/example/hive:candidate","git_hash":"abcdef","last_heartbeat_at":"2033-05-18T00:00:00Z","healthy":true,"crash_restarts_24h":0}]}' \
  "$promoter" promote 2>&1) && rc=0 || rc=$?
if [[ $rc -eq 0 ]] && grep -q '^decision=hold' <<<"$out" && grep -q 'superseded by' <<<"$out" && grep -q 'before the tag move' <<<"$out"; then
  pass "a candidate superseded before the tag move holds instead of failing the run"
else
  bad "a candidate superseded before the tag move must hold, not exit 1 (rc=${rc}; output: ${out})"
fi

if sed -n '/^workflow_success()/,/^}/p' "$promoter" | grep -q 'failure) return 1'; then
  pass "ancestor walk stops at a failure instead of inheriting past it"
else
  bad "ancestor walk must stop at a failure"
fi

# The candidate digest is pushed part-way through its docker.yml run, so a
# scheduled promotion can observe a candidate whose run is still in progress.
# workflow_run_created_at then finds no completed run for that generation and
# the gate used to die under set -e with exit 1 and no output at all (observed
# 2026-09-10, run 34488998982). It must instead report an explicit hold.
inflight="$tmp/inflight"
mkdir -p "$inflight/bin"
cat > "$inflight/bin/docker" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
# imagetools inspect REF --format '{{.Manifest.Digest}}'  -> a digest
# imagetools inspect --format '{{json (index .Image ...)}}' REF -> platform config
if [[ $1 == buildx && $2 == imagetools && $3 == inspect ]]; then
  if [[ $* == *'.Manifest.Digest'* ]]; then
    if [[ $* == *':stable'* ]]; then echo 'sha256:stable'; else echo 'sha256:candidate'; fi
    exit 0
  fi
  ref=${@: -1}
  if [[ $ref == *':stable'* ]]; then gen=100; else gen=200; fi
  printf '{"config":{"Labels":{"io.kubestellar.hive.github-actions-run-number":"%s","org.opencontainers.image.revision":"abcdef"}}}\n' "$gen"
  exit 0
fi
echo "unexpected docker invocation: $*" >&2
exit 1
MOCK
cat > "$inflight/bin/gh" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
# gh run list ... : the candidate's run (200) exists but is still in progress,
# so the completed-only filter the promoter applies yields nothing for it.
if [[ $1 == run && $2 == list ]]; then
  echo '[{"number":200,"createdAt":"2026-09-10T14:31:50Z","status":"in_progress","conclusion":null},{"number":199,"createdAt":"2026-09-10T14:24:38Z","status":"completed","conclusion":"success"}]' \
    | jq -r "${@: -1}"
  exit 0
fi
echo '[]'
MOCK
chmod +x "$inflight/bin/docker" "$inflight/bin/gh"
out=$(PATH="$inflight/bin:$PATH" REPO=example/repo OWNER=example IMAGE_PREFIX=ghcr.io/example IMAGE_NAMES=hive DRY_RUN=true STABLE_PROMOTION_STATE_JSON='{"auto_promote":true}' \
  "$promoter" promote 2>&1) && rc=0 || rc=$?
if [[ $rc -eq 0 ]] && grep -q '^decision=hold' <<<"$out" && grep -q 'has not completed yet' <<<"$out"; then
  pass "an in-flight candidate run yields an explicit hold instead of a silent exit 1"
else
  bad "an in-flight candidate run must hold with a reason (rc=${rc}; output: ${out})"
fi

# The soak clock starts when the candidate's docker.yml run FINISHED, not when
# it was queued: a multi-arch build takes tens of minutes, and measuring from
# the run's createdAt credits the candidate with time before its digest existed
# (#10042). Run 200 below was queued two days ago but completed 33 minutes ago,
# so it is still short of the window.
queued_early="$tmp/queued-early"
mkdir -p "$queued_early/bin"
cp "$inflight/bin/docker" "$queued_early/bin/docker"
cat > "$queued_early/bin/gh" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
if [[ $1 == run && $2 == list ]]; then
  echo '[{"number":200,"createdAt":"2033-05-16T00:00:00Z","updatedAt":"2033-05-18T03:00:00Z","status":"completed","conclusion":"success"}]' \
    | jq -r "${@: -1}"
  exit 0
fi
if [[ $1 == issue && $2 == list ]]; then
  echo 0
  exit 0
fi
if [[ $1 == api ]]; then
  if [[ $* == *'head_sha='* ]]; then
    json='{"workflow_runs":[{"status":"completed","conclusion":"success"}]}'
  else
    json='{"workflow_runs":[]}'
  fi
  jqexpr=""
  prev=""
  for a in "$@"; do
    [[ $prev == --jq ]] && jqexpr=$a
    prev=$a
  done
  if [[ -n $jqexpr ]]; then
    jq -r "$jqexpr" <<<"$json"
  else
    echo "$json"
  fi
  exit 0
fi
echo '[]'
MOCK
chmod +x "$queued_early/bin/gh"
out=$(PATH="$queued_early/bin:$PATH" REPO=example/repo OWNER=example IMAGE_PREFIX=ghcr.io/example IMAGE_NAMES=hive DRY_RUN=true NOW_EPOCH=2000000000 \
  STABLE_PROMOTION_STATE_JSON='{"auto_promote":true,"maintained_hives":[{"id":"h","image_ref":"ghcr.io/example/hive:candidate","git_hash":"abcdef","last_heartbeat_at":"2033-05-18T00:00:00Z","healthy":true,"crash_restarts_24h":0}]}' \
  "$promoter" promote 2>&1) && rc=0 || rc=$?
if [[ $rc -eq 0 ]] && grep -q '^decision=hold' <<<"$out" && grep -q 'candidate age' <<<"$out"; then
  pass "soak is measured from the publishing run's completion, not when it was queued"
else
  bad "a build queued before the window but completed inside it must hold (rc=${rc}; output: ${out})"
fi

echo
if [[ $fail -ne 0 ]]; then
  echo "RESULT: FAIL — stable promotion gate regressed."
  exit 1
fi
echo "RESULT: PASS — stable promotion gate enforces the policy."
