#!/usr/bin/env bash
# Exercises the real `just contribute-hive` container recipe against stubbed
# Docker and Podman CLIs. No image, daemon, credential, or network is used.
#
# The regression in #6485 was behavioral: the actual runtime argv had no
# resource ceiling, and exit 137 did not distinguish a cgroup OOM from another
# SIGKILL. Source-text checks can pass while flags sit outside the run command,
# lose their override value through word splitting, or inspect the wrong state.
#
# Run: bash src/deploy/test_contributor_container_resources.sh
set -uo pipefail

PASS=0
FAIL=0
# Shared skip discipline (#5388): CI guarantees `just`, so a missing binary
# there is a broken test rather than a permissible skip.
# shellcheck source=src/deploy/test_lib.sh
. "$(cd "$(dirname "$0")" && pwd)/test_lib.sh"
pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
fail() {
  echo "  FAIL: $1"
  [ $# -gt 1 ] && [ -n "${2:-}" ] && echo "        $2"
  FAIL=$((FAIL + 1))
}
check() {
  local label="$1" want="$2" got="$3"
  if [ "$want" = "$got" ]; then pass "$label"; else fail "$label" "want: '$want'  got: '$got'"; fi
}
contains() {
  local label="$1" haystack="$2" needle="$3"
  if grep -qF -- "$needle" <<<"$haystack"; then pass "$label"
  else fail "$label" "missing: '$needle'"; fi
}
lacks() {
  local label="$1" haystack="$2" needle="$3"
  if grep -qF -- "$needle" <<<"$haystack"; then fail "$label" "unexpectedly present: '$needle'"
  else pass "$label"; fi
}

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
if ! command -v just >/dev/null 2>&1; then
  hive_test_skip "'just' not installed; cannot exercise the recipe"
  hive_test_report; exit $?
fi

echo "=== contributor container resource limits (#6485) ==="

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
FAKE_HOME="$WORK/home"
STUB_BIN="$WORK/bin"
CAPTURE="$WORK/runtime-argv"
FAKE_RUNTIME_DIR="$WORK/runtime-dir"
mkdir -p "$FAKE_HOME/.config/hive" "$STUB_BIN" "$FAKE_RUNTIME_DIR"

printf '%s\n' \
  'HIVE_HUB=wss://hive.example.test/contribute' \
  'HIVE_REGISTRATION_TOKEN=placeholder-registration-token' \
  'AGENT_BACKEND=codex' \
  > "$FAKE_HOME/.config/hive/contributor.env"
printf '%s\n' 'GH_TOKEN=placeholder-github-token' \
  > "$FAKE_HOME/.config/hive/gh-auth.env"

# One implementation serves both runtime names. It records each run argument
# on its own line, making argument boundaries (and not just rendered text)
# observable. Inspect responses model startup state, final state, and OOMKilled.
cat > "$STUB_BIN/runtime" <<'EOF'
#!/usr/bin/env bash
case "${1:-}" in
  run)
    shift
    printf '%s\n' "$@" > "$RUNTIME_CAPTURE"
    echo stub-container-id
    ;;
  inspect)
    case "$*" in
      *'.State.Running'*)   echo "${STUB_RUNNING:-true}" ;;
      *'.State.ExitCode'*)  echo "${STUB_EXIT:-0}" ;;
      *'.State.OOMKilled'*) echo "${STUB_OOM:-false}" ;;
      *) exit 1 ;;
    esac
    ;;
  logs|rm|pull) exit 0 ;;
  *) exit 1 ;;
esac
EOF
chmod +x "$STUB_BIN/runtime"
ln -s runtime "$STUB_BIN/docker"
ln -s runtime "$STUB_BIN/podman"

cat > "$STUB_BIN/gh" <<'EOF'
#!/usr/bin/env bash
[ "${1:-} ${2:-}" = "api user" ] && echo test-contributor
exit 0
EOF
cat > "$STUB_BIN/sleep" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
chmod +x "$STUB_BIN/gh" "$STUB_BIN/sleep"

run_contributor() {
  local runtime="$1"
  shift
  (
    cd "$ROOT" || exit 1
    env -u HIVE_CONTAINER_MEMORY -u HIVE_CONTAINER_CPUS \
      HOME="$FAKE_HOME" PATH="$STUB_BIN:$PATH" \
      XDG_RUNTIME_DIR="$FAKE_RUNTIME_DIR" \
      RUNTIME_CAPTURE="$CAPTURE" HIVE_CONTAINER_RUNTIME="$runtime" \
      HIVE_SKIP_VERSION_CHECK=true HIVE_SKIP_PULL=true \
      "$@" just contribute-hive codex 2>&1
  )
}

arg_after() {
  local flag="$1"
  awk -v flag="$flag" '$0 == flag { getline; print; exit }' "$CAPTURE"
}

echo ""
echo "-- Docker defaults --"
OUT="$(run_contributor docker)"; RC=$?
check "recipe exits successfully" "0" "$RC"
check "default memory limit matches Kubernetes" "4g" "$(arg_after --memory)"
check "combined memory+swap is capped at the memory limit" "4g" "$(arg_after --memory-swap)"
check "default CPU limit matches Kubernetes" "2" "$(arg_after --cpus)"
contains "the active limits are visible to the operator" "$OUT" "Limits:    4g memory, 2 CPUs"

echo ""
echo "-- Podman operator overrides --"
OUT="$(run_contributor podman HIVE_CONTAINER_MEMORY=6g HIVE_CONTAINER_CPUS=3.5)"; RC=$?
check "recipe exits successfully with overrides" "0" "$RC"
check "memory override reaches Podman as one argument" "6g" "$(arg_after --memory)"
check "swap ceiling follows the memory override" "6g" "$(arg_after --memory-swap)"
check "fractional CPU override reaches Podman" "3.5" "$(arg_after --cpus)"
contains "the overridden limits are visible" "$OUT" "Limits:    6g memory, 3.5 CPUs"

echo ""
echo "-- unsupported-controller opt-outs --"
OUT="$(run_contributor podman HIVE_CONTAINER_MEMORY=none HIVE_CONTAINER_CPUS=none)"; RC=$?
check "recipe exits successfully with both limits omitted" "0" "$RC"
if grep -qxF -- '--memory' "$CAPTURE"; then
  fail "memory opt-out omits --memory"
else
  pass "memory opt-out omits --memory"
fi
if grep -qxF -- '--memory-swap' "$CAPTURE"; then
  fail "memory opt-out omits --memory-swap"
else
  pass "memory opt-out omits --memory-swap"
fi
if grep -qxF -- '--cpus' "$CAPTURE"; then
  fail "CPU opt-out omits --cpus"
else
  pass "CPU opt-out omits --cpus"
fi

echo ""
echo "-- runtime-confirmed startup OOM --"
OUT="$(run_contributor podman STUB_RUNNING=false STUB_EXIT=137 STUB_OOM=true)"; RC=$?
check "startup OOM exits non-zero" "1" "$RC"
contains "diagnostic identifies an OOM kill" "$OUT" "container was OOM-killed"
contains "diagnostic names the active memory limit" "$OUT" "Memory:    4g"
contains "diagnostic points to the override" "$OUT" "HIVE_CONTAINER_MEMORY"

echo ""
echo "-- exit 137 without runtime OOM evidence --"
OUT="$(run_contributor docker STUB_RUNNING=true STUB_EXIT=137 STUB_OOM=false)"; RC=$?
check "post-start exit keeps the recipe's existing status" "0" "$RC"
contains "diagnostic decodes exit 137 as SIGKILL" "$OUT" "SIGKILL (exit 137)"
contains "diagnostic points to host OOM evidence" "$OUT" "host's OOM logs"
lacks "diagnostic does not claim an unreported OOM" "$OUT" "container was OOM-killed"

hive_test_report
