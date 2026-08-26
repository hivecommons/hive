#!/usr/bin/env bash
# The SELinux label reader is resolved, never assumed (#4490).
# Run: bash src/deploy/test_selinux_label_reader.sh
#
# WHY THIS EXISTS. src/deploy/qualify_podman_selinux.sh — the release
# qualification, deliberately not CI because hosted runners have no SELinux
# (#4211/#4337) — read labels with bare `stat -c '%C'`. Under uutils coreutils,
# the default on the Fedora-atomic hosts that lane targets, `stat` has no %C:
# it prints "unsupported for this operating system" to STDOUT and exits 0, so
# the sentence was compared against container_file_t and a healthy enforcing
# host was reported FAIL (2), ledger row included. The sibling scripts
# (probe_podman_selinux_avc.sh, bin/hive-podman-preflight-host.sh) had already
# documented and hardened against exactly this; the fix extracts that reader
# into src/deploy/selinux_label_reader.sh so it cannot drift again.
#
# The defect is in how the label is READ, so it reproduces anywhere uutils
# shadows GNU — including a runner with no SELinux at all. That is why this
# half is the half that can be covered in CI.
set -uo pipefail

PASS=0
FAIL=0
pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "  FAIL: $1"; [ $# -gt 1 ] && echo "        $2"; FAIL=$((FAIL + 1)); }
assert_eq() { # desc want got
  if [ "$3" = "$2" ]; then pass "$1"; else fail "$1" "want '$2', got '$3'"; fi
}

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
HELPER="${ROOT}/src/deploy/selinux_label_reader.sh"

echo "=== SELinux label reader is resolved, never assumed (#4490) ==="

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
STUB="${WORK}/bin"; mkdir -p "$STUB"
PROBE="${WORK}/probe.txt"
printf 'probe' >"$PROBE"

REAL_CONTEXT="unconfined_u:object_r:user_home_t:s0"

# The uutils shape: the error goes to STDOUT and the exit status is 0, so
# neither `||` nor a non-empty test can catch it. Only shape-validation can.
cat >"${STUB}/uustat" <<'EOF'
#!/usr/bin/env bash
printf 'unsupported for this operating system\n'
exit 0
EOF
# A working GNU-shaped reader.
cat >"${STUB}/gnustat" <<EOF
#!/usr/bin/env bash
printf '%s\n' "${REAL_CONTEXT}"
exit 0
EOF
# Exits 0 with chatter that is not a context — a reader must be rejected on
# shape, not trusted on status.
cat >"${STUB}/chatty" <<'EOF'
#!/usr/bin/env bash
printf 'stat: cannot read security context\n'
exit 0
EOF
chmod +x "${STUB}"/uustat "${STUB}"/gnustat "${STUB}"/chatty

run_case() {
  # $1 readers, $2 path; prints "<rc>|<reader>|<label>"
  # shellcheck disable=SC2016 # the inner script expands in the child bash
  env PATH="${STUB}:${PATH}" HIVE_SELINUX_LABEL_READERS="$1" bash -c '
    set -u
    . "$1"
    if hive_selinux_resolve_label_reader "$2"; then rc=0; else rc=$?; fi
    printf "%s|%s|%s" "$rc" "$HIVE_SELINUX_LABEL_READER" "$(hive_selinux_label_of "$2")"
  ' _ "$HELPER" "$2"
}

# --- The Bluefin/Universal Blue shape: uutils first, GNU still reachable -----
OUT="$(run_case "uustat -c %C,gnustat -c %C" "$PROBE")"
IFS='|' read -r rc reader label <<<"$OUT"
assert_eq "uutils-with-fallback resolves a reader" "0" "$rc"
assert_eq "the fallback is the one resolved, not the uutils shape" "gnustat -c %C" "$reader"
assert_eq "label_of returns the real context" "$REAL_CONTEXT" "$label"
case "$label" in
  *"unsupported for this operating system"*)
    fail "the uutils error string never reads as a label" "got: $label" ;;
  *) pass "the uutils error string never reads as a label" ;;
esac

# --- uutils alone: no label to be had, and saying so is the only honest answer
OUT="$(run_case "uustat -c %C" "$PROBE")"
IFS='|' read -r rc reader label <<<"$OUT"
if [ "$rc" != "0" ]; then
  pass "no working reader resolves to failure, not a verdict"
else
  fail "no working reader resolves to failure, not a verdict" "rc=$rc reader=$reader"
fi
if [ -z "$reader" ] && [ -z "$label" ]; then
  pass "no working reader leaves both reader and label empty"
else
  fail "no working reader leaves both reader and label empty" "reader='$reader' label='$label'"
fi

# --- Exit status is not trusted: shape decides ------------------------------
OUT="$(run_case "chatty -c %C,gnustat -c %C" "$PROBE")"
IFS='|' read -r rc reader label <<<"$OUT"
assert_eq "a zero-exit non-context answer is rejected on shape" "gnustat -c %C" "$reader"

# --- A missing command is skipped, not fatal --------------------------------
OUT="$(run_case "no-such-reader -c %C,gnustat -c %C" "$PROBE")"
IFS='|' read -r rc reader label <<<"$OUT"
assert_eq "an absent candidate is skipped (rc)" "0" "$rc"
assert_eq "an absent candidate is skipped (reader)" "gnustat -c %C" "$reader"

# --- The reader does not launder a wrong label ------------------------------
# The qualification must still FAIL on a genuinely mislabeled host: a resolved
# reader returns the label VERBATIM, so user_home_t where container_file_t is
# expected still reads as user_home_t and the verdict logic sees the truth.
OUT="$(run_case "gnustat -c %C" "$PROBE")"
IFS='|' read -r rc reader label <<<"$OUT"
assert_eq "a wrong label is reported verbatim (a mislabeled host still fails)" \
  "$REAL_CONTEXT" "$label"

# --- The consumers actually consume it --------------------------------------
QUAL="${ROOT}/src/deploy/qualify_podman_selinux.sh"
PROBE_SH="${ROOT}/src/deploy/probe_podman_selinux_avc.sh"

if grep -q 'selinux_label_reader\.sh' "$QUAL"; then
  pass "the qualification sources the shared reader"
else
  fail "the qualification sources the shared reader"
fi
if grep -q 'selinux_label_reader\.sh' "$PROBE_SH"; then
  pass "the AVC probe sources the shared reader"
else
  fail "the AVC probe sources the shared reader"
fi
# %C may appear in comments; a bare command substitution of it may not.
if grep -qE "\\\$\\(stat -c '%C'" "$QUAL" "$PROBE_SH"; then
  fail "no bare stat -c %C label read remains" "$(grep -nE "\\\$\\(stat -c '%C'" "$QUAL" "$PROBE_SH")"
else
  pass "no bare stat -c %C label read remains"
fi
if grep -A1 'hive_selinux_resolve_label_reader "' "$QUAL" | grep -q 'not_qualifiable'; then
  pass "an unreadable label is NOT QUALIFIABLE, never FAIL plus a ledger row"
else
  fail "an unreadable label is NOT QUALIFIABLE, never FAIL plus a ledger row"
fi

echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ] || exit 1
exit 0
