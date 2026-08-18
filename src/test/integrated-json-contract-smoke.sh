#!/usr/bin/env sh
set -eu

hive="${1:?packaged Hive executable is required}"
work_root="${2:?writable JSON smoke root is required}"
mkdir -p "$work_root"

commands='setup
status
doctor
start
stop
daemon
installer-transition
pause
resume
run
hosted-cycle
approve-merge
approve-baseline
revoke-merge-approval
retry-repair
recover-dispatch
transfer-setup-authorizer
set-coverage
set-automation
set-issue-limit
set-retry-limit
upgrade
rollback
uninstall'

printf '%s\n' "$commands" | while IFS= read -r command; do
  stdout="$work_root/$command.stdout.json"
  stderr="$work_root/$command.stderr.log"
  if "$hive" "$command" --json --hive-json-contract-invalid >"$stdout" 2>"$stderr"; then
    echo "$command accepted a deliberately invalid packaged invocation" >&2
    exit 1
  fi
  python3 - "$stdout" "$command" <<'PY'
import json, pathlib, sys
path, command = pathlib.Path(sys.argv[1]), sys.argv[2]
raw = path.read_bytes()
try:
    text = raw.decode("utf-8")
except UnicodeDecodeError as error:
    raise SystemExit(f"{command} stdout is not UTF-8: {error}")
decoder = json.JSONDecoder()
try:
    value, end = decoder.raw_decode(text)
except Exception as error:
    raise SystemExit(f"{command} stdout is not JSON: {error}: {text!r}")
if text[end:].strip() or not isinstance(value, dict):
    raise SystemExit(f"{command} stdout is not exactly one JSON object: {text!r}")
PY
done

visual_stdout="$work_root/visual.stdout.json"
if "$hive" visual --json --hive-json-contract-invalid >"$visual_stdout" 2>"$work_root/visual.stderr.log"; then
  echo "visual accepted a deliberately invalid packaged invocation" >&2
  exit 1
fi
python3 - "$visual_stdout" <<'PY'
import json, pathlib, sys
text = pathlib.Path(sys.argv[1]).read_bytes().decode("utf-8")
decoder = json.JSONDecoder()
value, end = decoder.raw_decode(text)
if text[end:].strip() or not isinstance(value, dict):
    raise SystemExit("visual stdout is not exactly one JSON object")
PY

echo "Packaged Hive JSON contract smoke passed."
