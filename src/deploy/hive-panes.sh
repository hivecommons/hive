#!/bin/bash
# hive-panes — show the last N lines of every OTHER agent's tmux pane.
# Uses pluk JSONL logs (readable by the shared node group) so any agent can call it.
# Automatically skips the calling agent's own session (via HIVE_PROXY_AGENT).
#
# Usage:  hive-panes [lines]    (default: 30)

LINES="${1:-30}"
SELF="${HIVE_PROXY_AGENT:-}"
LOG_DIR="/var/run/pluk/logs"

if [ ! -d "$LOG_DIR" ]; then
  echo "ERROR: pluk log directory not found at $LOG_DIR" >&2
  exit 1
fi

for logfile in "$LOG_DIR"/hive-*.jsonl; do
  [ -f "$logfile" ] || continue
  session=$(basename "$logfile" .jsonl)
  agent="${session#hive-}"

  # Skip own session
  [ "$agent" = "$SELF" ] && continue

  echo "══════════════════════════════════════════════════════════"
  echo "  AGENT: $agent"
  echo "══════════════════════════════════════════════════════════"

  # Extract the last N raw_output lines, strip ANSI escapes, deduplicate blanks
  tail -n 2000 "$logfile" 2>/dev/null \
    | python3 -c "
import json, sys, re

# Strip ANSI escape sequences but preserve spaces
ansi_re = re.compile(r'\x1b\[[0-9;]*[A-Za-z]|\x1b\][^\x07]*\x07|\x1b[^[]\S*|\r')
# Collapse cursor-position sequences that eat whitespace
cursor_re = re.compile(r'\x1b\[\d+G')
lines = []
for raw in sys.stdin:
    raw = raw.strip()
    if not raw:
        continue
    try:
        entry = json.loads(raw)
    except:
        continue
    if entry.get('type') != 'raw_output':
        continue
    line = entry.get('data', {}).get('line', '')
    # Replace cursor position escapes with a space before stripping
    line = cursor_re.sub(' ', line)
    clean = ansi_re.sub('', line).rstrip()
    if clean:
        lines.append(clean)

for line in lines[-int(sys.argv[1]):]:
    print(line)
" "$LINES" 2>/dev/null

  echo ""
done
