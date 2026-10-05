#!/usr/bin/env python3
"""Read-only issue admission before gh-aw starts its engine; fail closed."""
import json
import os
import sys


def prepare(issue, repo):
    if issue.get("state") != "open" or "pull_request" in issue:
        raise ValueError("only open issues are admitted (not pull requests)")
    labels = [label["name"] for label in issue["labels"]]
    # Conservative subset of bin/enumerate-actionable.sh's issue exclusions.
    # Do not call the enumerator: it can label/comment and scans the whole queue.
    excluded = {"blocked", "do-not-merge", "auto-qa-tuning-report", "needs-human"}
    if any("hold" in label.lower() or label.lower().startswith("lfx")
           or label.lower() in excluded for label in labels):
        raise ValueError("issue is held, blocked, or reserved for human triage")
    return {"issues": {"items": [{
        "repo": repo, "number": issue["number"], "title": issue["title"],
        "labels": labels, "author": issue["user"]["login"],
        "created_at": issue["created_at"],
    }]}}


if __name__ == "__main__":
    try:
        result = prepare(json.load(sys.stdin), os.environ["GITHUB_REPOSITORY"])
    except (ValueError, KeyError, TypeError) as exc:
        print(f"Hive admission refused: {exc}", file=sys.stderr)
        sys.exit(1)
    json.dump(result, sys.stdout)
    print()
