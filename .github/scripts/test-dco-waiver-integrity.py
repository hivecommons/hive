#!/usr/bin/env python3
"""Keep post-merge DCO SHA waivers narrow and documented."""

from __future__ import annotations

import re
import sys
from pathlib import Path


WORKFLOW = Path(".github/workflows/dco-post-merge.yml")
SHA_RE = re.compile(r"^[0-9a-f]{40}$")
DOCUMENTED_SHA_RE = re.compile(r"^\s*#\s+([0-9a-f]{40})\s+—\s+\S", re.MULTILINE)


def main() -> int:
    text = WORKFLOW.read_text(encoding="utf-8")
    waiver_match = re.search(
        r"(?m)^\s*DCO_WAIVED_COMMITS:\s*'([^']*)'\s*$",
        text,
    )
    if waiver_match is None:
        print(f"{WORKFLOW}: missing DCO_WAIVED_COMMITS env var", file=sys.stderr)
        return 1

    waived = waiver_match.group(1).split()
    failures: list[str] = []

    for sha in waived:
        if SHA_RE.fullmatch(sha) is None:
            failures.append(
                f"DCO_WAIVED_COMMITS entry {sha!r} must be a full 40-character lowercase hex SHA"
            )

    seen: set[str] = set()
    duplicates: set[str] = set()
    for sha in waived:
        if sha in seen:
            duplicates.add(sha)
        seen.add(sha)
    for sha in sorted(duplicates):
        failures.append(f"DCO_WAIVED_COMMITS contains duplicate SHA {sha}")

    prefix = text[: waiver_match.start()]
    marker = "# Per-commit maintainer DCO dispositions, by FULL SHA."
    rationale_start = prefix.find(marker)
    if rationale_start == -1:
        failures.append(f"{WORKFLOW}: missing waiver rationale block marker")
        rationale_block = prefix
    else:
        rationale_block = prefix[rationale_start:]

    documented = DOCUMENTED_SHA_RE.findall(rationale_block)
    if documented != waived:
        failures.append(
            "DCO_WAIVED_COMMITS must exactly match the full-SHA rationale entries "
            f"in the waiver comment block (documented={documented!r}, env={waived!r})"
        )

    if failures:
        print("DCO waiver integrity check failed:", file=sys.stderr)
        for failure in failures:
            print(f"  - {failure}", file=sys.stderr)
        return 1

    print(f"OK: {len(waived)} DCO waived commit(s) are full SHAs, unique, and documented.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
