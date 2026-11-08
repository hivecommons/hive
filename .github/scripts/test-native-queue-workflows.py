#!/usr/bin/env python3
"""Structural contracts for required native queue workflows (not a Go runner)."""
import pathlib
import re
import unittest

import yaml

ROOT = pathlib.Path(__file__).resolve().parents[2]


def workflow(name):
    return yaml.safe_load((ROOT / ".github/workflows" / name).read_text())


class NativeQueueWorkflows(unittest.TestCase):
    def test_six_required_contexts_have_unfiltered_group_triggers(self):
        expected = {
            "v2-ci.yml": ["build-and-test"],
            "v2-tests.yml": ["test", "coverage"],
            "go-security-analysis.yml": [
                "golangci-lint",
                "gosec (insecure patterns)",
                "govulncheck (known vulnerabilities)",
            ],
        }
        for file, contexts in expected.items():
            with self.subTest(file=file):
                data = workflow(file)
                # PyYAML's YAML 1.1 loader interprets the key `on` as True.
                events = data.get("on", data.get(True))
                self.assertEqual(events["merge_group"], {"types": ["checks_requested"]})
                self.assertTrue(all(value == "read" for value in data["permissions"].values()))
                jobs = {job.get("name", key): job for key, job in data["jobs"].items()}
                for context in contexts:
                    self.assertIn(context, jobs)
                    for step in jobs[context].get("steps", []):
                        if step.get("uses", "").startswith("actions/checkout@"):
                            self.assertNotIn("ref", step.get("with", {}))

    def test_group_coverage_is_not_pr_only(self):
        coverage = workflow("v2-tests.yml")["jobs"]["coverage"]
        self.assertIn("github.event_name == 'merge_group'", coverage["if"])
        self.assertNotIn("issues", coverage.get("permissions", {}))
        comment = workflow("v2-tests.yml")["jobs"]["patch-coverage"]
        self.assertIn("github.event_name == 'pull_request'", comment["if"])
        self.assertEqual(comment["permissions"]["issues"], "write")
        self.assertIn(".coverage-tools/.github/scripts/patch-coverage.sh",
                      comment["steps"][-1]["run"])

    def test_owned_gate_uses_trusted_base_not_member_code(self):
        data = workflow("native-queue-policy.yml")
        events = data.get("on", data.get(True))
        self.assertIn("pull_request_target", events)
        self.assertNotIn("pull_request", events)
        self.assertTrue(all(value == "read" for value in data["permissions"].values()))
        checkout = data["jobs"]["policy"]["steps"][0]["with"]
        self.assertEqual(checkout["ref"],
                         "${{ github.event.merge_group.base_sha || format('refs/heads/{0}', github.event.pull_request.base.ref) }}")
        self.assertFalse(checkout["persist-credentials"])
        manifest = yaml.safe_load((ROOT / ".github/release-lines.yml").read_text())
        # The admission gate covers the active feature lines (v5 and v6) and
        # not feature-frozen v4, however the manifest spells that: v6 became a
        # `release_lines` entry on 2026-11-07, so it is covered without an extra.
        pinned = manifest["pinned"]["native-queue-policy.yml"]
        covered = set(manifest["release_lines"]) | {p for p in pinned if not p.startswith("-")}
        covered -= {p[1:] for p in pinned if p.startswith("-")}
        self.assertEqual(sorted(covered), ["v5", "v6"])

    def test_owned_gate_retriggers_never_cancel_the_evidence_wait(self):
        # Stage-label flips re-fire the PR-target run every few minutes; a
        # cancelling concurrency group would never let the bounded evidence
        # wait finish (#16942). Re-triggers must still run (a hold added after
        # a green run must turn the check red) but must queue, not cancel.
        data = workflow("native-queue-policy.yml")
        events = data.get("on", data.get(True))
        for action in ("edited", "labeled", "unlabeled"):
            self.assertIn(action, events["pull_request_target"]["types"])
        concurrency = data["concurrency"]
        self.assertIn("github.event.pull_request.number", concurrency["group"])
        self.assertIs(concurrency["cancel-in-progress"], False)

    def test_cosmetic_label_flips_skip_the_evidence_wait(self):
        # Only labels matching the policy's BLOCKED_LABEL are read; any other
        # labeled/unlabeled flip must skip the 22.5 min wait (#16945) while
        # the job still concludes successfully.
        steps = workflow("native-queue-policy.yml")["jobs"]["policy"]["steps"]
        gate = next(s for s in steps
                    if s.get("name") == "Replay last verdict for cosmetic label flips")
        self.assertEqual(gate["id"], "label-flip")
        self.assertIn("isCosmeticLabelFlip", gate["with"]["script"])
        self.assertIn("replayLastVerdict", gate["with"]["script"])
        # Nothing to replay (no prior success, or a transient API error) must
        # fall through to the full wait rather than fail or fabricate a pass.
        self.assertIn("replayed === null", gate["with"]["script"])
        self.assertIn("isTransientAPIError(error)", gate["with"]["script"])
        self.assertIn("cosmetic = false", gate["with"]["script"])
        wait = next(s for s in steps if s.get("name", "").startswith("Validate exact-head evidence"))
        self.assertIn("steps.label-flip.outputs.cosmetic != 'true'", wait["if"])
        self.assertLess(steps.index(gate), steps.index(wait))

    def test_dependabot_runs_the_same_trusted_pr_verifier(self):
        verifier = workflow("pr-verifier.yml")
        events = verifier.get("on", verifier.get(True))
        self.assertIn("opened", events["pull_request_target"]["types"])
        # A github.token push fires no pull_request_target event; App/PAT label
        # flips must re-run the required context on the current head (#17036).
        for action in ("synchronize", "ready_for_review", "labeled", "unlabeled"):
            self.assertIn(action, events["pull_request_target"]["types"])
        job = verifier["jobs"]["verify"]
        self.assertNotIn("if", job)
        self.assertEqual(job["name"], "verify PR contents")
        verify_step = next(step for step in job["steps"]
                           if step.get("name") == "Verifier action")
        self.assertIn("docker run --rm", verify_step["run"])

    def test_autofix_push_deterministically_redispatches_the_verifier(self):
        # #16567: the autofix push landed after the last prow label and no
        # label event followed for three days, so labeled/unlabeled alone has
        # no producer. workflow_dispatch is one of the two events a
        # github.token may trigger; the guard dispatches pr-verifier on the
        # PR branch so the check lands on the pushed head (#17036).
        verifier = workflow("pr-verifier.yml")
        events = verifier.get("on", verifier.get(True))
        self.assertIn("pr_number", events["workflow_dispatch"]["inputs"])
        self.assertTrue(events["workflow_dispatch"]["inputs"]["pr_number"]["required"])
        self.assertIn("inputs.pr_number", verifier["concurrency"]["group"])
        steps = verifier["jobs"]["verify"]["steps"]
        hydrate = next(s for s in steps if s.get("id") == "dispatch_event")
        self.assertEqual(hydrate["if"], "github.event_name == 'workflow_dispatch'")
        # Never verify a fork's PR or a commit that is no longer the PR head.
        self.assertIn("only same-repo PRs may be dispatched", hydrate["run"])
        self.assertIn('echo "stale=true"', hydrate["run"])
        verify_step = next(s for s in steps if s.get("id") == "verifier")
        self.assertIn("steps.dispatch_event.outputs.stale != 'true'", verify_step["if"])
        self.assertIn("steps.dispatch_event.outputs.event_path", verify_step["env"]["VERIFIER_EVENT_PATH"])
        self.assertIn('-v "$VERIFIER_EVENT_PATH:/github/event.json:ro"', verify_step["run"])

        guard = workflow("changelog-fragment-guard.yml")
        self.assertEqual(guard["permissions"]["actions"], "write")
        gsteps = guard["jobs"]["changelog-fragment-guard"]["steps"]
        autofix = next(s for s in gsteps if s.get("id") == "autofix")
        dispatch = next(s for s in gsteps if "Dispatch pr-verifier" in s.get("name", ""))
        self.assertLess(gsteps.index(autofix), gsteps.index(dispatch))
        self.assertIn("steps.autofix.outputs.pushed == 'true'", dispatch["if"])
        # A PAT push already fires pull_request_target: dispatch only on the
        # github.token path, and dispatch the PR branch so the check lands on
        # the pushed head.
        self.assertEqual(dispatch["env"]["HAS_PUSH_TOKEN"], "${{ secrets.TOPUP_PUSH_TOKEN != '' }}")
        self.assertIn('[ "$HAS_PUSH_TOKEN" = "true" ]', dispatch["run"])
        self.assertIn('gh workflow run pr-verifier.yml --repo "$GITHUB_REPOSITORY" --ref "$HEAD_REF" -f "pr_number=${PR_NUMBER}"', dispatch["run"])

    def test_gosec_concurrency_fits_the_runner_cpu_limit(self):
        # gosec defaults -concurrency to runtime.NumCPU(), which inside a
        # runner pod is the NODE's vCPU count, not the pod's cgroup limit.
        # Unbounded, every worker type-checks and builds SSA for its own
        # package at once, a plausible source of runner memory pressure
        # in hivecommons/hive#16040. The pod CPU limit is a ceiling, not an
        # OOM diagnosis or memory proof: it only bounds worker fanout.
        values = yaml.safe_load(
            (ROOT / "src/deploy/ci-runners/lke/hive-runners-lke-values.yaml").read_text())
        runner = next(c for c in values["template"]["spec"]["containers"]
                      if c["name"] == "runner")
        cpu = str(runner["resources"]["limits"]["cpu"])
        cpu_limit = int(cpu[:-1]) // 1000 if cpu.endswith("m") else int(float(cpu))
        for file, job in (
            ("go-security-analysis.yml", "gosec"),
            ("integration-technical-go-security-analysis.yml", "gosec"),
            ("integration-candidate.yml", "security-gosec"),
        ):
            with self.subTest(file=file):
                steps = workflow(file)["jobs"][job]["steps"]
                scan = next(s for s in steps if s.get("name") == "Scan")["run"]
                match = re.search(r"\bgosec\b.*\s-concurrency[ =](\d+)\b", scan)
                self.assertIsNotNone(match, f"gosec must pass an explicit -concurrency: {scan!r}")
                self.assertGreaterEqual(int(match.group(1)), 1)
                self.assertLessEqual(int(match.group(1)), cpu_limit)


if __name__ == "__main__":
    unittest.main()
