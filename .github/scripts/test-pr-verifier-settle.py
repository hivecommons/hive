#!/usr/bin/env python3
"""PR Verifier never leaves `PR Type` / `PR Issue` in progress (#18764).

The verifier image creates both check runs in_progress and then PATCHes the
result. On #18667 that PATCH returned 404, the two required checks stayed
in_progress for ever and the `verify PR contents` job still went green. The
"Settle PR Type and PR Issue check runs" step in pr-verifier.yml must:

  * conclude every unfinished PR Type / PR Issue run of THIS run's check
    suite as `failure` with the reason, falling back to creating a completed
    `failure` run of the same name when the PATCH fails too;
  * fail the job whenever the verifier could not write a result;
  * leave finished check runs (the verifier's real verdict) alone.

This test extracts that step's script from the workflow and runs it with a
stubbed `gh` (real `jq` applies the step's own --jq filters to fixtures), so
no network call is made.
"""
import json
import os
import pathlib
import shutil
import stat
import subprocess
import tempfile
import textwrap
import unittest

import yaml

ROOT = pathlib.Path(__file__).resolve().parents[2]
WORKFLOW = ROOT / ".github/workflows/pr-verifier.yml"
STEP_NAME = "Settle PR Type and PR Issue check runs"

GH_STUB = textwrap.dedent(
    r"""
    #!/usr/bin/env bash
    # Minimal `gh api` stand-in driven by files in $STUB_DIR.
    set -u
    method=GET path="" jq_expr="" prev=""
    for arg in "$@"; do
      case "$prev" in
        -X) method="$arg"; prev=""; continue ;;
        --jq) jq_expr="$arg"; prev=""; continue ;;
        -f|-F) prev=""; continue ;;
      esac
      case "$arg" in
        -X|--jq|-f|-F) prev="$arg" ;;
        repos/*) path="$arg" ;;
      esac
    done
    printf '%s %s\n' "$method" "$path" >> "$STUB_DIR/calls.log"
    case "$method $path" in
      "GET repos/o/r/actions/runs/77")
        [ -f "$STUB_DIR/run-fails" ] && { echo "HTTP 502" >&2; exit 1; }
        body='{"check_suite_id": 555}' ;;
      "GET repos/o/r/check-suites/555/check-runs"*)
        body="$(cat "$STUB_DIR/check-runs.json")" ;;
      "PATCH repos/o/r/check-runs/"*)
        [ -f "$STUB_DIR/patch-404" ] && { echo "HTTP 404: Not Found" >&2; exit 1; }
        body='{}' ;;
      "POST repos/o/r/check-runs")
        [ -f "$STUB_DIR/post-fails" ] && { echo "HTTP 403" >&2; exit 1; }
        body='{}' ;;
      *)
        echo "unexpected gh call: $method $path" >&2; exit 99 ;;
    esac
    if [ -n "$jq_expr" ]; then
      printf '%s' "$body" | jq -r "$jq_expr"
    else
      printf '%s\n' "$body"
    fi
    """
).lstrip()


def load_steps():
    data = yaml.safe_load(WORKFLOW.read_text())
    return data["jobs"]["verify"]["steps"]


def check_run(run_id, name, status, conclusion=None):
    return {
        "id": run_id,
        "name": name,
        "status": status,
        "conclusion": conclusion,
        "head_sha": "c" * 40,
    }


class SettleStep(unittest.TestCase):
    def setUp(self):
        steps = load_steps()
        matches = [s for s in steps if s.get("name") == STEP_NAME]
        self.assertEqual(len(matches), 1, f"pr-verifier.yml must have one {STEP_NAME!r} step")
        self.step = matches[0]
        self.steps = steps
        self.tmp = pathlib.Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.tmp)
        bindir = self.tmp / "bin"
        bindir.mkdir()
        gh = bindir / "gh"
        gh.write_text(GH_STUB)
        gh.chmod(gh.stat().st_mode | stat.S_IEXEC)
        self.bindir = bindir
        (self.tmp / "calls.log").write_text("")
        self.log = self.tmp / "pr-verifier.log"

    def run_step(self, check_runs, verifier_log="2 plugins ran\n", flags=()):
        (self.tmp / "check-runs.json").write_text(json.dumps({"check_runs": check_runs}))
        for flag in flags:
            (self.tmp / flag).write_text("")
        self.log.write_text(verifier_log)
        env = dict(os.environ)
        env.update(
            PATH=f"{self.bindir}:{env.get('PATH', '')}",
            STUB_DIR=str(self.tmp),
            GITHUB_REPOSITORY="o/r",
            GITHUB_RUN_ID="77",
            GH_TOKEN="x",
            VERIFIER_LOG=str(self.log),
        )
        proc = subprocess.run(
            ["bash", "-e", "-c", self.step["run"]],
            env=env,
            capture_output=True,
            text=True,
            timeout=60,
        )
        calls = (self.tmp / "calls.log").read_text().splitlines()
        writes = [c for c in calls if not c.startswith("GET ")]
        return proc, writes

    # --- contract -------------------------------------------------------

    def test_step_is_not_advisory_and_runs_after_a_failed_verifier(self):
        self.assertFalse(self.step.get("continue-on-error", False),
                         "a verifier that cannot write its result must fail the job")
        cond = self.step.get("if", "")
        self.assertIn("steps.verifier.outcome == 'failure'", cond)
        self.assertIn("steps.verifier.outcome == 'success'", cond)

    def test_verifier_output_is_kept_for_the_settle_step(self):
        verifier = next(s for s in self.steps if s.get("id") == "verifier")
        self.assertIn("set -o pipefail", verifier["run"])
        self.assertIn('tee "${RUNNER_TEMP}/pr-verifier.log"', verifier["run"])
        self.assertEqual(self.step["env"]["VERIFIER_LOG"], "${{ runner.temp }}/pr-verifier.log")
        names = [s.get("name") for s in self.steps]
        self.assertGreater(names.index(STEP_NAME), names.index("Verifier action"))

    # --- behaviour ------------------------------------------------------

    def test_404_on_update_concludes_stuck_checks_and_fails_the_job(self):
        log = (
            "::error::unable to update check run with results: PATCH "
            "https://api.github.com/repos/o/r/check-runs/11: 404 Not Found []\n"
            "::error::unable to update check run with results: PATCH "
            "https://api.github.com/repos/o/r/check-runs/12: 404 Not Found []\n"
            "2 plugins ran\n::error::2 plugins had errors\n"
        )
        proc, writes = self.run_step(
            [check_run(11, "PR Type", "in_progress"), check_run(12, "PR Issue", "in_progress"),
             check_run(13, "verify PR contents", "in_progress")],
            verifier_log=log,
        )
        self.assertNotEqual(proc.returncode, 0, proc.stdout + proc.stderr)
        self.assertEqual(writes, ["PATCH repos/o/r/check-runs/11", "PATCH repos/o/r/check-runs/12"])
        self.assertIn("::error::PR Type (check run 11)", proc.stdout)
        self.assertIn("unable to update check run with results", proc.stdout)

    def test_when_the_patch_also_fails_a_failed_run_replaces_it(self):
        proc, writes = self.run_step(
            [check_run(11, "PR Type", "in_progress"), check_run(12, "PR Issue", "completed", "success")],
            flags=("patch-404",),
        )
        self.assertNotEqual(proc.returncode, 0)
        self.assertEqual(writes, ["PATCH repos/o/r/check-runs/11", "POST repos/o/r/check-runs"])
        self.assertIn("created a failed PR Type check run", proc.stdout)

    def test_unreplaceable_stuck_check_still_fails_the_job(self):
        proc, _ = self.run_step(
            [check_run(11, "PR Type", "queued")], flags=("patch-404", "post-fails"),
        )
        self.assertNotEqual(proc.returncode, 0)
        self.assertIn("could neither be updated nor replaced", proc.stdout)

    def test_finished_verdicts_are_left_alone(self):
        proc, writes = self.run_step(
            [check_run(11, "PR Type", "completed", "failure"), check_run(12, "PR Issue", "completed", "success")],
        )
        self.assertEqual(proc.returncode, 0, proc.stdout + proc.stderr)
        self.assertEqual(writes, [])

    def test_write_error_without_a_stuck_run_still_fails_the_job(self):
        proc, writes = self.run_step(
            [], verifier_log="::error::unable to create check run: POST ...: 403 Forbidden []\n",
        )
        self.assertNotEqual(proc.returncode, 0)
        self.assertEqual(writes, [])
        self.assertIn("could not write a check run result", proc.stdout)

    def test_unreadable_suite_fails_instead_of_passing(self):
        proc, writes = self.run_step([check_run(11, "PR Type", "in_progress")], flags=("run-fails",))
        self.assertNotEqual(proc.returncode, 0)
        self.assertEqual(writes, [])


if __name__ == "__main__":
    unittest.main(verbosity=2)
