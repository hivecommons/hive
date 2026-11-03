#!/usr/bin/env bash
# Repeat-failure harness for openhands (hivecommons/hive#14748).
#
# Runs the real OpenHands CLI headless against a stub model provider
# (../stub_provider.py) that asks for one `terminal` call, `ls /nonexistent`,
# then copies the two saved events Hive's openhands route reads (the terminal
# ActionEvent and its ObservationEvent, from
# ~/.openhands/conversations/<id>/events/) into events/, and the CLI's own
# `openhands --version` into version.txt. TestRepeatFailureOpenHandsHarness
# replays events/ through the route and expects exactly one failing record.
#
# Installs the pinned CLI (OPENHANDS_VERSION, default 1.16.0) with
# `uv tool install openhands --python 3.12`, as docs/backend-setup.md does,
# into the throwaway run directory; OPENHANDS_BIN uses an installed CLI
# instead. Needs uv (unless OPENHANDS_BIN is set), python3 and jq. Runs in a
# throwaway HOME; touches nothing else.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
work="$here/.run"
rm -rf "$work"
mkdir -p "$work/home" "$work/cwd"
trap 'rm -rf "$work"' EXIT

bin="${OPENHANDS_BIN:-}"
if [ -z "$bin" ]; then
  UV_TOOL_DIR="$work/uv/tools" UV_TOOL_BIN_DIR="$work/uv/bin" UV_PYTHON_INSTALL_DIR="$work/uv/python" \
    uv tool install -q "openhands==${OPENHANDS_VERSION:-1.16.0}" --python 3.12 >/dev/null
  bin="$work/uv/bin/openhands"
fi

RF_TOOL=terminal RF_EXTRA_ARGS='{"security_risk":"LOW"}' timeout 200 python3 "$here/../stub_provider.py" "$work/port" &
stub=$!
trap 'kill "$stub" 2>/dev/null; rm -rf "$work"' EXIT
for _ in $(seq 50); do [ -s "$work/port" ] && break; sleep 0.1; done
port=$(cat "$work/port")

version=$(HOME="$work/home" OPENHANDS_SUPPRESS_BANNER=1 "$bin" --version | grep -m1 'OpenHands CLI')
(cd "$work/cwd" && HOME="$work/home" OPENHANDS_SUPPRESS_BANNER=1 LLM_API_KEY=stub LLM_MODEL=openai/rf-stub \
  LLM_BASE_URL="http://127.0.0.1:$port/v1" timeout 150 \
  "$bin" --headless --always-approve --override-with-envs -t "run the command" >/dev/null 2>&1)

events=$(ls -d "$work/home/.openhands/conversations"/*/events | sed -n 1p)
keep=()
for f in "$events"/event-*.json; do
  if jq -e '.tool_name == "terminal" and (.kind == "ActionEvent" or .kind == "ObservationEvent")' "$f" >/dev/null; then
    keep+=("$f")
  fi
done
if [ "${#keep[@]}" -ne 2 ]; then
  echo "run.sh: openhands did not save one terminal action and observation (got ${#keep[@]})" >&2
  exit 1
fi
rm -rf "$here/events"
mkdir -p "$here/events"
for f in "${keep[@]}"; do jq . "$f" > "$here/events/$(basename "$f")"; done
echo "$version" > "$here/version.txt"
echo "recorded $version: $(ls "$here/events" | wc -l) events"
