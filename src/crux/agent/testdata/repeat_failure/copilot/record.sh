#!/usr/bin/env bash
# Repeat-failure harness for copilot (hivecommons/hive#14746).
#
# Makes the real Copilot CLI run one failing shell command and records the two
# session-log events Hive's copilot route reads (tool.execution_start and
# tool.execution_complete of that call) into events.jsonl, plus the CLI's own
# `copilot --version` into version.txt. TestRepeatFailureCopilotHarness replays
# events.jsonl through the route and expects exactly one failing record.
#
# Needs a logged-in copilot (uses ${COPILOT_HOME:-~/.copilot}) and jq.
# RF_COPILOT_MODEL picks the model (default claude-haiku-4.5).
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
command='ls /nonexistent'

state="${COPILOT_HOME:-$HOME/.copilot}/session-state"
before=$(ls "$state" 2>/dev/null || true)
copilot --version | sed -n 1p > "$here/version.txt.new"
copilot --no-auto-update --allow-all-tools --model "${RF_COPILOT_MODEL:-claude-haiku-4.5}" \
  -p "Run exactly one shell command with your bash tool, then stop: $command" >/dev/null 2>&1 || true

# The session this run created (not a session another copilot is writing).
session=$(ls "$state" | grep -vxF -e "$before" -e '' | sed -n 1p || true)
log="$state/$session/events.jsonl"
id=$(jq -r --arg c "$command" 'select(.type == "tool.execution_start" and .data.toolName == "bash" and .data.arguments.command == $c) | .data.toolCallId' "$log" | tail -n1)
if [ -z "$id" ]; then
  echo "record.sh: copilot did not run '$command' (session: ${session:-none})" >&2
  rm -f "$here/version.txt.new"
  exit 1
fi
jq -c --arg id "$id" 'select((.type == "tool.execution_start" or .type == "tool.execution_complete") and .data.toolCallId == $id)' "$log" > "$here/events.jsonl"
mv "$here/version.txt.new" "$here/version.txt"
echo "recorded $(cat "$here/version.txt"): $(wc -l < "$here/events.jsonl") events for toolCallId $id"
