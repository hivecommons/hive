#!/usr/bin/env python3
"""Generate the quality report that feeds the README badges (#18979).

Every entry is derived from a real run or a published data file and names its
source and timestamp. A value that cannot be computed is "unknown"; a value
older than its freshness window is "stale". Nothing is guessed and nothing is
carried over from an earlier report.

Writes, into --out:
  report.json   every entry with source, timestamp and status
  <id>.json     one shields.io endpoint-schema file per entry

Only the default workflow token is used. Every source failure is caught per
entry, so one bad source yields one "unknown" badge and never an exit error.
"""
import argparse
import datetime
import glob
import io
import json
import os
import subprocess
import sys
import urllib.request
import zipfile

REPO_DEFAULT = "hivecommons/hive"


def env_int(name, default):
    try:
        return int(os.environ.get(name, "") or default)
    except ValueError:
        return default


# Freshness windows. A source older than its window renders "stale".
HOURS_PER_DAY = 24
STALE_TESTS_HOURS = env_int("QUALITY_STALE_TESTS_HOURS", 72)
STALE_COVERAGE_HOURS = env_int("QUALITY_STALE_COVERAGE_HOURS", 48)
STALE_SECURITY_HOURS = env_int("QUALITY_STALE_SECURITY_HOURS", 7 * HOURS_PER_DAY)
STALE_SCORECARD_HOURS = env_int("QUALITY_STALE_SCORECARD_HOURS", 14 * HOURS_PER_DAY)
STALE_ACMM_HOURS = env_int("QUALITY_STALE_ACMM_HOURS", 7 * HOURS_PER_DAY)
STALE_OUTCOMES_HOURS = env_int("QUALITY_STALE_OUTCOMES_DAYS", 62) * HOURS_PER_DAY

# Colour bands.
COVERAGE_GOOD = env_int("QUALITY_COVERAGE_GOOD", 90)
COVERAGE_OK = env_int("QUALITY_COVERAGE_OK", 80)
COVERAGE_LOW = env_int("QUALITY_COVERAGE_LOW", 60)
SCORECARD_GOOD = env_int("QUALITY_SCORECARD_GOOD", 7)
SCORECARD_OK = env_int("QUALITY_SCORECARD_OK", 4)

# Shields endpoint refresh hint, in seconds (the service enforces a floor).
BADGE_CACHE_SECONDS = env_int("QUALITY_BADGE_CACHE_SECONDS", 3600)
# Same minimum denominator the outcomes report uses before printing a rate.
OUTCOMES_MIN_N = env_int("QUALITY_OUTCOMES_MIN_N", 10)
ACMM_MIN_LEVEL = 1
ACMM_MAX_LEVEL = 6
RUNS_PAGE_SIZE = env_int("QUALITY_RUNS_PAGE_SIZE", 30)
COVERAGE_RUNS_TRIED = env_int("QUALITY_COVERAGE_RUNS_TRIED", 6)
HTTP_TIMEOUT_SECONDS = env_int("QUALITY_HTTP_TIMEOUT_SECONDS", 20)
PERCENT = 100.0

WORKFLOW_TESTS = "v2-tests.yml"
WORKFLOW_COVERAGE = "coverage-hourly.yml"
WORKFLOW_SECURITY = "go-security-analysis.yml"
COVERAGE_ARTIFACT_PREFIX = "coverage-profile-"
SCORECARD_API = "https://api.securityscorecards.dev/projects/github.com/"

COLOR_GOOD = "brightgreen"
COLOR_OK = "yellowgreen"
COLOR_WARN = "yellow"
COLOR_BAD = "red"
COLOR_STALE = "orange"
COLOR_UNKNOWN = "lightgrey"
COLOR_INFO = "blue"

STATUS_OK = "ok"
STATUS_FAILING = "failing"
STATUS_STALE = "stale"
STATUS_UNKNOWN = "unknown"

LABELS = {
    "tests": "tests",
    "coverage": "coverage",
    "acmm": "ACMM level",
    "security": "security scan",
    "scorecard": "scorecard",
    "outcomes": "revised, then approved",
}
ORDER = ["tests", "coverage", "acmm", "security", "scorecard", "outcomes"]


def utcnow():
    return datetime.datetime.now(datetime.timezone.utc)


def parse_ts(value):
    if not value:
        return None
    try:
        parsed = datetime.datetime.fromisoformat(str(value).replace("Z", "+00:00"))
    except ValueError:
        return None
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=datetime.timezone.utc)
    return parsed


def iso(ts):
    return ts.strftime("%Y-%m-%dT%H:%M:%SZ") if ts else None


def entry(eid, status, message, color, source, as_of=None, value=None, detail=""):
    return {
        "id": eid,
        "label": LABELS[eid],
        "status": status,
        "message": message,
        "color": color,
        "value": value,
        "source": source,
        "as_of": iso(as_of),
        "detail": detail,
    }


def unknown(eid, source, why):
    return entry(eid, STATUS_UNKNOWN, "unknown", COLOR_UNKNOWN, source, detail=why)


def apply_freshness(e, as_of, max_hours, now):
    """Replace the rendered value with "stale" when the source is too old."""
    if as_of is None:
        return unknown(e["id"], e["source"], "source carries no timestamp")
    age_hours = (now - as_of).total_seconds() / 3600.0
    if age_hours > max_hours:
        stale = dict(e)
        stale.update(status=STATUS_STALE, message="stale", color=COLOR_STALE,
                     detail="source is %.0f h old; window is %d h; last value %s"
                     % (age_hours, max_hours, e["message"]))
        return stale
    return e


# ---- data access (replaced by fakes in the tests) ---------------------------

def gh_json(path):
    out = subprocess.run(["gh", "api", path], check=True, capture_output=True,
                         timeout=HTTP_TIMEOUT_SECONDS * 3).stdout
    return json.loads(out)


def gh_bytes(path):
    return subprocess.run(["gh", "api", path], check=True, capture_output=True,
                          timeout=HTTP_TIMEOUT_SECONDS * 6).stdout


def http_json(url):
    req = urllib.request.Request(url, headers={"Accept": "application/json",
                                               "User-Agent": "quality-report"})
    with urllib.request.urlopen(req, timeout=HTTP_TIMEOUT_SECONDS) as resp:
        return json.loads(resp.read().decode("utf-8"))


class Api:
    def __init__(self, repo):
        self.repo = repo
        self.gh_json = gh_json
        self.gh_bytes = gh_bytes
        self.http_json = http_json


def workflow_runs(api, workflow, branch):
    path = ("repos/%s/actions/workflows/%s/runs?branch=%s&status=completed&per_page=%d"
            % (api.repo, workflow, branch, RUNS_PAGE_SIZE))
    runs = api.gh_json(path).get("workflow_runs", [])
    return [r for r in runs if r.get("event") != "pull_request"]


def run_url(api, workflow):
    return "https://github.com/%s/actions/workflows/%s" % (api.repo, workflow)


# ---- entries -----------------------------------------------------------------

def workflow_verdict(api, eid, workflow, branch, now, max_hours, good_msg, bad_msg):
    source = run_url(api, workflow)
    try:
        runs = workflow_runs(api, workflow, branch)
    except Exception as exc:  # noqa: BLE001 - every failure becomes "unknown"
        return unknown(eid, source, "could not list runs: %s" % type(exc).__name__)
    for r in runs:
        if r.get("conclusion") not in ("success", "failure"):
            continue
        as_of = parse_ts(r.get("updated_at") or r.get("run_started_at"))
        ok = r["conclusion"] == "success"
        e = entry(eid, STATUS_OK if ok else STATUS_FAILING,
                  good_msg if ok else bad_msg, COLOR_GOOD if ok else COLOR_BAD,
                  r.get("html_url") or source, as_of, r["conclusion"],
                  "latest completed %s run on %s" % (workflow, branch))
        return apply_freshness(e, as_of, max_hours, now)
    return unknown(eid, source, "no completed %s run on %s" % (workflow, branch))


def coverage_pct(profile_text):
    """Statement-weighted total, the same ratio `go tool cover -func` reports."""
    blocks = {}
    for line in profile_text.splitlines():
        if not line or line.startswith("mode:"):
            continue
        try:
            head, count = line.rsplit(" ", 1)
            key, stmts = head.rsplit(" ", 1)
            stmts, count = int(stmts), int(count)
        except ValueError:
            continue
        prev = blocks.get(key, (stmts, 0))
        blocks[key] = (stmts, prev[1] + count)
    total = sum(s for s, _ in blocks.values())
    if total == 0:
        return None
    covered = sum(s for s, c in blocks.values() if c > 0)
    return PERCENT * covered / total


def coverage_color(pct):
    if pct >= COVERAGE_GOOD:
        return COLOR_GOOD
    if pct >= COVERAGE_OK:
        return COLOR_OK
    if pct >= COVERAGE_LOW:
        return COLOR_WARN
    return COLOR_BAD


def coverage_entry(api, branch, now):
    source = run_url(api, WORKFLOW_COVERAGE)
    try:
        runs = workflow_runs(api, WORKFLOW_COVERAGE, branch)
    except Exception as exc:  # noqa: BLE001
        return unknown("coverage", source, "could not list runs: %s" % type(exc).__name__)
    tried = 0
    for r in runs:
        if tried >= COVERAGE_RUNS_TRIED:
            break
        try:
            arts = api.gh_json("repos/%s/actions/runs/%s/artifacts" % (api.repo, r["id"]))
        except Exception:  # noqa: BLE001
            tried += 1
            continue
        for a in arts.get("artifacts", []):
            if not a.get("name", "").startswith(COVERAGE_ARTIFACT_PREFIX) or a.get("expired"):
                continue
            tried += 1
            try:
                blob = api.gh_bytes("repos/%s/actions/artifacts/%s/zip" % (api.repo, a["id"]))
                with zipfile.ZipFile(io.BytesIO(blob)) as zf:
                    names = [n for n in zf.namelist() if n.endswith("coverage.out")]
                    if not names:
                        continue
                    pct = coverage_pct(zf.read(names[0]).decode("utf-8", "replace"))
            except Exception:  # noqa: BLE001
                continue
            if pct is None:
                continue
            as_of = parse_ts(a.get("created_at"))
            e = entry("coverage", STATUS_OK, "%.1f%%" % pct, coverage_color(pct),
                      r.get("html_url") or source, as_of, round(pct, 2),
                      "statement coverage of the coverage profile uploaded by %s" % WORKFLOW_COVERAGE)
            return apply_freshness(e, as_of, STALE_COVERAGE_HOURS, now)
    return unknown("coverage", source, "no readable coverage profile among the latest runs")


def scorecard_entry(api, now):
    url = SCORECARD_API + api.repo
    try:
        data = api.http_json(url)
        score = float(data["score"])
        as_of = parse_ts(data.get("date"))
    except Exception as exc:  # noqa: BLE001
        return unknown("scorecard", url, "could not read the published score: %s" % type(exc).__name__)
    if score >= SCORECARD_GOOD:
        color = COLOR_GOOD
    elif score >= SCORECARD_OK:
        color = COLOR_WARN
    else:
        color = COLOR_BAD
    e = entry("scorecard", STATUS_OK, "%.1f/10" % score, color, url, as_of, score,
              "published by scorecard.yml")
    # The API reports a date, not a time: compare with a day of slack.
    return apply_freshness(e, as_of, STALE_SCORECARD_HOURS + HOURS_PER_DAY, now)


def acmm_entry(url, now):
    if not url:
        return unknown("acmm", "not configured",
                       "set the QUALITY_ACMM_URL repository variable to a public JSON "
                       "with overall_level and last_evaluated_at")
    try:
        data = http_json(url)
        level = int(data["overall_level"])
        as_of = parse_ts(data.get("last_evaluated_at"))
    except Exception as exc:  # noqa: BLE001
        return unknown("acmm", url, "could not read the level: %s" % type(exc).__name__)
    if not ACMM_MIN_LEVEL <= level <= ACMM_MAX_LEVEL:
        return unknown("acmm", url, "level %d is outside L%d to L%d"
                       % (level, ACMM_MIN_LEVEL, ACMM_MAX_LEVEL))
    e = entry("acmm", STATUS_OK, "L%d" % level, COLOR_INFO, url, as_of, level,
              "overall_level from the evaluation JSON")
    return apply_freshness(e, as_of, STALE_ACMM_HOURS, now)


def outcomes_entry(repo, root, now):
    pattern = os.path.join(root, "src", "docs", "benchmarks", "outcomes-*.json")
    files = sorted(glob.glob(pattern))
    source = "src/docs/benchmarks/"
    if not files:
        return unknown("outcomes", source, "no outcomes-*.json file")
    latest = files[-1]
    rel = os.path.relpath(latest, root)
    url = "https://github.com/%s/blob/v5/%s" % (repo, rel)
    try:
        with open(latest, encoding="utf-8") as fh:
            data = json.load(fh)
        fa = data["Followup"]["FixedAndApproved"]
        k, n = int(fa["K"]), int(fa["N"])
        as_of = parse_ts(data.get("GeneratedAt"))
    except Exception as exc:  # noqa: BLE001
        return unknown("outcomes", url, "could not read the file: %s" % type(exc).__name__)
    if n < OUTCOMES_MIN_N:
        return unknown("outcomes", url, "n=%d is below the reporting minimum %d" % (n, OUTCOMES_MIN_N))
    pct = PERCENT * k / n
    e = entry("outcomes", STATUS_OK, "%.1f%% (%d/%d)" % (pct, k, n), COLOR_INFO, url,
              as_of, round(pct, 2),
              "Followup.FixedAndApproved in %s" % os.path.basename(latest))
    return apply_freshness(e, as_of, STALE_OUTCOMES_HOURS, now)


# ---- assembly ----------------------------------------------------------------

def build(api, branch, root, acmm_url, now):
    by_id = {
        "tests": workflow_verdict(api, "tests", WORKFLOW_TESTS, branch, now,
                                  STALE_TESTS_HOURS, "passing", "failing"),
        "coverage": coverage_entry(api, branch, now),
        "acmm": acmm_entry(acmm_url, now),
        "security": workflow_verdict(api, "security", WORKFLOW_SECURITY, branch, now,
                                     STALE_SECURITY_HOURS, "clean", "findings"),
        "scorecard": scorecard_entry(api, now),
        "outcomes": outcomes_entry(api.repo, root, now),
    }
    return {
        "schema": 1,
        "generated_at": iso(now),
        "repository": api.repo,
        "branch": branch,
        "entries": [by_id[i] for i in ORDER],
    }


def shields(e):
    return {"schemaVersion": 1, "label": e["label"], "message": e["message"],
            "color": e["color"], "cacheSeconds": BADGE_CACHE_SECONDS}


def write(report, out):
    os.makedirs(out, exist_ok=True)
    with open(os.path.join(out, "report.json"), "w", encoding="utf-8") as fh:
        json.dump(report, fh, indent=2)
        fh.write("\n")
    for e in report["entries"]:
        with open(os.path.join(out, e["id"] + ".json"), "w", encoding="utf-8") as fh:
            json.dump(shields(e), fh, indent=2)
            fh.write("\n")


def summary(report):
    lines = ["## Quality report", "", "| Badge | Value | Status | As of | Source |", "|---|---|---|---|---|"]
    for e in report["entries"]:
        lines.append("| %s | %s | %s | %s | %s |" % (
            e["label"], e["message"], e["status"], e["as_of"] or "-", e["source"]))
    return "\n".join(lines) + "\n"


def main(argv):
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", required=True)
    ap.add_argument("--root", default=".")
    args = ap.parse_args(argv)
    repo = os.environ.get("GITHUB_REPOSITORY") or REPO_DEFAULT
    branch = os.environ.get("QUALITY_REPORT_BRANCH") or "v5"
    report = build(Api(repo), branch, args.root, os.environ.get("QUALITY_ACMM_URL", ""), utcnow())
    write(report, args.out)
    text = summary(report)
    print(text)
    path = os.environ.get("GITHUB_STEP_SUMMARY")
    if path:
        with open(path, "a", encoding="utf-8") as fh:
            fh.write(text)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
