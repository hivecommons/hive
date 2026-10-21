#!/usr/bin/env python3
"""Exercise the deployed prune shell against disposable cache fixtures (#13425)."""

import os
from pathlib import Path
import subprocess
import tempfile
import textwrap
import time
import unittest


MANIFEST = Path(__file__).resolve().parents[2] / (
    "src/deploy/ci-runners/lke/cache-prune-daemonset.yaml"
)


class RunnerCachePruneTest(unittest.TestCase):
    def test_old_archive_files_survive_with_new_complete_marker(self):
        manifest = MANIFEST.read_text()
        # Extract the actual shell block without requiring a YAML dependency.
        shell = textwrap.dedent(
            manifest.split("            - |\n", 1)[1].split(
                "          resources:\n", 1
            )[0]
        )
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            old = time.time() - 60 * 86400

            def fixture(relative, aged=False):
                path = root / relative
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text("fixture\n")
                if aged:
                    os.utime(path, (old, old))
                return path

            # Archive members retain release mtimes, whereas setup-* writes
            # the completion marker today. The former pruner erased binaries.
            files = []
            for tool, version, bins in (
                ("go", "1.26.9", ("go", "gofmt")),
                ("node", "22.0.0", ("node", "npm", "npx")),
            ):
                base = f"toolcache/hostedtoolcache/{tool}/{version}/x64"
                files.append(fixture(base + ".complete"))
                for binary in bins:
                    path = fixture(f"{base}/bin/{binary}", aged=True)
                    path.chmod(0o755)
                    files.append(path)
            files.append(fixture("toolcache/hostedtoolcache/.warm.lock", aged=True))
            stale_module = fixture("gomod/cache/download/old.zip", aged=True)
            fresh_module = fixture("gomod/cache/download/new.zip")
            # Redirect both historical mounts so the parent implementation
            # fails safely inside this fixture, never against host caches.
            shell = shell.replace("/mnt/gomod", str(root / "gomod"))
            shell = shell.replace("/mnt/toolcache", str(root / "toolcache"))
            # Runner pods mount the live shared tool cache; refuse to execute
            # any manifest path literal the substitution above did not cover.
            self.assertNotIn("/mnt/", shell)
            self.assertNotIn("/var/lib/hive-ci", shell)
            result = subprocess.run(
                ["sh", "-c", "sleep() { exit 0; };\n" + shell],
                env={**os.environ, "MAX_AGE_DAYS": "14"},
                capture_output=True, text=True, timeout=10,
            )
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            self.assertFalse(stale_module.exists())
            self.assertTrue(fresh_module.exists())
            for path in files:
                self.assertTrue(path.exists(), f"pruned live tool-cache file: {path}")

    def test_pruner_cannot_mount_live_toolcache(self):
        self.assertNotIn("mountPath: /mnt/toolcache", MANIFEST.read_text())


if __name__ == "__main__":
    unittest.main()
