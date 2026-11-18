#!/usr/bin/env python3
"""Offline tests for quality-report.py (#18979): no network, no tokens."""
import datetime
import importlib.util
import io
import json
import os
import pathlib
import tempfile
import unittest
import zipfile

HERE = pathlib.Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location("quality_report", HERE / "quality-report.py")
qr = importlib.util.module_from_spec(spec)
spec.loader.exec_module(qr)

NOW = datetime.datetime(2026, 11, 18, 12, 0, tzinfo=datetime.timezone.utc)
HOUR = datetime.timedelta(hours=1)


def stamp(delta):
    return (NOW - delta).strftime("%Y-%m-%dT%H:%M:%SZ")


def profile(covered, uncovered):
    lines = ["mode: atomic"]
    lines += ["a.go:%d.1,%d.2 1 1" % (i, i) for i in range(covered)]
    lines += ["b.go:%d.1,%d.2 1 0" % (i, i) for i in range(uncovered)]
    return "\n".join(lines)


class FakeApi(qr.Api):
    def __init__(self, runs=None, artifacts=None, zips=None, score=None, fail=False):
        super().__init__("hivecommons/hive")
        self.runs, self.artifacts, self.zips, self.score, self.fail = runs or {}, artifacts or {}, zips or {}, score, fail
        self.gh_json = self._json
        self.gh_bytes = lambda p: self.zips[p]
        self.http_json = self._http

    def _json(self, path):
        if self.fail:
            raise RuntimeError("api down")
        for wf, runs in self.runs.items():
            if "/workflows/%s/" % wf in path:
                return {"workflow_runs": runs}
        for rid, arts in self.artifacts.items():
            if path.endswith("/runs/%s/artifacts" % rid):
                return {"artifacts": arts}
        return {}

    def _http(self, url):
        if self.score is None:
            raise RuntimeError("unreachable")
        return self.score


class QualityReportTest(unittest.TestCase):
    def test_coverage_pct_weights_statements_and_dedupes_blocks(self):
        self.assertAlmostEqual(qr.coverage_pct(profile(9, 1)), 90.0)
        dup = "mode: set\na.go:1.1,2.2 4 0\na.go:1.1,2.2 4 3\nb.go:1.1,2.2 4 0"
        self.assertAlmostEqual(qr.coverage_pct(dup), 50.0)
        self.assertIsNone(qr.coverage_pct("mode: set\n"))

    def test_workflow_verdict_pass_fail_stale_unknown(self):
        ok = {"conclusion": "success", "updated_at": stamp(HOUR), "event": "push", "html_url": "u"}
        bad = dict(ok, conclusion="failure")
        cancelled = dict(ok, conclusion="cancelled")
        v = lambda api: qr.workflow_verdict(api, "tests", qr.WORKFLOW_TESTS, "v5", NOW, 72, "passing", "failing")
        self.assertEqual(v(FakeApi(runs={qr.WORKFLOW_TESTS: [cancelled, ok]}))["message"], "passing")
        self.assertEqual(v(FakeApi(runs={qr.WORKFLOW_TESTS: [bad, ok]}))["status"], qr.STATUS_FAILING)
        old = dict(ok, updated_at=stamp(HOUR * 100))
        r = v(FakeApi(runs={qr.WORKFLOW_TESTS: [old]}))
        self.assertEqual((r["status"], r["message"]), (qr.STATUS_STALE, "stale"))
        self.assertEqual(v(FakeApi(runs={qr.WORKFLOW_TESTS: [cancelled]}))["message"], "unknown")
        self.assertEqual(v(FakeApi(fail=True))["message"], "unknown")

    def test_pull_request_runs_are_ignored(self):
        pr = {"conclusion": "success", "updated_at": stamp(HOUR), "event": "pull_request"}
        api = FakeApi(runs={qr.WORKFLOW_TESTS: [pr]})
        r = qr.workflow_verdict(api, "tests", qr.WORKFLOW_TESTS, "v5", NOW, 72, "passing", "failing")
        self.assertEqual(r["message"], "unknown")

    def test_coverage_from_artifact_and_unknown_without_one(self):
        buf = io.BytesIO()
        with zipfile.ZipFile(buf, "w") as zf:
            zf.writestr("coverage.out", profile(9, 1))
        art = {"id": 7, "name": qr.COVERAGE_ARTIFACT_PREFIX + "1-1", "expired": False, "created_at": stamp(HOUR)}
        api = FakeApi(runs={qr.WORKFLOW_COVERAGE: [{"id": 1, "conclusion": "success", "event": "schedule"}]},
                      artifacts={1: [art]},
                      zips={"repos/hivecommons/hive/actions/artifacts/7/zip": buf.getvalue()})
        r = qr.coverage_entry(api, "v5", NOW)
        self.assertEqual((r["message"], r["color"]), ("90.0%", qr.COLOR_GOOD))
        api2 = FakeApi(runs={qr.WORKFLOW_COVERAGE: [{"id": 1, "conclusion": "success", "event": "schedule"}]})
        self.assertEqual(qr.coverage_entry(api2, "v5", NOW)["message"], "unknown")

    def test_acmm_unset_invalid_and_valid(self):
        self.assertEqual(qr.acmm_entry("", NOW)["message"], "unknown")
        orig = qr.http_json
        try:
            qr.http_json = lambda u: {"overall_level": 3, "last_evaluated_at": stamp(HOUR)}
            self.assertEqual(qr.acmm_entry("https://x", NOW)["message"], "L3")
            qr.http_json = lambda u: {"overall_level": 9, "last_evaluated_at": stamp(HOUR)}
            self.assertEqual(qr.acmm_entry("https://x", NOW)["message"], "unknown")
            qr.http_json = lambda u: {"overall_level": 3, "last_evaluated_at": stamp(HOUR * 24 * 30)}
            self.assertEqual(qr.acmm_entry("https://x", NOW)["message"], "stale")
        finally:
            qr.http_json = orig

    def test_scorecard(self):
        ok = qr.scorecard_entry(FakeApi(score={"score": 7.5, "date": NOW.strftime("%Y-%m-%d")}), NOW)
        self.assertEqual((ok["message"], ok["color"]), ("7.5/10", qr.COLOR_GOOD))
        self.assertEqual(qr.scorecard_entry(FakeApi(), NOW)["message"], "unknown")

    def test_outcomes_reads_latest_file_and_applies_minimum_n(self):
        with tempfile.TemporaryDirectory() as root:
            d = pathlib.Path(root, "src/docs/benchmarks")
            d.mkdir(parents=True)
            body = lambda k, n, ts: json.dumps({"GeneratedAt": ts, "Followup": {"FixedAndApproved": {"K": k, "N": n}}})
            (d / "outcomes-2026-09.json").write_text(body(1, 2, stamp(HOUR)))
            (d / "outcomes-2026-10.json").write_text(body(369, 563, stamp(HOUR)))
            r = qr.outcomes_entry("hivecommons/hive", root, NOW)
            self.assertEqual(r["message"], "65.5% (369/563)")
            (d / "outcomes-2026-11.json").write_text(body(1, 2, stamp(HOUR)))
            self.assertEqual(qr.outcomes_entry("hivecommons/hive", root, NOW)["message"], "unknown")
        with tempfile.TemporaryDirectory() as root:
            self.assertEqual(qr.outcomes_entry("hivecommons/hive", root, NOW)["message"], "unknown")

    def test_build_with_everything_down_is_all_unknown_and_writes_files(self):
        with tempfile.TemporaryDirectory() as root, tempfile.TemporaryDirectory() as out:
            report = qr.build(FakeApi(fail=True), "v5", root, "", NOW)
            self.assertEqual([e["id"] for e in report["entries"]], qr.ORDER)
            self.assertTrue(all(e["message"] == "unknown" for e in report["entries"]))
            qr.write(report, out)
            for eid in qr.ORDER:
                badge = json.load(open(os.path.join(out, eid + ".json")))
                self.assertEqual(badge["schemaVersion"], 1)
                self.assertEqual(badge["message"], "unknown")
            self.assertEqual(json.load(open(os.path.join(out, "report.json")))["schema"], 1)


if __name__ == "__main__":
    unittest.main()
