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
        self.assertEqual(manifest["pinned"]["native-queue-policy.yml"], ["v6", "-v4"])

    def test_dependabot_runs_the_same_trusted_pr_verifier(self):
        verifier = workflow("pr-verifier.yml")
        events = verifier.get("on", verifier.get(True))
        self.assertIn("opened", events["pull_request_target"]["types"])
        job = verifier["jobs"]["verify"]
        self.assertNotIn("if", job)
        self.assertEqual(job["name"], "verify PR contents")
        self.assertIn("docker run --rm", job["steps"][-1]["run"])

    def test_gosec_concurrency_fits_the_runner_cpu_limit(self):
        # gosec defaults -concurrency to runtime.NumCPU(), which inside a
        # runner pod is the NODE's vCPU count, not the pod's cgroup limit.
        # Unbounded, every worker type-checks and builds SSA for its own
        # package at once, and the pod is OOM-killed or evicted mid-Scan:
        # "runner lost communication", hivecommons/hive#16040. The pod CPU
        # limit is a ceiling, not a memory proof: it only guarantees gosec
        # never runs more workers than the pod can schedule.
        values = yaml.safe_load(
            (ROOT / "src/deploy/ci-runners/lke/hive-runners-lke-values.yaml").read_text())
        runner = next(c for c in values["template"]["spec"]["containers"]
                      if c["name"] == "runner")
        cpu = str(runner["resources"]["limits"]["cpu"])
        cpu_limit = int(cpu[:-1]) // 1000 if cpu.endswith("m") else int(float(cpu))
        for file in ("go-security-analysis.yml",
                     "integration-technical-go-security-analysis.yml"):
            with self.subTest(file=file):
                steps = workflow(file)["jobs"]["gosec"]["steps"]
                scan = next(s for s in steps if s.get("name") == "Scan")["run"]
                match = re.search(r"\bgosec\b.*\s-concurrency[ =](\d+)\b", scan)
                self.assertIsNotNone(match, f"gosec must pass an explicit -concurrency: {scan!r}")
                self.assertGreaterEqual(int(match.group(1)), 1)
                self.assertLessEqual(int(match.group(1)), cpu_limit)


if __name__ == "__main__":
    unittest.main()
