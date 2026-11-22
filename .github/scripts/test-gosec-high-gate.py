#!/usr/bin/env python3
"""Regression tests for gosec-high-gate.py, the blocking gosec gate (#19443)."""
import contextlib
import importlib.util
import io
import json
import os
import pathlib
import tempfile
import unittest

HERE = pathlib.Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location("gosec_high_gate", HERE / "gosec-high-gate.py")
gate = importlib.util.module_from_spec(spec)
spec.loader.exec_module(gate)


def issue(severity, rule="G204", **extra):
    out = {
        "severity": severity,
        "confidence": "HIGH",
        "rule_id": rule,
        "details": "Subprocess launched with variable",
        "file": "/work/hive/src/pkg/x/x.go",
        "line": "42",
        "column": "3",
    }
    out.update(extra)
    return out


class GosecHighGate(unittest.TestCase):
    def run_gate(self, *reports, baseline=None, extra=()):
        paths = []
        with tempfile.TemporaryDirectory() as tmp:
            base = os.path.join(tmp, "baseline.json")
            with open(base, "w", encoding="utf-8") as f:
                json.dump(baseline if baseline is not None else {"findings": []}, f)
            for i, report in enumerate(reports):
                path = os.path.join(tmp, f"r{i}.json")
                with open(path, "w", encoding="utf-8") as f:
                    if isinstance(report, str):
                        f.write(report)
                    else:
                        json.dump(report, f)
                paths.append(path)
            out = io.StringIO()
            with contextlib.redirect_stdout(out):
                code = gate.main(["--baseline", base, *extra, *paths])
        return code, out.getvalue()

    def test_high_finding_blocks_and_is_annotated(self):
        os.environ["GITHUB_WORKSPACE"] = "/work/hive"
        code, out = self.run_gate({"Issues": [issue("HIGH")]})
        self.assertEqual(code, 1)
        self.assertIn("::error file=src/pkg/x/x.go,line=42::gosec G204", out)

    def test_medium_and_low_do_not_block(self):
        code, out = self.run_gate({"Issues": [issue("MEDIUM"), issue("LOW")]})
        self.assertEqual(code, 0)
        self.assertNotIn("::error", out)

    def test_empty_report_passes(self):
        self.assertEqual(self.run_gate({"Issues": None})[0], 0)
        self.assertEqual(self.run_gate({})[0], 0)

    def test_suppressed_high_does_not_block(self):
        code, _ = self.run_gate({"Issues": [
            issue("HIGH", nosec=True),
            issue("HIGH", suppressions=[{"kind": "inSource", "justification": "constant"}]),
        ]})
        self.assertEqual(code, 0)

    def test_any_report_with_high_blocks(self):
        code, out = self.run_gate({"Issues": []}, {"Issues": [issue("high", rule="G402")]})
        self.assertEqual(code, 1)
        self.assertIn("G402", out)

    def test_unreadable_report_fails_closed(self):
        code, out = self.run_gate("not json")
        self.assertEqual(code, 1)
        self.assertIn("missing or unreadable", out)
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            self.assertEqual(gate.main(["--baseline", "", "/nonexistent/gosec.json"]), 1)

    def test_baselined_high_does_not_block(self):
        os.environ["GITHUB_WORKSPACE"] = "/work/hive"
        found = issue("HIGH", code="41: a\n42: \tcmd := exec.Command(x)\n43: b\n")
        baseline = {"findings": [{"file": "src/pkg/x/x.go", "rule_id": "G204",
                                  "code": "cmd := exec.Command(x)"}]}
        code, out = self.run_gate({"Issues": [found]}, baseline=baseline)
        self.assertEqual(code, 0, out)
        self.assertNotIn("::error", out)

    def test_baseline_survives_a_line_shift(self):
        os.environ["GITHUB_WORKSPACE"] = "/work/hive"
        shifted = issue("HIGH", line="90", code="90: cmd := exec.Command(x)\n")
        baseline = {"findings": [{"file": "src/pkg/x/x.go", "rule_id": "G204",
                                  "code": "cmd := exec.Command(x)"}]}
        self.assertEqual(self.run_gate({"Issues": [shifted]}, baseline=baseline)[0], 0)

    def test_new_high_beyond_baseline_count_blocks(self):
        os.environ["GITHUB_WORKSPACE"] = "/work/hive"
        a = issue("HIGH", code="42: cmd := exec.Command(x)\n")
        b = issue("HIGH", line="50", code="50: cmd := exec.Command(x)\n")
        baseline = {"findings": [{"file": "src/pkg/x/x.go", "rule_id": "G204",
                                  "code": "cmd := exec.Command(x)"}]}
        code, out = self.run_gate({"Issues": [a, b]}, baseline=baseline)
        self.assertEqual(code, 1)
        self.assertIn("1 new", out)

    def test_edited_baselined_line_blocks(self):
        os.environ["GITHUB_WORKSPACE"] = "/work/hive"
        found = issue("HIGH", code="42: cmd := exec.Command(y)\n")
        baseline = {"findings": [{"file": "src/pkg/x/x.go", "rule_id": "G204",
                                  "code": "cmd := exec.Command(x)"}]}
        self.assertEqual(self.run_gate({"Issues": [found]}, baseline=baseline)[0], 1)

    def test_unreadable_baseline_fails_closed(self):
        code, out = self.run_gate({"Issues": []}, baseline="not a baseline")
        self.assertEqual(code, 1)
        self.assertIn("baseline", out)

    def test_write_baseline_round_trips(self):
        os.environ["GITHUB_WORKSPACE"] = "/work/hive"
        found = [issue("HIGH", code="42: cmd := exec.Command(x)\n"),
                 issue("HIGH", line="50", code="50: cmd := exec.Command(x)\n")]
        with tempfile.TemporaryDirectory() as tmp:
            base = os.path.join(tmp, "b.json")
            rep = os.path.join(tmp, "r.json")
            with open(rep, "w", encoding="utf-8") as f:
                json.dump({"Issues": found}, f)
            with contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(gate.main(["--baseline", base, "--write-baseline", rep]), 0)
                self.assertEqual(gate.main(["--baseline", base, rep]), 0)
            with open(base, encoding="utf-8") as f:
                self.assertEqual(json.load(f)["findings"][0]["count"], 2)

    def test_keep_existing_takes_the_union(self):
        os.environ["GITHUB_WORKSPACE"] = "/work/hive"
        a = issue("HIGH", code="42: cmd := exec.Command(x)\n")
        b = issue("HIGH", rule="G703", line="7", code="7: os.ReadFile(p)\n")
        with tempfile.TemporaryDirectory() as tmp:
            base = os.path.join(tmp, "b.json")
            ra, rb = os.path.join(tmp, "a.json"), os.path.join(tmp, "b2.json")
            for path, found in ((ra, [a, a]), (rb, [a, b])):
                with open(path, "w", encoding="utf-8") as f:
                    json.dump({"Issues": found}, f)
            with contextlib.redirect_stdout(io.StringIO()):
                gate.main(["--baseline", base, "--write-baseline", ra])
                gate.main(["--baseline", base, "--write-baseline", "--keep-existing", rb])
            counts = gate.load_baseline(base)
        self.assertEqual(sorted(counts.values()), [1, 2])

    def test_repo_baseline_is_readable(self):
        self.assertTrue(gate.load_baseline(gate.DEFAULT_BASELINE))

    def test_line_range_uses_first_line(self):
        line = gate.annotation(issue("HIGH", line="10-12"), "")
        self.assertIn("line=10::", line)


if __name__ == "__main__":
    unittest.main()
