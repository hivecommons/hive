#!/usr/bin/env python3
"""Move CHANGELOG.md's real Unreleased section to a dated release entry."""

from __future__ import annotations

import os
import pathlib
import re
import subprocess
import sys


def main() -> int:
    if len(sys.argv) != 2:
        print("usage: move-unreleased-to-release.py YYYY-MM-DD", file=sys.stderr)
        return 2

    today = sys.argv[1]
    version = os.environ["VERSION"]
    path = pathlib.Path("CHANGELOG.md")
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
    body = re.sub(r"[ \t]*<!-- *release: *(none|major|minor|patch) *-->\n?", "", body)

    replacement = f"## Unreleased\n\n## {today} (v{version})\n{body}"
    path.write_text(text[:start] + replacement + text[body_end:], encoding="utf-8")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
