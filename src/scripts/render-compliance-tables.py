#!/usr/bin/env python3
"""render-compliance-tables.py — regenerate the per-framework control-mapping
tables in src/docs/compliance.md from src/pkg/compliance/profiles/*.yaml
(hivecommons/hive#11084).

The profiles are the source of truth; the doc tables are generated so they
never drift. Each table lives between a pair of markers:

    <!-- BEGIN GENERATED: compliance-profile <id> -->
    ...
    <!-- END GENERATED: compliance-profile <id> -->

The rendering mirrors RenderTable in src/pkg/compliance/render.go byte for
byte; TestComplianceDocTablesMatchProfiles (pkg/compliance) fails CI when the
committed doc differs from the profiles.

Usage:
  src/scripts/render-compliance-tables.py           # rewrite the doc in place
  src/scripts/render-compliance-tables.py --check   # exit 1 if it would change

Requires PyYAML (pip install pyyaml).
"""
from __future__ import annotations

import argparse
import sys
from pathlib import Path

import yaml

SRC = Path(__file__).resolve().parent.parent
PROFILES = SRC / "pkg" / "compliance" / "profiles"
DOC = SRC / "docs" / "compliance.md"


def cell(text: str | None) -> str:
    return " ".join((text or "").split()).replace("|", "\\|")


def render(profile: dict) -> str:
    lines = [
        "| Control | Domain | Hive setting | Recommended | Evaluator | Notes |",
        "|---|---|---|---|---|---|",
    ]
    for c in profile["controls"]:
        control = cell(f"{c['id']} — {c['title']}")
        domain = cell(c["domain"])
        notes = cell(c.get("rationale"))
        if c.get("not_covered"):
            lines.append(f"| {control} | {domain} | _not covered by Hive_ | — | — | {notes} |")
            continue
        for i, m in enumerate(c.get("mappings") or []):
            n = notes if i == 0 else ""
            lines.append(
                f"| {control} | {domain} | `{m['setting_path']}` | `{m['recommended']}` | `{m['evaluator']}` | {n} |"
            )
    return "\n".join(lines) + "\n"


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--check", action="store_true", help="fail instead of rewriting")
    args = ap.parse_args()

    doc = DOC.read_text(encoding="utf-8")
    out = doc
    for path in sorted(PROFILES.glob("*.yaml")):
        profile = yaml.safe_load(path.read_text(encoding="utf-8"))
        pid = profile["id"]
        begin = f"<!-- BEGIN GENERATED: compliance-profile {pid} -->\n"
        end = f"<!-- END GENERATED: compliance-profile {pid} -->"
        b = out.find(begin)
        e = out.find(end)
        if b < 0 or e < b:
            print(f"{DOC}: missing generated-table markers for profile {pid}", file=sys.stderr)
            return 1
        out = out[: b + len(begin)] + render(profile) + out[e:]

    if out == doc:
        print("compliance tables are up to date")
        return 0
    if args.check:
        print(f"{DOC} is stale; run src/scripts/render-compliance-tables.py", file=sys.stderr)
        return 1
    DOC.write_text(out, encoding="utf-8")
    print(f"rewrote {DOC}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
