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
# THE INVARIANT IS RELATIVE, not a pinned pair of values. Either coherent
# posture is allowed:
#
#   - verification advisory  => registry access advisory too (today's answer)
#   - verification fatal     => registry access may be fatal as well
#
# What is forbidden is the split that was there: registry access treated MORE
# harshly than the verification it exists to enable. An unreachable verifier is
# strictly less informative than a verifier that ran and objected, so it must
# not fail harder. #6085 says as much: if a missing verifier should block, the
# continue-on-error on the verification is the thing to reconsider -- the split
# is the one combination that cannot be right either way.
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

echo "=== #6085: PR Verifier reports on the PR, not on registry reachability ==="

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

# The whole verdict is computed in one python pass over the PARSED workflow --
# not greps -- because `continue-on-error: true` and an `if:` expression are
# structure, and a substring check cannot tell which step owns them.
verdict="$(python3 - "$WORKFLOW" <<'PY'
import os, re, subprocess, sys, yaml

doc = yaml.safe_load(open(sys.argv[1]))
jobs = doc.get("jobs") or {}
problems = []
notes = []

for job_id, job in jobs.items():
    steps = job.get("steps") or []

    def find(pred):
        return [s for s in steps if pred(s)]

    # The verification step: the one that runs the verifier image.
    verifier = find(lambda s: s.get("id") == "verifier")
    if not verifier:
        problems.append(f"{job_id}: no step with id 'verifier' -- this suite can no longer find the verification it is about")
        continue
    verifier = verifier[0]
    verifier_advisory = bool(verifier.get("continue-on-error"))

    # Keep the known single-platform digest on a native runner, without
    # letting an untrusted fork opt into the repository's self-hosted override.
    selector = "${{ github.event.pull_request.head.repo.full_name == github.repository && fromJSON(vars.HIVE_ARM64_RUNNER || '[\"ubuntu-24.04-arm\"]') || 'ubuntu-24.04-arm' }}"
    if re.sub(r"\s+", "", str(job.get("runs-on", ""))) != re.sub(r"\s+", "", selector):
        problems.append(f"{job_id}: verifier must select native ARM64 and keep forks off the self-hosted override")
    image = str((job.get("env") or {}).get("PR_VERIFIER_IMAGE", ""))
    if not re.fullmatch(r"ghcr\.io/hivecommons/pr-verifier@sha256:[0-9a-f]{64}", image):
        problems.append(f"{job_id}: verifier must retain an immutable canonical image digest")

    # Registry-touching steps: the GHCR login, and the pull.
    registry = find(lambda s:
        "login" in str(s.get("name", "")).lower()
        or "docker pull" in str(s.get("run", ""))
        or "login-action" in str(s.get("uses", "")))
    if not registry:
        problems.append(f"{job_id}: no registry login/pull step found -- the suite would assert nothing")
        continue
    notes.append(f"{job_id}: verification advisory={verifier_advisory}, registry steps={len(registry)}")

    # THE INVARIANT: registry access must not be treated more harshly than the
    # verification it exists to enable.
    if verifier_advisory:
        for s in registry:
            if not s.get("continue-on-error"):
                problems.append(
                    f"{job_id}: step {s.get('name')!r} touches the registry and is FATAL, "
                    f"while the verification it enables is advisory -- a registry hiccup fails every PR")

    # The verifier must not run against an image that was not pulled.
    pull = find(lambda s: "docker pull" in str(s.get("run", "")))
    if pull:
        pull_id = pull[0].get("id")
        if not pull_id:
            problems.append(f"{job_id}: the pull step has no id, so nothing can gate on whether it succeeded")
        elif f"steps.{pull_id}.outcome" not in str(verifier.get("if", "")):
            problems.append(
                f"{job_id}: the verification does not gate on the pull's outcome -- "
                f"it would run against an image that was never pulled")

        # A skip has to be legible. Green-because-nothing-ran and
        # green-because-it-passed must not look identical.
        announced = [s for s in steps
                     if pull_id and f"steps.{pull_id}.outcome" in str(s.get("if", ""))
                     and "::warning" in str(s.get("run", ""))]
        if not announced:
            problems.append(
                f"{job_id}: a skipped verification is silent -- no step warns when the image "
                f"could not be pulled, so 'nothing ran' reads as 'it passed'")

        # Exercise the actual pull/preflight script, with no registry or
        # engine access. A bad override must stop before even pulling.
        mock_docker = r'''
docker() {
    if [ "$1" = "pull" ]; then
        echo MOCK_PULL
        return "$MOCK_PULL_EXIT"
    elif [ "$1" = "image" ] && [ "$2" = "inspect" ]; then
        printf '%s\n' "$MOCK_IMAGE_PLATFORM"
    else
        echo "unexpected docker command" >&2
        return 90
    fi
}
'''
        cases = [
            ("ARM64", "linux/arm64", "0", True, True),
            ("X64", "linux/arm64", "0", False, False),
            ("ARM", "linux/arm64", "0", False, False),
            ("ARM64", "linux/amd64", "0", False, True),
            ("ARM64", "windows/arm64", "0", False, True),
            ("ARM64", "", "0", False, True),
            ("ARM64", "linux/arm64", "1", False, True),
        ]
        for arch, platform, pull_exit, want_ok, want_pull in cases:
            result = subprocess.run(
                ["bash", "-e", "-c", mock_docker + "\n" + str(pull[0].get("run", ""))],
                env={
                    "PATH": os.environ.get("PATH", "/usr/bin:/bin"),
                    "RUNNER_ARCH": arch,
                    "PR_VERIFIER_IMAGE": image,
                    "MOCK_IMAGE_PLATFORM": platform,
                    "MOCK_PULL_EXIT": pull_exit,
                },
                capture_output=True, text=True, timeout=10,
            )
            if (result.returncode == 0) != want_ok or ("MOCK_PULL" in result.stdout) != want_pull:
                problems.append(
                    f"{job_id}: platform preflight accepted/rejected the wrong case "
                    f"runner={arch}, image={platform!r}, pull_exit={pull_exit}: {result.stdout} {result.stderr}")
        notes.append(f"{job_id}: native runner/image preflight exercised in {len(cases)} mocked cases")

    failed_verifier_announced = [
        s for s in steps
        if "steps.verifier.outcome" in str(s.get("if", ""))
        and "failure" in str(s.get("if", ""))
        and "::warning" in str(s.get("run", ""))
    ]
    if not failed_verifier_announced:
        problems.append(f"{job_id}: a failed verifier must not silently look like successful verification")

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
  ok "registry access is not treated more harshly than the verification it enables"
  ok "the verification is gated on the pull having succeeded"
  ok "a skipped verification announces itself rather than passing silently"
fi

echo
echo "=== $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ]
