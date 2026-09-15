#!/usr/bin/env bash
# Exercises ci-install-tool.sh's optional apt .deb cache with hermetic package
# manager test doubles. No real apt, dpkg, sudo, or network access is used.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SCRIPT="${ROOT}/.github/scripts/ci-install-tool.sh"
TMP_ROOT="${ROOT}/.github/.test-tmp"
mkdir -p "$TMP_ROOT"
TMP="$(mktemp -d "$TMP_ROOT/ci-install-tool-apt-cache.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT

failures=0
pass() { printf '  ok: %s\n' "$*"; }
bad() {
  printf '  FAIL: %s\n' "$*"
  failures=$((failures + 1))
}
assert_eq() {
  local got="$1" want="$2" ok_msg="$3" fail_msg="$4"
  if [ "$got" = "$want" ]; then
    pass "$ok_msg"
  else
    bad "$fail_msg"
  fi
}
assert_contains() {
  local haystack="$1" needle="$2" ok_msg="$3" fail_msg="$4"
  if printf '%s\n' "$haystack" | grep -qF "$needle"; then
    pass "$ok_msg"
  else
    bad "$fail_msg"
  fi
}

manifest() {
  local cache_dir="$1" require="$2" apt_pkgs="$3"
  mkdir -p "$cache_dir"
  {
    echo "version=1"
    echo "require=${require}"
    echo "apt_pkgs=${apt_pkgs}"
    echo "system=$(uname -s 2>/dev/null || echo unknown)"
    echo "machine=$(uname -m 2>/dev/null || echo unknown)"
  } > "${cache_dir}/.hive-ci-apt-cache.manifest"
}

install_fakes() {
  local bin_dir="$1"
  mkdir -p "$bin_dir"

  cat > "${bin_dir}/sudo" <<'SH'
#!/usr/bin/env bash
exec "$@"
SH

  cat > "${bin_dir}/dpkg" <<'SH'
#!/usr/bin/env bash
printf 'dpkg %s\n' "$*" >> "${HIVE_FAKE_LOG:?}"
for arg in "$@"; do
  case "$arg" in
    *corrupt*.deb) exit 42 ;;
  esac
done
if [ -n "${HIVE_FAKE_DPKG_INSTALLS:-}" ]; then
  cat > "${HIVE_FAKE_BIN:?}/${HIVE_FAKE_DPKG_INSTALLS}" <<EOF
#!/usr/bin/env bash
echo "${HIVE_FAKE_DPKG_INSTALLS} fake 1.0"
EOF
  chmod +x "${HIVE_FAKE_BIN}/${HIVE_FAKE_DPKG_INSTALLS}"
fi
exit 0
SH

  cat > "${bin_dir}/apt-get" <<'SH'
#!/usr/bin/env bash
printf 'apt-get %s\n' "$*" >> "${HIVE_FAKE_LOG:?}"
if [ "${HIVE_FAKE_APT_MODE:-fail}" = "fail" ]; then
  exit 124
fi

archives=""
download_only=0
for arg in "$@"; do
  case "$arg" in
    Dir::Cache::archives=*) archives="${arg#Dir::Cache::archives=}" ;;
    --download-only) download_only=1 ;;
  esac
done

if [ "$download_only" -eq 1 ]; then
  [ -n "$archives" ] || archives="${HIVE_FAKE_ARCHIVES:?}"
  mkdir -p "${archives}/partial"
  printf 'downloaded deb\n' > "${archives}/gcc_1_fake.deb"
  exit 0
fi

case " $* " in
  *" update "*) exit 0 ;;
esac

if [ -n "${HIVE_FAKE_APT_INSTALLS:-}" ]; then
  cat > "${HIVE_FAKE_BIN:?}/${HIVE_FAKE_APT_INSTALLS}" <<EOF
#!/usr/bin/env bash
echo "${HIVE_FAKE_APT_INSTALLS} fake 1.0"
EOF
  chmod +x "${HIVE_FAKE_BIN}/${HIVE_FAKE_APT_INSTALLS}"
fi
exit 0
SH

  chmod +x "${bin_dir}/sudo" "${bin_dir}/dpkg" "${bin_dir}/apt-get"
}

run_installer() {
  local work="$1"
  shift
  local fake_bin="${work}/bin"
  install_fakes "$fake_bin"
  HIVE_FAKE_BIN="$fake_bin" \
  HIVE_FAKE_LOG="${work}/commands.log" \
  PATH="${fake_bin}:${PATH}" \
  "$@" bash "$SCRIPT" \
    --require hive-fake-gcc --for '-race tests' \
    --apt 'gcc libc6-dev' --apk 'gcc musl-dev' --verify 'hive-fake-gcc --version'
}

scenario_cache_hit_offline_success() {
  printf '\n=== cache hit + simulated total egress failure succeeds ===\n'
  local work="${TMP}/cache-hit"
  local cache="${work}/cache"
  mkdir -p "$cache"
  printf 'good deb\n' > "${cache}/gcc_1_fake.deb"
  manifest "$cache" hive-fake-gcc 'gcc libc6-dev'

  local output rc
  set +e
  output=$(HIVE_CI_APT_CACHE_DIR="$cache" HIVE_FAKE_APT_MODE=fail HIVE_FAKE_DPKG_INSTALLS=hive-fake-gcc run_installer "$work" env 2>&1)
  rc=$?
  set -e
  printf '%s\n' "$output" | sed 's/^/    | /'

  assert_eq "$rc" 0 "installer succeeds from cached .debs" "installer exited ${rc}"
  if [ -s "${work}/commands.log" ] && grep -q '^apt-get ' "${work}/commands.log"; then
    bad "apt-get was called despite a complete cache hit"
    sed 's/^/    | commands: /' "${work}/commands.log"
  else
    pass "apt-get was not called"
  fi
  printf '    | command log:\n'
  sed 's/^/    |   /' "${work}/commands.log"
}

scenario_cache_miss_egress_failure() {
  printf '\n=== cache miss + simulated egress failure fails loudly ===\n'
  local work="${TMP}/cache-miss"
  local cache="${work}/cache"
  mkdir -p "$cache"

  local output rc
  set +e
  output=$(HIVE_CI_APT_CACHE_DIR="$cache" HIVE_CI_APT_ATTEMPTS=1 HIVE_CI_APT_DEADLINE_SECONDS=5 HIVE_FAKE_APT_MODE=fail run_installer "$work" env 2>&1)
  rc=$?
  set -e
  printf '%s\n' "$output" | sed 's/^/    | /'

  assert_eq "$rc" 1 "installer exits 1 on cache miss plus dead egress" "installer exited ${rc}, want 1"
  assert_contains "$output" "::error::hive-fake-gcc is required for -race tests and could not be installed" \
    "existing ::error:: remediation is preserved" "missing loud ::error:: remediation"
  assert_contains "$output" "Remediations, best first:" \
    "detailed remediation ladder is preserved" "missing remediation ladder"
}

scenario_tool_present_noop() {
  printf '\n=== tool already present skips apt and cache ===\n'
  local work="${TMP}/tool-present"
  local fake_bin="${work}/bin"
  mkdir -p "$fake_bin"
  cat > "${fake_bin}/hive-fake-gcc" <<'SH'
#!/usr/bin/env bash
echo "hive-fake-gcc already present"
SH
  chmod +x "${fake_bin}/hive-fake-gcc"
  install_fakes "$fake_bin"

  local cache="${work}/cache"
  local output rc
  set +e
  output=$(HIVE_FAKE_BIN="$fake_bin" HIVE_FAKE_LOG="${work}/commands.log" HIVE_CI_APT_CACHE_DIR="$cache" HIVE_FAKE_APT_MODE=fail PATH="${fake_bin}:${PATH}" bash "$SCRIPT" --require hive-fake-gcc --for '-race tests' --apt 'gcc libc6-dev' --verify 'hive-fake-gcc --version' 2>&1)
  rc=$?
  set -e
  printf '%s\n' "$output" | sed 's/^/    | /'

  assert_eq "$rc" 0 "installer exits 0 when the tool is already present" "installer exited ${rc}"
  if [ -s "${work}/commands.log" ]; then
    bad "package-manager double was called on no-op path"
    sed 's/^/    | commands: /' "${work}/commands.log"
  else
    pass "no apt, dpkg, or cache install work was attempted"
  fi
  if [ ! -e "$cache" ]; then
    pass "cache directory was not created on no-op path"
  else
    bad "cache directory was touched on no-op path"
  fi
}

scenario_corrupt_cache_falls_back() {
  printf '\n=== corrupt cache falls back to network path ===\n'
  local work="${TMP}/corrupt-cache"
  local cache="${work}/cache"
  mkdir -p "$cache"
  printf 'not a valid deb\n' > "${cache}/corrupt-gcc.deb"
  manifest "$cache" hive-fake-gcc 'gcc libc6-dev'

  local output rc
  set +e
  output=$(HIVE_CI_APT_CACHE_DIR="$cache" HIVE_CI_APT_ATTEMPTS=1 HIVE_FAKE_APT_MODE=success HIVE_FAKE_APT_INSTALLS=hive-fake-gcc run_installer "$work" env 2>&1)
  rc=$?
  set -e
  printf '%s\n' "$output" | sed 's/^/    | /'

  assert_eq "$rc" 0 "installer recovers through the apt network path" "installer exited ${rc}"
  assert_contains "$output" "cached .debs did not install cleanly; falling back to apt network path" \
    "bad cache is reported as fallback, not fatal" "missing bad-cache fallback message"
  if grep -q '^apt-get ' "${work}/commands.log"; then
    pass "apt-get was used after the corrupt cache failed"
  else
    bad "apt-get was not used after corrupt cache"
  fi
  if [ -f "${cache}/gcc_1_fake.deb" ]; then
    pass "successful download refreshed the cache directory"
  else
    bad "downloaded .deb was not copied into the cache directory"
  fi
  printf '    | command log:\n'
  sed 's/^/    |   /' "${work}/commands.log"
}

printf '=== ci-install-tool apt .deb cache tests ===\n'
scenario_cache_hit_offline_success
scenario_cache_miss_egress_failure
scenario_tool_present_noop
scenario_corrupt_cache_falls_back

printf '\nChecked 4 scenario(s), %d failure(s).\n' "$failures"
[ "$failures" -eq 0 ] || exit 1
