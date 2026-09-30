#!/usr/bin/env python3
"""Act on runner-infra CI failures: rerun once, and alert on the rate (#9664).

Two subcommands, each driven by its own workflow:

  rerun  (.github/workflows/ci-infra-rerun.yml, on workflow_run completed)
         Classifies every failed job of one run attempt with
         ci_infra_classify.py, writes the verdicts to a JSON file (uploaded as
         an artifact so `rate` can count them), and reruns the failed jobs
         ONCE when every failed job is infra (or only reports other jobs'
         failures). Never for a `code` failure, never past the first attempt,
         never for a run from a fork.

  rate   (.github/workflows/ci-infra-rate.yml, hourly)
         Computes the share of the last N completed CI runs that hit at least
         one infra failure. At or above the threshold it opens (or updates)
         ONE tracking issue, found by a hidden marker, with a class x runner
         breakdown; below it, it closes that issue.

All GitHub access goes through `gh api` (GH_TOKEN), wrapped in GhApi so the
decision logic can be tested against a fake.

Exit codes: 0 success (including "decided to do nothing"), 1 a GitHub API call
failed, 2 usage error.
"""

import argparse
import collections
import io
import json
import os
import re
import subprocess
import sys
import urllib.parse
import zipfile

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import ci_infra_classify as classifier  # noqa: E402

EXIT_OK = 0
EXIT_API = 1
EXIT_USAGE = 2

# --- rerun -----------------------------------------------------------------

# Only the first attempt of a run is ever rerun automatically, so a failure
# that looks like infra but is not can cost at most one extra attempt.
MAX_AUTO_RERUN_ATTEMPT = 1
# Job conclusions that count as "failed" for classification.
FAILED_JOB_CONCLUSIONS = ("failure", "timed_out")
ANNOTATIONS_PER_PAGE = 50
JOBS_PER_PAGE = 100
RERUN_SWITCH_OFF = ("off", "false", "0", "no")

VERDICTS_ARTIFACT_PREFIX = "ci-infra-verdicts"
VERDICTS_SCHEMA = 1

# --- rate ------------------------------------------------------------------

# Defaults; each can be overridden by the env var named beside it.
DEFAULT_WINDOW_RUNS = 100  # CI_INFRA_WINDOW_RUNS
DEFAULT_ALERT_THRESHOLD = 0.15  # CI_INFRA_ALERT_THRESHOLD (fraction, 0-1)
DEFAULT_MIN_RUNS = 20  # CI_INFRA_MIN_RUNS: below this, no verdict either way
DEFAULT_ALERT_LABELS = "ci"  # CI_INFRA_ALERT_LABELS (comma-separated)
RUNS_PER_PAGE = 100
MAX_RUN_PAGES = 10
ISSUES_PER_PAGE = 100
MAX_ISSUE_PAGES = 5
TOP_BREAKDOWN_ROWS = 15
SAMPLE_RUN_LINKS = 10
PERCENT = 100

# Run conclusions that count toward the rate. Cancelled and skipped runs say
# nothing about the runner pool either way.
COUNTED_RUN_CONCLUSIONS = ("success", "failure", "timed_out")

TRACKING_MARKER = "<!-- hive-ci-infra-rate-tracker -->"
TRACKING_TITLE = "CI runner infrastructure failure rate is above threshold"
# The tracking issue is opened by the workflow's GITHUB_TOKEN; only issues by
# this author are candidates, so a human quoting the marker can never be
# mistaken for the tracker.
TRACKING_ISSUE_CREATOR = "github-actions[bot]"

# Workflows whose runs are watched. KEEP IN SYNC with the `workflows:` list
# in .github/workflows/ci-infra-rerun.yml (the test suite enforces it).
# Release and publish workflows are deliberately absent: an automatic rerun
# must never re-publish anything.
DEFAULT_WATCHED_WORKFLOWS = (
    "v2 CI",
    "v2 Tests",
    "Go Security Analysis",
    "Backend smoke",
    "Flue smoke",
    "Dashboard Lint",
    "Coverage Hourly",
    "testutil-guard",
    "SUID + NET_ADMIN Contract",
    "Podman Rootless Contract",
    "Docs Link Check",
)

# Runner pods are named <scale-set>-runner-<suffix>; the suffix is per pod.
_RUNNER_POD_SUFFIX = re.compile(r"-runner-[a-z0-9]+$")

# Mirrors advisory.NeutralizeMentions (src/pkg/advisory/sanitize.go): outside
# code, "@user" becomes "`user`"; inside code spans and fences only the "@" is
# dropped. Emails and URL paths are untouched.
_MENTION = re.compile(r"(^|[^A-Za-z0-9`/])@([A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?)")
_FENCE = "```"


def neutralize_mentions(text):
    """Rewrite every GitHub @mention so posted text can never notify anyone."""
    if "@" not in text:
        return text
    out = []
    in_fence = False
    for line in text.split("\n"):
        if line.strip().startswith(_FENCE):
            in_fence = not in_fence
            out.append(line)
            continue
        if in_fence:
            out.append(_MENTION.sub(r"\1\2", line))
            continue
        segs = line.split("`")
        for i, seg in enumerate(segs):
            repl = r"\1`\2`" if i % 2 == 0 else r"\1\2"
            segs[i] = _MENTION.sub(repl, seg)
        out.append("`".join(segs))
    return "\n".join(out)


class ApiError(RuntimeError):
    """A GitHub API call failed."""


class GhApi:
    """Thin `gh api` wrapper. Paths are relative to https://api.github.com/."""

    def __init__(self, runner=subprocess.run):
        self._runner = runner

    def _call(self, args, body=None):
        cmd = ["gh", "api"] + args
        kwargs = {"capture_output": True}
        if body is not None:
            cmd += ["--input", "-"]
            kwargs["input"] = json.dumps(body).encode()
        proc = self._runner(cmd, **kwargs)
        if proc.returncode != 0:
            err = proc.stderr.decode(errors="replace").strip() if proc.stderr else ""
            raise ApiError("gh api %s failed: %s" % (" ".join(args), err))
        return proc.stdout

    def get_json(self, path, params=None):
        if params:
            path = path + "?" + urllib.parse.urlencode(params)
        out = self._call([path])
        return json.loads(out.decode() or "null")

    def get_bytes(self, path):
        return self._call([path])

    def get_text_or_empty(self, path):
        """Fetch text; "" when unavailable (a dead runner uploads no log)."""
        try:
            return self._call([path]).decode(errors="replace")
        except ApiError:
            return ""

    def post(self, path, body=None):
        out = self._call(["-X", "POST", path], body if body is not None else {})
        return json.loads(out.decode() or "null")

    def patch(self, path, body):
        out = self._call(["-X", "PATCH", path], body)
        return json.loads(out.decode() or "null")


# ---------------------------------------------------------------------------
# rerun


def decide_rerun(run_attempt, current_attempt, same_repo, enabled, verdicts):
    """Decide whether to rerun a failed run's failed jobs.

    run_attempt is the attempt that was classified; current_attempt is the
    run's latest attempt as the API reports it now. verdicts is the list of
    per-job verdict strings. Returns (rerun, reason).

    Reruns only when every failed job is infra or derived and at least one is
    infra, on the first attempt, while that attempt is still the latest (so
    re-running this workflow cannot rerun the run a second time), for a
    same-repo run, with the switch on.
    """
    if not enabled:
        return False, "auto-rerun is switched off (HIVE_CI_INFRA_RERUN)"
    if not same_repo:
        return False, "run is from a fork"
    if run_attempt > MAX_AUTO_RERUN_ATTEMPT:
        return False, "attempt %d is already a rerun; auto-rerun fires at most once" % run_attempt
    if current_attempt != run_attempt:
        return False, "the run has moved on to attempt %d" % current_attempt
    if not verdicts:
        return False, "no failed jobs to classify"
    if any(v == classifier.VERDICT_CODE for v in verdicts):
        return False, "at least one failed job is a code failure"
    if not any(classifier.is_infra(v) for v in verdicts):
        return False, "no failed job is classified infra"
    return True, "every failed job is runner infrastructure"


def classify_job(api, repo, job, signatures):
    log = api.get_text_or_empty("repos/%s/actions/jobs/%d/logs" % (repo, job["id"]))
    try:
        anns = api.get_json(
            "repos/%s/check-runs/%d/annotations" % (repo, job["id"]),
            {"per_page": ANNOTATIONS_PER_PAGE},
        ) or []
    except ApiError:
        anns = []
    ann_text = "\n".join(a.get("message") or "" for a in anns)
    return classifier.classify(log, ann_text, signatures)


def run_rerun(api, repo, run_id, run_attempt, workflow, same_repo, enabled, signatures):
    """Classify one run attempt's failed jobs and rerun them once if infra.

    Returns the verdict record (what gets uploaded for `rate`).
    """
    data = api.get_json(
        "repos/%s/actions/runs/%d/attempts/%d/jobs" % (repo, run_id, run_attempt),
        {"per_page": JOBS_PER_PAGE},
    ) or {}
    failed = [j for j in data.get("jobs") or [] if j.get("conclusion") in FAILED_JOB_CONCLUSIONS]
    jobs = []
    for job in failed:
        jobs.append({
            "id": job["id"],
            "name": job.get("name") or "",
            "runner_name": job.get("runner_name") or "",
            "verdict": classify_job(api, repo, job, signatures),
        })
    run = api.get_json("repos/%s/actions/runs/%d" % (repo, run_id)) or {}
    current_attempt = run.get("run_attempt") or run_attempt
    rerun, reason = decide_rerun(run_attempt, current_attempt, same_repo, enabled,
                                 [j["verdict"] for j in jobs])
    error = ""
    if rerun:
        try:
            api.post("repos/%s/actions/runs/%d/rerun-failed-jobs" % (repo, run_id))
        except ApiError as err:
            # Keep the verdicts (the rate still needs them); fail the step.
            rerun, error = False, str(err)
            reason = "rerun request failed"
    return {
        "schema": VERDICTS_SCHEMA,
        "run_id": run_id,
        "run_attempt": run_attempt,
        "workflow": workflow,
        "jobs": jobs,
        "rerun": rerun,
        "reason": reason,
        "error": error,
    }


def render_rerun_summary(record):
    lines = [
        "### CI infra classifier",
        "",
        "Run %d attempt %d (%s): %s - %s."
        % (
            record["run_id"],
            record["run_attempt"],
            record["workflow"] or "unknown workflow",
            "rerunning failed jobs" if record["rerun"] else "no rerun",
            record["reason"],
        ),
        "",
    ]
    if record["jobs"]:
        lines += ["| Job | Runner | Verdict |", "|---|---|---|"]
        for j in record["jobs"]:
            lines.append("| %s | %s | `%s` |" % (j["name"], j["runner_name"] or "-", j["verdict"]))
    return "\n".join(lines) + "\n"


# ---------------------------------------------------------------------------
# rate


def artifact_name(run_id, run_attempt):
    return "%s-%d-%d" % (VERDICTS_ARTIFACT_PREFIX, run_id, run_attempt)


def list_window_runs(api, repo, watched, window):
    """The newest `window` completed runs of the watched workflows."""
    runs = []
    for page in range(1, MAX_RUN_PAGES + 1):
        data = api.get_json(
            "repos/%s/actions/runs" % repo,
            {"status": "completed", "per_page": RUNS_PER_PAGE, "page": page},
        ) or {}
        batch = data.get("workflow_runs") or []
        for run in batch:
            if run.get("name") in watched and run.get("conclusion") in COUNTED_RUN_CONCLUSIONS:
                runs.append(run)
                if len(runs) >= window:
                    return runs
        if len(batch) < RUNS_PER_PAGE:
            break
    return runs


def fetch_verdicts(api, repo, run_id, run_attempt):
    """Download the verdict record `rerun` uploaded for one attempt, or None."""
    data = api.get_json(
        "repos/%s/actions/artifacts" % repo,
        {"name": artifact_name(run_id, run_attempt), "per_page": 1},
    ) or {}
    arts = [a for a in data.get("artifacts") or [] if not a.get("expired")]
    if not arts:
        return None
    blob = api.get_bytes("repos/%s/actions/artifacts/%d/zip" % (repo, arts[0]["id"]))
    try:
        with zipfile.ZipFile(io.BytesIO(blob)) as zf:
            return json.loads(zf.read("verdicts.json").decode())
    except (zipfile.BadZipFile, KeyError, ValueError):
        return None


def needs_verdicts(run):
    """Only a run that failed, or was rerun, can have a verdict record."""
    return run.get("conclusion") != "success" or (run.get("run_attempt") or 1) > 1


def compute_rate(runs, verdicts_by_run):
    """Summarise infra failures over a window of runs.

    runs: run objects from the Actions API. verdicts_by_run: {run_id: [record,
    ...]} holding every attempt's verdict record found. A run counts as infra
    when ANY attempt had an infra-classified job, so a run that an automatic
    rerun turned green still counts.
    """
    classes = collections.Counter()
    pairs = collections.Counter()
    infra_runs = []
    unclassified = 0
    for run in runs:
        records = verdicts_by_run.get(run["id"]) or []
        if run.get("conclusion") != "success" and not records:
            unclassified += 1
        hit = False
        for rec in records:
            for job in rec.get("jobs") or []:
                verdict = job.get("verdict") or ""
                if not classifier.is_infra(verdict):
                    continue
                hit = True
                klass = verdict[len(classifier.INFRA_PREFIX):]
                pool = _RUNNER_POD_SUFFIX.sub("", job.get("runner_name") or "") or "unknown"
                classes[klass] += 1
                pairs[(klass, job.get("runner_name") or "unknown", pool)] += 1
        if hit:
            infra_runs.append(run)
    total = len(runs)
    return {
        "total": total,
        "infra_runs": len(infra_runs),
        "rate": (len(infra_runs) / total) if total else 0.0,
        "classes": classes,
        "pairs": pairs,
        "unclassified": unclassified,
        "samples": [r.get("html_url") or "" for r in infra_runs[:SAMPLE_RUN_LINKS]],
    }


def render_tracking_body(report, threshold, window):
    lines = [
        TRACKING_MARKER,
        "## CI runner infrastructure failure rate",
        "",
        "**%d of the last %d** completed CI runs (%.1f%%) hit at least one runner"
        " infrastructure failure. The alert threshold is %.1f%% (window: last %d"
        " runs of the watched workflows)."
        % (
            report["infra_runs"],
            report["total"],
            report["rate"] * PERCENT,
            threshold * PERCENT,
            window,
        ),
        "",
        "These failures are the runner pool, not code: rerunning or fixing the"
        " pool helps, changing code or adding tests does not. Classes and"
        " signatures: `.github/scripts/ci-infra-signatures.tsv`.",
        "",
        "### By class",
        "",
        "| Class | Failed jobs |",
        "|---|---|",
    ]
    for klass, count in sorted(report["classes"].items(), key=lambda kv: (-kv[1], kv[0])):
        lines.append("| `%s` | %d |" % (klass, count))
    lines += [
        "",
        "### By class and runner",
        "",
        "Runner pods are ephemeral; the pool column groups them by scale set. The"
        " Actions API does not expose the Kubernetes node.",
        "",
        "| Class | Runner | Pool | Failed jobs |",
        "|---|---|---|---|",
    ]
    ranked = sorted(report["pairs"].items(), key=lambda kv: (-kv[1], kv[0]))
    for (klass, runner, pool), count in ranked[:TOP_BREAKDOWN_ROWS]:
        lines.append("| `%s` | %s | %s | %d |" % (klass, runner, pool, count))
    if len(ranked) > TOP_BREAKDOWN_ROWS:
        lines.append("")
        lines.append("(%d more class/runner pairs not shown.)" % (len(ranked) - TOP_BREAKDOWN_ROWS))
    if report["unclassified"]:
        lines += [
            "",
            "%d failed run(s) in the window have no classifier record (the"
            " classifier did not run for them, or its artifact expired)."
            % report["unclassified"],
        ]
    if report["samples"]:
        lines += ["", "### Recent affected runs", ""]
        lines += ["- %s" % url for url in report["samples"]]
    lines += [
        "",
        "This issue is maintained by `.github/workflows/ci-infra-rate.yml`: it is"
        " updated hourly while the rate is at or above the threshold and closed"
        " automatically when it falls below.",
    ]
    return neutralize_mentions("\n".join(lines) + "\n")


class TrackingPlan:
    """What to do with the tracking issue this hour."""

    NOOP = "noop"
    OPEN = "open"
    UPDATE = "update"
    CLOSE = "close"

    def __init__(self, action, reason, number=None, body=None, comment=None):
        self.action = action
        self.reason = reason
        self.number = number
        self.body = body
        self.comment = comment


def plan_tracking(report, existing, threshold, min_runs, window):
    """Pure decision: open / update / close the ONE tracking issue, or nothing.

    existing is the open tracking issue (dict with "number") or None.
    """
    if report["total"] < min_runs:
        return TrackingPlan(
            TrackingPlan.NOOP,
            "only %d completed runs in the window (< %d); no verdict" % (report["total"], min_runs),
        )
    above = report["rate"] >= threshold
    if above:
        body = render_tracking_body(report, threshold, window)
        if existing:
            return TrackingPlan(TrackingPlan.UPDATE, "rate still above threshold",
                                number=existing["number"], body=body)
        return TrackingPlan(TrackingPlan.OPEN, "rate crossed the threshold", body=body)
    if existing:
        comment = neutralize_mentions(
            "The runner infrastructure failure rate is back below the threshold:"
            " %d of the last %d runs (%.1f%%, threshold %.1f%%). Closing."
            % (report["infra_runs"], report["total"], report["rate"] * PERCENT, threshold * PERCENT)
        )
        return TrackingPlan(TrackingPlan.CLOSE, "rate recovered", number=existing["number"],
                            comment=comment)
    return TrackingPlan(TrackingPlan.NOOP, "rate below threshold and no open tracking issue")


def find_tracking_issue(api, repo):
    """The open tracking issue (lowest number if somehow several), or None."""
    found = []
    for page in range(1, MAX_ISSUE_PAGES + 1):
        batch = api.get_json(
            "repos/%s/issues" % repo,
            {"state": "open", "creator": TRACKING_ISSUE_CREATOR,
             "per_page": ISSUES_PER_PAGE, "page": page},
        ) or []
        for issue in batch:
            if "pull_request" in issue:
                continue
            if TRACKING_MARKER in (issue.get("body") or ""):
                found.append(issue)
        if len(batch) < ISSUES_PER_PAGE:
            break
    return min(found, key=lambda i: i["number"]) if found else None


def apply_plan(api, repo, plan, labels):
    if plan.action == TrackingPlan.OPEN:
        body = {"title": TRACKING_TITLE, "body": plan.body}
        if labels:
            body["labels"] = labels
        created = api.post("repos/%s/issues" % repo, body)
        return created.get("number") if created else None
    if plan.action == TrackingPlan.UPDATE:
        api.patch("repos/%s/issues/%d" % (repo, plan.number), {"body": plan.body})
        return plan.number
    if plan.action == TrackingPlan.CLOSE:
        api.post("repos/%s/issues/%d/comments" % (repo, plan.number), {"body": plan.comment})
        api.patch("repos/%s/issues/%d" % (repo, plan.number),
                  {"state": "closed", "state_reason": "completed"})
        return plan.number
    return None


def run_rate(api, repo, watched, window, threshold, min_runs, labels):
    runs = list_window_runs(api, repo, watched, window)
    verdicts = {}
    for run in runs:
        if not needs_verdicts(run):
            continue
        for attempt in range(1, (run.get("run_attempt") or 1) + 1):
            rec = fetch_verdicts(api, repo, run["id"], attempt)
            if rec:
                verdicts.setdefault(run["id"], []).append(rec)
    report = compute_rate(runs, verdicts)
    plan = plan_tracking(report, find_tracking_issue(api, repo), threshold, min_runs, window)
    number = apply_plan(api, repo, plan, labels)
    return report, plan, number


# ---------------------------------------------------------------------------
# CLI


def _env_int(name, default):
    raw = os.environ.get(name, "").strip()
    return int(raw) if raw else default


def _env_float(name, default):
    raw = os.environ.get(name, "").strip()
    return float(raw) if raw else default


def _env_list(name, default):
    raw = os.environ.get(name, "")
    items = [s.strip() for s in raw.split(",") if s.strip()]
    return items or list(default)


def _append_summary(text):
    path = os.environ.get("GITHUB_STEP_SUMMARY")
    if path:
        with open(path, "a", encoding="utf-8") as fh:
            fh.write(text)


def main(argv=None, api=None):
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    sub = parser.add_subparsers(dest="cmd")
    p_rerun = sub.add_parser("rerun", help="classify one run attempt and rerun once if infra")
    p_rerun.add_argument("--out", required=True, help="where to write the verdicts JSON")
    sub.add_parser("rate", help="update the infra-failure-rate tracking issue")
    try:
        args = parser.parse_args(argv)
    except SystemExit as exc:
        return EXIT_OK if exc.code == 0 else EXIT_USAGE
    if not args.cmd:
        parser.print_usage(sys.stderr)
        return EXIT_USAGE

    try:
        window = _env_int("CI_INFRA_WINDOW_RUNS", DEFAULT_WINDOW_RUNS)
        threshold = _env_float("CI_INFRA_ALERT_THRESHOLD", DEFAULT_ALERT_THRESHOLD)
        min_runs = _env_int("CI_INFRA_MIN_RUNS", DEFAULT_MIN_RUNS)
    except ValueError as err:
        print("ci-infra: bad configuration: %s" % err, file=sys.stderr)
        return EXIT_USAGE
    repo = os.environ.get("GITHUB_REPOSITORY", "")
    if not repo:
        print("ci-infra: GITHUB_REPOSITORY is not set", file=sys.stderr)
        return EXIT_USAGE
    api = api or GhApi()
    try:
        if args.cmd == "rerun":
            try:
                run_id = int(os.environ["CI_INFRA_RUN_ID"])
                attempt = int(os.environ["CI_INFRA_RUN_ATTEMPT"])
            except (KeyError, ValueError):
                print("ci-infra: CI_INFRA_RUN_ID and CI_INFRA_RUN_ATTEMPT must be integers",
                      file=sys.stderr)
                return EXIT_USAGE
            head_repo = os.environ.get("CI_INFRA_HEAD_REPO", "")
            enabled = os.environ.get("CI_INFRA_RERUN", "on").strip().lower() not in RERUN_SWITCH_OFF
            try:
                sigs = classifier.load_signatures(
                    os.environ.get("CI_INFRA_SIGNATURES") or classifier.DEFAULT_SIGNATURES
                )
            except (OSError, classifier.SignatureError) as err:
                print("ci-infra: signature table: %s" % err, file=sys.stderr)
                return EXIT_USAGE
            record = run_rerun(api, repo, run_id, attempt, os.environ.get("CI_INFRA_WORKFLOW", ""),
                               head_repo == repo, enabled, sigs)
            out_dir = os.path.dirname(args.out)
            if out_dir:
                os.makedirs(out_dir, exist_ok=True)
            with open(args.out, "w", encoding="utf-8") as fh:
                json.dump(record, fh, indent=2)
            summary = render_rerun_summary(record)
            _append_summary(summary)
            print(summary)
            if record["error"]:
                print("ci-infra: %s" % record["error"], file=sys.stderr)
                return EXIT_API
            return EXIT_OK

        watched = _env_list("CI_INFRA_WORKFLOWS", DEFAULT_WATCHED_WORKFLOWS)
        labels = _env_list("CI_INFRA_ALERT_LABELS", DEFAULT_ALERT_LABELS.split(","))
        report, plan, number = run_rate(api, repo, set(watched), window, threshold, min_runs, labels)
        summary = (
            "### CI infra failure rate\n\n%d of %d runs (%.1f%%), threshold %.1f%%: %s (%s)%s\n"
            % (report["infra_runs"], report["total"], report["rate"] * PERCENT,
               threshold * PERCENT, plan.action, plan.reason,
               ", issue #%d" % number if number else "")
        )
        _append_summary(summary)
        print(summary)
        return EXIT_OK
    except (ApiError, ValueError) as err:
        # ValueError here is an undecodable API response.
        print("ci-infra: %s" % err, file=sys.stderr)
        return EXIT_API


if __name__ == "__main__":
    sys.exit(main())
