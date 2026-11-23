#!/usr/bin/env python3
"""Self-test for the reusable Contributing drift gate (#19487).

The checker lives inline in .github/workflows/contributing-drift.yml, so a
caller's pinned SHA pins the checker too. This test pulls it out from between
its BEGIN/END markers, the same text the workflow's heredoc feeds to python3,
and runs it against throwaway fixture repos. Stdlib only, offline.
"""
import importlib.util
import os
import pathlib
import subprocess
import sys
import tempfile
import textwrap
import unittest

ROOT = pathlib.Path(__file__).resolve().parents[2]
WORKFLOW = ROOT / ".github/workflows/contributing-drift.yml"
BEGIN = "# BEGIN contributing-drift checker"
END = "# END contributing-drift checker"


def checker_source():
    lines = WORKFLOW.read_text(encoding="utf-8").split("\n")
    start = next(i for i, l in enumerate(lines) if l.strip() == BEGIN)
    end = next(i for i, l in enumerate(lines) if l.strip() == END)
    indent = len(lines[start]) - len(lines[start].lstrip())
    body = lines[start : end + 1]
    for l in body:
        assert not l.strip() or l[:indent].strip() == "", f"checker line under-indented: {l!r}"
    return "\n".join(l[indent:] for l in body) + "\n"


SRC = checker_source()
_spec = importlib.util.spec_from_loader("contributing_drift", loader=None)
drift = importlib.util.module_from_spec(_spec)
exec(compile(SRC, str(WORKFLOW), "exec"), drift.__dict__)


class Repo:
    def __init__(self, tmp):
        self.root = pathlib.Path(tmp)

    def write(self, rel, text):
        p = self.root / rel
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_text(textwrap.dedent(text).lstrip("\n"), encoding="utf-8")

    def check(self, exemptions=".github/contributing-drift-exemptions.txt"):
        findings, errors = drift.check(str(self.root), exemptions)
        return [f[2] for f in findings], errors


BENCHMARKS_PAGES = """
name: pages
on: [push, pull_request]
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-python@v5
        with:
          python-version: '3.12'
      - name: Install
        run: pip install -r requirements.txt
      - name: Unit tests
        run: python3 -m unittest discover -s scripts -p 'test_*.py'
      - name: Build
        run: |
          mkdir -p site
          cp -r static site/
          scripts/build-index.sh
      - name: Guard
        run: scripts/check-no-temp-artifacts.sh
      - uses: actions/upload-pages-artifact@v3
        with:
          path: site
"""


class ContributingDriftTest(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.repo = Repo(self._tmp.name)

    def tearDown(self):
        self._tmp.cleanup()

    def test_benchmarks_gap_is_reported(self):
        # The motivating case from #19487: CONTRIBUTING documents only the build.
        self.repo.write(".github/workflows/pages.yml", BENCHMARKS_PAGES)
        self.repo.write("CONTRIBUTING.md", "Run `scripts/build-index.sh` before you open a PR.\n")
        found, errors = self.repo.check()
        self.assertEqual(errors, [])
        self.assertEqual(found, ["python -m unittest", "scripts/check-no-temp-artifacts.sh"])

    def test_documented_commands_pass(self):
        self.repo.write(".github/workflows/pages.yml", BENCHMARKS_PAGES)
        self.repo.write(
            "CONTRIBUTING.md",
            """
            Run `./scripts/build-index.sh`, then
            `python -m unittest discover -s scripts -p 'test_*.py'`.
            """,
        )
        self.repo.write("README.md", "CI also runs scripts/check-no-temp-artifacts.sh.\n")
        self.assertEqual(self.repo.check(), ([], []))

    def test_setup_and_plumbing_are_not_candidates(self):
        self.repo.write(
            ".github/workflows/ci.yml",
            """
            jobs:
              t:
                defaults:
                  run:
                    shell: bash
                steps:
                  - run: |
                      sudo apt-get update && sudo apt-get install -y jq
                      npm ci
                      pip install -e .
                      python -m pip install --upgrade pip
                      mkdir -p out; cp a b
                      echo "done" | tee out/log
                      git fetch --depth=1 origin main
                      curl -sSf https://example.com/x -o /tmp/x.sh
                      bash /tmp/x.sh
                      cat <<'EOF' > out/cfg.sh
                      scripts/inside-heredoc.sh
                      EOF
                      go install golang.org/x/tools/cmd/goimports@latest
                      python3 -c 'print(1)'
                      "$RUNNER_TEMP/tool.sh"
                      command -v npm >/dev/null 2>&1 || exit 0
                      bash -n scripts/syntax-only.sh
            """,
        )
        self.assertEqual(self.repo.check(), ([], []))

    def test_continue_on_error_steps_are_skipped(self):
        self.repo.write(
            ".github/workflows/ci.yml",
            """
            jobs:
              t:
                steps:
                  - name: advisory
                    continue-on-error: true
                    run: scripts/advisory.sh
                  - run: scripts/also-advisory.sh
                    continue-on-error: true
                  - name: gate
                    run: scripts/gate.sh
                    env:
                      continue-on-error: true
            """,
        )
        # The env var named continue-on-error is not the step key.
        self.assertEqual(self.repo.check()[0], ["scripts/gate.sh"])

    def test_runner_forms(self):
        self.repo.write(
            ".github/workflows/ci.yml",
            """
            jobs:
              t:
                steps:
                  - run: npm test
                  - run: npm run lint -- --max-warnings 0
                  - run: pnpm run typecheck
                  - run: make -C src test
                  - run: just fmt-check
                  - run: go test -race ./...
                  - run: cargo clippy -- -D warnings
                  - run: npx vitest run --coverage
                  - run: uv run pytest -q
                  - run: FOO=1 timeout 300 node tools/smoke.mjs
                  - run: "python3 ./ci/check.py --strict"
            """,
        )
        self.repo.write(
            "CONTRIBUTING.md",
            "Before a PR: `yarn test`, `npm run lint`, `make test`.\n",
        )
        found, _ = self.repo.check()
        self.assertEqual(
            found,
            [
                "pnpm run typecheck",
                "just fmt-check",
                "go test",
                "cargo clippy",
                "vitest",
                "pytest",
                "tools/smoke.mjs",
                "ci/check.py",
            ],
        )

    def test_exemptions_need_a_reason(self):
        self.repo.write(
            ".github/workflows/ci.yml",
            """
            jobs:
              t:
                steps:
                  - run: scripts/publish.sh
                  - run: scripts/undocumented.sh
            """,
        )
        self.repo.write(
            ".github/workflows/bench.yml",
            """
            jobs:
              b:
                steps:
                  - run: scripts/bench.sh
            """,
        )
        self.repo.write(
            ".github/contributing-drift-exemptions.txt",
            """
            # comment lines and blanks are ignored

            scripts/publish.sh        # release-only, run by the release workflow
            workflow:bench.yml        # scheduled benchmark, not a PR check
            scripts/undocumented.sh
            """,
        )
        found, errors = self.repo.check()
        self.assertEqual(found, ["scripts/undocumented.sh"])
        self.assertEqual(len(errors), 1)
        self.assertIn("needs a reason", errors[0])

    def test_custom_exemptions_path_and_missing_file(self):
        self.repo.write(".github/workflows/ci.yml", "jobs:\n  t:\n    steps:\n      - run: ./ci.sh\n")
        self.assertEqual(self.repo.check("nope.txt")[0], ["ci.sh"])
        self.repo.write("ci/exempt.txt", "ci.sh  # wrapper for the documented make target\n")
        self.assertEqual(self.repo.check("ci/exempt.txt"), ([], []))

    def test_no_workflows_passes(self):
        self.assertEqual(self.repo.check(), ([], []))

    def test_workflow_heredoc_runs_the_checker(self):
        # End to end through the exact `python3 - ... <<'CONTRIBUTING_DRIFT_PY'`
        # shape the workflow step uses, with GitHub annotations on.
        self.repo.write(".github/workflows/pages.yml", BENCHMARKS_PAGES)
        self.repo.write("CONTRIBUTING.md", "scripts/build-index.sh\n")
        env = dict(os.environ, GITHUB_ACTIONS="true")
        r = subprocess.run(
            [sys.executable, "-", "--root", str(self.repo.root)],
            input=SRC, text=True, capture_output=True, env=env,
        )
        self.assertEqual(r.returncode, 1, r.stderr)
        self.assertIn("::error file=.github/workflows/pages.yml,line=", r.stdout)
        self.assertIn("scripts/check-no-temp-artifacts.sh", r.stdout)
        self.repo.write("CONTRIBUTING.md", "scripts/build-index.sh scripts/check-no-temp-artifacts.sh python3 -m unittest\n")
        r = subprocess.run([sys.executable, "-", "--root", str(self.repo.root)], input=SRC, text=True, capture_output=True)
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)

    def test_workflow_contract(self):
        text = WORKFLOW.read_text(encoding="utf-8")
        self.assertIn("workflow_call:", text)
        self.assertIn("python3 - --root . --exemptions \"$EXEMPTIONS\" <<'CONTRIBUTING_DRIFT_PY'", text)
        self.assertNotIn("import yaml", SRC)  # offline: no third-party parser


if __name__ == "__main__":
    unittest.main()
