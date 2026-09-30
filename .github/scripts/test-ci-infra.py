#!/usr/bin/env python3
"""Hermetic tests for the CI infra classifier, rerun-once guard and rate
alert (#9664). No network, no gh: GitHub is a fake.

    python3 .github/scripts/test-ci-infra.py
"""

import contextlib
import io
import json
import os
import re
import subprocess
import sys
import tempfile
import unittest
import zipfile

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import ci_infra_actions as actions  # noqa: E402
import ci_infra_classify as classifier  # noqa: E402

FIXTURES = os.path.join(HERE, "testdata", "ci-infra")
WORKFLOWS = os.path.join(HERE, "..", "workflows")
RERUN_WORKFLOW = os.path.join(WORKFLOWS, "ci-infra-rerun.yml")
REPO = "hivecommons/hive"


def read(path):
    with open(path, encoding="utf-8", errors="replace") as fh:
        return fh.read()


def fixture(name):
    log = read(os.path.join(FIXTURES, name + ".log"))
    ann_path = os.path.join(FIXTURES, name + ".annotations")
    ann = read(ann_path) if os.path.exists(ann_path) else ""
    return log, ann


def expected_fixtures():
    rows = {}
    for line in read(os.path.join(FIXTURES, "expected.tsv")).splitlines():
        if not line.strip() or line.startswith("#"):
            continue
        name, verdict = line.split("\t")
        rows[name] = verdict
    return rows


SIGS = classifier.load_signatures()

# Real lines from 2026-09-29 jobs, used to compose logs for the region tests.
STEP_OK_WITH_INFRA_NOISE = (
    "2026-09-29T11:18:30.0000000Z ##[group]Run bash warm-cache.sh\n"
    "2026-09-29T11:18:30.0000001Z \x1b[36;1mbash warm-cache.sh\x1b[0m\n"
    "2026-09-29T11:18:30.0000002Z ##[endgroup]\n"
    "2026-09-29T11:18:36.4213376Z open /mnt/gocache/build/13/139c8a6aaecaad15c602dc0095a2d7b36cb10c87b0473905b4c84f9435da6436-a: permission denied\n"
)
STEP_CODE_FAILURE = (
    "2026-09-29T08:34:00.2800000Z ##[group]Run go vet ./...\n"
    "2026-09-29T08:34:00.2800001Z \x1b[36;1mgo vet ./...\x1b[0m\n"
    "2026-09-29T08:34:00.2800002Z ##[endgroup]\n"
    "2026-09-29T08:34:00.2887556Z ##[error]pkg/github/prclaims.go:403:2: urlPattern redeclared in this block\n"
    "2026-09-29T08:34:00.2915019Z ##[error]\tpkg/github/issue_rejection.go:45:5: other declaration of urlPattern\n"
    "2026-09-29T08:34:00.2920000Z ##[error]Process completed with exit code 1.\n"
)
LIST_STEP_HEADER = (
    "2026-09-29T20:06:37.1354877Z ##[group]Run set -o pipefail\n"
    "2026-09-29T20:06:37.1376095Z \x1b[36;1mgo test ./pkg/agent -short -list '.*' | grep -E '^(Test|Example|Fuzz)' | LC_ALL=C sort > all-tests.txt\x1b[0m\n"
    "2026-09-29T20:06:37.1394002Z ##[endgroup]\n"
)


class ClassifierFixtureTest(unittest.TestCase):
    def test_every_fixture_is_listed_and_every_listed_fixture_exists(self):
        on_disk = {f[:-4] for f in os.listdir(FIXTURES) if f.endswith(".log")}
        self.assertEqual(on_disk, set(expected_fixtures()))

    def test_real_fixtures_classify_as_expected(self):
        for name, want in sorted(expected_fixtures().items()):
            with self.subTest(fixture=name):
                log, ann = fixture(name)
                self.assertEqual(classifier.classify(log, ann, SIGS), want)

    def test_every_infra_class_has_a_real_fixture(self):
        classes = {s.klass for s in SIGS if s.klass != classifier.DERIVED_CLASS}
        covered = {
            v[len(classifier.INFRA_PREFIX):]
            for v in expected_fixtures().values()
            if classifier.is_infra(v)
        }
        self.assertEqual(classes, covered, "a signature class without a real-log fixture")

    def test_code_negatives_exist(self):
        codes = [n for n, v in expected_fixtures().items() if v == classifier.VERDICT_CODE]
        self.assertGreaterEqual(len(codes), 5)

    def test_fixtures_carry_no_credentials(self):
        cred = re.compile(r"gh[pousr]_[A-Za-z0-9]{20,}|github_pat_|AUTHORIZATION: basic", re.I)
        for f in os.listdir(FIXTURES):
            with self.subTest(file=f):
                self.assertIsNone(cred.search(read(os.path.join(FIXTURES, f))))


class ClassifierRegionTest(unittest.TestCase):
    def test_infra_noise_in_a_passing_step_does_not_hide_a_code_failure(self):
        log = STEP_OK_WITH_INFRA_NOISE + STEP_CODE_FAILURE
        self.assertEqual(classifier.classify(log, "", SIGS), "code")

    def test_infra_line_in_the_failing_step_wins(self):
        log = STEP_CODE_FAILURE.replace(
            "##[error]Process completed",
            "open /mnt/gocache/build/ab/ab-a: permission denied\n2026-09-29T08:34:00.2930000Z ##[error]Process completed",
        )
        self.assertEqual(classifier.classify(log, "", SIGS), "infra:gocache-permission")

    def test_silent_list_step_is_infra_but_a_list_step_with_output_is_not(self):
        silent = LIST_STEP_HEADER + "2026-09-29T20:06:39.4197316Z ##[error]Process completed with exit code 1.\n"
        self.assertEqual(classifier.classify(silent, "", SIGS), "infra:test-list-empty")
        noisy = (
            LIST_STEP_HEADER
            + "2026-09-29T08:34:00.2887556Z ##[error]pkg/github/prclaims.go:403:2: urlPattern redeclared in this block\n"
            + "2026-09-29T08:34:00.2920000Z ##[error]Process completed with exit code 1.\n"
        )
        self.assertEqual(classifier.classify(noisy, "", SIGS), "code")

    def test_silent_non_list_step_is_code(self):
        log = (
            "2026-09-29T08:34:00.2800000Z ##[group]Run go vet ./...\n"
            "2026-09-29T08:34:00.2800001Z \x1b[36;1mgo vet ./...\x1b[0m\n"
            "2026-09-29T08:34:00.2800002Z ##[endgroup]\n"
            "2026-09-29T08:34:00.2920000Z ##[error]Process completed with exit code 1.\n"
        )
        self.assertEqual(classifier.classify(log, "", SIGS), "code")

    def test_missing_log_without_annotation_is_code(self):
        log, _ = fixture("infra-runner-lost")
        self.assertEqual(classifier.classify(log, "", SIGS), "code")
        self.assertEqual(classifier.classify("", "", SIGS), "code")

    def test_first_matching_row_wins(self):
        sigs = classifier.parse_signatures("a\tstep\tboom\nb\tstep\tboom\n")
        self.assertEqual(classifier.classify("##[error]boom", "", sigs), "infra:a")


class SignatureTableTest(unittest.TestCase):
    def test_bad_tables_are_rejected(self):
        for bad in (
            "",
            "# only comments\n",
            "cls\tstep\n",
            "cls\tnowhere\tx\n",
            "Bad_Class\tstep\tx\n",
            "cls\tstep\t(unclosed\n",
        ):
            with self.subTest(table=bad):
                with self.assertRaises(classifier.SignatureError):
                    classifier.parse_signatures(bad)

    def test_shipped_table_keeps_derived_rows_last(self):
        kinds = [s.klass == classifier.DERIVED_CLASS for s in SIGS]
        self.assertIn(True, kinds)
        self.assertEqual(kinds, sorted(kinds), "a derived row precedes an infra row")


class ClassifierCliTest(unittest.TestCase):
    def run_cli(self, argv):
        out, err = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            rc = classifier.main(argv)
        return rc, out.getvalue().strip()

    def test_prints_verdict_and_exits_zero(self):
        rc, out = self.run_cli(["--log", os.path.join(FIXTURES, "infra-disk-full-setup-job.log")])
        self.assertEqual((rc, out), (classifier.EXIT_OK, "infra:disk-full"))
        rc, out = self.run_cli([
            "--log", os.path.join(FIXTURES, "infra-runner-lost.log"),
            "--annotations", os.path.join(FIXTURES, "infra-runner-lost.annotations"),
        ])
        self.assertEqual((rc, out), (classifier.EXIT_OK, "infra:runner-lost"))

    def test_missing_log_file_is_code_not_an_error(self):
        rc, out = self.run_cli(["--log", os.path.join(FIXTURES, "does-not-exist.log")])
        self.assertEqual((rc, out), (classifier.EXIT_OK, "code"))

    def test_usage_and_bad_table_exit_codes(self):
        rc, _ = self.run_cli([])
        self.assertEqual(rc, classifier.EXIT_USAGE)
        with tempfile.NamedTemporaryFile("w", suffix=".tsv", delete=False) as fh:
            fh.write("cls\tnowhere\tx\n")
        try:
            rc, _ = self.run_cli(["--log", "/dev/null", "--signatures", fh.name])
            self.assertEqual(rc, classifier.EXIT_BAD_SIGNATURES)
        finally:
            os.unlink(fh.name)
        rc, _ = self.run_cli(["--log", "/dev/null", "--signatures", "/nonexistent/table.tsv"])
        self.assertEqual(rc, classifier.EXIT_USAGE)


# ---------------------------------------------------------------------------
# Fake GitHub


class FakeGitHub:
    """Just enough of the REST API for rerun and rate, with a call log."""

    def __init__(self):
        self.runs = {}  # run_id -> run dict (latest attempt)
        self.attempt_jobs = {}  # (run_id, attempt) -> [job]
        self.job_logs = {}  # job_id -> text
        self.job_annotations = {}  # job_id -> [message]
        self.artifacts = {}  # name -> (id, verdicts dict)
        self.issues = {}  # number -> issue dict
        self.comments = []  # (number, body)
        self.reruns = []  # run ids
        self.next_issue = 100

    # GhApi surface -------------------------------------------------------
    def get_json(self, path, params=None):
        params = params or {}
        m = re.fullmatch(r"repos/[^/]+/[^/]+/actions/runs/(\d+)/attempts/(\d+)/jobs", path)
        if m:
            return {"jobs": self.attempt_jobs.get((int(m.group(1)), int(m.group(2))), [])}
        m = re.fullmatch(r"repos/[^/]+/[^/]+/actions/runs/(\d+)", path)
        if m:
            return self.runs[int(m.group(1))]
        m = re.fullmatch(r"repos/[^/]+/[^/]+/check-runs/(\d+)/annotations", path)
        if m:
            return [{"message": msg} for msg in self.job_annotations.get(int(m.group(1)), [])]
        if re.fullmatch(r"repos/[^/]+/[^/]+/actions/runs", path):
            page = int(params.get("page", 1))
            per = int(params.get("per_page", actions.RUNS_PER_PAGE))
            ordered = sorted(self.runs.values(), key=lambda r: -r["id"])
            return {"workflow_runs": ordered[(page - 1) * per: page * per]}
        if re.fullmatch(r"repos/[^/]+/[^/]+/actions/artifacts", path):
            hit = self.artifacts.get(params.get("name"))
            return {"artifacts": [{"id": hit[0], "expired": False}] if hit else []}
        if re.fullmatch(r"repos/[^/]+/[^/]+/issues", path):
            assert params.get("creator") == actions.TRACKING_ISSUE_CREATOR
            if int(params.get("page", 1)) > 1:
                return []
            return [i for i in self.issues.values() if i["state"] == "open"]
        raise AssertionError("unexpected GET " + path)

    def get_bytes(self, path):
        m = re.fullmatch(r"repos/[^/]+/[^/]+/actions/artifacts/(\d+)/zip", path)
        assert m, path
        for art_id, record in self.artifacts.values():
            if art_id == int(m.group(1)):
                buf = io.BytesIO()
                with zipfile.ZipFile(buf, "w") as zf:
                    zf.writestr("verdicts.json", json.dumps(record))
                return buf.getvalue()
        raise actions.ApiError("no artifact")

    def get_text_or_empty(self, path):
        m = re.fullmatch(r"repos/[^/]+/[^/]+/actions/jobs/(\d+)/logs", path)
        assert m, path
        return self.job_logs.get(int(m.group(1)), "")

    def post(self, path, body=None):
        m = re.fullmatch(r"repos/[^/]+/[^/]+/actions/runs/(\d+)/rerun-failed-jobs", path)
        if m:
            run_id = int(m.group(1))
            self.reruns.append(run_id)
            self.runs[run_id]["run_attempt"] += 1
            return None
        if re.fullmatch(r"repos/[^/]+/[^/]+/issues", path):
            number = self.next_issue
            self.next_issue += 1
            self.issues[number] = {
                "number": number, "state": "open", "title": body["title"],
                "body": body["body"], "labels": body.get("labels", []),
            }
            return {"number": number}
        m = re.fullmatch(r"repos/[^/]+/[^/]+/issues/(\d+)/comments", path)
        if m:
            self.comments.append((int(m.group(1)), body["body"]))
            return {}
        raise AssertionError("unexpected POST " + path)

    def patch(self, path, body):
        m = re.fullmatch(r"repos/[^/]+/[^/]+/issues/(\d+)", path)
        assert m, path
        self.issues[int(m.group(1))].update(body)
        return {}

    # helpers ---------------------------------------------------------------
    def add_failed_run(self, run_id, fixtures, workflow="v2 Tests", attempt=1):
        self.runs[run_id] = {
            "id": run_id, "name": workflow, "run_attempt": attempt,
            "conclusion": "failure", "html_url": "https://example.invalid/runs/%d" % run_id,
        }
        jobs = []
        for n, name in enumerate(fixtures):
            job_id = run_id * 10 + n
            log, ann = fixture(name)
            self.job_logs[job_id] = log
            if ann:
                self.job_annotations[job_id] = [ann]
            jobs.append({"id": job_id, "name": "job-%d" % n, "conclusion": "failure",
                         "runner_name": "hive-runners-w7f7t-runner-ab%d" % n})
        jobs.append({"id": run_id * 10 + 9, "name": "passing", "conclusion": "success",
                     "runner_name": "hive-runners-w7f7t-runner-zz"})
        self.attempt_jobs[(run_id, attempt)] = jobs


class DecideRerunTest(unittest.TestCase):
    INFRA = ["infra:gocache-permission"]

    def test_table(self):
        cases = [
            # attempt, current, same_repo, enabled, verdicts, want
            (1, 1, True, True, ["infra:disk-full"], True),
            (1, 1, True, True, ["infra:disk-full", "derived"], True),
            (1, 1, True, True, ["infra:disk-full", "code"], False),
            (1, 1, True, True, ["code"], False),
            (1, 1, True, True, ["derived"], False),
            (1, 1, True, True, [], False),
            (2, 2, True, True, ["infra:disk-full"], False),
            (1, 2, True, True, ["infra:disk-full"], False),
            (1, 1, False, True, ["infra:disk-full"], False),
            (1, 1, True, False, ["infra:disk-full"], False),
        ]
        for attempt, current, same, enabled, verdicts, want in cases:
            with self.subTest(attempt=attempt, current=current, same=same, enabled=enabled,
                              verdicts=verdicts):
                got, reason = actions.decide_rerun(attempt, current, same, enabled, verdicts)
                self.assertEqual(got, want, reason)
                self.assertTrue(reason)


class RerunOnceTest(unittest.TestCase):
    def rerun(self, gh, run_id, attempt, same_repo=True, enabled=True):
        return actions.run_rerun(gh, REPO, run_id, attempt, "v2 Tests", same_repo, enabled, SIGS)

    def test_infra_run_is_rerun_exactly_once_across_its_whole_life(self):
        gh = FakeGitHub()
        gh.add_failed_run(7, ["infra-gocache-permission-list", "derived-shard-gate"])
        rec = self.rerun(gh, 7, 1)
        self.assertTrue(rec["rerun"])
        self.assertEqual(gh.reruns, [7])
        self.assertEqual([j["verdict"] for j in rec["jobs"]],
                         ["infra:gocache-permission", "derived"])
        # The classifier workflow is re-run for attempt 1 by hand: no second rerun.
        rec = self.rerun(gh, 7, 1)
        self.assertFalse(rec["rerun"])
        # Attempt 2 fails on infra again: still no rerun.
        gh.attempt_jobs[(7, 2)] = gh.attempt_jobs[(7, 1)]
        rec = self.rerun(gh, 7, 2)
        self.assertFalse(rec["rerun"])
        self.assertEqual(gh.reruns, [7], "auto-rerun fired more than once for one run")

    def test_code_failure_is_never_rerun(self):
        gh = FakeGitHub()
        gh.add_failed_run(8, ["code-compile-redeclared"])
        gh.add_failed_run(9, ["infra-disk-full-setup-job", "code-test-shuffle-failure"])
        for run_id in (8, 9):
            rec = self.rerun(gh, run_id, 1)
            self.assertFalse(rec["rerun"])
            self.assertIn("code", rec["reason"])
        self.assertEqual(gh.reruns, [])

    def test_fork_and_switch_off_are_never_rerun(self):
        gh = FakeGitHub()
        gh.add_failed_run(10, ["infra-lint-timeout"])
        self.assertFalse(self.rerun(gh, 10, 1, same_repo=False)["rerun"])
        self.assertFalse(self.rerun(gh, 10, 1, enabled=False)["rerun"])
        self.assertEqual(gh.reruns, [])

    def test_failed_rerun_request_keeps_verdicts_and_fails_the_step(self):
        gh = FakeGitHub()
        gh.add_failed_run(15, ["infra-lint-timeout"])

        def refuse(path, body=None):
            raise actions.ApiError("HTTP 403")

        gh.post = refuse
        rec = self.rerun(gh, 15, 1)
        self.assertEqual((rec["rerun"], rec["error"]), (False, "HTTP 403"))
        self.assertEqual(rec["jobs"][0]["verdict"], "infra:lint-timeout")
        with tempfile.TemporaryDirectory() as tmp:
            out = os.path.join(tmp, "v.json")
            env = {"GITHUB_REPOSITORY": REPO, "CI_INFRA_RUN_ID": "15", "CI_INFRA_RUN_ATTEMPT": "1",
                   "CI_INFRA_HEAD_REPO": REPO}
            with patched_env(env), contextlib.redirect_stdout(io.StringIO()), \
                    contextlib.redirect_stderr(io.StringIO()):
                self.assertEqual(actions.main(["rerun", "--out", out], api=gh), actions.EXIT_API)
            self.assertTrue(os.path.exists(out))

    def test_runner_lost_uses_annotations(self):
        gh = FakeGitHub()
        gh.add_failed_run(11, ["infra-runner-lost"])
        rec = self.rerun(gh, 11, 1)
        self.assertEqual(rec["jobs"][0]["verdict"], "infra:runner-lost")
        self.assertTrue(rec["rerun"])

    def test_summary_names_every_job(self):
        gh = FakeGitHub()
        gh.add_failed_run(12, ["infra-lint-no-go-files", "code-vet-undefined"])
        text = actions.render_rerun_summary(self.rerun(gh, 12, 1))
        self.assertIn("no rerun", text)
        self.assertIn("`infra:lint-no-go-files`", text)
        self.assertIn("`code`", text)

    def test_main_rerun_writes_verdicts_and_validates_env(self):
        gh = FakeGitHub()
        gh.add_failed_run(13, ["infra-build-cache-corrupt"])
        with tempfile.TemporaryDirectory() as tmp:
            out = os.path.join(tmp, "sub", "verdicts.json")
            env = {
                "GITHUB_REPOSITORY": REPO, "CI_INFRA_RUN_ID": "13", "CI_INFRA_RUN_ATTEMPT": "1",
                "CI_INFRA_HEAD_REPO": REPO, "CI_INFRA_WORKFLOW": "v2 Tests",
                "GITHUB_STEP_SUMMARY": os.path.join(tmp, "summary.md"),
            }
            with patched_env(env), contextlib.redirect_stdout(io.StringIO()):
                rc = actions.main(["rerun", "--out", out], api=gh)
            self.assertEqual(rc, actions.EXIT_OK)
            rec = json.loads(read(out))
            self.assertEqual(rec["jobs"][0]["verdict"], "infra:build-cache-corrupt")
            self.assertTrue(rec["rerun"])
            self.assertIn("CI infra classifier", read(env["GITHUB_STEP_SUMMARY"]))
            env["CI_INFRA_RUN_ID"] = "not-a-number"
            with patched_env(env), contextlib.redirect_stderr(io.StringIO()):
                self.assertEqual(actions.main(["rerun", "--out", out], api=gh), actions.EXIT_USAGE)
        self.assertEqual(gh.reruns, [13])

    def test_main_switch_off_classifies_without_rerunning(self):
        gh = FakeGitHub()
        gh.add_failed_run(14, ["infra-build-cache-corrupt"])
        with tempfile.TemporaryDirectory() as tmp:
            env = {"GITHUB_REPOSITORY": REPO, "CI_INFRA_RUN_ID": "14", "CI_INFRA_RUN_ATTEMPT": "1",
                   "CI_INFRA_HEAD_REPO": REPO, "CI_INFRA_RERUN": "off"}
            with patched_env(env), contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(actions.main(["rerun", "--out", os.path.join(tmp, "v.json")],
                                              api=gh), actions.EXIT_OK)
        self.assertEqual(gh.reruns, [])


@contextlib.contextmanager
def patched_env(values):
    keys = set(values) | {
        "CI_INFRA_RERUN", "CI_INFRA_WINDOW_RUNS", "CI_INFRA_ALERT_THRESHOLD",
        "CI_INFRA_MIN_RUNS", "CI_INFRA_WORKFLOWS", "CI_INFRA_ALERT_LABELS",
        "GITHUB_STEP_SUMMARY", "CI_INFRA_SIGNATURES",
    }
    saved = {k: os.environ.get(k) for k in keys}
    try:
        for k in keys:
            os.environ.pop(k, None)
        os.environ.update(values)
        yield
    finally:
        for k, v in saved.items():
            if v is None:
                os.environ.pop(k, None)
            else:
                os.environ[k] = v


# ---------------------------------------------------------------------------
# rate


def record(run_id, verdicts, attempt=1, runner="hive-runners-w7f7t-runner-abcde"):
    return {"run_id": run_id, "run_attempt": attempt,
            "jobs": [{"id": i, "name": "j", "runner_name": runner, "verdict": v}
                     for i, v in enumerate(verdicts)]}


class ComputeRateTest(unittest.TestCase):
    def test_counts_runs_not_jobs_and_counts_rerun_green_runs(self):
        runs = [
            {"id": 1, "conclusion": "failure", "run_attempt": 1},
            # Turned green by the automatic rerun: still an infra hit.
            {"id": 2, "conclusion": "success", "run_attempt": 2},
            {"id": 3, "conclusion": "failure", "run_attempt": 1},
            {"id": 4, "conclusion": "success", "run_attempt": 1},
            {"id": 5, "conclusion": "failure", "run_attempt": 1},  # no record
        ]
        verdicts = {
            1: [record(1, ["infra:disk-full", "infra:disk-full", "derived"])],
            2: [record(2, ["infra:gocache-permission"]), record(2, [], attempt=2)],
            3: [record(3, ["code"])],
        }
        rep = actions.compute_rate(runs, verdicts)
        self.assertEqual((rep["total"], rep["infra_runs"], rep["unclassified"]), (5, 2, 1))
        self.assertAlmostEqual(rep["rate"], 0.4)
        self.assertEqual(rep["classes"], {"disk-full": 2, "gocache-permission": 1})
        pools = {pool for (_, _, pool) in rep["pairs"]}
        self.assertEqual(pools, {"hive-runners-w7f7t"})

    def test_empty_window(self):
        rep = actions.compute_rate([], {})
        self.assertEqual((rep["total"], rep["rate"]), (0, 0.0))


def report(total, infra):
    runs = [{"id": i, "conclusion": "failure", "run_attempt": 1} for i in range(total)]
    verdicts = {i: [record(i, ["infra:disk-full"])] for i in range(infra)}
    return actions.compute_rate(runs, verdicts)


class PlanTrackingTest(unittest.TestCase):
    T, MIN, WIN = 0.15, 20, 100

    def plan(self, rep, existing):
        return actions.plan_tracking(rep, existing, self.T, self.MIN, self.WIN)

    def test_open_update_close_noop(self):
        above, below = report(100, 20), report(100, 5)
        self.assertEqual(self.plan(above, None).action, "open")
        up = self.plan(above, {"number": 5})
        self.assertEqual((up.action, up.number), ("update", 5))
        close = self.plan(below, {"number": 5})
        self.assertEqual((close.action, close.number), ("close", 5))
        self.assertIn("back below", close.comment)
        self.assertEqual(self.plan(below, None).action, "noop")

    def test_threshold_is_inclusive_and_small_windows_decide_nothing(self):
        self.assertEqual(self.plan(report(100, 15), None).action, "open")
        self.assertEqual(self.plan(report(100, 14), None).action, "noop")
        self.assertEqual(self.plan(report(10, 10), None).action, "noop")
        self.assertEqual(self.plan(report(10, 0), {"number": 5}).action, "noop")

    def test_body_has_marker_breakdown_and_no_raw_mentions(self):
        rep = report(100, 30)
        rep["samples"] = ["see @example-user"]
        body = self.plan(rep, None).body
        self.assertTrue(body.startswith(actions.TRACKING_MARKER))
        self.assertIn("**30 of the last 100**", body)
        self.assertIn("| `disk-full` | 30 |", body)
        self.assertIn("hive-runners-w7f7t", body)
        self.assertNotRegex(body, r"(^|[^A-Za-z0-9`/])@[A-Za-z0-9]")


class RateLifecycleTest(unittest.TestCase):
    """The rate alert opens, updates and closes ONE tracking issue."""

    def setup_hour(self, gh, infra_runs, total=40, base=1000):
        gh.runs = {}
        gh.artifacts = {}
        for i in range(total):
            run_id = base + i
            gh.runs[run_id] = {"id": run_id, "name": "v2 CI", "run_attempt": 1,
                               "conclusion": "failure" if i < infra_runs else "success",
                               "html_url": "https://example.invalid/runs/%d" % run_id}
            if i < infra_runs:
                gh.artifacts[actions.artifact_name(run_id, 1)] = (
                    run_id, record(run_id, ["infra:gocache-permission"]))
        # Runs of unwatched workflows never count.
        gh.runs[99999] = {"id": 99999, "name": "Tagged Release", "run_attempt": 1,
                          "conclusion": "failure", "html_url": ""}

    def hour(self, gh):
        return actions.run_rate(gh, REPO, set(actions.DEFAULT_WATCHED_WORKFLOWS),
                                window=100, threshold=0.15, min_runs=20, labels=["ci"])

    def test_one_issue_opened_updated_then_closed(self):
        gh = FakeGitHub()
        # An unrelated open bot issue must never be mistaken for the tracker.
        gh.issues[1] = {"number": 1, "state": "open", "title": "other", "body": "no marker"}

        self.setup_hour(gh, infra_runs=12)
        rep, plan, number = self.hour(gh)
        self.assertEqual((rep["total"], rep["infra_runs"], plan.action), (40, 12, "open"))
        self.assertEqual(gh.issues[number]["labels"], ["ci"])
        self.assertIn("12 of the last 40", gh.issues[number]["body"])

        self.setup_hour(gh, infra_runs=20)
        _, plan, again = self.hour(gh)
        self.assertEqual((plan.action, again), ("update", number))
        self.assertIn("20 of the last 40", gh.issues[number]["body"])

        self.setup_hour(gh, infra_runs=2)
        _, plan, closed = self.hour(gh)
        self.assertEqual((plan.action, closed), ("close", number))
        self.assertEqual(gh.issues[number]["state"], "closed")
        self.assertEqual([n for n, _ in gh.comments], [number])

        self.setup_hour(gh, infra_runs=2)
        _, plan, _ = self.hour(gh)
        self.assertEqual(plan.action, "noop")

        trackers = [i for i in gh.issues.values() if actions.TRACKING_MARKER in i["body"]]
        self.assertEqual(len(trackers), 1, "more than one tracking issue was opened")
        self.assertEqual(gh.issues[1]["state"], "open")

    def test_main_rate_reads_env_tunables(self):
        gh = FakeGitHub()
        self.setup_hour(gh, infra_runs=5)
        env = {"GITHUB_REPOSITORY": REPO, "CI_INFRA_ALERT_THRESHOLD": "0.1"}
        with patched_env(env), contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(actions.main(["rate"], api=gh), actions.EXIT_OK)
        self.assertEqual(len(gh.issues), 1)  # 5/40 = 12.5% >= 10%
        with patched_env({"GITHUB_REPOSITORY": REPO, "CI_INFRA_MIN_RUNS": "x"}), \
                contextlib.redirect_stderr(io.StringIO()):
            self.assertEqual(actions.main(["rate"], api=gh), actions.EXIT_USAGE)


class MentionTest(unittest.TestCase):
    def test_neutralize(self):
        cases = {
            "ping @alice now": "ping `alice` now",
            "mail a@b.example": "mail a@b.example",
            "https://x.example/@alice": "https://x.example/@alice",
            "code `@alice` span": "code `alice` span",
            "```\n@alice\n```": "```\nalice\n```",
            "no mentions": "no mentions",
        }
        for src, want in cases.items():
            with self.subTest(src=src):
                got = actions.neutralize_mentions(src)
                self.assertEqual(got, want)
                self.assertEqual(actions.neutralize_mentions(got), got)


class GhApiTest(unittest.TestCase):
    def test_builds_commands_and_raises_on_failure(self):
        calls = []

        def runner(cmd, **kw):
            calls.append((cmd, kw.get("input")))
            # Match the fake failing path exactly: "rerun-failed-jobs"
            # also contains "fail" and must succeed.
            if any(arg.endswith("/fail") for arg in cmd):
                return subprocess.CompletedProcess(cmd, 1, b"", b"HTTP 404")
            return subprocess.CompletedProcess(cmd, 0, b'{"ok": true}', b"")

        api = actions.GhApi(runner=runner)
        self.assertEqual(api.get_json("repos/o/r/issues", {"creator": "github-actions[bot]"}),
                         {"ok": True})
        self.assertEqual(calls[-1][0], ["gh", "api", "repos/o/r/issues?creator=github-actions%5Bbot%5D"])
        api.post("repos/o/r/actions/runs/1/rerun-failed-jobs")
        self.assertEqual(calls[-1][0][:5], ["gh", "api", "-X", "POST", "repos/o/r/actions/runs/1/rerun-failed-jobs"])
        self.assertEqual(json.loads(calls[-1][1]), {})
        with self.assertRaises(actions.ApiError):
            api.get_json("repos/o/r/fail")
        self.assertEqual(api.get_text_or_empty("repos/o/r/fail"), "")


class WorkflowSyncTest(unittest.TestCase):
    def watched_in_workflow(self):
        names, inside = [], False
        for line in read(RERUN_WORKFLOW).splitlines():
            if re.match(r"^\s+workflows:\s*$", line):
                inside = True
                continue
            if inside:
                m = re.match(r"^\s+-\s+(.+?)\s*$", line)
                if not m:
                    break
                names.append(m.group(1))
        return names

    def test_rerun_trigger_matches_script_default(self):
        self.assertEqual(self.watched_in_workflow(), list(actions.DEFAULT_WATCHED_WORKFLOWS))

    def test_every_watched_workflow_exists(self):
        declared = set()
        for f in os.listdir(WORKFLOWS):
            if f.endswith((".yml", ".yaml")):
                m = re.search(r"^name:\s*(.+?)\s*$", read(os.path.join(WORKFLOWS, f)), re.M)
                if m:
                    declared.add(m.group(1).strip("'\""))
        missing = set(actions.DEFAULT_WATCHED_WORKFLOWS) - declared
        self.assertEqual(missing, set(), "watched workflow renamed or removed")


if __name__ == "__main__":
    unittest.main(verbosity=2)
