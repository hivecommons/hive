#!/usr/bin/env python3
"""Fail the gosec job when a report holds a HIGH-severity finding (#19443).

gosec runs once per Go module with `-no-fail -fmt json -out <file> -stdout
-verbose text`: the full text report (every severity) stays in the job log,
and the JSON copy lands here. This script is the gate. It prints each
HIGH-severity finding as a GitHub error annotation and exits 1 if there is
any; MEDIUM and LOW findings stay visible in the log and do not block.

A finding suppressed in the source (`#nosec G204 -- <reason>`) is not in the
report's Issues list, or carries a non-empty `suppressions` list when gosec
runs with -track-suppressions; either way it does not block. Suppress with a
reason a reviewer can check, never to turn the gate green.

A missing or unreadable report fails closed: a scan that crashed before it
wrote its report has not shown the code is clean.

Usage: gosec-high-gate.py REPORT.json [REPORT.json ...]
"""
import json
import os
import sys


def high_findings(report):
    """Return the unsuppressed HIGH-severity issues of one gosec report."""
    out = []
    for issue in report.get("Issues") or []:
        if str(issue.get("severity", "")).upper() != "HIGH":
            continue
        if issue.get("nosec") or issue.get("suppressions"):
            continue
        out.append(issue)
    return out


def relative(path, workspace):
    """Path relative to the checkout, so the annotation lands on the file."""
    if workspace and path.startswith(workspace.rstrip("/") + "/"):
        return path[len(workspace.rstrip("/")) + 1:]
    return path


def annotation(issue, workspace):
    """One `::error` workflow command for an issue."""
    path = relative(str(issue.get("file", "")), workspace)
    line = str(issue.get("line", "1")).split("-")[0] or "1"
    rule = issue.get("rule_id", "?")
    details = " ".join(str(issue.get("details", "")).split())
    confidence = issue.get("confidence", "?")
    return f"::error file={path},line={line}::gosec {rule} (HIGH severity, {confidence} confidence): {details}"


def main(argv):
    if not argv:
        print("usage: gosec-high-gate.py REPORT.json [REPORT.json ...]", file=sys.stderr)
        return 2
    workspace = os.environ.get("GITHUB_WORKSPACE", "")
    total = 0
    for path in argv:
        try:
            with open(path, encoding="utf-8") as f:
                report = json.load(f)
        except (OSError, ValueError) as err:
            print(f"::error::gosec report {path} is missing or unreadable ({err}); the scan did not finish")
            return 1
        found = high_findings(report)
        for issue in found:
            print(annotation(issue, workspace))
        total += len(found)
        print(f"{path}: {len(found)} HIGH-severity finding(s)")
    if total:
        print(f"gosec: {total} HIGH-severity finding(s). Fix them, or suppress one with "
              "`#nosec <rule> -- <reason>` where the reason shows it is not exploitable.")
        return 1
    print("gosec: no HIGH-severity findings")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
