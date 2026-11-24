#!/usr/bin/env python3
"""Compare a CodeQL SARIF run on a fixture with the fixture's expectations.

A fixture line that ends in `// $ Alert` must carry a result; no other line
may. Both missed alerts (the query stopped finding a flow it must find) and
unexpected ones (the query now fires on a guarded shape) fail, each printed
as a GitHub error annotation on the fixture line.

Usage: check-fixture-alerts.py SARIF_DIR_OR_FILE FIXTURE_DIR RULE_ID
       check-fixture-alerts.py --report SARIF_DIR_OR_FILE RULE_ID

--report is the src/ run: it prints every result as a warning annotation and
exits 0, so the query's findings on the real tree are visible without
blocking while the existing flows are triaged.
"""
import json
import pathlib
import sys

MARKER = "// $ Alert"


def expected_alerts(fixture):
    out = set()
    for path in sorted(fixture.rglob("*.go")):
        rel = path.relative_to(fixture).as_posix()
        for number, line in enumerate(path.read_text(encoding="utf-8").splitlines(), 1):
            if line.rstrip().endswith(MARKER):
                out.add((rel, number))
    return out


def sarif_files(target):
    if target.is_dir():
        return sorted(target.rglob("*.sarif"))
    return [target]


def actual_alerts(sarif_paths, fixture, rule_id):
    names = {p.relative_to(fixture).as_posix() for p in fixture.rglob("*.go")}
    out = set()
    for sarif in sarif_paths:
        data = json.loads(sarif.read_text(encoding="utf-8"))
        for run in data.get("runs", []):
            for result in run.get("results", []):
                if result.get("ruleId") != rule_id:
                    continue
                for loc in result.get("locations", [])[:1]:
                    phys = loc.get("physicalLocation", {})
                    uri = phys.get("artifactLocation", {}).get("uri", "")
                    line = phys.get("region", {}).get("startLine")
                    # The URI is relative to the source root, which may be the
                    # fixture or the checkout; match it by fixture-relative
                    # suffix.
                    rel = next((n for n in names if uri == n or uri.endswith("/" + n)), uri)
                    out.add((rel, line))
    return out


def report(target, rule_id):
    count = 0
    for sarif in sarif_files(target):
        data = json.loads(sarif.read_text(encoding="utf-8"))
        for run in data.get("runs", []):
            for result in run.get("results", []):
                if result.get("ruleId") != rule_id:
                    continue
                phys = (result.get("locations") or [{}])[0].get("physicalLocation", {})
                uri = phys.get("artifactLocation", {}).get("uri", "")
                line = phys.get("region", {}).get("startLine", 1)
                text = " ".join(result.get("message", {}).get("text", "").split())
                path = uri if uri.startswith("src/") else "src/" + uri
                print(f"::warning file={path},line={line}::{rule_id}: {text}")
                count += 1
    print(f"{rule_id}: {count} result(s) on src/")
    return 0


def main(argv):
    if len(argv) == 3 and argv[0] == "--report":
        return report(pathlib.Path(argv[1]), argv[2])
    if len(argv) != 3:
        print(__doc__, file=sys.stderr)
        return 2
    target, fixture, rule_id = pathlib.Path(argv[0]), pathlib.Path(argv[1]), argv[2]
    sarifs = sarif_files(target)
    if not sarifs:
        print(f"::error::no SARIF output under {target}; the analysis did not run")
        return 1
    want = expected_alerts(fixture)
    if not want:
        print(f"::error::{fixture} has no `{MARKER}` lines; the fixture cannot fail")
        return 1
    got = actual_alerts(sarifs, fixture, rule_id)
    missed, unexpected = sorted(want - got), sorted(got - want)
    for rel, line in missed:
        print(f"::error file={fixture / rel},line={line}::{rule_id} should report this line and did not")
    for rel, line in unexpected:
        print(f"::error file={fixture / rel},line={line}::{rule_id} reported a line the fixture marks as safe")
    print(f"{rule_id}: {len(want)} expected, {len(got)} reported, "
          f"{len(missed)} missed, {len(unexpected)} unexpected")
    return 1 if missed or unexpected else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
