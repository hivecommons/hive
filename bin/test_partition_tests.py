#!/usr/bin/env python3
import os
import sys
import json
import tempfile
import subprocess
import unittest

from importlib.machinery import SourceFileLoader

# Import partition-tests.py
partition_module = SourceFileLoader(
    "partition_tests",
    os.path.join(os.path.dirname(__file__), "partition-tests.py")
).load_module()

is_hub_package = partition_module.is_hub_package
get_package_weight = partition_module.get_package_weight
partition_packages = partition_packages = partition_module.partition_packages


class TestPartitionTests(unittest.TestCase):
    def setUp(self):
        self.sample_packages = [
            "github.com/kubestellar/hive/pkg/hub",
            "github.com/kubestellar/hive/pkg/hub/sub1",
            "github.com/kubestellar/hive/pkg/agent",
            "github.com/kubestellar/hive/pkg/dashboard",
            "github.com/kubestellar/hive/pkg/proxy",
            "github.com/kubestellar/hive/pkg/github",
            "github.com/kubestellar/hive/pkg/hubbackup",
            "github.com/kubestellar/hive/pkg/discord",
            "github.com/kubestellar/hive/cmd/hive",
            "github.com/kubestellar/hive/pkg/config",
            "github.com/kubestellar/hive/pkg/channels",
            "github.com/kubestellar/hive/pkg/snapshot",
            "github.com/kubestellar/hive/pkg/mint",
            "github.com/kubestellar/hive/pkg/auth",
            "github.com/kubestellar/hive/pkg/tokens",
            "github.com/kubestellar/hive/pkg/resolve",
            "github.com/kubestellar/hive/pkg/policies",
            "github.com/kubestellar/hive/pkg/skillreg",
            "github.com/kubestellar/hive/pkg/beads",
            "github.com/kubestellar/hive/pkg/knowledge",
            "github.com/kubestellar/hive/pkg/intent",
            "github.com/kubestellar/hive/pkg/sandbox",
            "github.com/kubestellar/hive/pkg/custom_new_pkg",
        ]

    def test_hub_package_detection(self):
        self.assertTrue(is_hub_package("github.com/kubestellar/hive/pkg/hub"))
        self.assertTrue(is_hub_package("github.com/kubestellar/hive/pkg/hub/sub"))
        self.assertTrue(is_hub_package("./pkg/hub"))
        self.assertTrue(is_hub_package("./pkg/hub/sub"))
        self.assertFalse(is_hub_package("github.com/kubestellar/hive/pkg/hubbackup"))
        self.assertFalse(is_hub_package("github.com/kubestellar/hive/pkg/agent"))

    def test_package_weights(self):
        self.assertEqual(get_package_weight("github.com/kubestellar/hive/pkg/agent"), 80.0)
        self.assertEqual(get_package_weight("./pkg/agent"), 80.0)
        self.assertEqual(get_package_weight("github.com/kubestellar/hive/pkg/dashboard"), 55.0)
        self.assertEqual(get_package_weight("github.com/kubestellar/hive/pkg/unknown_pkg"), 1.0)

    def test_partition_exhaustiveness_and_uniqueness(self):
        result = partition_packages(self.sample_packages, num_shards=3, include_hub=True)
        includes = result["include"]

        # Hub shard must be present
        hub_entry = [e for e in includes if e["shard"] == "hub"]
        self.assertEqual(len(hub_entry), 1)
        self.assertEqual(hub_entry[0]["pkgs"], "./pkg/hub/...")

        # Rest shards
        rest_entries = [e for e in includes if e["shard"].startswith("rest-")]
        self.assertEqual(len(rest_entries), 3)

        # Collect all packages in rest shards
        all_partitioned_pkgs = []
        for entry in rest_entries:
            pkgs = entry["pkgs"].split()
            all_partitioned_pkgs.extend(pkgs)

        # Non-hub packages from sample
        expected_rest = [p for p in self.sample_packages if not is_hub_package(p)]

        self.assertEqual(sorted(all_partitioned_pkgs), sorted(expected_rest))
        self.assertEqual(len(all_partitioned_pkgs), len(set(all_partitioned_pkgs)), "No duplicates across shards")

    def test_partition_determinism(self):
        res1 = partition_packages(self.sample_packages, num_shards=3)
        res2 = partition_packages(list(reversed(self.sample_packages)), num_shards=3)
        self.assertEqual(res1, res2)

    def test_heavy_package_balancing(self):
        result = partition_packages(self.sample_packages, num_shards=3)
        rest_entries = [e for e in result["include"] if e["shard"].startswith("rest-")]

        # Top heavy packages should not all end up in the same shard
        shards_with_top_heavy = []
        for entry in rest_entries:
            pkgs = entry["pkgs"].split()
            for heavy in ["github.com/kubestellar/hive/pkg/agent", "github.com/kubestellar/hive/pkg/dashboard", "github.com/kubestellar/hive/pkg/proxy"]:
                if heavy in pkgs:
                    shards_with_top_heavy.append(entry["shard"])

        # Agent, Dashboard, Proxy should each be in different shards
        self.assertEqual(len(set(shards_with_top_heavy)), 3)

    def test_cli_execution_and_github_output(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            gh_out_file = os.path.join(tmpdir, "gh_out.txt")
            gh_sum_file = os.path.join(tmpdir, "gh_sum.txt")
            env = os.environ.copy()
            env["GITHUB_OUTPUT"] = gh_out_file
            env["GITHUB_STEP_SUMMARY"] = gh_sum_file

            cmd = [
                sys.executable,
                os.path.join(os.path.dirname(__file__), "partition-tests.py"),
                "--shards=3",
                "--github-output",
            ]
            input_pkgs = "\n".join(self.sample_packages) + "\n"
            proc = subprocess.run(cmd, input=input_pkgs, text=True, capture_output=True, env=env)
            self.assertEqual(proc.returncode, 0)

            # Check stdout JSON
            stdout_data = json.loads(proc.stdout.strip())
            self.assertIn("include", stdout_data)
            self.assertEqual(len(stdout_data["include"]), 4)

            # Check GITHUB_OUTPUT file
            with open(gh_out_file, "r") as f:
                content = f.read()
            self.assertTrue(content.startswith("matrix="))
            matrix_from_file = json.loads(content.replace("matrix=", "").strip())
            self.assertEqual(matrix_from_file, stdout_data)

            # Check GITHUB_STEP_SUMMARY file
            with open(gh_sum_file, "r") as f:
                sum_content = f.read()
            self.assertIn("Test Shard Partition", sum_content)


if __name__ == "__main__":
    unittest.main()
