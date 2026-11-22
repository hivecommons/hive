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
    def run_gate(self, *reports):
        paths = []
        with tempfile.TemporaryDirectory() as tmp:
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
                code = gate.main(paths)
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
            self.assertEqual(gate.main(["/nonexistent/gosec.json"]), 1)

    def test_line_range_uses_first_line(self):
        line = gate.annotation(issue("HIGH", line="10-12"), "")
        self.assertIn("line=10::", line)


if __name__ == "__main__":
    unittest.main()
