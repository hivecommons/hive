#!/usr/bin/env bash
# Repeat-failure harness for omp (hivecommons/hive#14747).
#
# Runs the real omp CLI against a stub model provider (../stub_provider.py)
# that asks for one bash call, `ls /nonexistent`, with Hive's record extension
# loaded the way Hive loads it (`--extension`). omp_extension.ts is the
# extension exactly as Hive renders it (TestRepeatFailureOmpExtensionGolden);
# this script only fills in the record path. Writes `omp --version` and the
# record file the extension produced to evidence.txt, which
# TestRepeatFailureOmpHarnessEvidence checks.
#
# Needs omp and python3. Runs in a throwaway HOME; touches nothing else.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
work="$here/.run"
rm -rf "$work"
mkdir -p "$work/home/.omp/agent" "$work/cwd" "$work/records"
timeout 180 python3 "$here/../stub_provider.py" "$work/port" &
stub=$!
trap 'kill "$stub" 2>/dev/null; rm -rf "$work"' EXIT
for _ in $(seq 50); do [ -s "$work/port" ] && break; sleep 0.1; done
port=$(cat "$work/port")
cat > "$work/home/.omp/agent/models.yml" <<YML
providers:
  rfstub:
    baseUrl: http://127.0.0.1:$port/v1
    apiKey: stub
    api: openai-completions
    models:
      - id: rf-stub
        name: rf-stub
YML
record="$work/records/records.jsonl"
sed "s|__RF_RECORD_PATH__|$record|" "$here/omp_extension.ts" > "$work/omp_extension.ts"

version=$(HOME="$work/home" omp --version)
(cd "$work/cwd" && HOME="$work/home" OMP_SKIP_SETUP=1 timeout 120 \
  omp -p --model rfstub/rf-stub --approval-mode yolo --no-session \
  --extension "$work/omp_extension.ts" "run the command" >/dev/null)

{
  echo "# omp --version"
  echo "$version"
  echo "# stub provider: one bash call, ls /nonexistent"
  echo "# records.jsonl written by Hive's omp extension"
  cat "$record"
} > "$here/evidence.txt"
cat "$here/evidence.txt"
