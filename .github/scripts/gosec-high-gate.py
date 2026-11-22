#!/usr/bin/env python3
"""Fail the gosec job when a report holds a NEW HIGH-severity finding (#19443).

gosec runs once per Go module with `-no-fail -fmt json -out <file> -stdout
-verbose text`: the full text report (every severity) stays in the job log,
and the JSON copy lands here. This script is the gate. MEDIUM and LOW
findings stay visible in the log and do not block.

RATCHET, NOT A FLAG DAY. When the gate was added the tree already held ~230
HIGH findings (mostly G101 "hardcoded credentials" on constant *names*, plus
G703/G704 taint findings and G115 conversions). Those are recorded in
.github/gosec-high-baseline.json and do not block; a HIGH finding that is not
in the baseline does. Same shape as the Semgrep --baseline-commit ratchet in
static-security.yml. A finding is keyed by (file, rule, the text of the
flagged source line), not by line number, so unrelated edits that shift
lines do not break the gate, while editing a baselined line re-opens it for
review. Burning down the baseline is the #4903 backlog: fix the site, or
suppress it with `#nosec <rule> -- <reason>`, then regenerate.

A finding suppressed in the source (`#nosec G204 -- <reason>`) is not in the
report's Issues list, or carries a non-empty `suppressions` list when gosec
runs with -track-suppressions; either way it does not block. Suppress with a
reason a reviewer can check, never to turn the gate green. Never add a new
finding to the baseline to turn the gate green either: the baseline only
shrinks.

A missing or unreadable report fails closed: a scan that crashed before it
wrote its report has not shown the code is clean. A missing or unreadable
baseline fails closed too.

Usage:
  gosec-high-gate.py [--baseline FILE] REPORT.json [REPORT.json ...]
  gosec-high-gate.py [--baseline FILE] --write-baseline REPORT.json [...]
"""
import collections
import json
import os
import pathlib
import re
import sys

HERE = pathlib.Path(__file__).resolve().parent
REPO_ROOT = HERE.parent.parent
DEFAULT_BASELINE = HERE.parent / "gosec-high-baseline.json"


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


def workspace_root():
    return os.environ.get("GITHUB_WORKSPACE", "") or str(REPO_ROOT)


def relative(path, workspace):
    """Path relative to the checkout, so the annotation lands on the file."""
    if workspace and path.startswith(workspace.rstrip("/") + "/"):
        return path[len(workspace.rstrip("/")) + 1:]
    return path


def first_line(issue):
    try:
        return int(str(issue.get("line", "1")).split("-")[0] or "1")
    except ValueError:
        return 1


def source_text(issue, workspace):
    """Whitespace-normalised text of the flagged line.

    Read from the checked-out file; fall back to gosec's own `code` snippet
    (lines prefixed `N: `) when the file is not readable.
    """
    line = first_line(issue)
    path = str(issue.get("file", ""))
    if not os.path.isabs(path) and workspace:
        path = os.path.join(workspace, path)
    text = None
    try:
        with open(path, encoding="utf-8", errors="replace") as f:
            for i, content in enumerate(f, 1):
                if i == line:
                    text = content
                    break
    except OSError:
        pass
    if text is None:
        for snippet in str(issue.get("code", "")).splitlines():
            m = re.match(r"\s*(\d+):\s?(.*)$", snippet)
            if m and int(m.group(1)) == line:
                text = m.group(2)
                break
    return " ".join((text or "").split())


def fingerprint(issue, workspace):
    return (relative(str(issue.get("file", "")), workspace),
            str(issue.get("rule_id", "?")),
            source_text(issue, workspace))


def annotation(issue, workspace):
    """One `::error` workflow command for an issue."""
    path = relative(str(issue.get("file", "")), workspace)
    line = str(issue.get("line", "1")).split("-")[0] or "1"
    rule = issue.get("rule_id", "?")
    details = " ".join(str(issue.get("details", "")).split())
    confidence = issue.get("confidence", "?")
    return f"::error file={path},line={line}::gosec {rule} (HIGH severity, {confidence} confidence): {details}"


def load_baseline(path):
    with open(path, encoding="utf-8") as f:
        data = json.load(f)
    counts = collections.Counter()
    for entry in data.get("findings") or []:
        key = (entry["file"], entry["rule_id"], " ".join(str(entry.get("code", "")).split()))
        counts[key] += int(entry.get("count", 1))
    return counts


def write_baseline(path, counts):
    findings = [
        {"file": f, "rule_id": r, "code": c, **({"count": n} if n > 1 else {})}
        for (f, r, c), n in sorted(counts.items())
    ]
    doc = {
        "_comment": ("Pre-existing gosec HIGH findings the gate in "
                     ".github/scripts/gosec-high-gate.py tolerates (#19443). This "
                     "list only shrinks: fix or #nosec a site, then regenerate with "
                     "--write-baseline. Never add a new finding here."),
        "findings": findings,
    }
    with open(path, "w", encoding="utf-8") as f:
        json.dump(doc, f, indent=1, sort_keys=False)
        f.write("\n")


def main(argv):
    baseline_path = str(DEFAULT_BASELINE)
    write = False
    args = []
    it = iter(argv)
    for a in it:
        if a == "--baseline":
            baseline_path = next(it, "")
        elif a == "--write-baseline":
            write = True
        else:
            args.append(a)
    if not args:
        print(__doc__.strip().split("Usage:")[1], file=sys.stderr)
        return 2
    workspace = workspace_root()

    found = []
    for path in args:
        try:
            with open(path, encoding="utf-8") as f:
                report = json.load(f)
        except (OSError, ValueError) as err:
            print(f"::error::gosec report {path} is missing or unreadable ({err}); the scan did not finish")
            return 1
        issues = high_findings(report)
        found.extend(issues)
        print(f"{path}: {len(issues)} HIGH-severity finding(s)")

    if write:
        counts = collections.Counter(fingerprint(i, workspace) for i in found)
        write_baseline(baseline_path, counts)
        print(f"wrote {sum(counts.values())} finding(s) to {baseline_path}")
        return 0

    if baseline_path:
        try:
            allowed = load_baseline(baseline_path)
        except (OSError, ValueError, KeyError, TypeError, AttributeError) as err:
            print(f"::error::gosec baseline {baseline_path} is missing or unreadable ({err})")
            return 1
    else:
        allowed = collections.Counter()

    remaining = collections.Counter(allowed)
    new = []
    for issue in found:
        key = fingerprint(issue, workspace)
        if remaining[key] > 0:
            remaining[key] -= 1
        else:
            new.append(issue)
    for issue in new:
        print(annotation(issue, workspace))
    stale = sum(remaining.values())
    print(f"gosec: {len(found)} HIGH-severity finding(s), {len(found) - len(new)} in the baseline, {len(new)} new")
    if stale:
        print(f"gosec: {stale} baseline entr(y/ies) no longer found; regenerate the baseline with "
              "--write-baseline to lock in the improvement")
    if new:
        print("gosec: fix each new HIGH finding, or suppress it with `#nosec <rule> -- <reason>` "
              "where the reason shows it is not exploitable. Do not add it to the baseline.")
        return 1
    print("gosec: no new HIGH-severity findings")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
