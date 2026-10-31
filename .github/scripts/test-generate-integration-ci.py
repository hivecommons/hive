#!/usr/bin/env python3
import copy
import importlib.util
import json
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location("candidate_generator", Path(__file__).with_name("generate-integration-ci.py"))
generator = importlib.util.module_from_spec(spec)
spec.loader.exec_module(generator)


class CompositionTest(unittest.TestCase):
    def setUp(self):
        self.documents = {
            name: generator.yaml.load((generator.ROOT / ".github/workflows" / name).read_text(),
                                      Loader=generator.WorkflowLoader)
            for name in generator.SOURCES
        }

    def test_real_jobs_are_top_level_with_the_entire_exact_dag(self):
        workflow = generator.yaml.load(generator.compose(self.documents), Loader=generator.WorkflowLoader)
        jobs = workflow["jobs"]
        expected = generator.CONTEXTS | set(generator.GOVERNANCE.values()) | {"gate"}
        bindings = json.loads(jobs["receipt"]["steps"][0]["env"]["REQUIRED_CONTEXT_JOBS"])
        self.assertEqual(set(bindings), expected)
        for name, key in bindings.items():
            self.assertEqual(jobs[key]["name"], name)
            self.assertIn("steps", jobs[key])
            self.assertNotIn("uses", jobs[key])
            self.assertEqual(sum(job.get("name") == name for job in jobs.values()), 1)
        self.assertEqual(set(jobs["receipt"]["needs"]), set(jobs) - {"receipt"})
        for key, job in jobs.items():
            for dependency in job.get("needs", []):
                self.assertIn(dependency, jobs, key)
            if key.startswith(("ci-", "tests-", "security-")):
                self.assertIn("needs.subject.result == 'success'", job["if"])
        script = next(step["run"] for step in jobs["tests-test"]["steps"] if step.get("name") == "Check shard results")
        self.assertIn('Candidate shards must all complete"; exit 1', script)
        self.assertEqual(workflow["permissions"], {"contents": "read", "pull-requests": "read"})

    def test_unqualified_producer_permissions_or_context_drift_cannot_generate_green_aliases(self):
        for mutation in ("permission", "missing", "checkout", "needs"):
            documents = copy.deepcopy(self.documents)
            job = documents["v2-ci.yml"]["jobs"]["build-and-test"]
            if mutation == "permission":
                job["permissions"] = {"contents": "write"}
            elif mutation == "missing":
                job["name"] = "not-the-required-context"
            elif mutation == "checkout":
                checkout = next(step for step in job["steps"] if str(step.get("uses", "")).startswith("actions/checkout@"))
                checkout.setdefault("with", {})["ref"] = "other-head"
            else:
                job["needs"] = ["does-not-exist"]
            with self.assertRaises(ValueError, msg=mutation):
                generator.compose(documents)


if __name__ == "__main__":
    unittest.main()
