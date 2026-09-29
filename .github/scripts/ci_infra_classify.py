#!/usr/bin/env python3
"""Classify a failed CI job as runner infrastructure or code (#9664).

On 2026-09-29 about 160 of 371 failed jobs on the self-hosted runner pool
were infrastructure (a shared Go build cache, a full disk, a runner pod that
died), not code, and nothing said so. This sorts one failed job into
`infra:<class>` or `code` from its log and check-run annotations, using the
signature table in ci-infra-signatures.tsv (see that file for the format).

`code` is the default for anything the table does not recognise: it is the
answer that never triggers an automatic rerun.

Usage:
    ci_infra_classify.py --log FILE [--annotations FILE] [--signatures FILE]

--log is the raw job log as the Actions API returns it (a missing or empty
file is allowed: a runner that died never uploads one). --annotations is a
text file holding the job's annotation messages, one per line.

Prints exactly one line: `infra:<class>`, `derived` (the job only reports
that other jobs failed, e.g. a shard gate) or `code`.

Exit codes:
    0  classified (whatever the verdict)
    2  usage error, or an input file could not be read
    3  the signature table is invalid
"""

import argparse
import os
import re
import sys

EXIT_OK = 0
EXIT_USAGE = 2
EXIT_BAD_SIGNATURES = 3

VERDICT_CODE = "code"
VERDICT_DERIVED = "derived"
INFRA_PREFIX = "infra:"
# A signature row with this class marks a job that failed only because a job
# it aggregates failed (a shard gate). It is neither infra nor code: the
# rerun decision looks through it to the jobs it depends on.
DERIVED_CLASS = "derived"

SCOPE_STEP = "step"
SCOPE_ANNOTATION = "annotation"
SCOPE_SILENT_STEP = "silent-step"
SCOPES = (SCOPE_STEP, SCOPE_ANNOTATION, SCOPE_SILENT_STEP)

SIGNATURE_FIELDS = 3

DEFAULT_SIGNATURES = os.path.join(
    os.path.dirname(os.path.abspath(__file__)), "ci-infra-signatures.tsv"
)

# Actions log lines start with an RFC 3339 timestamp; the first line may carry
# a UTF-8 byte-order mark.
_TIMESTAMP = re.compile(r"^\ufeff?\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?Z ?")
# Lines of a `run:` script are echoed with this ANSI colour prefix.
_SCRIPT_ECHO = re.compile(r"^\x1b\[36;1m(.*?)(?:\x1b\[0m)?$")
_STEP_START = "##[group]Run "
_GROUP_END = "##[endgroup]"
_ERROR = "##[error]"
_POST_JOB = "Post job cleanup."
_PROCESS_EXIT = re.compile(r"^##\[error\]Process completed with exit code \d+\.$")


class SignatureError(ValueError):
    """The signature table could not be parsed."""


class Signature:
    """One row of the signature table."""

    def __init__(self, klass, scope, pattern, lineno):
        self.klass = klass
        self.scope = scope
        self.pattern = pattern
        self.lineno = lineno

    def __repr__(self):
        return "Signature(%s, %s, line %d)" % (self.klass, self.scope, self.lineno)


def parse_signatures(text):
    """Parse the TSV signature table. Raises SignatureError on a bad row."""
    sigs = []
    for lineno, raw in enumerate(text.splitlines(), start=1):
        line = raw.rstrip("\r")
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        parts = line.split("\t")
        if len(parts) != SIGNATURE_FIELDS:
            raise SignatureError(
                "line %d: want %d tab-separated fields, got %d"
                % (lineno, SIGNATURE_FIELDS, len(parts))
            )
        klass, scope, pattern = (p.strip() for p in parts)
        if not re.fullmatch(r"[a-z0-9][a-z0-9-]*", klass):
            raise SignatureError("line %d: bad class name %r" % (lineno, klass))
        if scope not in SCOPES:
            raise SignatureError("line %d: unknown scope %r" % (lineno, scope))
        try:
            compiled = re.compile(pattern, re.MULTILINE)
        except re.error as err:
            raise SignatureError("line %d: bad regex: %s" % (lineno, err))
        sigs.append(Signature(klass, scope, compiled, lineno))
    if not sigs:
        raise SignatureError("signature table has no rows")
    return sigs


def load_signatures(path=DEFAULT_SIGNATURES):
    with open(path, encoding="utf-8") as fh:
        return parse_signatures(fh.read())


def _strip(line):
    return _TIMESTAMP.sub("", line.rstrip("\r"))


class FailingStep:
    """The step that failed: its echoed script and what it printed."""

    def __init__(self, script, output):
        self.script = script
        self.output = output

    @property
    def silent(self):
        """True when the step printed nothing but its own exit-code error.

        A compiler diagnostic is also an ##[error] line (the Go problem
        matcher), so only the runner's "Process completed" line counts as
        silence.
        """
        for line in self.output:
            if _PROCESS_EXIT.match(line):
                return True
            if line.strip():
                return False
        return True


def failing_step(log_text):
    """Locate the failing step in a job log.

    The failing step is the one that printed the first `##[error]` line: it
    runs from the last `##[group]Run ` header before that line to the next
    step header (or post-job cleanup). A job that failed before any step ran
    (e.g. "Set up job" on a full disk) has no header, so the step starts at
    the top of the log. A log with no `##[error]` at all (empty, or cut off
    by a dying runner) yields the whole log as output and no script.
    """
    lines = [_strip(line) for line in log_text.splitlines()]
    first_error = next((i for i, l in enumerate(lines) if l.startswith(_ERROR)), None)
    if first_error is None:
        return FailingStep([], lines)
    start = 0
    for i in range(first_error, -1, -1):
        if lines[i].startswith(_STEP_START):
            start = i
            break
    end = len(lines)
    for i in range(first_error + 1, len(lines)):
        if lines[i].startswith(_STEP_START) or lines[i] == _POST_JOB:
            end = i
            break
    region = lines[start:end]

    script = []
    output_from = 0
    if region and region[0].startswith(_STEP_START):
        # The header, the echoed script, then shell/env lines up to the first
        # ##[endgroup]; everything after that is what the step printed.
        output_from = len(region)
        for i, line in enumerate(region):
            m = _SCRIPT_ECHO.match(line)
            if m:
                script.append(m.group(1))
            if line == _GROUP_END:
                output_from = i + 1
                break
        if not script:
            script.append(region[0][len(_STEP_START):])
    return FailingStep(script, region[output_from:])


def classify(log_text, annotations_text, signatures):
    """Return the verdict of the first matching signature, else `code`.

    A match is `infra:<class>`, or `derived` for the reserved class of the
    same name.
    """
    step = failing_step(log_text or "")
    step_text = "\n".join(step.output)
    script_text = "\n".join(step.script)
    for sig in signatures:
        if sig.scope == SCOPE_STEP:
            hit = sig.pattern.search(step_text)
        elif sig.scope == SCOPE_ANNOTATION:
            hit = sig.pattern.search(annotations_text or "")
        else:
            hit = step.silent and step.script and sig.pattern.search(script_text)
        if hit:
            if sig.klass == DERIVED_CLASS:
                return VERDICT_DERIVED
            return INFRA_PREFIX + sig.klass
    return VERDICT_CODE


def is_infra(verdict):
    return verdict.startswith(INFRA_PREFIX)


def _read_optional(path):
    if not path or not os.path.exists(path):
        return ""
    with open(path, encoding="utf-8", errors="replace") as fh:
        return fh.read()


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--log", required=True, help="raw job log file")
    parser.add_argument("--annotations", help="job annotation messages, one per line")
    parser.add_argument(
        "--signatures",
        default=os.environ.get("CI_INFRA_SIGNATURES") or DEFAULT_SIGNATURES,
        help="signature table (default: ci-infra-signatures.tsv beside this script)",
    )
    try:
        args = parser.parse_args(argv)
    except SystemExit as exc:
        return EXIT_OK if exc.code == 0 else EXIT_USAGE
    try:
        sigs = load_signatures(args.signatures)
    except SignatureError as err:
        print("ci-infra-classify: %s: %s" % (args.signatures, err), file=sys.stderr)
        return EXIT_BAD_SIGNATURES
    except OSError as err:
        print("ci-infra-classify: %s" % err, file=sys.stderr)
        return EXIT_USAGE
    try:
        log_text = _read_optional(args.log)
        annotations = _read_optional(args.annotations)
    except OSError as err:
        print("ci-infra-classify: %s" % err, file=sys.stderr)
        return EXIT_USAGE
    print(classify(log_text, annotations, sigs))
    return EXIT_OK


if __name__ == "__main__":
    sys.exit(main())
