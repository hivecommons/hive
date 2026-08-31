#!/usr/bin/env bash
# test-release-push-retry.sh — exercises the `push_v4` step of
# .github/workflows/release.yml (#5142, #5222) by extracting its script from
# the workflow file itself and driving it with stubbed `git`/`gh`, so the
# merge state machine is proven against the shipped source rather than a copy.
#
# Since #5222 the step opens a PR from the scratch branch into v4 and merges
# it via `gh pr merge` (SHA-keyed required-status evaluation) instead of a
# raw `git push` to v4 (ref-keyed evaluation, which is why the raw push could
# never succeed — see the workflow's RACES header comment). The step
# distinguishes three ways that merge can fail:
#   not-yet-mergeable — GitHub is still settling mergeable_state after the
#                    gate check landed: retry on a deadline, then hard-fail.
#   base moved       — v4 advanced past the release SHA: defer GREEN with
#                    pushed=false, because the successor run releases the
#                    newer merge and retagging here would publish images
#                    built from a different tree.
#   anything else    — hard-fail.
#
# Usage: src/scripts/test-release-push-retry.sh
set -uo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
repo_root=$(cd -- "$script_dir/../.." && pwd)
workflow="$repo_root/.github/workflows/release.yml"
tmp_root=${TMPDIR:-"$script_dir/../.test-tmp"}
mkdir -p "$tmp_root"
tmp=$(mktemp -d "$tmp_root/release-push.XXXXXX")
trap 'rm -rf "$tmp"' EXIT

fail=0
note_fail() { echo "  FAIL: $*"; fail=1; }
note_ok()   { echo "  ok: $*"; }

if ! python3 -c 'import yaml' 2>/dev/null; then
  echo "FAIL: python3 with PyYAML is required to extract the step under test" >&2
  exit 1
fi

# --- extract the step's script verbatim from the workflow ---------------------
python3 - "$workflow" > "$tmp/push_v4.sh" <<'PY'
import sys, yaml
w = yaml.safe_load(open(sys.argv[1]))
for st in w["jobs"]["release"]["steps"]:
    if st.get("id") == "push_v4":
        sys.stdout.write(st["run"])
        break
else:
    sys.exit("no step with id push_v4 in the release job")
PY

# --- stub git + gh + sleep -----------------------------------------------------
# The stubs read their scenario from RPR_SCENARIO and count merge attempts in
# RPR_STATE. `gh pr merge` output mimics real rejection text so the ORDER of
# the step's grep checks is exercised, not just their presence: "base branch
# was modified" must NOT be captured as a generic retry case, and a
# not-yet-mergeable rejection must NOT be captured by the base-moved pattern.
mkdir -p "$tmp/bin"
cat > "$tmp/bin/git" <<'STUB'
#!/usr/bin/env bash
state="$RPR_STATE"
case "$1 $2 ${3:-}" in
  "rev-parse HEAD")
    echo "deadbeefcafe0000000000000000000000000000"; exit 0 ;;
  "rev-parse origin/v4")
    echo "f00df00df00d0000000000000000000000000000"; exit 0 ;;
  "fetch origin")
    exit 0 ;;
  "tag "*) exit 0 ;;
  "push origin --delete")
    exit 0 ;;
  "push origin refs/tags/"*)
    n=$(( $(cat "$state/tag" 2>/dev/null || echo 0) + 1 ))
    echo "$n" > "$state/tag"
    case "${RPR_TAG_SCENARIO:-ok}" in
      ok) exit 0 ;;
      flaky_twice) [ "$n" -le 2 ] && exit 1; exit 0 ;;
      dead) exit 1 ;;
    esac ;;
  *) exit 0 ;;
esac
STUB
cat > "$tmp/bin/gh" <<'STUB'
#!/usr/bin/env bash
state="$RPR_STATE"
case "$1 $2" in
  "pr create")
    echo "https://github.com/kubestellar/hive/pull/9999"
    exit 0 ;;
  "pr merge")
    n=$(( $(cat "$state/merge" 2>/dev/null || echo 0) + 1 ))
    echo "$n" > "$state/merge"
    case "$RPR_SCENARIO" in
      ok) echo "✓ Merged pull request #9999"; exit 0 ;;
      settle_then_ok)
        if [ "$n" -le 2 ]; then
          echo "Pull request #9999 is not mergeable: the merge commit cannot be cleanly created."
          exit 1
        fi
        echo "✓ Merged pull request #9999"; exit 0 ;;
      settle_forever)
        echo "Pull request #9999 is not mergeable: the merge commit cannot be cleanly created."
        exit 1 ;;
      base_moved)
        echo "Pull request #9999 is not mergeable: the base branch was modified. Review the PR and try again."
        exit 1 ;;
      unknown)
        echo "gh: connection reset by peer"
        exit 1 ;;
    esac ;;
  "pr close")
    exit 0 ;;
  *) exit 0 ;;
esac
STUB
cat > "$tmp/bin/sleep" <<'STUB'
#!/usr/bin/env bash
echo "$1" >> "$RPR_STATE/sleeps"
exit 0
STUB
chmod +x "$tmp/bin/git" "$tmp/bin/gh" "$tmp/bin/sleep"

# run_step <scenario> [tag-scenario] [gh006-window] — runs the extracted step;
# sets rc, output, ghout (the $GITHUB_OUTPUT contents), state dir in $st.
run_step() {
  st="$tmp/state.$RANDOM"
  mkdir -p "$st"
  : > "$st/out"
  RPR_SCENARIO="$1" RPR_TAG_SCENARIO="${2:-ok}" RPR_STATE="$st" \
    RELEASE_PUSH_GH006_WINDOW="${3:-120}" \
    VERSION="4.0.1" SHA="deadbeefcafe" GITHUB_OUTPUT="$st/gh_output" \
    PATH="$tmp/bin:$PATH" bash "$tmp/push_v4.sh" > "$st/out" 2>&1
  rc=$?
  output=$(cat "$st/out")
  ghout=$(cat "$st/gh_output" 2>/dev/null || true)
}

echo "case: clean merge succeeds first try"
run_step ok
[ "$rc" -eq 0 ] && note_ok "exit 0" || note_fail "exit $rc, want 0: $output"
grep -q '^pushed=true$' <<<"$ghout" && note_ok "pushed=true" || note_fail "GITHUB_OUTPUT lacks pushed=true: $ghout"
[ "$(cat "$st/tag" 2>/dev/null)" = 1 ] && note_ok "tag pushed once" || note_fail "tag not pushed exactly once"

echo "case: mergeable_state settling twice, then success"
run_step settle_then_ok
[ "$rc" -eq 0 ] && note_ok "exit 0 after retries" || note_fail "exit $rc, want 0: $output"
[ "$(cat "$st/merge")" = 3 ] && note_ok "3 merge attempts" || note_fail "$(cat "$st/merge") attempts, want 3"
grep -q '^pushed=true$' <<<"$ghout" && note_ok "pushed=true" || note_fail "GITHUB_OUTPUT lacks pushed=true"
grep -qx '8' "$st/sleeps" && note_ok "waited between retries" || note_fail "no 8s sleep recorded between retries"
# A generic not-yet-mergeable rejection must not have been misread as the
# base having moved — that would defer green on a settling-time artifact and
# silently skip the release.
grep -q 'pushed=false' <<<"$ghout" && note_fail "settling rejection was misclassified as base-moved and deferred" || note_ok "settling rejection not misread as base-moved"

echo "case: settling past the deadline hard-fails"
run_step settle_forever ok 0
[ "$rc" -ne 0 ] && note_ok "non-zero exit" || note_fail "exhausted retries must fail, got exit 0"
grep -q '::error::' <<<"$output" && note_ok "::error:: emitted" || note_fail "no ::error:: on exhaustion: $output"
[ -f "$st/tag" ] && note_fail "tag was pushed despite the merge failing" || note_ok "no tag pushed"

echo "case: base branch moved defers green with pushed=false"
run_step base_moved
[ "$rc" -eq 0 ] && note_ok "exit 0 (deferral is not a failure)" || note_fail "exit $rc, want 0: $output"
grep -q '^pushed=false$' <<<"$ghout" && note_ok "pushed=false" || note_fail "GITHUB_OUTPUT lacks pushed=false: $ghout"
grep -q '::notice::' <<<"$output" && note_ok "::notice:: explains the deferral" || note_fail "deferral is silent: $output"
[ -f "$st/tag" ] && note_fail "tag was pushed for a release that deferred" || note_ok "no tag pushed"
[ "$(cat "$st/merge")" = 1 ] && note_ok "no pointless retry of a lost race" || note_fail "base-moved was retried; it can never succeed without a rebase"

echo "case: unrecognized merge failure hard-fails"
run_step unknown
[ "$rc" -ne 0 ] && note_ok "non-zero exit" || note_fail "unknown failure must not be retried or deferred, got exit 0"
grep -q 'pushed=' <<<"$ghout" && note_fail "unknown failure still wrote a pushed= output" || note_ok "no pushed= output"

echo "case: transient tag-push failure is retried"
run_step ok flaky_twice
[ "$rc" -eq 0 ] && note_ok "exit 0" || note_fail "exit $rc, want 0: $output"
[ "$(cat "$st/tag")" = 3 ] && note_ok "3 tag-push attempts" || note_fail "$(cat "$st/tag") tag attempts, want 3"
grep -q '^pushed=true$' <<<"$ghout" && note_ok "pushed=true" || note_fail "GITHUB_OUTPUT lacks pushed=true"

echo "case: tag push dead after 3 attempts hard-fails"
run_step ok dead
[ "$rc" -ne 0 ] && note_ok "non-zero exit" || note_fail "a tag that never pushed must fail the run, got exit 0"
grep -q '^pushed=true$' <<<"$ghout" && note_fail "pushed=true written although the tag never landed" || note_ok "no pushed=true"

# --- structural pins on the workflow itself -----------------------------------
# These can't be driven as scripts (they are `if:` expressions and job wiring),
# so pin them the way the Justfile tests pin recipes: by asserting the source.
echo "case: workflow wiring"
python3 - "$workflow" <<'PY' && note_ok "precheck + release-gating wiring intact" || fail=1
import sys, yaml
w = yaml.safe_load(open(sys.argv[1]))
jobs = w["jobs"]
ok = True
def bad(msg):
    global ok; ok = False; print(f"  FAIL: {msg}")
pre = jobs.get("precheck")
if not pre:
    bad("the precheck job is gone — a superseded run will re-enter the gate dance it is guaranteed to lose (#5142)")
elif "proceed" not in (pre.get("outputs") or {}):
    bad("precheck no longer exposes a proceed output")
rel = jobs.get("release", {})
needs = rel.get("needs")
needs = [needs] if isinstance(needs, str) else (needs or [])
if "precheck" not in needs:
    bad("the release job no longer waits on precheck")
if "needs.precheck.outputs.proceed == 'true'" not in (rel.get("if") or ""):
    bad("the release job no longer honors precheck's proceed output")
gh_release = next((s for s in rel.get("steps", [])
                   if "Create GitHub Release" in (s.get("name") or "")), None)
if gh_release is None:
    bad("the Create GitHub Release step was not found")
elif "steps.push_v4.outputs.pushed == 'true'" not in (gh_release.get("if") or ""):
    bad("Create GitHub Release is not gated on pushed=true — a deferred run would "
        "publish a Release for a tag that was never pushed")
perms = w.get("permissions", {})
if perms.get("pull-requests") != "write":
    bad("permissions no longer grant pull-requests: write — the #5222 PR-merge path needs it")
push_step = next((s for s in rel.get("steps", [])
                   if s.get("id") == "push_v4"), None)
if push_step is None:
    bad("no step with id push_v4 found")
elif "gh pr merge" not in push_step.get("run", ""):
    bad("push_v4 no longer merges via a PR (#5222) — check for a regression back to a raw v4 push")
sys.exit(0 if ok else 1)
PY

if [ "$fail" -ne 0 ]; then
  echo "FAILED"
  exit 1
fi
echo "PASSED"
