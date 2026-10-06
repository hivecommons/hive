import copy
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

HERE = Path(__file__).resolve().parent
ROOT = HERE.parents[2]
spec = importlib.util.spec_from_file_location("prepare", HERE / "prepare.py")
prepare = importlib.util.module_from_spec(spec)
spec.loader.exec_module(prepare)


class AdmissionTest(unittest.TestCase):
    def setUp(self):
        self.issue = {
            "state": "open", "number": 12, "title": "docs: clarify setup",
            "labels": [], "user": {"login": "contributor"},
            "created_at": "2026-10-05T12:00:00Z",
        }

    def test_admits_and_runs_real_classifier_with_custom_rules(self):
        with tempfile.TemporaryDirectory() as directory:
            directory = Path(directory)
            actionable = directory / "actionable.json"
            actionable.write_text(json.dumps(prepare.prepare(self.issue, "owner/repo")))
            config = directory / "project.yaml"
            config.write_text("classification:\n  complexity:\n    default_model: custom\n  lanes:\n    architect:\n      title_patterns: ['^docs:']\n")
            subprocess.run(["bash", str(ROOT / "bin/issue-classifier.sh")], check=True,
                           env={**os.environ, "HIVE_ACTIONABLE_FILE": str(actionable),
                                "HIVE_CLASSIFIER_LOG": str(directory / "classifier.log"),
                                "HIVE_PROJECT_YAML": str(config)})
            issue = json.loads(actionable.read_text())["issues"]["items"][0]
            self.assertEqual(issue["repo"], "owner/repo")
            self.assertEqual(issue["number"], 12)
            self.assertEqual(issue["lane"], "architect")
            self.assertTrue(issue["needs_architecture_review"])
            self.assertEqual(issue["model_recommendation"], "custom")

    def test_refuses_excluded_labels_case_insensitively(self):
        for label in ["BLOCKED", "do-not-merge", "hold/review", "On-Hold",
                      "LFX-mentorship", "auto-qa-tuning-report", "needs-human"]:
            with self.subTest(label=label):
                issue = copy.deepcopy(self.issue)
                issue["labels"] = [{"name": label}]
                with self.assertRaises(ValueError):
                    prepare.prepare(issue, "owner/repo")

    def test_refuses_closed_issues_and_pull_requests(self):
        for changes in [{"state": "closed"}, {"pull_request": {}}]:
            with self.assertRaises(ValueError):
                prepare.prepare({**self.issue, **changes}, "owner/repo")

    def test_malformed_input_exits_nonzero(self):
        for payload in ["not json", "{}", "null"]:
            with self.subTest(payload=payload):
                result = subprocess.run(["python3", str(HERE / "prepare.py")],
                                        input=payload, text=True, capture_output=True,
                                        env={**os.environ, "GITHUB_REPOSITORY": "owner/repo"})
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(result.stdout, "")


if __name__ == "__main__":
    unittest.main()
