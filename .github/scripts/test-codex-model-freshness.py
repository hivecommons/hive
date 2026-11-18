#!/usr/bin/env python3
"""Hermetic tests for codex-model-freshness.py (#19036).

No network and no real Codex: the pinned CLI is a fake script that answers
--version and the `app-server` initialize + model/list exchange.

    python3 .github/scripts/test-codex-model-freshness.py
"""

import importlib.util
import json
import os
import re
import shutil
import stat
import subprocess
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
SCRIPT = os.path.join(HERE, "codex-model-freshness.py")

spec = importlib.util.spec_from_file_location("codexfresh", SCRIPT)
fresh = importlib.util.module_from_spec(spec)
spec.loader.exec_module(fresh)

GO = '''package dashboard

// codexStaticModels is the fallback. This snapshot
// matches codex 0.166.0 (captured 2026-11-07 from a live exchange.
var codexStaticModels = []string{
	"gpt-old-sol",
	// Hidden in the codex 0.166.0 picker but still valid --model values.
	"codex-auto-review",
}
'''

JS = '''    // this is only the pre-discovery fallback. Snapshot of codex 0.166.0 (captured 2026-11-07
    // from a live exchange.
    const CODEX_CLI_MODELS = [
      { value: 'gpt-old-sol', label: 'GPT-Old Sol' },
      { value: 'codex-auto-review', label: 'Codex Auto Review' },
    ];
'''

FAKE_CODEX = """#!/usr/bin/env python3
import json, sys
if sys.argv[1:] == ["--version"]:
    print("codex-cli " + {version!r})
    sys.exit(0)
assert sys.argv[1:] == ["app-server"]
for line in sys.stdin:
    msg = json.loads(line)
    if msg["method"] == "initialize":
        print(json.dumps({{"id": msg["id"], "result": {{}}}}), flush=True)
    elif msg["method"] == "model/list":
        assert msg["params"] == {{"includeHidden": True}}
        print("not json noise", flush=True)
        print(json.dumps({{"id": msg["id"], "result": {{"data": {entries!r}}}}}), flush=True)
        break
"""


def entry(mid, hidden=False, default=False, name=None):
    return {"id": mid, "model": mid, "hidden": hidden, "isDefault": default, "displayName": name or mid}


CATALOG = [
    entry("gpt-new-astra", name="GPT-New Astra"),
    entry("gpt-new-sol", default=True, name="GPT-New Sol"),
    entry("gpt-old-sol", name="GPT-Old-Sol"),
    entry("codex-auto-review", hidden=True),
]


def read(path):
    with open(path, encoding="utf-8") as fh:
        return fh.read()


class Base(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp(prefix="codex-freshness-test-")
        self.root = os.path.join(self.tmp, "repo")
        for rel, text in ((fresh.CLI_MODELS_GO, GO), (fresh.BACKENDS_JS, JS), (fresh.INDEX_HTML, JS),
                          (fresh.DOCKERFILE, "FROM x\nARG CODEX_VERSION=0.170.0\n")):
            path = os.path.join(self.root, rel)
            os.makedirs(os.path.dirname(path), exist_ok=True)
            with open(path, "w", encoding="utf-8") as fh:
                fh.write(text)

    def tearDown(self):
        shutil.rmtree(self.tmp, ignore_errors=True)

    def fake_codex(self, entries=CATALOG, version="0.170.0"):
        path = os.path.join(self.tmp, "codex")
        with open(path, "w", encoding="utf-8") as fh:
            fh.write(FAKE_CODEX.format(version=version, entries=entries))
        os.chmod(path, os.stat(path).st_mode | stat.S_IXUSR)
        return path

    def snapshot(self):
        return {rel: read(os.path.join(self.root, rel))
                for rel in (fresh.CLI_MODELS_GO, fresh.BACKENDS_JS, fresh.INDEX_HTML)}

    def run_cli(self, codex, *extra):
        return subprocess.run([sys.executable, "-I", SCRIPT, "--root", self.root, "--codex-bin", codex, *extra],
                              stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, timeout=120)


class Ordering(unittest.TestCase):
    def test_default_then_visible_then_hidden(self):
        ids, hidden = fresh.order_served(CATALOG)
        self.assertEqual(ids, ["gpt-new-sol", "gpt-new-astra", "gpt-old-sol", "codex-auto-review"])
        self.assertEqual(hidden, {"codex-auto-review"})

    def test_entry_with_only_model_slug(self):
        ids, _ = fresh.order_served([{"model": "gpt-x"}, {"id": ""}])
        self.assertEqual(ids, ["gpt-x"])


class Apply(Base):
    def test_rewrites_all_three_files_and_stamps_the_pin(self):
        proc = self.run_cli(self.fake_codex())
        self.assertEqual(proc.returncode, 0, proc.stdout)
        got = self.snapshot()
        want_ids = ["gpt-new-sol", "gpt-new-astra", "gpt-old-sol", "codex-auto-review"]
        self.assertEqual(fresh.go_ids(got[fresh.CLI_MODELS_GO]), want_ids)
        for rel in (fresh.BACKENDS_JS, fresh.INDEX_HTML):
            self.assertEqual(re.findall(r"value: '([^']+)'", got[rel]), want_ids)
            self.assertIn("Snapshot of codex 0.170.0 (captured ", got[rel])
        self.assertIn("matches codex 0.170.0 (captured ", got[fresh.CLI_MODELS_GO])
        self.assertIn("Hidden in the codex 0.170.0 picker", got[fresh.CLI_MODELS_GO])
        # Existing labels are kept; new ids take the CLI's display name.
        self.assertIn("{ value: 'gpt-old-sol', label: 'GPT-Old Sol' }", got[fresh.BACKENDS_JS])
        self.assertIn("{ value: 'gpt-new-sol', label: 'GPT-New Sol' }", got[fresh.BACKENDS_JS])
        self.assertIn("gpt-new-sol", proc.stdout)

    def test_reapply_is_a_no_op(self):
        codex = self.fake_codex()
        self.run_cli(codex)
        first = self.snapshot()
        proc = self.run_cli(codex)
        self.assertEqual(proc.returncode, 0, proc.stdout)
        self.assertEqual(self.snapshot(), first)
        self.assertIn("nothing to change", proc.stdout)

    def test_dry_run_writes_nothing(self):
        before = self.snapshot()
        proc = self.run_cli(self.fake_codex(), "--dry-run")
        self.assertEqual(proc.returncode, 0, proc.stdout)
        self.assertEqual(self.snapshot(), before)
        self.assertIn("would change", proc.stdout)


class Gate(Base):
    def assert_refused(self, codex, text):
        before = self.snapshot()
        proc = self.run_cli(codex)
        self.assertNotEqual(proc.returncode, 0, proc.stdout)
        self.assertIn(text, proc.stdout)
        self.assertEqual(self.snapshot(), before)

    def test_version_not_the_pin(self):
        self.assert_refused(self.fake_codex(version="0.169.0"), "pins 0.170.0")

    def test_empty_catalogue(self):
        self.assert_refused(self.fake_codex(entries=[]), "no models")

    def test_duplicate_ids(self):
        self.assert_refused(self.fake_codex(entries=[entry("a"), entry("a")]), "duplicate")

    def test_only_hidden(self):
        self.assert_refused(self.fake_codex(entries=[entry("a", hidden=True)]), "only hidden")

    def test_missing_binary(self):
        self.assert_refused(os.path.join(self.tmp, "no-such-codex"), "probe failed")


if __name__ == "__main__":
    unittest.main()
