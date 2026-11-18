#!/usr/bin/env python3
"""Required checks must never be CANCELLED by a re-check of the same head (#18578).

GitHub scores every check-run of a required name on a commit, so one cancelled
sibling keeps a PR BLOCKED beside a green survivor and Tide jams on it
(#11250, #18578). Two concurrency behaviours produce such siblings:

  * cancel-in-progress: a new run in the group cancels the one in flight;
  * queue replacement: a group holds at most one pending run, and the next run
    to queue cancels it, even with cancel-in-progress false.

So any event that re-checks the SAME head (a label flip, an edit, a reopen)
must land in a concurrency group of its own. This test parses every workflow
under .github/workflows/, finds the jobs that produce a required context on
v5, and evaluates their concurrency expressions for a push followed by a
burst of label events on one head SHA. It fails if any two of those runs
share a group.

The expression evaluator below covers the subset of GitHub's expression
syntax these workflows use (contexts, literals, ==, !=, !, &&, ||,
parentheses, contains/fromJSON/format/startsWith/endsWith). An unsupported
construct fails loudly rather than passing.
"""
import json
import os
import pathlib
import re
import unittest

import yaml

ROOT = pathlib.Path(__file__).resolve().parents[2]
WORKFLOWS = ROOT / ".github/workflows"

# Mirror of the v5 branch protection's required contexts, read with
# `gh api repos/hivecommons/hive/branches/v5/protection/required_status_checks`.
# Keep in step when a context is added; contexts produced by an app (DCO) and
# not by a workflow job are simply never matched.
REQUIRED_CONTEXTS = {
    "gate",
    "DCO",
    "build-and-test",
    "test",
    "coverage",
    "golangci-lint",
    "gosec (insecure patterns)",
    "govulncheck (known vulnerabilities)",
    "changelog-fragment-guard",
    "verify PR contents",
    "Sign-off survives the squash",
}

PR_EVENTS = ("pull_request", "pull_request_target")
# Events that re-check a head SHA that already has (or is getting) a run.
RECHECK_ACTIONS = ("labeled", "unlabeled")
# GitHub's default activity types when `types:` is omitted.
DEFAULT_PR_TYPES = ["opened", "synchronize", "reopened"]

PR_NUMBER = 4242
HEAD_SHA = "a" * 40
MERGE_SHA = "b" * 40


# --- minimal GitHub expression evaluator ------------------------------------

TOKEN = re.compile(r"""
    \s*(?:
      (?P<num>-?\d+(?:\.\d+)?)
    | (?P<str>'(?:[^']|'')*')
    | (?P<op>==|!=|&&|\|\||<=|>=|[!<>()\[\],.*])
    | (?P<ident>[A-Za-z_][A-Za-z0-9_-]*)
    )""", re.X)


class ExprError(ValueError):
    pass


def tokenize(src):
    pos, out = 0, []
    while pos < len(src):
        if src[pos:].strip() == "":
            break
        m = TOKEN.match(src, pos)
        if not m or m.end() == pos:
            raise ExprError(f"cannot tokenize {src[pos:]!r}")
        pos = m.end()
        kind = m.lastgroup
        out.append((kind, m.group(kind)))
    return out


def truthy(v):
    return not (v is None or v is False or v == 0 or v == "")


def to_str(v):
    if v is None:
        return ""
    if v is True:
        return "true"
    if v is False:
        return "false"
    if isinstance(v, (dict, list)):
        return json.dumps(v)
    return str(v)


def loose_eq(a, b):
    if type(a) is type(b):
        return a == b
    if isinstance(a, str) and isinstance(b, str):
        return a.lower() == b.lower()
    return to_str(a) == to_str(b)


def fn_format(fmt, *args):
    return re.sub(r"\{(\d+)\}", lambda m: to_str(args[int(m.group(1))]), fmt)


FUNCTIONS = {
    "contains": lambda hay, needle: (
        any(loose_eq(x, needle) for x in hay) if isinstance(hay, list)
        else to_str(needle).lower() in to_str(hay).lower()),
    "fromjson": lambda s: json.loads(s),
    "format": fn_format,
    "startswith": lambda s, p: to_str(s).lower().startswith(to_str(p).lower()),
    "endswith": lambda s, p: to_str(s).lower().endswith(to_str(p).lower()),
}


class Parser:
    def __init__(self, src, ctx):
        self.toks, self.i, self.ctx = tokenize(src), 0, ctx

    def peek(self, value=None):
        if self.i >= len(self.toks):
            return None
        tok = self.toks[self.i]
        return tok if value is None or tok[1] == value else None

    def take(self, value=None):
        tok = self.peek(value)
        if tok is None:
            raise ExprError(f"expected {value!r} at token {self.i}")
        self.i += 1
        return tok

    def parse(self):
        v = self.or_()
        if self.i != len(self.toks):
            raise ExprError(f"trailing tokens: {self.toks[self.i:]}")
        return v

    def or_(self):
        v = self.and_()
        while self.peek("||"):
            self.take()
            rhs = self.and_()
            v = v if truthy(v) else rhs
        return v

    def and_(self):
        v = self.cmp()
        while self.peek("&&"):
            self.take()
            rhs = self.cmp()
            v = rhs if truthy(v) else v
        return v

    def cmp(self):
        v = self.unary()
        while self.peek("==") or self.peek("!="):
            op = self.take()[1]
            rhs = self.unary()
            v = loose_eq(v, rhs) if op == "==" else not loose_eq(v, rhs)
        return v

    def unary(self):
        if self.peek("!"):
            self.take()
            return not truthy(self.unary())
        return self.postfix()

    def postfix(self):
        v = self.primary()
        while True:
            if self.peek("."):
                self.take()
                name = self.take()[1]
                v = v.get(name) if isinstance(v, dict) else None
            elif self.peek("["):
                self.take()
                key = self.or_()
                self.take("]")
                v = v.get(key) if isinstance(v, dict) else None
            else:
                return v

    def primary(self):
        kind, val = self.take()
        if val == "(":
            v = self.or_()
            self.take(")")
            return v
        if kind == "num":
            return float(val) if "." in val else int(val)
        if kind == "str":
            return val[1:-1].replace("''", "'")
        if kind != "ident":
            raise ExprError(f"unexpected token {val!r}")
        if val in ("true", "false"):
            return val == "true"
        if val == "null":
            return None
        if self.peek("("):
            fn = FUNCTIONS.get(val.lower())
            if fn is None:
                raise ExprError(f"unsupported function {val}()")
            self.take("(")
            args = []
            while not self.peek(")"):
                args.append(self.or_())
                if not self.peek(")"):
                    self.take(",")
            self.take(")")
            return fn(*args)
        if val not in self.ctx:
            raise ExprError(f"unsupported context {val!r}")
        return self.ctx[val]


def evaluate(template, ctx):
    """Evaluate a workflow value: a plain scalar or a string with ${{ }} parts."""
    if not isinstance(template, str):
        return template
    parts = re.split(r"\$\{\{(.*?)\}\}", template)
    if len(parts) == 3 and parts[0].strip() == "" and parts[2].strip() == "":
        return Parser(parts[1], ctx).parse()
    out = []
    for idx, part in enumerate(parts):
        out.append(part if idx % 2 == 0 else to_str(Parser(part, ctx).parse()))
    return "".join(out)


# --- workflow inspection -----------------------------------------------------

def load(path):
    data = yaml.safe_load(path.read_text())
    # PyYAML's YAML 1.1 loader reads the key `on` as True.
    events = data.get("on", data.get(True)) or {}
    if isinstance(events, str):
        events = {events: None}
    elif isinstance(events, list):
        events = {e: None for e in events}
    return data, events


def pr_types(events, name):
    cfg = events.get(name)
    if name not in events:
        return []
    if isinstance(cfg, dict) and "types" in cfg:
        return list(cfg["types"])
    return list(DEFAULT_PR_TYPES)


def context_for(workflow_name, event_name, action, run_id):
    return {
        "github": {
            "workflow": workflow_name,
            "event_name": event_name,
            "ref": f"refs/pull/{PR_NUMBER}/merge",
            "sha": MERGE_SHA,
            "run_id": run_id,
            "repository": "hivecommons/hive",
            "event": {
                "action": action,
                "number": PR_NUMBER,
                "pull_request": {
                    "number": PR_NUMBER,
                    "head": {"sha": HEAD_SHA, "repo": {"full_name": "hivecommons/hive"}},
                    "base": {"ref": "v5"},
                },
            },
        },
        "inputs": {},
        "vars": {},
    }


def same_head_runs(data, events):
    """Yield (event_name, action, run_id) for a push then a burst of re-checks."""
    run_id = 1000
    for event_name in PR_EVENTS:
        types = pr_types(events, event_name)
        if not any(a in types for a in RECHECK_ACTIONS):
            continue
        first = "synchronize" if "synchronize" in types else types[0]
        yield event_name, first, run_id
        for action in RECHECK_ACTIONS:
            if action in types:
                # Two of each: lgtm + approved land together on approval.
                for _ in range(2):
                    run_id += 1
                    yield event_name, action, run_id


def collisions(workflow_name, concurrency, runs):
    """Return a list of human-readable problems for one concurrency block."""
    if concurrency is None:
        return []
    if isinstance(concurrency, str):
        concurrency = {"group": concurrency}
    problems, seen = [], {}
    for event_name, action, run_id in runs:
        ctx = context_for(workflow_name, event_name, action, run_id)
        group = to_str(evaluate(concurrency["group"], ctx))
        if group in seen:
            other = seen[group]
            cancel = truthy(evaluate(concurrency.get("cancel-in-progress", False), ctx))
            how = "cancels it in flight" if cancel else "replaces it in the queue"
            problems.append(
                f"{event_name}/{action} (run {run_id}) shares group {group!r} with "
                f"{other[0]}/{other[1]} (run {other[2]}) on the same head SHA and {how}")
        else:
            seen[group] = (event_name, action, run_id)
    return problems


def required_jobs(data):
    for key, job in (data.get("jobs") or {}).items():
        name = job.get("name", key) if isinstance(job, dict) else key
        if name in REQUIRED_CONTEXTS:
            yield name, job


def audit(path):
    data, events = load(path)
    jobs = list(required_jobs(data))
    if not jobs:
        return None, []
    runs = list(same_head_runs(data, events))
    if not runs:
        return [n for n, _ in jobs], []
    name = data.get("name", path.name)
    problems = collisions(name, data.get("concurrency"), runs)
    for job_name, job in jobs:
        problems += [f"job {job_name!r}: {p}"
                     for p in collisions(name, job.get("concurrency"), runs)]
    return [n for n, _ in jobs], problems


# Migration flag. GitHub blocks `pull_request_target` workflows by default on
# public repositories from 2026-12-02, so once the hive posts `verify PR
# contents` as a commit status no workflow may provide a required context from
# that event. Until then pr-verifier.yml legitimately does, so the check below
# is only enforced when this is set to "1" (flip it in the same change that
# retires the legacy verifier trigger).
PR_TARGET_MIGRATION_DONE = os.environ.get("HIVE_PR_GATE_MIGRATION_DONE") == "1"


def pull_request_target_providers(data, events):
    """Required contexts a workflow provides while triggered by pull_request_target."""
    if "pull_request_target" not in events:
        return []
    return [name for name, _ in required_jobs(data)]


class PullRequestTargetMigration(unittest.TestCase):
    def test_detector_flags_a_required_context_on_pull_request_target(self):
        data = {"jobs": {"verify": {"name": "verify PR contents"}, "other": {"name": "lint-x"}}}
        self.assertEqual(
            pull_request_target_providers(data, {"pull_request_target": None}),
            ["verify PR contents"])
        self.assertEqual(pull_request_target_providers(data, {"pull_request": None}), [])
        self.assertEqual(
            pull_request_target_providers({"jobs": {"o": {"name": "lint-x"}}},
                                          {"pull_request_target": None}), [])

    def test_no_required_context_comes_from_pull_request_target(self):
        offenders, audited = {}, set()
        for path in sorted(WORKFLOWS.glob("*.yml")):
            data, events = load(path)
            if "pull_request_target" in events:
                audited.add(path.name)
            found = pull_request_target_providers(data, events)
            if found:
                offenders[path.name] = found
        # Positive control: the scan really sees pull_request_target workflows
        # and the legacy verifier, so an empty result is not a vacuous pass.
        self.assertIn("pr-verifier.yml", audited)
        self.assertIn("verify PR contents", offenders.get("pr-verifier.yml", []))
        if PR_TARGET_MIGRATION_DONE:
            self.assertEqual(offenders, {})


class RequiredCheckConcurrency(unittest.TestCase):
    def test_same_head_rechecks_never_share_a_group(self):
        audited = {}
        for path in sorted(WORKFLOWS.glob("*.yml")):
            with self.subTest(workflow=path.name):
                contexts, problems = audit(path)
                if contexts:
                    audited[path.name] = contexts
                self.assertEqual(problems, [], f"{path.name}: see #18578")
        # Positive control: the label-triggered required checks this guard
        # exists for must actually have been found and evaluated.
        for file, context in (
            ("pr-verifier.yml", "verify PR contents"),
            ("dco-squash-attribution.yml", "Sign-off survives the squash"),
            ("changelog-fragment-guard.yml", "changelog-fragment-guard"),
        ):
            self.assertIn(context, audited.get(file, []), file)

    def test_guard_flags_the_pre_18578_shapes(self):
        events = {"pull_request_target": {"types": ["opened", "synchronize", "labeled", "unlabeled"]}}
        runs = list(same_head_runs({}, events))
        # pr-verifier.yml before #18578: per-PR group, cancel on PR events.
        old_verifier = {
            "group": "${{ github.workflow }}-${{ github.event.pull_request.number || inputs.pr_number || github.ref }}",
            "cancel-in-progress": "${{ github.event_name == 'pull_request' || github.event_name == 'pull_request_target' }}",
        }
        problems = collisions("PR Verifier", old_verifier, runs)
        self.assertTrue(problems)
        self.assertIn("cancels it in flight", problems[0])
        # Head SHA in the group alone still lets label runs on that SHA cancel
        # each other.
        sha_only = {
            "group": "${{ github.workflow }}-${{ github.event.pull_request.number }}-${{ github.event.pull_request.head.sha }}",
            "cancel-in-progress": True,
        }
        self.assertTrue(collisions("PR Verifier", sha_only, runs))
        # changelog-fragment-guard before #18578: no cancel on labels, but a
        # shared group still drops queued runs.
        queued = {
            "group": "changelog-fragment-guard-${{ github.event.pull_request.number }}",
            "cancel-in-progress": "${{ github.event.action == 'synchronize' }}",
        }
        problems = collisions("changelog-fragment-guard", queued, runs)
        self.assertTrue(problems)
        self.assertIn("replaces it in the queue", problems[0])

    def test_head_moving_events_still_share_the_per_sha_group(self):
        # The run_id suffix is for re-checks only: the push-triggered run keeps
        # the plain per-PR, per-SHA group.
        data, _ = load(WORKFLOWS / "pr-verifier.yml")
        ctx = context_for("PR Verifier", "pull_request_target", "synchronize", 1)
        self.assertEqual(evaluate(data["concurrency"]["group"], ctx),
                         f"PR Verifier-{PR_NUMBER}-{HEAD_SHA}")
        ctx = context_for("PR Verifier", "pull_request_target", "labeled", 7)
        self.assertEqual(evaluate(data["concurrency"]["group"], ctx),
                         f"PR Verifier-{PR_NUMBER}-{HEAD_SHA}-labeled-7")
        self.assertIs(data["concurrency"]["cancel-in-progress"], False)

    def test_evaluator_rejects_what_it_cannot_model(self):
        with self.assertRaises(ExprError):
            evaluate("${{ hashFiles('x') }}", context_for("w", "pull_request", "opened", 1))
        with self.assertRaises(ExprError):
            evaluate("${{ needs.a.outputs.b }}", context_for("w", "pull_request", "opened", 1))


if __name__ == "__main__":
    unittest.main()
