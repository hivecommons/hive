#!/usr/bin/env python3
"""Prepare and render owner-supplied monthly outcome aggregates; never collect data."""

import argparse
import copy
import datetime as dt
import hashlib
import json
import math
import os
from pathlib import Path
import re
import subprocess
import sys

UTC = dt.timezone.utc
MATURATION_DAYS = 14
MIN_N = 10
MAX_INPUT_BYTES = 48_000
ZERO_TIME = "0001-01-01T00:00:00Z"
ROOT = Path(__file__).resolve().parents[2]
REPORT_DIR = Path("src/docs/benchmarks")


class InputError(ValueError):
    """Only static, non-sensitive validation messages belong here."""


def require(condition, message):
    if not condition:
        raise InputError(message)


def timestamp(value):
    require(isinstance(value, str), "timestamp must be RFC3339 UTC")
    require(re.fullmatch(r"\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?Z", value),
            "timestamp must be RFC3339 UTC")
    return dt.datetime.fromisoformat(value.replace("Z", "+00:00"))


def window(month):
    require(isinstance(month, str) and re.fullmatch(r"\d{4}-\d\d", month),
            "month must be YYYY-MM")
    start = dt.datetime.strptime(month, "%Y-%m").replace(tzinfo=UTC)
    end = (start.replace(day=28) + dt.timedelta(days=4)).replace(day=1)
    return start, end


def select_month(month, now):
    current = now.astimezone(UTC).replace(day=1, hour=0, minute=0, second=0, microsecond=0)
    month = month or (current - dt.timedelta(days=1)).strftime("%Y-%m")
    start, end = window(month)
    require(end <= current, "requested month is not completed in UTC")
    require(now >= end + dt.timedelta(days=MATURATION_DAYS),
            "wait until 14 days after month end for outcome maturation")
    return month, start, end


def count(value):
    require(type(value) is int and value >= 0, "counts must be nonnegative integers")
    return value


def number(value):
    require(type(value) in (int, float) and math.isfinite(value) and value >= 0,
            "metrics must be finite nonnegative numbers")
    return value


def rate(value):
    require(set(value) == {"K", "N", "Rate", "Lo", "Hi", "Reported"}, "invalid rate fields")
    k, n = count(value["K"]), count(value["N"])
    require(k <= n, "rate numerator exceeds denominator")
    require(type(value["Reported"]) is bool and value["Reported"] == (n >= MIN_N),
            "invalid small-n reporting")
    expected = [0, 0, 0]
    if n >= MIN_N:
        p, z = k / n, 1.96
        divisor = 1 + z * z / n
        centre = (p + z * z / (2 * n)) / divisor
        half = z * math.sqrt(p * (1 - p) / n + z * z / (4 * n * n)) / divisor
        expected = [p, max(0, centre - half), min(1, centre + half)]
    for key, wanted in zip(("Rate", "Lo", "Hi"), expected):
        require(math.isclose(number(value[key]), wanted, abs_tol=1e-12),
                "rate or Wilson interval does not match its counts")
    return copy.deepcopy(value)


def median(value):
    require(set(value) == {"Value", "N", "Reported"}, "invalid median fields")
    n = count(value["N"])
    number(value["Value"])
    require(type(value["Reported"]) is bool and value["Reported"] == (n >= MIN_N),
            "invalid median reporting")
    require(n >= MIN_N or value["Value"] == 0, "small-n median must be suppressed")
    return copy.deepcopy(value)


def metrics(value, counts, rates=(), medians=()):
    require(set(value) == set(counts) | set(rates) | set(medians), "invalid metric fields")
    return {**{k: count(value[k]) for k in counts},
            **{k: rate(value[k]) for k in rates},
            **{k: median(value[k]) for k in medians}}


def cohort(value, name):
    require(value["Name"] == name, "cohort name mismatch")
    kinds = value["ByAuthorKind"]
    require(set(kinds) <= {"agent", "bot", "human", "unknown"}, "unknown author category")
    clean = metrics({k: v for k, v in value.items() if k not in ("Name", "ByAuthorKind")},
                    ("Merged", "Matured"),
                    ("RevertRate", "ChangeFailure", "CIFailureAtHead", "CICoverage",
                     "ChangesRequested"), ("TimeToMerge",))
    require(clean["Matured"] == clean["Merged"],
            "completed monthly cohort must be fully matured at this observation time")
    for key in ("RevertRate", "ChangeFailure"):
        require(clean[key]["N"] == clean["Matured"], "matured denominator mismatch")
    require(clean["CICoverage"]["N"] == clean["Merged"], "CI coverage denominator mismatch")
    require(clean["CIFailureAtHead"]["N"] == clean["CICoverage"]["K"],
            "CI failure denominator mismatch")
    require(clean["TimeToMerge"]["N"] <= clean["Merged"], "time-to-merge denominator mismatch")
    require(clean["ChangesRequested"]["N"] == clean["Merged"],
            "review denominator mismatch")
    require(name != "unmediated" or clean["ChangesRequested"]["K"] == 0,
            "unmediated cohort cannot have recorded review objections")
    clean.update(Name=name, ByAuthorKind={k: count(v) for k, v in kinds.items()})
    require(sum(clean["ByAuthorKind"].values()) == clean["Merged"], "author counts mismatch")
    return clean


def sanitize(report, start, end, now, source_hash):
    """Project onto aggregate fields: no raw prose, repository identities or links."""
    observed = timestamp(report["Now"])
    require(timestamp(report["Since"]) == start and timestamp(report["Until"]) == end,
            "report window differs from requested completed month")
    require(end + dt.timedelta(days=MATURATION_DAYS) <= observed <= now,
            "snapshot is premature or from the future")
    require(timestamp(report["GeneratedAt"]) == observed, "generation/observation mismatch")
    clean = {k: report[k] for k in ("GeneratedAt", "Since", "Until", "Now")}
    clean.update({k: count(report[k]) for k in ("Bundles", "Unreadable", "PRs", "Verdicts")})
    require(clean["Unreadable"] == 0, "partial collection: unreadable bundles")
    require(clean["Bundles"] >= clean["PRs"] > 0, "empty or inconsistent evidence")
    for name in ("mediated", "unmediated"):
        clean[name.capitalize()] = cohort(report[name.capitalize()], name)
    merged = clean["Mediated"]["Merged"] + clean["Unmediated"]["Merged"]
    require(0 < merged <= clean["PRs"], "empty or inconsistent merged cohort")
    repos = report["Repos"]
    require(isinstance(repos, list) and len({r["Repo"] for r in repos}) == len(repos),
            "duplicate repositories")
    # Labels are deliberately independent of the private repository names.
    clean["Repos"] = [{"Repo": f"repository-{i + 1:03d}", "Merged": count(r["Merged"])}
                      for i, r in enumerate(repos)]
    require(sum(r["Merged"] for r in clean["Repos"]) == merged, "repository counts mismatch")
    clean["Source"] = f"owner-supplied aggregate; evidence snapshot SHA-256 {source_hash}"
    first = report["FirstMediatedMerge"]
    require(timestamp(first) <= observed or first == ZERO_TIME, "invalid first mediated merge")
    clean["FirstMediatedMerge"] = first
    months = report["Months"]
    require(isinstance(months, list) and months == sorted(set(months)), "invalid trend months")
    for month in months:
        require(window(month)[0] <= observed, "future trend month")
    clean["Months"] = months
    clean["Trend"] = {}
    for name in ("mediated", "unmediated"):
        points = report["Trend"][name]
        require(len(points) == len(months), "partial trend")
        clean["Trend"][name] = []
        for month, point in zip(months, points):
            require(point["Month"] == month, "trend month mismatch")
            fields = metrics({k: v for k, v in point.items() if k != "Month"},
                             ("Merged",), ("RevertRate",), ("TimeToMerge",))
            require(fields["RevertRate"]["N"] <= fields["Merged"] and
                    fields["TimeToMerge"]["N"] <= fields["Merged"], "invalid trend denominator")
            if window(month)[1] <= end:
                require(fields["RevertRate"]["N"] == fields["Merged"],
                        "completed trend cohort must be fully matured")
            clean["Trend"][name].append(dict(Month=month, **fields))
    clean["Followup"] = metrics(
        report["Followup"],
        ("SentBack", "NewHead", "MergedSameHead", "MergedSameHeadApproved", "Unresolved",
         "CIPairs", "CIFixed"), ("FixedAndApproved", "CICoverage"), ("TimeToApproval",))
    followup = clean["Followup"]
    require(followup["FixedAndApproved"]["N"] == followup["SentBack"] and
            followup["FixedAndApproved"]["K"] <= followup["NewHead"] <= followup["SentBack"],
            "follow-up denominator mismatch")
    require(followup["CICoverage"]["N"] == followup["NewHead"] and
            followup["CICoverage"]["K"] == followup["CIPairs"] and
            followup["CIFixed"] <= followup["CIPairs"] and
            followup["MergedSameHeadApproved"] <= followup["MergedSameHead"] <= followup["SentBack"] and
            followup["TimeToApproval"]["N"] == followup["FixedAndApproved"]["K"],
            "follow-up counts mismatch")
    clean["Spend"] = None
    if report.get("Spend") is not None:
        spend = report["Spend"]
        require(set(spend) == {"usd", "merged_prs", "window_start", "window_end", "basis"},
                "invalid spend fields (no cost confidence intervals)")
        require(timestamp(spend["window_start"]) < timestamp(spend["window_end"]) <= observed,
                "invalid spend observation window")
        clean["Spend"] = dict(usd=number(spend["usd"]), merged_prs=count(spend["merged_prs"]),
                              window_start=spend["window_start"], window_end=spend["window_end"],
                              basis="hive-attributed")
        require(spend["basis"] == "hive-attributed", "unsupported cost denominator basis")
    clean["Hub"] = None
    clean["ReadmeMinBase"] = count(report.get("ReadmeMinBase", 0))
    require(clean["ReadmeMinBase"] in (0, 200), "monthly publication keeps existing card threshold")
    return clean


def digest(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(",", ":")).encode()).hexdigest()


def validate(payload, month, now):
    month, start, end = select_month(month, now)
    require(set(payload) == {"schema", "month", "collection_complete", "publication_approved",
                            "snapshot_sha256", "generator_revision", "report"}, "invalid input envelope")
    require(payload["schema"] == 1 and payload["month"] == month, "stale or unsupported input")
    require(payload["collection_complete"] is True, "owner has not attested complete collection")
    require(payload["publication_approved"] is True, "owner has not approved aggregate publication")
    require(re.fullmatch(r"[a-f0-9]{64}", payload["snapshot_sha256"]), "invalid snapshot digest")
    require(re.fullmatch(r"[a-f0-9]{40}", payload["generator_revision"]), "invalid generator revision")
    clean = copy.deepcopy(payload)
    clean["report"] = sanitize(payload["report"], start, end, now, payload["snapshot_sha256"])
    return clean


def read_input(path):
    data = Path(path).read_bytes() if path else os.environ.get("MONTHLY_OUTCOMES_INPUT", "").encode()
    require(data, "missing owner-supplied input: set MONTHLY_OUTCOMES_INPUT; no source access is configured")
    require(len(data) <= MAX_INPUT_BYTES, "aggregate exceeds 48000-byte secret limit")
    return json.loads(data)


def render(payload, directory, revision):
    month = payload["month"]
    provenance = {
        "schema": 1, "month": month, "input_sha256": digest(payload),
        "snapshot_sha256": payload["snapshot_sha256"],
        "generator_revision": payload["generator_revision"],
        "renderer_revision": revision,
        "observed_at": payload["report"]["Now"],
        "collection_complete": True, "publication_approved": True,
        "privacy": "aggregates only; repository labels anonymized; no stories, hub, eval or raw evidence",
    }
    committed = ROOT / REPORT_DIR / f"outcomes-{month}.provenance.json"
    if committed.exists() and json.loads(committed.read_text())["input_sha256"] == digest(payload):
        return False
    directory.mkdir(parents=True, exist_ok=True)
    require(not any(directory.iterdir()), "render staging directory must be empty")
    source = directory / "report-input.json"
    source.write_text(json.dumps(payload["report"]) + "\n")
    subprocess.run(["go", "run", "./cmd/hivectl", "evidence", "benchmark",
                    "--from-json", str(source.resolve()), "--out-dir", str(directory.resolve())],
                   cwd=ROOT / "src", check=True, stdout=subprocess.DEVNULL,
                   stderr=subprocess.DEVNULL)
    source.unlink()
    (directory / f"outcomes-{month}.provenance.json").write_text(
        json.dumps(provenance, indent=2) + "\n")
    return True


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("prepare", "render"))
    parser.add_argument("--input", help="envelope JSON (otherwise MONTHLY_OUTCOMES_INPUT)")
    parser.add_argument("--month", default="", help="completed YYYY-MM; default prior UTC month")
    parser.add_argument("--out", required=True, help="prepared envelope file or render staging directory")
    args = parser.parse_args()
    payload = validate(read_input(args.input), args.month, dt.datetime.now(UTC))
    if args.command == "prepare":
        Path(args.out).write_text(json.dumps(payload, indent=2) + "\n")
    else:
        revision = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip()
        changed = render(payload, Path(args.out), revision)
        if os.environ.get("GITHUB_OUTPUT"):
            with open(os.environ["GITHUB_OUTPUT"], "a") as output:
                output.write(f"month={payload['month']}\nchanged={str(changed).lower()}\n")
        print(f"monthly outcomes {payload['month']}: {'generated' if changed else 'unchanged'}")


if __name__ == "__main__":
    try:
        main()
    except InputError as error:
        print(f"monthly outcomes failed: {error}", file=sys.stderr)
        sys.exit(1)
    except (ValueError, KeyError, TypeError, OSError, subprocess.SubprocessError):
        # Source JSON and generator stderr can contain private paths/prose.
        print("monthly outcomes failed: missing, invalid, premature or incomplete supplied input, "
              "or renderer failure; see the aggregate input contract (input is never logged)", file=sys.stderr)
        sys.exit(1)
