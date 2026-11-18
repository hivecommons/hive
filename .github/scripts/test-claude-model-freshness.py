#!/usr/bin/env python3
"""Hermetic tests for check-static-model-freshness.py --apply claude (#18754).

No network and no real Claude Code: the catalog is a fixture and the pinned
CLI is a fake script that answers --version and the unauthenticated probe.

    python3 .github/scripts/test-claude-model-freshness.py
"""

import importlib.util
import os
import re
import shutil
import stat
import subprocess
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
SCRIPT = os.path.join(HERE, "check-static-model-freshness.py")
FIXTURES = os.path.join(HERE, "testdata", "claude-model-freshness")

spec = importlib.util.spec_from_file_location("freshness", SCRIPT)
freshness = importlib.util.module_from_spec(spec)
spec.loader.exec_module(freshness)

LAYOUT = {
    "cli_models.go.txt": freshness.CLI_MODELS_GO,
    "backends-tokens.js.txt": freshness.BACKENDS_JS,
    "cli-model-requirements.txt": freshness.REQUIREMENTS,
    "Dockerfile.txt": freshness.DOCKERFILE,
}

FAKE_CLAUDE = """#!/usr/bin/env python3
import sys
args = sys.argv[1:]
if args == ["--version"]:
    print({version!r} + " (Claude Code)")
    sys.exit(0)
model = args[args.index("--model") + 1]
if {mark!r} and (model in {unknown!r} or model.startswith("claude-hive-probe")):
    print('[claude-code:unrecognized_model] {{"model":"' + model + '","query_source":"sdk"}}')
print("Not logged in \\u00b7 Please run /login")
sys.exit(1)
"""


def read(path):
    with open(path, encoding="utf-8") as fh:
        return fh.read()


def load_catalog(name):
    import json
    with open(os.path.join(FIXTURES, name), encoding="utf-8") as fh:
        return json.load(fh)["data"]


def go_list(src, name):
    return freshness.go_string_list(src, name)


def js_values(src):
    m = re.search(r"const CLAUDE_CLI_MODELS = \[\n(.*?)\n\s*\];", src, re.S)
    return re.findall(r"value: '([^']+)'", m.group(1))


class Base(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp(prefix="claude-freshness-test-")
        self.root = os.path.join(self.tmp, "repo")
        for fixture, rel in LAYOUT.items():
            dst = os.path.join(self.root, rel)
            os.makedirs(os.path.dirname(dst), exist_ok=True)
            shutil.copyfile(os.path.join(FIXTURES, fixture), dst)

    def tearDown(self):
        shutil.rmtree(self.tmp, ignore_errors=True)

    def fake_claude(self, unknown=(), version="2.1.326", mark=True):
        path = os.path.join(self.tmp, "claude")
        with open(path, "w", encoding="utf-8") as fh:
            fh.write(FAKE_CLAUDE.format(version=version, unknown=list(unknown), mark=mark))
        os.chmod(path, os.stat(path).st_mode | stat.S_IXUSR)
        return path

    def snapshot(self):
        return {rel: read(os.path.join(self.root, rel)) for rel in LAYOUT.values()}

    def run_cli(self, *extra, catalog="openrouter.json"):
        accepted = os.path.join(self.tmp, "accepted.txt")
        proc = subprocess.run([sys.executable, "-I", SCRIPT, "--apply", "claude", "--root", self.root,
                               "--catalog", os.path.join(FIXTURES, catalog), "--accepted-out", accepted, *extra],
                              stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, timeout=120)
        self.assertEqual(proc.returncode, 0, proc.stdout)
        return proc.stdout, read(accepted).split()


class Candidates(Base):
    def test_same_line_newer_only_dashed_and_labelled(self):
        src = read(os.path.join(self.root, freshness.CLI_MODELS_GO))
        got = freshness.claude_candidates(src, load_catalog("openrouter.json"))
        self.assertEqual([(c["cli_id"], c["source_id"], c["label"]) for c in got], [
            ("claude-fable-5-1", "anthropic/claude-fable-5.1", "Fable 5.1"),
            ("claude-fable-5-2", "anthropic/claude-fable-5.2", "Fable 5.2"),
            ("claude-haiku-5-5", "anthropic/claude-haiku-5.5", "Haiku 5.5"),
        ])

    def test_excluded(self):
        src = read(os.path.join(self.root, freshness.CLI_MODELS_GO))
        ids = {c["cli_id"] for c in freshness.claude_candidates(src, load_catalog("openrouter.json"))}
        for never in ("claude-mythos-5", "claude-mythos-5-5-preview",  # limited access
                      "claude-sonnet-6",                                # expiration_date set
                      "claude-opus-4-1",                                # older than the list
                      "claude-opus-5-5",                                # already listed
                      "claude-opus-6-fast", "claude-3-7-sonnet"):       # not a plain line id
            self.assertNotIn(never, ids)

    def test_dotted_to_dashed(self):
        self.assertEqual(freshness.norm("anthropic/claude-fable-5.2"), "claude-fable-5-2")
        self.assertEqual(freshness.claude_line("claude-fable-5-2"), ("fable", (5, 2)))
        self.assertIsNone(freshness.claude_line("claude-mythos-5"))
        self.assertEqual(freshness.claude_label("haiku", (5, 5)), "Haiku 5.5")


class Apply(Base):
    def test_dry_run_prints_every_edit_and_writes_nothing(self):
        before = self.snapshot()
        out, accepted = self.run_cli("--dry-run", "--claude-bin", self.fake_claude(unknown=["claude-fable-5-2"]))
        self.assertEqual(accepted, ["claude-fable-5-1", "claude-haiku-5-5"])
        self.assertEqual(self.snapshot(), before, "a dry run must not write")
        self.assertIn("**Dry run:** nothing was written.", out)
        for cid, label in (("claude-fable-5-1", "Fable 5.1"), ("claude-haiku-5-5", "Haiku 5.5")):
            section = out.split(f"#### {label} (`{cid}`")[1].split("####")[0].split("###")[0]
            for want in (f"`claudePinnedCLIModels` gains `\"{cid}\"`",
                         f"`claudeStaticModels` gains `\"{cid}\"`",
                         f"`CLAUDE_CLI_MODELS` gains `{{ value: '{cid}', label: '{label}' }},`",
                         "`src/pkg/dashboard/static/index.html`: rebuilt with `make dashboard-gen`",
                         f"`claude {cid} 2.1.326`",
                         "`modelPrices` row from the OpenRouter quote, through `pricetable --add`"):
                self.assertIn(want, section, f"{cid}: missing edit {want!r}\n{out}")
        needs = out.split("### Needs a check against the Claude Code changelog (1)")[1]
        self.assertIn("Fable 5.2 (`claude-fable-5-2`, from `anthropic/claude-fable-5.2`): the pinned Claude Code does not recognise this id", needs)
        self.assertNotIn("#### Fable 5.2", out, "an id the pinned CLI does not recognise is never added")

    def test_apply_edits_in_sync_and_idempotent(self):
        claude = self.fake_claude(unknown=["claude-fable-5-2"])
        _, accepted = self.run_cli("--claude-bin", claude)
        self.assertEqual(accepted, ["claude-fable-5-1", "claude-haiku-5-5"])
        go = read(os.path.join(self.root, freshness.CLI_MODELS_GO))
        js = read(os.path.join(self.root, freshness.BACKENDS_JS))
        reqs = read(os.path.join(self.root, freshness.REQUIREMENTS))
        want = ["claude-opus-5-5", "claude-sonnet-5-5", "claude-fable-5-1", "claude-fable-5",
                "claude-haiku-5-5", "claude-haiku-4-5-20251001", "opus", "sonnet", "haiku"]
        self.assertEqual(go_list(go, "claudeStaticModels"), want)
        self.assertEqual(go_list(go, "claudePinnedCLIModels"), want)
        self.assertEqual(js_values(js), want, "JS list must stay an exact copy of claudeStaticModels")
        self.assertEqual(go_list(go, "geminiStaticModels"), ["gemini-2.5-pro"], "other lists untouched")
        self.assertIn("      { value: 'claude-fable-5-1', label: 'Fable 5.1' },\n", js)
        self.assertIn('\t"claude-haiku-5-5",\n', go)
        self.assertTrue(reqs.endswith("claude claude-fable-5-1 2.1.326\nclaude claude-haiku-5-5 2.1.326\n"), reqs)

        after = self.snapshot()
        out, again = self.run_cli("--claude-bin", claude)
        self.assertEqual(again, [])
        self.assertEqual(self.snapshot(), after, "a second run with the same candidates changes nothing")
        self.assertIn("### Added (0)", out)

    def test_insert_before_aliases_when_line_is_new_to_the_list(self):
        lines = ['\t"claude-opus-5-5",', "\t// aliases", '\t"opus",']
        got = freshness.insert_listed(lines, "claude-haiku-5-5", lambda ind: f'{ind}"claude-haiku-5-5",', freshness.GO_ID)
        self.assertEqual(got, ['\t"claude-opus-5-5",', '\t"claude-haiku-5-5",', "\t// aliases", '\t"opus",'])
        self.assertIsNone(freshness.insert_listed(got, "claude-haiku-5-5", lambda ind: "", freshness.GO_ID))

    def test_unreliable_probe_adds_nothing(self):
        before = self.snapshot()
        out, accepted = self.run_cli("--claude-bin", self.fake_claude(mark=False))
        self.assertEqual(accepted, [])
        self.assertEqual(self.snapshot(), before)
        self.assertIn("probe controls failed", out)
        self.assertIn("### Needs a check against the Claude Code changelog (3)", out)

    def test_wrong_version_adds_nothing(self):
        before = self.snapshot()
        out, accepted = self.run_cli("--claude-bin", self.fake_claude(version="2.1.311"))
        self.assertEqual(accepted, [])
        self.assertEqual(self.snapshot(), before)
        self.assertIn("not the pinned `2.1.326`", out)

    def test_no_binary_adds_nothing(self):
        before = self.snapshot()
        out, accepted = self.run_cli()
        self.assertEqual(accepted, [])
        self.assertEqual(self.snapshot(), before)
        self.assertIn("no pinned Claude Code binary", out)

    def test_no_candidates_no_edits(self):
        before = self.snapshot()
        out, accepted = self.run_cli("--claude-bin", self.fake_claude(), catalog="openrouter-current.json")
        self.assertEqual(accepted, [])
        self.assertEqual(self.snapshot(), before)
        self.assertIn("Gate: no candidates.", out)
        self.assertNotIn("Needs a check", out)


class Report(unittest.TestCase):
    def test_compare_list_shared_with_report(self):
        by_vendor = freshness.index_catalog(load_catalog("openrouter.json"))
        not_found, newer = freshness.compare_list(["claude-fable-5", "claude-opus-9"], "anthropic", by_vendor)
        self.assertEqual(not_found, ["claude-opus-9"])
        self.assertIn(("anthropic", "claude-fable-5-2"), newer)
        self.assertIn(("anthropic", "claude-fable-5-1"), newer)


if __name__ == "__main__":
    unittest.main(verbosity=2)
