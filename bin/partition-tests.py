#!/usr/bin/env python3
"""
partition-tests.py — Partition Go test packages into balanced shards (kubestellar/hive#4118).

Computes an exhaustive, non-overlapping partition of all packages in ./pkg/... and ./cmd/...
(excluding pkg/hub, which runs in its own dedicated shard).

Known-heavy packages (measured under `go test -short -race -count=1`) carry static weight hints
and are placed greedily into the least-loaded shard; unweighted packages follow deterministically
over the sorted list.
"""

import sys
import os
import json
import argparse
import subprocess
from typing import List, Dict, Any

# Static weight hints in seconds, measured under `go test -short -race -count=1`.
# Unlisted packages default to weight 1.0.
WEIGHT_HINTS: Dict[str, float] = {
    "github.com/kubestellar/hive/pkg/agent": 80.0,
    "github.com/kubestellar/hive/pkg/dashboard": 55.0,
    "github.com/kubestellar/hive/pkg/proxy": 45.0,
    "github.com/kubestellar/hive/pkg/github": 45.0,
    "github.com/kubestellar/hive/pkg/hubbackup": 25.0,
    "github.com/kubestellar/hive/pkg/discord": 13.0,
    "github.com/kubestellar/hive/cmd/hive": 13.0,
    "github.com/kubestellar/hive/pkg/config": 7.0,
    "github.com/kubestellar/hive/pkg/channels": 5.0,
    "github.com/kubestellar/hive/pkg/snapshot": 4.0,
    "github.com/kubestellar/hive/pkg/mint": 4.0,
    "github.com/kubestellar/hive/pkg/auth": 3.0,
    "github.com/kubestellar/hive/pkg/tokens": 2.0,
    "github.com/kubestellar/hive/pkg/resolve": 2.0,
    "github.com/kubestellar/hive/pkg/policies": 2.0,
    "github.com/kubestellar/hive/pkg/skillreg": 2.0,
    "github.com/kubestellar/hive/pkg/beads": 2.0,
    "github.com/kubestellar/hive/pkg/knowledge": 2.0,
}

def is_hub_package(pkg: str) -> bool:
    """Returns True if the package is pkg/hub or a subpackage of pkg/hub."""
    pkg = pkg.strip()
    return (
        pkg == "github.com/kubestellar/hive/pkg/hub"
        or pkg.startswith("github.com/kubestellar/hive/pkg/hub/")
        or pkg == "./pkg/hub"
        or pkg.startswith("./pkg/hub/")
        or pkg == "pkg/hub"
        or pkg.startswith("pkg/hub/")
    )

def get_package_weight(pkg: str) -> float:
    """Returns the weight for a package."""
    pkg = pkg.strip()
    if pkg in WEIGHT_HINTS:
        return WEIGHT_HINTS[pkg]
    for k, v in WEIGHT_HINTS.items():
        suffix = k.replace("github.com/kubestellar/hive/", "")
        if pkg == suffix or pkg == f"./{suffix}" or pkg.endswith(f"/{suffix}"):
            return v
    return 1.0

def partition_packages(
    packages: List[str], num_shards: int = 3, include_hub: bool = True
) -> Dict[str, Any]:
    """
    Partitions packages into N balanced rest shards, plus a dedicated hub shard if include_hub is True.
    Returns GitHub Actions matrix format dict: {"include": [...]}
    """
    rest_pkgs = [p.strip() for p in packages if p.strip() and not is_hub_package(p.strip())]
    # Deduplicate and sort for deterministic input
    rest_pkgs = sorted(list(set(rest_pkgs)))

    if not rest_pkgs:
        matrix_include = []
        if include_hub:
            matrix_include.append({"shard": "hub", "pkgs": "./pkg/hub/..."})
        return {"include": matrix_include}

    # Sort packages primarily by weight descending, secondarily by package name ascending (tie-breaker)
    sorted_pkgs = sorted(rest_pkgs, key=lambda p: (-get_package_weight(p), p))

    # Initialize N shards
    shard_pkgs: List[List[str]] = [[] for _ in range(num_shards)]
    shard_weights: List[float] = [0.0 for _ in range(num_shards)]

    # Greedy placement into least-loaded shard (stable index on tie)
    for pkg in sorted_pkgs:
        w = get_package_weight(pkg)
        min_idx = 0
        min_weight = shard_weights[0]
        for i in range(1, num_shards):
            if shard_weights[i] < min_weight:
                min_weight = shard_weights[i]
                min_idx = i
        shard_pkgs[min_idx].append(pkg)
        shard_weights[min_idx] += w

    matrix_include = []
    if include_hub:
        matrix_include.append({
            "shard": "hub",
            "pkgs": "./pkg/hub/...",
        })

    for i in range(num_shards):
        if not shard_pkgs[i]:
            continue
        # Sort packages within each shard alphabetically for deterministic command lines
        pkgs_in_shard = sorted(shard_pkgs[i])
        matrix_include.append({
            "shard": f"rest-{i+1}" if num_shards > 1 else "rest",
            "pkgs": " ".join(pkgs_in_shard),
        })

    return {"include": matrix_include}

def list_go_packages(src_dir: str = ".") -> List[str]:
    """Runs `go list ./pkg/... ./cmd/...` in src_dir."""
    cmd = ["go", "list", "./pkg/...", "./cmd/..."]
    res = subprocess.run(cmd, cwd=src_dir, capture_output=True, text=True, check=True)
    return [line.strip() for line in res.stdout.strip().split("\n") if line.strip()]

def main():
    parser = argparse.ArgumentParser(description="Partition Go packages into test shards")
    parser.add_argument("--shards", type=int, default=3, help="Number of rest shards (default: 3)")
    parser.add_argument("--no-hub", action="store_true", help="Do not include dedicated hub shard in output")
    parser.add_argument("--src-dir", type=str, default=".", help="Source directory for `go list` (default: .)")
    parser.add_argument("--github-output", action="store_true", help="Write matrix to $GITHUB_OUTPUT if set")
    parser.add_argument("--summary", action="store_true", help="Print human-readable summary to stderr")
    args = parser.parse_args()

    # Read packages from stdin if not a tty, otherwise run `go list`
    if not sys.stdin.isatty():
        packages = [line.strip() for line in sys.stdin if line.strip()]
    else:
        packages = list_go_packages(args.src_dir)

    matrix = partition_packages(packages, num_shards=args.shards, include_hub=not args.no_hub)
    matrix_json = json.dumps(matrix, separators=(',', ':'))

    # Output to GITHUB_OUTPUT if requested
    github_output_path = os.environ.get("GITHUB_OUTPUT")
    if args.github_output and github_output_path:
        with open(github_output_path, "a") as f:
            f.write(f"matrix={matrix_json}\n")

    # Output summary to GITHUB_STEP_SUMMARY if present
    github_summary_path = os.environ.get("GITHUB_STEP_SUMMARY")
    if github_summary_path and args.github_output:
        try:
            with open(github_summary_path, "a") as f:
                f.write("### Test Shard Partition\n\n")
                f.write(f"Partitioned {len(packages)} packages into {len(matrix['include'])} shards:\n\n")
                f.write("| Shard | Packages | Estimated Weight |\n")
                f.write("|-------|----------|------------------|\n")
                for entry in matrix["include"]:
                    shard_name = entry["shard"]
                    if shard_name == "hub":
                        f.write(f"| `{shard_name}` | `./pkg/hub/...` | ~168s |\n")
                    else:
                        pkgs_list = entry["pkgs"].split()
                        w_sum = sum(get_package_weight(p) for p in pkgs_list)
                        f.write(f"| `{shard_name}` | {len(pkgs_list)} packages | ~{w_sum:.1f}s |\n")
                f.write("\n")
        except Exception:
            pass

    if args.summary:
        for entry in matrix["include"]:
            shard_name = entry["shard"]
            if shard_name == "hub":
                print(f"Shard {shard_name}: ./pkg/hub/...", file=sys.stderr)
            else:
                pkgs_list = entry["pkgs"].split()
                w_sum = sum(get_package_weight(p) for p in pkgs_list)
                print(f"Shard {shard_name}: {len(pkgs_list)} pkgs, estimated weight {w_sum:.1f}s", file=sys.stderr)

    # Always print JSON matrix to stdout
    print(matrix_json)

if __name__ == "__main__":
    main()
