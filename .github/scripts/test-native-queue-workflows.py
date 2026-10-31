#!/usr/bin/env python3
"""Structural contracts for required native queue workflows (not a Go runner)."""
import pathlib
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
                         "${{ github.event.merge_group.base_sha || github.event.pull_request.base.sha }}")
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


if __name__ == "__main__":
    unittest.main()
