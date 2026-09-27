#!/usr/bin/env bash
set -euo pipefail

if ! command -v tmux >/dev/null 2>&1; then
  echo "tmux not found; install tmux to run this opt-in smoke test" >&2
  exit 77
fi

session="hive-relay-paste-smoke-$$"
buffer="hive-relay-paste-smoke-buffer-$$"

cleanup() {
  tmux kill-session -t "$session" >/dev/null 2>&1 || true
  tmux delete-buffer -b "$buffer" >/dev/null 2>&1 || true
}
trap cleanup EXIT

tmux new-session -d -s "$session" "bash --noprofile --norc"
tmux set-option -t "$session" history-limit 20000 >/dev/null

send_and_assert() {
  local label="$1"
  local text="$2"
  tmux set-buffer -b "$buffer" -- "$text"
  tmux paste-buffer -p -d -b "$buffer" -t "$session"
  sleep 1
  local pane
  pane="$(tmux capture-pane -t "$session" -p -S -20000)"
  local prefix="${text:0:40}"
  local suffix="${text: -40}"
  case "$pane" in
    *"$prefix"*"$suffix"*) ;;
    *)
      echo "bracketed paste smoke failed for $label prompt" >&2
      exit 1
      ;;
  esac
  tmux send-keys -t "$session" C-c
}

short_prompt="HIVE_SHORT_PASTE_OK"
long_prompt="$(node -e "process.stdout.write('HIVE_LONG_PASTE_OK ' + 'padding '.repeat(850) + 'HIVE_LONG_PASTE_END')")"

send_and_assert short "$short_prompt"
send_and_assert long "$long_prompt"

echo "tmux bracketed paste smoke passed"
