#!/usr/bin/env python3
"""Exercise the shipped Cline RUN commands without npm, network, or Docker."""

import os
from pathlib import Path
import re
import subprocess
import tempfile
import unittest


SRC = Path(__file__).resolve().parents[1]
MOCKS = r'''
count=0
npm() {
    if [ "$1" = root ]; then echo /mock/node_modules; return 0; fi
    printf 'npm %s\n' "$*" >> "$CALL_LOG"
    count=$((count + 1))
    if [ "$count" -le "$FAILURES" ]; then
        echo 'npm error code ETARGET: No matching version found' >&2
        return 42
    fi
}
sleep() { echo "sleep $*" >> "$CALL_LOG"; }
ln() { echo "ln $*" >> "$CALL_LOG"; }
cline() { echo "cline $*" >> "$CALL_LOG"; return "$VERSION_STATUS"; }
'''


class ClineInstallRetryTest(unittest.TestCase):
    def run_install(self, dockerfile, failures, version_status=0):
        # Join Docker continuations, then run the actual RUN body under /bin/sh
        # just like the images. No copied retry implementation in this test.
        text = (SRC / dockerfile).read_text().replace("\\\n", " ")
        commands = [
            line.removeprefix("RUN ")
            for line in text.splitlines()
            if line.startswith("RUN ")
            and re.search(r"npm install .*cline@", line)
        ]
        self.assertEqual(len(commands), 1, dockerfile)
        with tempfile.TemporaryDirectory() as directory:
            log = Path(directory) / "calls"
            log.touch()
            result = subprocess.run(
                ["/bin/sh", "-c", MOCKS + commands[0]],
                env={**os.environ, "CALL_LOG": str(log),
                     "FAILURES": str(failures), "CLINE_VERSION": "3.0.74",
                     "VERSION_STATUS": str(version_status)},
                capture_output=True, text=True, timeout=5,
            )
            calls = log.read_text().splitlines()
        return result, calls

    def test_install_retry_and_failure(self):
        for dockerfile in ("Dockerfile", "Dockerfile.contributor"):
            # Includes success on the final allowed attempt and exhaustion.
            for failures in (0, 1, 3, 4):
                with self.subTest(dockerfile=dockerfile, failures=failures):
                    result, calls = self.run_install(dockerfile, failures)
                    self.assertEqual(result.returncode, 42 if failures == 4 else 0,
                                     result.stderr)
                    installs = [call for call in calls if call.startswith("npm ")]
                    self.assertEqual(len(installs), min(failures + 1, 4))
                    for install in installs:
                        self.assertEqual(install.split(), [
                            "npm", "install", "-g", "--ignore-scripts", "--no-fund",
                            "--no-audit", "--prefer-online", "cline@3.0.74",
                        ])
                    self.assertEqual(calls.count("sleep 60"), min(failures, 3))
                    self.assertEqual([call for call in calls if call.startswith("sleep ")],
                                     ["sleep 60"] * min(failures, 3))
                    post_install = [call for call in calls
                                    if call.startswith(("ln ", "cline "))]
                    self.assertEqual(post_install, [] if failures == 4 else [
                        "ln -sf /mock/node_modules/cline/bin/cline /usr/local/bin/cline",
                        "cline --version",
                    ])

    def test_version_failure_is_not_retried_or_masked(self):
        for dockerfile in ("Dockerfile", "Dockerfile.contributor"):
            with self.subTest(dockerfile=dockerfile):
                result, calls = self.run_install(dockerfile, 0, version_status=17)
                self.assertEqual(result.returncode, 17, result.stderr)
                self.assertEqual(sum(call.startswith("npm ") for call in calls), 1)
                self.assertFalse(any(call.startswith("sleep ") for call in calls))


if __name__ == "__main__":
    unittest.main()
