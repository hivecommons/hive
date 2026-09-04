#!/usr/bin/env python3
"""Move CHANGELOG.md's real Unreleased section under a dated release heading."""

from __future__ import annotations

import argparse
import re
from pathlib import Path


RELEASE_MARKER_RE = re.compile(
    r"[ \t]*<!-- *release: *(none|major|minor|patch) *-->\n?"
)


def is_fence(line: str) -> bool:
    stripped = line.lstrip()
    return stripped.startswith("```") or stripped.startswith("~~~")


def find_unreleased_heading(lines: list[str], path: Path) -> int:
    in_fence = False
    for index, line in enumerate(lines):
        if is_fence(line):
            in_fence = not in_fence
        if not in_fence and re.fullmatch(r"## Unreleased[ \t]*(?:\n)?", line):
            return index
    raise SystemExit(
        f"::error::{path} has no '## Unreleased' heading — refusing to move a release section."
    )


def find_next_h2(lines: list[str], start: int) -> int:
    in_fence = False
    for index in range(start, len(lines)):
        line = lines[index]
        if is_fence(line):
            in_fence = not in_fence
        if not in_fence and line.startswith("## "):
            return index
    return len(lines)


def move_unreleased(path: Path, today: str, version: str) -> None:
    lines = path.read_text(encoding="utf-8").splitlines(keepends=True)
    start = find_unreleased_heading(lines, path)
    body_start = start + 1
    body_end = find_next_h2(lines, body_start)
    body = "".join(lines[body_start:body_end])
    body = RELEASE_MARKER_RE.sub("", body)

    replacement = f"## Unreleased\n\n## {today} (v{version})\n{body}"
    text = "".join(lines[:start]) + replacement + "".join(lines[body_end:])
    path.write_text(text, encoding="utf-8")


def main() -> None:
    parser = argparse.ArgumentParser(
        description="Move the real CHANGELOG.md Unreleased section into a dated release entry."
    )
    parser.add_argument("today", help="UTC release date, YYYY-MM-DD")
    parser.add_argument("version", help="release version without leading v")
    parser.add_argument(
        "changelog",
        nargs="?",
        default="CHANGELOG.md",
        type=Path,
        help="changelog path (default: CHANGELOG.md)",
    )
    args = parser.parse_args()
    move_unreleased(args.changelog, args.today, args.version)


if __name__ == "__main__":
    main()
