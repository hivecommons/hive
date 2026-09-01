#!/usr/bin/env bash
# #5388 item 3: the guard on the guard.
#
# test_lib.sh only helps if the suites that claim to use it ACTUALLY become
# fatal under HIVE_TEST_REQUIRE_BEHAVIOURAL=1. A suite could source the lib and
# still `exit 0` on a skip — and it would look adopted while asserting nothing.
# That is the exact defect class #5388 is about, reproduced one level up.
#
# So this does not grep for adoption. It EXECUTES each registered suite in a
# deliberately unsuitable environment and requires:
#
#   flag unset → exit 0   (a correct skip must stay permissive)
#   flag=1     → exit 1   (the same skip must be fatal)
#
# The second half is the load-bearing one: a suite that exits 0 under the flag
# has a skip nobody acts on, which is what the whole issue is about. Note the
# trap this deliberately avoids — an inline `grep -l hive_test_skip src/deploy/*`
# would pass on a suite that sources the lib and never reaches the call, and a
# `find ... -print -quit` style probe exits 0 on NO match, greening while
# asserting nothing (#5435). Here the suite's own exit status is the assertion.
#
# Run: bash src/deploy/test_skip_discipline.sh
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=src/deploy/test_lib.sh
. "${HERE}/test_lib.sh"

# This suite tests the discipline itself, so it must never inherit the flag from
# an outer lane — its whole method is running children with the flag OFF and ON.
unset HIVE_TEST_REQUIRE_BEHAVIOURAL

echo "=== #5388: a skip is fatal where the caller guarantees the precondition ==="

# ── 1. test_lib.sh's own contract ────────────────────────────────────────────
#
# Exercised directly, because everything below inherits it. A subshell per case
# so the PASS/FAIL counters of this suite are not disturbed.

lib_case() {
  # lib_case <flag> <expected-exit> <expected-substring> <label>
  local flag="$1" want_rc="$2" want_str="$3" label="$4" out rc
  out="$(
    HIVE_TEST_REQUIRE_BEHAVIOURAL="$flag" bash -c '
      . "$1/test_lib.sh"
      hive_test_pass "a real assertion ran"
      hive_test_skip "synthetic precondition missing"
      hive_test_report
    ' _ "$HERE" 2>&1
  )"
  rc=$?
  if [ "$rc" = "$want_rc" ] && printf '%s' "$out" | grep -q "$want_str"; then
    hive_test_pass "$label"
  else
    hive_test_fail "$label" "want rc=$want_rc containing '$want_str'; got rc=$rc: $(printf '%s' "$out" | tr '\n' '|')"
  fi
}

lib_case "0" 0 "SKIP: synthetic precondition missing" \
  "flag unset: hive_test_skip is permissive and the suite exits 0"
lib_case "1" 1 "FAIL: synthetic precondition missing" \
  "flag=1: the same skip is fatal and the suite exits 1"

# Anti-vacuity: a suite that skips NOTHING must stay green under the flag, or
# every lane we set the flag in would go red for the wrong reason.
out="$(
  HIVE_TEST_REQUIRE_BEHAVIOURAL=1 bash -c '
    . "$1/test_lib.sh"; hive_test_pass "ran"; hive_test_report' _ "$HERE" 2>&1
)"
if [ $? -eq 0 ]; then
  hive_test_pass "flag=1 with no skip stays green (the flag is not a blanket failure)"
else
  hive_test_fail "flag=1 with no skip stays green" "got: $out"
fi

# ── 2. Every registered suite honours the flag, proven by running it ─────────
#
# Each entry names a suite and an environment that FORCES its behavioural block
# to skip. Suites are run with PATH stripped of the tool they need, which is the
# honest way to reach the skip path on a machine that has the tool.
#
# ADDING A SUITE: source test_lib.sh, route its skip through hive_test_skip,
# and add a line here. An unregistered suite is not covered — same failure mode
# as an unregistered test in v2-ci.yml.

# ── Denying a tool honestly ──────────────────────────────────────────────────
#
# Deleting PATH entries would take the whole of /usr/bin with it — dirname and
# mktemp included — and a suite would die at line 1 for an unrelated reason,
# which is a false red. A stub that resolves but exits non-zero is also wrong:
# suites that probe with `command -v` sail past their guard and fail later, and
# that misreports as "this suite does not honour the flag".
#
# Shadowing with a stub does NOT work, and it is worth recording why: a stub
# that exits non-zero still RESOLVES, so `command -v <tool>` succeeds and the
# suite sails past its own guard; and a non-executable placeholder is simply
# skipped by the PATH search, which then finds the real binary further along.
# Both were tried. The only thing that makes a lookup genuinely fail is a PATH
# that does not contain the tool.
#
# So: build ONE directory of symlinks to everything on PATH, then per case
# build a directory of symlinks INTO that farm, omitting the denied tools, and
# run the child with PATH set to it alone. Symlinking (not copying) keeps each
# case cheap; every unrelated utility still resolves normally.
SANDBOX_ROOT="$(mktemp -d)"
trap 'rm -rf "$SANDBOX_ROOT"' EXIT

REAL_PYTHON3="$(command -v python3 || true)"

FULL_BIN="${SANDBOX_ROOT}/full"
mkdir -p "$FULL_BIN"
while IFS= read -r _d; do
  [ -d "$_d" ] || continue
  for _entry in "$_d"/*; do
    [ -x "$_entry" ] || continue
    _name="${_entry##*/}"
    [ -e "${FULL_BIN}/${_name}" ] && continue
    ln -s "$_entry" "${FULL_BIN}/${_name}" 2>/dev/null || true
  done
done <<<"$(printf '%s' "$PATH" | tr ':' '\n')"

if [ ! -x "${FULL_BIN}/bash" ] || [ ! -x "${FULL_BIN}/mktemp" ]; then
  hive_test_fail "could not build a PATH sandbox" \
    "bash or mktemp missing under ${FULL_BIN} — every case below would be a false red"
  hive_test_report; exit $?
fi

# A yaml.py that raises on import, shadowing the real PyYAML via PYTHONPATH.
mkdir -p "${SANDBOX_ROOT}/yamlblock"
printf 'raise ImportError("PyYAML masked by test_skip_discipline.sh")\n' \
  >"${SANDBOX_ROOT}/yamlblock/yaml.py"

_case_n=0

# check_suite <suite> <how-the-precondition-is-denied> [tool ...]
check_suite() {
  local suite="$1" how="$2"; shift 2
  local bindir entry name denied out rc_off rc_on tool

  _case_n=$((_case_n + 1))
  bindir="${SANDBOX_ROOT}/case${_case_n}"
  mkdir -p "$bindir"

  # Symlink the farm into this case, omitting anything denied by name.
  for entry in "$FULL_BIN"/*; do
    name="${entry##*/}"
    denied=0
    for tool in "$@"; do
      [ -n "$tool" ] && [ "$tool" != "NO_PYYAML" ] && [ "$name" = "$tool" ] && denied=1
    done
    [ "$denied" -eq 1 ] && continue
    ln -s "$entry" "${bindir}/${name}" 2>/dev/null || true
  done

  # The pseudo-tool NO_PYYAML denies the MODULE, not the interpreter: python3
  # stays on PATH and works, but `import yaml` fails. This axis matters because
  # these suites gate twice — once on python3, then on PyYAML — and denying
  # python3 outright exits at the FIRST gate, leaving the second one untested.
  # A regression in the PyYAML branch would then pass this guard.
  for tool in "$@"; do
    [ "$tool" = "NO_PYYAML" ] || continue
    if [ -z "$REAL_PYTHON3" ]; then
      hive_test_skip "${suite}: no python3 here to build a PyYAML-less sandbox"
      return 0
    fi
    rm -f "${bindir}/python3"
    {
      printf '#!/bin/sh\n'
      printf 'PYTHONPATH="%s:${PYTHONPATH:-}" exec "%s" "$@"\n' \
        "${SANDBOX_ROOT}/yamlblock" "$REAL_PYTHON3"
    } >"${bindir}/python3"
    chmod +x "${bindir}/python3"
  done

  if [ ! -f "${HERE}/${suite}" ]; then
    hive_test_fail "${suite} is registered but does not exist" \
      "a renamed or deleted suite silently stops being covered"
    return 0
  fi

  # Anti-vacuity: if a denial did not take, this case would assert nothing.
  for tool in "$@"; do
    [ -z "$tool" ] && continue
    if [ "$tool" = "NO_PYYAML" ]; then
      # The interpreter must still WORK and `import yaml` must still FAIL —
      # either half being wrong makes this case assert nothing.
      if ! PATH="$bindir" python3 -c 'print(1)' >/dev/null 2>&1; then
        hive_test_fail "${suite}: python3 is broken in the NO_PYYAML sandbox" \
          "the suite would exit at the python3 gate, not the PyYAML one"
        return 0
      fi
      if PATH="$bindir" python3 -c 'import yaml' >/dev/null 2>&1; then
        hive_test_fail "${suite}: PyYAML is still importable in the sandbox" \
          "the denial did not take, so this case proves nothing"
        return 0
      fi
      continue
    fi
    if PATH="$bindir" command -v "$tool" >/dev/null 2>&1; then
      hive_test_fail "${suite}: '${tool}' is still reachable in the sandbox" \
        "the denial did not take, so this case proves nothing"
      return 0
    fi
  done

  out="$(PATH="$bindir" HIVE_TEST_REQUIRE_BEHAVIOURAL=0 \
         bash "${HERE}/${suite}" 2>&1)"; rc_off=$?
  out="$(PATH="$bindir" HIVE_TEST_REQUIRE_BEHAVIOURAL=1 \
         bash "${HERE}/${suite}" 2>&1)"; rc_on=$?

  if [ "$rc_off" -ne 0 ]; then
    hive_test_fail "${suite}: permissive run stays green when ${how}" \
      "exit ${rc_off} — a genuinely unsuitable environment must not fail by default"
    return 0
  fi
  hive_test_pass "${suite}: skips permissively (exit 0) when ${how}"

  if [ "$rc_on" -eq 0 ]; then
    hive_test_fail "${suite}: HIVE_TEST_REQUIRE_BEHAVIOURAL=1 makes the skip fatal" \
      "exit 0 under the flag — this suite's skip is still silent, so it is NOT adopted (#5388). Route it through hive_test_skip."
    return 0
  fi
  hive_test_pass "${suite}: the same skip is FATAL (exit ${rc_on}) under the flag"
}

echo
echo "--- registered suites ---"

# Deny python3 outright: the structural-assertion suites all gate on it.
check_suite test_entrypoint_system_gitconfig.sh   "git is absent"      git

# The two #5383 suites gate on being root, which cannot be denied by PATH — but
# this test process is not root anywhere it matters, so they reach the skip on
# their own. Running them unmodified is the migration's regression check.
check_suite test_entrypoint_runtime_config.sh   "the runner is not root"
check_suite test_entrypoint_data_ownership.sh   "the runner is not root"

hive_test_report
