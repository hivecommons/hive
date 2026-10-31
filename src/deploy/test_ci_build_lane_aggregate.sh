#!/usr/bin/env bash
set -euo pipefail
python3 - <<'PY'
from pathlib import Path
import os
import re
import subprocess

workflow = Path(".github/workflows/v2-ci.yml").read_text()
aggregate = workflow.split("\n  build-and-test:\n", 1)[1].split("\n  create-issue-on-failure:", 1)[0]
assert "!cancelled()" in aggregate, "cancelled workflows must not become green"
body = aggregate.split("        run: |\n", 1)[1]
body = "\n".join(line[10:] for line in body.splitlines() if line.strip())
lanes = ("GO_RESULT", "CONTRACT_RESULT", "RUNTIME_RESULT")
for scope in ("success", "failure"):
    env = dict(os.environ, SCOPE_RESULT=scope, **dict.fromkeys(lanes, "success"))
    assert subprocess.run(["bash", "-c", body], env=env, capture_output=True).returncode == 0
    for lane in lanes:
        for state in ("failure", "skipped", "cancelled"):
            failing = dict(env, **{lane: state})
            assert subprocess.run(["bash", "-c", body], env=failing, capture_output=True).returncode != 0, (scope, lane, state)
for lane in ("build-go", "contract-tests", "runtime-tests"):
    job = re.split(r"\n  (?=\S)", workflow.split(f"\n  {lane}:\n", 1)[1])[0]
    assert "needs.required-context-scope.result != 'success'" in job, lane
print("build aggregate: full-lane scope fallback; failed/skipped/cancelled lanes rejected")
PY
