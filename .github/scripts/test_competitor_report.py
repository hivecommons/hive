"""Offline report regression fixtures; CI is the executable verdict."""

import base64
import datetime
import json
from pathlib import Path
import unittest
from unittest.mock import patch

import competitor_report as report


ROOT = Path(__file__).resolve().parents[2]
FIXTURE = Path(__file__).parent / "testdata/competitor-report/multi-forge.json"
TODAY = datetime.date(2026, 11, 2)


def no_work(endpoint):
    return {"total_count": 0, "incomplete_results": False, "items": []}


class GroundingTests(unittest.TestCase):
    def setUp(self):
        self.bundle = json.loads(FIXTURE.read_text())
        self.claim = {
            "text": "Hive has implemented GitHub, GitLab and Gitea/Forgejo adapters; "
                    "the governor still enumerates through GitHub, and agent/proxy "
                    "execution is not end-to-end non-GitHub.",
            "sources": [self.ref("src/docs/integrations/forges.md", 2, 4),
                        self.ref("src/pkg/forge/forge.go", 2, 4),
                        self.ref("src/cmd/hive/main_eval_cycle.go", 3, 3)],
        }
        self.ledger = {"sha": self.bundle["sha"], "claims": [self.claim],
                       "news": [], "recommendations": []}
        self.extra = ""

    def ref(self, path, start, end):
        return {"path": path, "start": start, "end": end,
                "quote_b64": base64.b64encode(
                    "\n".join(self.bundle["files"][path].splitlines()[start - 1:end]).encode()
                ).decode()}

    def url(self, ref):
        return (f"https://github.com/hivecommons/hive/blob/{self.bundle['sha']}/"
                f"{ref['path']}#L{ref['start']}-L{ref['end']}")

    def payload(self):
        text = (
            f"Source: hivecommons/hive; SHA: {self.bundle['sha']}; "
            f"default: {self.bundle['default_branch']}\n"
            "**Hive weekly competitor report - 2026-11-02**\n"
            "**What changed this week**\nno news found\n"
            "**Who does what**\n"
            "**SWOT for Hive**\n"
        )
        for claim in self.ledger["claims"]:
            text += claim["text"] + " " + " ".join(
                f"<{self.url(ref)}>" for ref in claim["sources"]) + "\n"
        text += (
            "**One thing to watch**\n"
            "Follow reachable operations rather than mistaking a source interface "
            "for deployed feature parity. No new unsupported work is proposed.\n"
            "**Top 5 this week - stay ahead and stay integrated**\n"
            "Evidence limits: quotations establish provenance, not semantic proof. "
            "All-state work searches are bounded, and inaccessible vendor news is "
            "not a basis for opening work.\n" + self.extra
        )
        return {"items": [
            {"type": "add_comment", "item_number": 14613,
             "body": text.strip() + "\n" + report.LEDGER_START +
                     json.dumps(self.ledger) + report.LEDGER_END},
            {"type": "post_to_discord", "message": text.strip()},
        ]}

    def validate(self, payload=None, fetch=no_work, **kwargs):
        return report.validate(payload or self.payload(), self.bundle, fetch=fetch,
                               today=TODAY, **kwargs)

    def test_multiforge_implementation_and_execution_gap_both_grounded(self):
        self.validate()
        readable = report.readable(self.bundle)
        for symbol in ("GitLabForge", "GiteaForge", "EnumerateActionable"):
            self.assertIn(symbol, readable)

    def test_known_blanket_github_only_regression(self):
        self.claim["text"] = "Hive is GitHub-only. Design a forge adapter."
        with self.assertRaisesRegex(ValueError, "blanket GitHub-only"):
            self.validate()

    def test_default_branch_and_dispatched_source_revision_are_distinct(self):
        self.bundle["sha"] = "1479fd23fe09017b042b9d84f311daccfdd9b57b"
        self.ledger["sha"] = self.bundle["sha"]
        self.validate()
        self.assertIn("default: v5", self.payload()["items"][0]["body"])

    def test_stale_branch_and_revision_links(self):
        for revision in ("main", "v5", "v6", "a" * 40):
            payload = self.payload()
            for item in payload["items"]:
                field = "body" if item["type"] == "add_comment" else "message"
                item[field] = item[field].replace(
                    f"/blob/{self.bundle['sha']}/", f"/blob/{revision}/")
            with self.subTest(revision=revision), self.assertRaisesRegex(ValueError, "stale"):
                self.validate(payload)

    def test_ledger_revision_must_match_checkout(self):
        self.ledger["sha"] = "a" * 40
        with self.assertRaisesRegex(ValueError, "Ledger source SHA"):
            self.validate()

    def test_quotes_must_match_source_at_sha(self):
        self.claim["sources"][0]["quote_b64"] = base64.b64encode(
            b"Hive has no forge interface.").decode()
        with self.assertRaisesRegex(ValueError, "Quote does not match"):
            self.validate()

    def test_source_paths_and_line_anchors_exist(self):
        for suffix in ("missing.md", "README.md#L999-L1000"):
            self.extra = f"<https://github.com/hivecommons/hive/blob/{self.bundle['sha']}/{suffix}>"
            with self.subTest(suffix=suffix), self.assertRaises(ValueError):
                self.validate()

    def test_crux_mirror_is_not_launcher_source(self):
        self.extra = "<https://github.com/hivecommons/crux/blob/main/config/agent_runtime.go>"
        with self.assertRaisesRegex(ValueError, "generated mirror"):
            self.validate()

    def test_license_detection_is_not_product_license(self):
        for text in ("Hive has no license.", "| Project | NOASSERTION |", "| Project | none |"):
            self.extra = text
            with self.subTest(text=text), self.assertRaisesRegex(ValueError, "license"):
                self.validate()
        self.extra = "| Project | not detected (unverified) |"
        self.validate()

    def metric(self):
        claim = {
            "text": "Historical $2.13 estimate, n=2869 merged, 2026-08-08 to 2026-10-27; "
                    "upper-bound, order-of-magnitude estimate",
            "kind": "metric",
            "cohort": "n=2869 merged",
            "window": "2026-08-08 to 2026-10-27",
            "caveat": "upper-bound, order-of-magnitude estimate",
            "sources": [self.ref("src/docs/benchmarks/outcomes-2026-10.md", 2, 5)],
        }
        self.ledger["claims"].append(claim)
        return claim

    def test_historical_metric_carries_cohort_window_and_caveat(self):
        self.metric()
        self.validate()

    def test_metric_cannot_be_misattributed_to_readme(self):
        metric = self.metric()
        metric["sources"] = [self.ref("README.md", 1, 2)]
        with self.assertRaisesRegex(ValueError, "not README"):
            self.validate()

    def test_metric_cannot_drop_estimation_context(self):
        metric = self.metric()
        metric["caveat"] = ""
        with self.assertRaisesRegex(ValueError, "caveat"):
            self.validate()

    def recommendation(self, disposition="new"):
        item = {"text": "Do: design a new forge adapter.",
                "evidence": [self.claim["text"]], "query": "forge adapter",
                "disposition": disposition, "create_paths": [], "title": "Design a forge adapter",
                "issue_body": "Do: design a new forge adapter."}
        self.ledger["recommendations"] = [item]
        self.extra += f"\n{item['text']}\nWork check: forge adapter ({disposition})\n"
        return item

    def test_new_work_cannot_recreate_existing_source_seam(self):
        self.recommendation()["create_paths"] = ["src/pkg/forge"]
        with self.assertRaisesRegex(ValueError, "already exists"):
            self.validate()

    @staticmethod
    def existing_work(endpoint):
        return {"total_count": 1, "incomplete_results": False, "items": [{
            "number": 6170, "title": "Forge adapter design", "state": "closed",
            "html_url": "https://github.com/hivecommons/hive/issues/6170"}]}

    def test_closed_existing_work_blocks_new_recommendation(self):
        self.recommendation()
        with self.assertRaisesRegex(ValueError, "Existing work found"):
            self.validate(fetch=self.existing_work)

    def test_follow_up_must_cite_matching_existing_work(self):
        item = self.recommendation("deferred")
        url = "https://github.com/hivecommons/hive/issues/6170"
        item["existing"] = [url]
        self.extra += f"<{url}>"
        self.validate(fetch=self.existing_work)
        item["existing"] = ["https://github.com/hivecommons/hive/issues/123"]
        with self.assertRaisesRegex(ValueError, "matching existing"):
            self.validate(fetch=self.existing_work)

    def test_incomplete_or_failed_work_search_blocks_publication(self):
        self.recommendation()
        with self.assertRaisesRegex(ValueError, "Incomplete work search"):
            self.validate(fetch=lambda _: {"incomplete_results": True})
        with self.assertRaises(OSError):
            self.validate(fetch=lambda _: (_ for _ in ()).throw(OSError("API unavailable")))

    def test_query_cannot_exclude_closed_work_or_change_repository(self):
        for query in ("forge adapter state:open", "repo:other/repo forge", "forge"):
            with self.subTest(query=query), self.assertRaises(ValueError):
                report.search_work(query, no_work)

    def test_every_preparation_query_satisfies_the_search_contract(self):
        for query in report.BASE_QUERIES:
            with self.subTest(query=query):
                report.search_work(query, no_work)

    def test_unconfirmed_vendor_news_cannot_generate_work(self):
        text = "Cognition governance announcement is uncertain."
        self.ledger["news"] = [{
            "text": text, "url": "https://cognition.ai/unconfirmed",
            "date": "2026-10-30", "status": "uncertain", "primary_source": False}]
        self.extra += f"{text} <https://cognition.ai/unconfirmed>\n"
        item = self.recommendation()
        item["evidence"] = [text]
        with self.assertRaisesRegex(ValueError, "Unsupported/uncertain"):
            self.validate()

    def test_news_verification_requires_primary_and_current_date(self):
        text = "A release shipped on 2026-11-01."
        item = {"text": text, "url": "https://github.com/vendor/project/releases/tag/v1",
                "date": "2026-11-01", "status": "verified", "primary_source": True}
        self.ledger["news"] = [item]
        self.extra = f"{text} <{item['url']}>"
        self.validate()
        item["date"] = "2026-09-01"
        with self.assertRaisesRegex(ValueError, "outside this week"):
            self.validate()

    def test_issues_require_explicit_opt_in_and_checked_new_recommendation(self):
        item = self.recommendation()
        payload = self.payload()
        payload["items"].append({"type": "create_issue", "title": item["title"],
                                 "body": item["text"]})
        with self.assertRaisesRegex(ValueError, "explicitly enabled"):
            self.validate(payload)
        self.validate(payload, open_issues=True)
        payload["items"][-1]["body"] += " Unsupported extra rationale."
        with self.assertRaisesRegex(ValueError, "exact grounded excerpt"):
            self.validate(payload, open_issues=True)

    def test_empty_or_mismatched_delivery_cannot_succeed(self):
        with self.assertRaisesRegex(ValueError, "empty output"):
            self.validate({"items": []})
        payload = self.payload()
        payload["items"][1]["message"] += " Unsupported extra claim."
        with self.assertRaisesRegex(ValueError, "reports differ"):
            self.validate(payload)

    def test_preparation_reads_git_revision_not_default_branch_tip(self):
        bundle = self.bundle
        paths = set(bundle["files"]) | set(report.SOURCE_PATHS)

        def fake_git(*args):
            if args[0] == "rev-parse":
                return bundle["sha"]
            if args[0] == "ls-tree":
                return "\n".join(sorted(paths))
            self.assertTrue(args[1].startswith(bundle["sha"] + ":"))
            return bundle["files"].get(args[1].split(":", 1)[1], "# fixture")

        def fetch(endpoint):
            if endpoint == "repos/hivecommons/hive":
                return {"full_name": "hivecommons/hive", "default_branch": "v5"}
            if endpoint.startswith("search/"):
                return no_work(endpoint)
            return {"number": 6170, "state": "closed"}

        with patch.object(report, "git", side_effect=fake_git), patch.dict(
                "os.environ", {"GITHUB_SHA": bundle["sha"],
                               "GITHUB_REPOSITORY": "hivecommons/hive"}):
            prepared = report.prepare(fetch)
            self.assertEqual(prepared["sha"], bundle["sha"])
            self.assertEqual(prepared["default_branch"], "v5")
            with patch.dict("os.environ", {"GITHUB_SHA": "a" * 40}):
                with self.assertRaisesRegex(ValueError, "does not match"):
                    report.prepare(fetch)


class WorkflowContractTests(unittest.TestCase):
    def test_source_prompt_has_no_frozen_capability_assertions(self):
        source = (ROOT / ".github/workflows/weekly-competitor-report.md").read_text()
        self.assertNotIn("Hive has today:", source)
        self.assertNotIn("Forge:\n  GitHub today", source)
        self.assertNotIn("currently about $2.13", source)
        self.assertIn("report-source.md", source)
        self.assertIn("src/crux/config/agent_runtime.go", source)

    def test_generated_workflow_blocks_both_sinks_before_publication(self):
        import yaml
        workflow = yaml.safe_load(
            (ROOT / ".github/workflows/weekly-competitor-report.lock.yml").read_text())
        jobs = workflow["jobs"]
        steps = jobs["safe_outputs"]["steps"]
        gate = next(i for i, step in enumerate(steps) if
                    step.get("name") == "Validate source grounding before any report publication")
        publish = next(i for i, step in enumerate(steps) if
                       step.get("id") == "process_safe_outputs")
        self.assertLess(gate, publish)
        self.assertNotIn("continue-on-error", steps[gate])
        self.assertIn("steps.setup-agent-output-env.outputs.GH_AW_AGENT_OUTPUT",
                      steps[gate]["env"]["GH_AW_AGENT_OUTPUT"])
        self.assertIn("needs.safe_outputs.result == 'success'", jobs["post_to_discord"]["if"])
        self.assertIn("safe_outputs", jobs["post_to_discord"]["needs"])
        self.assertEqual(jobs["safe_outputs"]["permissions"]["contents"], "read")
        self.assertNotIn("contents: write", str(jobs["safe_outputs"]["permissions"]))
        checkout = steps[gate - 1]
        self.assertEqual(checkout["with"]["ref"], "${{ github.sha }}")
        self.assertFalse(checkout["with"]["persist-credentials"])


if __name__ == "__main__":
    unittest.main()
