#!/usr/bin/env python3
"""Focused CI fixtures; never collect deployment evidence or contact GitHub."""

import datetime as dt
import importlib.util
import json
import math
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest import mock

SCRIPT = Path(__file__).with_name("monthly-outcomes.py")
SPEC = importlib.util.spec_from_file_location("monthly_outcomes", SCRIPT)
monthly = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(monthly)
NOW = dt.datetime(2026, 11, 15, 6, 17, tzinfo=monthly.UTC)


def fixture():
    # Historical counts are test data, not a new October observation.
    report = json.loads((monthly.ROOT / monthly.REPORT_DIR / "outcomes-2026-10.json").read_text())
    report.update(Now="2026-11-15T00:00:00Z", GeneratedAt="2026-11-15T00:00:00Z", Unreadable=0)
    for name in ("mediated", "unmediated"):
        cohort = report[name.capitalize()]
        cohort["Matured"] = cohort["Merged"]
        for key in ("RevertRate", "ChangeFailure"):
            rate = cohort[key]
            k, n = rate["K"], cohort["Merged"]
            p, z = k / n, 1.96
            divisor = 1 + z * z / n
            centre = (p + z * z / (2 * n)) / divisor
            half = z * math.sqrt(p * (1 - p) / n + z * z / (4 * n * n)) / divisor
            rate.update(N=n, Rate=p, Lo=max(0, centre - half), Hi=min(1, centre + half), Reported=True)
        report["Trend"][name][-1]["RevertRate"] = dict(cohort["RevertRate"])
    return dict(schema=1, month="2026-10", collection_complete=True, publication_approved=True,
                snapshot_sha256="a" * 64, generator_revision="b" * 40, report=report)


class MonthlyOutcomes(unittest.TestCase):
    def test_utc_boundary_rollover_and_maturation(self):
        zone = dt.timezone(dt.timedelta(hours=-8))
        month, start, end = monthly.select_month(
            "", dt.datetime(2027, 1, 15, 0, 0, tzinfo=zone))
        self.assertEqual(month, "2026-12")
        self.assertEqual(start.isoformat(), "2026-12-01T00:00:00+00:00")
        self.assertEqual(end.isoformat(), "2027-01-01T00:00:00+00:00")
        for month, instant in [
                ("2026-11", NOW), ("2026-13", NOW), ("2026-1", NOW),
                ("", dt.datetime(2027, 1, 1, tzinfo=monthly.UTC)),
                ("2026-10", dt.datetime(2026, 11, 14, 23, 59, tzinfo=monthly.UTC))]:
            with self.subTest(month=month, instant=instant), self.assertRaises(ValueError):
                monthly.select_month(month, instant)
        self.assertEqual(monthly.select_month("2026-02", NOW)[2].day, 1)

    def test_projection_preserves_cost_and_removes_private_fields(self):
        payload = fixture()
        payload["report"]["Source"] = "PRIVATE-NAME and credential"
        payload["report"]["Stories"] = [{"Caught": "PRIVATE-BODY"}]
        payload["report"]["Hub"] = {"private": "PRIVATE-HUB"}
        payload["report"]["RedTeam"] = {"source": "PRIVATE-EVAL"}
        payload["report"]["Repos"][0]["Repo"] = "PRIVATE-REPO"
        payload["report"]["Unexpected"] = "PRIVATE-UNKNOWN"
        clean = monthly.validate(payload, "", NOW)
        text = json.dumps(clean)
        self.assertNotIn("PRIVATE", text)
        self.assertEqual(clean["report"]["Spend"], payload["report"]["Spend"])
        self.assertEqual(clean["report"]["Spend"]["merged_prs"], 2869)
        self.assertEqual(clean["report"]["Spend"]["usd"], 6124.75144225)
        self.assertEqual(clean["report"]["Spend"]["window_start"], "2026-08-08T00:00:00Z")
        self.assertEqual(monthly.validate(clean, "", NOW), clean)
        self.assertEqual(clean["report"]["Mediated"], payload["report"]["Mediated"])

    def test_empty_partial_stale_unapproved_and_invalid_data_fail(self):
        mutations = [
            lambda p: p.update(month="2026-09"),
            lambda p: p.update(collection_complete=False),
            lambda p: p.update(publication_approved=False),
            lambda p: p.update(generator_revision="not-a-revision"),
            lambda p: p["report"].update(Unreadable=1),
            lambda p: p["report"].update(Bundles=0),
            lambda p: p["report"].update(Until="2026-10-27T00:00:00Z"),
            lambda p: p["report"].update(Now="2026-11-01T00:00:00Z"),
            lambda p: p["report"].update(GeneratedAt="2026-11-16T00:00:00Z"),
            lambda p: p["report"]["Mediated"].update(Matured=99999),
            lambda p: p["report"]["Mediated"].update(Matured=43),
            lambda p: p["report"]["Mediated"]["RevertRate"].update(Hi=0.95),
            lambda p: p["report"]["Mediated"]["ByAuthorKind"].update(private_user=1),
            lambda p: p["report"]["Trend"]["mediated"].pop(),
            lambda p: p["report"]["Spend"].update(usd=float("nan")),
            lambda p: p["report"]["Spend"].update(confidence_interval=[0, 1]),
            lambda p: p["report"]["Followup"].update(CIPairs=999999),
        ]
        for index, mutate in enumerate(mutations):
            payload = fixture()
            mutate(payload)
            with self.subTest(index=index), self.assertRaises((ValueError, KeyError)):
                monthly.validate(payload, "", NOW)

    def test_missing_secret_fails_without_logging_input(self):
        with mock.patch.dict(os.environ, {"MONTHLY_OUTCOMES_INPUT": ""}):
            with self.assertRaisesRegex(ValueError, "missing owner-supplied input"):
                monthly.read_input(None)
        with tempfile.TemporaryDirectory(dir=monthly.ROOT) as directory:
            path = Path(directory) / "invalid.json"
            path.write_text('{"PRIVATE-SOURCE":')
            result = subprocess.run(
                ["python3", str(SCRIPT), "prepare", "--input", str(path),
                 "--out", str(Path(directory) / "output.json")],
                text=True, capture_output=True, check=False)
            self.assertNotEqual(result.returncode, 0)
            self.assertNotIn("PRIVATE-SOURCE", result.stderr + result.stdout)
            self.assertFalse((Path(directory) / "output.json").exists())

    def test_idempotent_committed_input_skips_renderer(self):
        payload = monthly.validate(fixture(), "", NOW)
        with tempfile.TemporaryDirectory(dir=monthly.ROOT) as directory:
            root = Path(directory)
            provenance = root / monthly.REPORT_DIR / "outcomes-2026-10.provenance.json"
            provenance.parent.mkdir(parents=True)
            provenance.write_text(json.dumps({"input_sha256": monthly.digest(payload)}))
            with mock.patch.object(monthly, "ROOT", root), mock.patch.object(monthly.subprocess, "run") as run:
                self.assertFalse(monthly.render(payload, root / "stage", "c" * 40))
                run.assert_not_called()
                self.assertFalse((root / "stage").exists())

    def test_renderer_error_propagates(self):
        with tempfile.TemporaryDirectory(dir=monthly.ROOT) as directory:
            with mock.patch.object(monthly.subprocess, "run", side_effect=subprocess.CalledProcessError(1, "go")):
                with self.assertRaises(subprocess.CalledProcessError):
                    monthly.render(monthly.validate(fixture(), "", NOW), Path(directory), "c" * 40)

    def test_real_renderer_retains_methodology_and_reproducibility(self):
        payload = monthly.validate(fixture(), "", NOW)
        with tempfile.TemporaryDirectory(dir=monthly.ROOT) as first, \
                tempfile.TemporaryDirectory(dir=monthly.ROOT) as second:
            for directory in (first, second):
                self.assertTrue(monthly.render(payload, Path(directory), "c" * 40))
            for path in Path(first).rglob("*"):
                if path.is_file():
                    self.assertEqual(path.read_bytes(), (Path(second) / path.relative_to(first)).read_bytes())
            text = (Path(first) / "outcomes-2026-10.md").read_text()
            for expected in ("95% Wilson", "upper-bound order of magnitude", "n=2869 merged",
                             "2026-08-08 → 2026-10-27", "not a controlled comparison",
                             "Window: merged 2026-10-01 → 2026-11-01"):
                self.assertIn(expected.lower(), text.lower())
            report = json.loads((Path(first) / "outcomes-2026-10.json").read_text())
            self.assertEqual(report["Spend"], payload["report"]["Spend"])
            self.assertFalse(report.get("Stories"))
            self.assertFalse(report.get("Hub"))
            self.assertFalse(report.get("RedTeam"))
            provenance = json.loads((Path(first) / "outcomes-2026-10.provenance.json").read_text())
            self.assertEqual(provenance["input_sha256"], monthly.digest(payload))
            self.assertEqual(provenance["generator_revision"], "b" * 40)
            self.assertEqual(provenance["renderer_revision"], "c" * 40)

    def test_publication_one_pr_rerun_no_diff_and_closed_pr(self):
        publisher = monthly.ROOT / ".github/scripts/monthly-outcomes-pr.sh"
        with tempfile.TemporaryDirectory(dir=monthly.ROOT) as directory:
            root = Path(directory)
            origin = root / "origin.git"
            seed = root / "seed"
            calls = root / "gh-calls"
            binary = root / "bin"
            binary.mkdir()
            gh = binary / "gh"
            gh.write_text(
                "#!/usr/bin/env bash\nset -eu\n"
                "if [ \"$2\" = list ]; then printf '%s' \"$MOCK_PRS\"; "
                "else echo create >> \"$MOCK_CALLS\"; fi\n")
            gh.chmod(0o755)
            env = dict(os.environ, PATH=f"{binary}:{os.environ['PATH']}",
                       GH_TOKEN="not-a-real-credential", GITHUB_REPOSITORY="fixture/repo",
                       MONTH="2026-10", MOCK_PRS="[]", MOCK_CALLS=str(calls))

            def git(*args, cwd=root):
                return subprocess.check_output(["git", *args], cwd=cwd, text=True,
                                               stderr=subprocess.DEVNULL).strip()

            git("init", "--bare", str(origin))
            git("init", "-b", "v5", str(seed))
            git("config", "user.name", "Fixture", cwd=seed)
            git("config", "user.email", "fixture@example.invalid", cwd=seed)
            dest = seed / monthly.REPORT_DIR
            (dest / "assets").mkdir(parents=True)
            (dest / "assets/keep.svg").write_text("unrelated")
            (dest / "assets/outcomes-2026-10-card-obsolete.svg").write_text("stale")
            (dest / "outcomes-2026-09.json").write_text("unrelated")
            git("add", ".", cwd=seed)
            git("commit", "-s", "-m", "fixture", cwd=seed)
            git("remote", "add", "origin", str(origin), cwd=seed)
            git("push", "origin", "v5", cwd=seed)
            heads = []
            for attempt, prs in enumerate(("[]", '[{"number":1,"state":"OPEN"}]',
                                           '[{"number":1,"state":"CLOSED"}]',
                                           '[{"number":1,"state":"MERGED"}]')):
                checkout = root / f"checkout-{attempt}"
                git("clone", "--branch", "v5", str(origin), str(checkout))
                stage = checkout / ".monthly-outcomes"
                (stage / "assets").mkdir(parents=True)
                for suffix in ("json", "md"):
                    (stage / f"outcomes-2026-10.{suffix}").write_text("same approved aggregate")
                (stage / "outcomes-2026-10.provenance.json").write_text(
                    json.dumps({"input_sha256": "a" * 64, "renderer_revision": str(attempt)}))
                (stage / "assets/outcomes-2026-10-bars.svg").write_text("same chart")
                env["MOCK_PRS"] = prs
                result = subprocess.run(["bash", str(publisher)], cwd=checkout, env=env,
                                        text=True, capture_output=True, check=False)
                if attempt < 2:
                    self.assertEqual(result.returncode, 0, result.stderr + result.stdout)
                    heads.append(git("rev-parse", "HEAD", cwd=checkout))
                    self.assertEqual((checkout / monthly.REPORT_DIR / "outcomes-2026-09.json").read_text(),
                                     "unrelated")
                    self.assertEqual((checkout / monthly.REPORT_DIR / "assets/keep.svg").read_text(),
                                     "unrelated")
                    self.assertFalse((checkout / monthly.REPORT_DIR /
                                      "assets/outcomes-2026-10-card-obsolete.svg").exists())
                    self.assertIn("Signed-off-by:", git("log", "-1", "--format=%B", cwd=checkout))
                else:
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn("already exists", result.stdout)
            self.assertEqual(heads[0], heads[1], "identical input must not create another commit")
            self.assertEqual(calls.read_text().splitlines(), ["create"])

    def test_publication_recovers_orphan_branch_but_not_empty_publication(self):
        publisher = monthly.ROOT / ".github/scripts/monthly-outcomes-pr.sh"
        branch = "automation/monthly-outcomes-2026-10"
        for scenario in ("orphan", "no-branch", "no-source-diff"):
            with self.subTest(scenario=scenario), \
                    tempfile.TemporaryDirectory(dir=monthly.ROOT) as directory:
                root = Path(directory)
                origin, seed, binary = root / "origin.git", root / "seed", root / "bin"
                calls, prs, failures = root / "calls", root / "prs", root / "failures"
                binary.mkdir()
                prs.write_text("[]")
                failures.write_text("2")
                gh = binary / "gh"
                gh.write_text(
                    "#!/usr/bin/env bash\nset -eu\n"
                    "if [ \"$2\" = list ]; then cat \"$MOCK_PRS_FILE\"; exit; fi\n"
                    "[ \"$2\" = create ]\n"
                    "echo create >> \"$MOCK_CALLS\"\n"
                    "remaining=$(cat \"$MOCK_FAILURES\")\n"
                    "if [ \"$remaining\" -gt 0 ]; then\n"
                    "  echo $((remaining - 1)) > \"$MOCK_FAILURES\"\n"
                    "  echo 'transient PR creation failure' >&2; exit 1\n"
                    "fi\n"
                    "printf '[{\"number\":1,\"state\":\"OPEN\"}]' > \"$MOCK_PRS_FILE\"\n")
                gh.chmod(0o755)
                env = dict(os.environ, PATH=f"{binary}:{os.environ['PATH']}",
                           GH_TOKEN="not-a-real-credential", GITHUB_REPOSITORY="fixture/repo",
                           MONTH="2026-10", MOCK_PRS_FILE=str(prs), MOCK_CALLS=str(calls),
                           MOCK_FAILURES=str(failures))

                def git(*args, cwd=root):
                    return subprocess.check_output(["git", *args], cwd=cwd, text=True,
                                                   stderr=subprocess.DEVNULL).strip()

                def outputs(dest, revision):
                    (dest / "assets").mkdir(parents=True, exist_ok=True)
                    for suffix in ("json", "md"):
                        (dest / f"outcomes-2026-10.{suffix}").write_text("approved aggregate")
                    (dest / "outcomes-2026-10.provenance.json").write_text(json.dumps(
                        {"input_sha256": "a" * 64, "renderer_revision": revision}))
                    (dest / "assets/outcomes-2026-10-bars.svg").write_text("approved chart")

                git("init", "--bare", str(origin))
                git("init", "-b", "v5", str(seed))
                git("config", "user.name", "Fixture", cwd=seed)
                git("config", "user.email", "fixture@example.invalid", cwd=seed)
                dest = seed / monthly.REPORT_DIR
                (dest / "assets").mkdir(parents=True)
                (dest / "assets/keep.svg").write_text("unrelated")
                if scenario != "orphan":
                    outputs(dest, "base")
                git("add", ".", cwd=seed)
                git("commit", "-s", "-m", "fixture", cwd=seed)
                git("remote", "add", "origin", str(origin), cwd=seed)
                git("push", "origin", "v5", cwd=seed)
                if scenario == "no-source-diff":
                    git("checkout", "-b", branch, cwd=seed)
                    (seed / "unrelated.txt").write_text("not a monthly publication")
                    git("add", ".", cwd=seed)
                    git("commit", "-s", "-m", "unrelated change", cwd=seed)
                    git("push", "origin", branch, cwd=seed)

                heads, provenances = [], []
                for attempt in range(4 if scenario == "orphan" else 1):
                    checkout = root / f"checkout-{attempt}"
                    git("clone", "--branch", "v5", str(origin), str(checkout))
                    outputs(checkout / ".monthly-outcomes", str(attempt))
                    result = subprocess.run(["bash", str(publisher)], cwd=checkout, env=env,
                                            text=True, capture_output=True, check=False)
                    expected_failure = scenario == "orphan" and attempt < 2
                    if expected_failure:
                        self.assertNotEqual(result.returncode, 0)
                        self.assertIn("transient PR creation failure", result.stderr)
                        self.assertEqual(prs.read_text(), "[]")
                    else:
                        self.assertEqual(result.returncode, 0, result.stderr + result.stdout)
                    heads.append(git("rev-parse", "HEAD", cwd=checkout))
                    provenances.append((checkout / monthly.REPORT_DIR /
                                        "outcomes-2026-10.provenance.json").read_bytes())
                    remote = git("ls-remote", "--heads", "origin", branch, cwd=checkout)
                    if scenario == "no-branch":
                        self.assertEqual(remote, "", "a no-op must not push an orphan branch")
                    else:
                        self.assertEqual(remote.split()[0], heads[0])
                self.assertEqual(len(set(heads)), 1, "recovery must not make another commit")
                self.assertEqual(len(set(provenances)), 1, "recovery must retain original provenance")
                if scenario == "orphan":
                    self.assertEqual(calls.read_text().splitlines(), ["create"] * 3)
                    self.assertEqual(json.loads(prs.read_text()), [{"number": 1, "state": "OPEN"}])
                else:
                    self.assertFalse(calls.exists(), "no monthly source diff means no PR")


if __name__ == "__main__":
    unittest.main()
