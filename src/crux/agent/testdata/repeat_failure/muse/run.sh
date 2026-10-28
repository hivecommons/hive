#!/usr/bin/env bash
# Repeat-failure harness for muse (hivecommons/hive#14749).
#
# Runs the real Muse Code CLI headless (`muse exec`) against a stub model
# provider (../stub_provider.py, OpenAI Responses format) that asks for one
# `bash` call, `ls /nonexistent`, then copies the two session-log lines Hive's
# muse route reads (the `assistant_tool_calls_committed` event carrying the
# bash call and the `tool_result_batch_committed` event carrying its result,
# from $XDG_DATA_HOME/muse/sessions/YYYY/MM/DD/<id>/session.jsonl) into
# session.jsonl, and the CLI's own `muse --version` into version.txt.
# TestRepeatFailureMuseHarness replays session.jsonl through the route and
# expects exactly one failing record.
#
# MUSE_BIN picks the CLI (default: muse on PATH). Needs python3 and jq. Runs
# in a throwaway HOME with the XDG variables unset; touches nothing else.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
work="$here/.run"
rm -rf "$work"
mkdir -p "$work/home" "$work/cwd"
trap 'rm -rf "$work"' EXIT
bin="${MUSE_BIN:-muse}"

RF_EXTRA_ARGS='{"description":"run the command"}' timeout 200 python3 "$here/../stub_provider.py" "$work/port" &
stub=$!
trap 'kill "$stub" 2>/dev/null; rm -rf "$work"' EXIT
for _ in $(seq 50); do [ -s "$work/port" ] && break; sleep 0.1; done
port=$(cat "$work/port")

run() {
  env -u XDG_DATA_HOME -u XDG_STATE_HOME -u XDG_CONFIG_HOME -u XDG_CACHE_HOME \
    HOME="$work/home" META_API_KEY=stub "$@"
}
version=$(run "$bin" --version | head -1)
(cd "$work/cwd" && run timeout 150 "$bin" exec --base-url "http://127.0.0.1:$port/v1" --model rf-stub --yolo \
  "run the command" >/dev/null 2>&1)

log=$(find "$work/home/.local/share/muse/sessions" -mindepth 5 -maxdepth 5 -name session.jsonl | sed -n 1p)
if [ -z "$log" ]; then
  echo "run.sh: muse wrote no session log" >&2
  exit 1
fi
jq -c 'select(
  (.payload.event.kind == "assistant_tool_calls_committed" and any(.payload.event.tool_calls[]; .name == "bash")) or
  (.payload.event.kind == "tool_result_batch_committed" and any(.payload.event.results[]; .tool_call_id == "call_rf_1")))' \
  "$log" > "$work/keep.jsonl"
if [ "$(wc -l < "$work/keep.jsonl")" -ne 2 ]; then
  echo "run.sh: muse did not log one bash call and its result (got $(wc -l < "$work/keep.jsonl"))" >&2
  exit 1
fi
cp "$work/keep.jsonl" "$here/session.jsonl"
echo "$version" > "$here/version.txt"
echo "recorded $version: $(wc -l < "$here/session.jsonl") session-log lines"
