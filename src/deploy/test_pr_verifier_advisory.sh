#!/usr/bin/env bash
# #6085: PR Verifier must not fail a PR on registry access.
#
# The workflow treated the VERIFICATION as advisory (continue-on-error) and the
# GHCR login and image pull as FATAL. So any registry-side problem -- an org
# rename, a package-visibility change, a GHCR outage -- failed the check on
# every open PR at once, before any PR content was examined. The one step
# allowed to have an opinion could not fail the job; the two steps with no
# opinion could.
#
# Measured: after the kubestellar->hivecommons move the pinned digest stopped
# resolving and 11 consecutive runs across 5 PRs went red on the same line,
# while the verification those PRs needed never ran once. An automated fix lane
# escalated one of them to needs-human on the strength of it.
#
# WHY THIS SUITE EXISTS AT ALL, and it is not the usual reason. The workflow
# runs on pull_request_target, so GitHub executes the BASE branch's copy: a PR
# that fixes this cannot show itself passing, and the property is only
# observable after merge. A static assertion is the only way to demonstrate it
# in CI, and the only thing that stops the combination drifting back.
#
# #18764 removed the image, and with it the registry: the verdicts are now
# computed in-repo (.github/scripts/pr-verifier.cjs) and each required check
# is recorded with one already-completed create. The image had created each
# run in_progress and then updated it; when GitHub 404'd the update, the
# required runs hung in_progress for ever while the continue-on-error wrapper
# stayed green. So the posture this suite pins is now:
#
#   - nothing touches a registry or a container image (the #6085 failure
#     mode cannot come back with it);
#   - the verdict about the PR's TITLE stays advisory for the wrapper job --
#     the required PR Type / PR Issue checks carry it (pr-verifier.test.cjs);
#   - failing to RECORD a verdict is not advisory: the verifier step is not
#     continue-on-error, so the job goes red with the reason instead of green
#     beside a check that never concluded.
#
# Run: bash src/deploy/test_pr_verifier_advisory.sh
# Exit codes: 0 the posture is coherent, 1 it is not.
set -uo pipefail

PASS=0
FAIL=0

HERE="$(cd "$(dirname "$0")" && pwd)"
WORKFLOW="$(cd "$HERE/../../.github/workflows" && pwd)/pr-verifier.yml"

ok()  { echo "  PASS: $1"; PASS=$((PASS + 1)); }
bad() { echo "  FAIL: $1"; [ -n "${2:-}" ] && echo "        $2"; FAIL=$((FAIL + 1)); }

echo "=== #6085/#18764: PR Verifier reports on the PR and never hides a lost result ==="

if [ ! -f "$WORKFLOW" ]; then
  bad "workflow not found: $WORKFLOW"
  echo; echo "=== $PASS passed, $FAIL failed ==="; exit 1
fi

# python3 + PyYAML is a hard requirement, not an optional nicety: a skip would
# report green in exactly the environment where nothing ran, which is the
# failure the repo's wiring guards (#4363, #5529) exist to stop.
if ! command -v python3 >/dev/null 2>&1 || ! python3 -c 'import yaml' >/dev/null 2>&1; then
  bad "python3 + PyYAML unavailable, so the posture was NOT checked"
  echo; echo "=== $PASS passed, $FAIL failed ==="; exit 1
fi

# One python pass over the PARSED workflow -- not greps -- because
# `continue-on-error` and an `if:` expression are structure, and a substring
# check cannot tell which step owns them.
verdict="$(python3 - "$WORKFLOW" <<'PY'
import sys, yaml

doc = yaml.safe_load(open(sys.argv[1]))
jobs = doc.get("jobs") or {}
problems = []
notes = []

if "packages" in (doc.get("permissions") or {}):
    problems.append("workflow still asks for packages permission -- nothing here pulls an image any more (#18764)")

for job_id, job in jobs.items():
    steps = job.get("steps") or []

    verifier = [s for s in steps if s.get("id") == "verifier"]
    if not verifier:
        problems.append(f"{job_id}: no step with id 'verifier' -- this suite can no longer find the verification it is about")
        continue
    verifier = verifier[0]

    # A result that could not be recorded must fail the job (#18764).
    if verifier.get("continue-on-error"):
        problems.append(f"{job_id}: the verifier step is continue-on-error, so a result it could not record reads as green")
    if not str(verifier.get("uses", "")).startswith("actions/github-script@"):
        problems.append(f"{job_id}: the verifier step must be the in-repo github-script verifier, not an image")
    if ".github/scripts/pr-verifier.cjs" not in str((verifier.get("with") or {}).get("script", "")):
        problems.append(f"{job_id}: the verifier step does not run .github/scripts/pr-verifier.cjs")
    if "steps.dispatch_event.outputs.stale != 'true'" not in str(verifier.get("if", "")):
        problems.append(f"{job_id}: the verifier must not record a verdict on a dispatched run whose head moved on")

    # No registry, no image: the #6085 failure mode stays gone.
    for s in steps:
        blob = " ".join(str(s.get(k, "")) for k in ("name", "run", "uses"))
        if "docker" in blob or "login-action" in blob or "ghcr.io" in blob:
            problems.append(f"{job_id}: step {s.get('name')!r} touches a registry or container image")
        if s.get("continue-on-error"):
            problems.append(f"{job_id}: step {s.get('name')!r} is continue-on-error; nothing in this job may hide a failure")
    if "PR_VERIFIER_IMAGE" in str(job.get("env", "")):
        problems.append(f"{job_id}: the job still pins a verifier image")
    notes.append(f"{job_id}: verifier={verifier.get('uses')}, steps={len(steps)}")

for n in notes:
    print("NOTE " + n)
for p in problems:
    print("PROBLEM " + p)
PY
)"

if [ -z "$verdict" ]; then
  bad "the analysis produced no output at all" "a silent pass here would assert nothing"
  echo; echo "=== $PASS passed, $FAIL failed ==="; exit 1
fi

while IFS= read -r line; do
  case "$line" in
    NOTE*)    echo "  ....  ${line#NOTE }" ;;
    PROBLEM*) bad "${line#PROBLEM }" ;;
  esac
done <<<"$verdict"

if ! printf '%s' "$verdict" | grep -q '^PROBLEM'; then
  ok "no registry or image stands between the PR and its verdict"
  ok "a result the verifier could not record fails the job"
  ok "a dispatched run whose head moved on records nothing"
fi

echo
echo "=== $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ]
