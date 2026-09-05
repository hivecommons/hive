#!/usr/bin/env python3
"""Move CHANGELOG.md's real Unreleased section under a dated release heading."""

from __future__ import annotations

import argparse
import pathlib
import re
import subprocess


RELEASE_MARKER_RE = re.compile(
    r"[ \t]*<!-- *release: *(none|major|minor|patch) *-->\n?"
)


def move_unreleased(path: pathlib.Path, today: str, version: str) -> None:
    script_dir = pathlib.Path(__file__).resolve().parent
    scanner = script_dir / "lib" / "changelog-unreleased.awk"

    text = path.read_text(encoding="utf-8")
    lines = text.splitlines(keepends=True)
    proc = subprocess.run(
        ["awk", "-v", "mode=range", "-f", str(scanner), str(path)],
        check=True,
        text=True,
        stdout=subprocess.PIPE,
    )
    body_start_line, body_end_line = (int(part) for part in proc.stdout.split())

    marker_line = body_start_line - 1
    start = sum(len(line) for line in lines[: marker_line - 1])
    body_start = sum(len(line) for line in lines[: body_start_line - 1])
    body_end = sum(len(line) for line in lines[:body_end_line])
    body = text[body_start:body_end]
    body = RELEASE_MARKER_RE.sub("", body)

    replacement = f"## Unreleased\n\n## {today} (v{version})\n{body}"
    path.write_text(text[:start] + replacement + text[body_end:], encoding="utf-8")


def main() -> None:
    parser = argparse.ArgumentParser(
        description="Move the real CHANGELOG.md Unreleased section into a dated release entry."
    )
    parser.add_argument("today", help="UTC release date, YYYY-MM-DD")
    parser.add_argument("version", help="release version without leading v")
    parser.add_argument(
        "changelog",
        nargs="?",
        default=pathlib.Path("CHANGELOG.md"),
        type=pathlib.Path,
        help="changelog path (default: CHANGELOG.md)",
    )
    args = parser.parse_args()
    move_unreleased(args.changelog, args.today, args.version)


if __name__ == "__main__":
    main()
