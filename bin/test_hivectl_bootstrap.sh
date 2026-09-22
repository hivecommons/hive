#!/usr/bin/env bash
# Contract tests for bin/hivectl-bootstrap.sh (#8200).
# Run: bash bin/test_hivectl_bootstrap.sh

set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BOOTSTRAP="${ROOT}/bin/hivectl-bootstrap.sh"
JUSTFILE="${ROOT}/Justfile"
TEST_TMP="${ROOT}/.test-hivectl-bootstrap.$$"
trap 'rm -rf "$TEST_TMP"' EXIT

FAKE_BIN="${TEST_TMP}/fakebin"
CALL_LOG="${TEST_TMP}/podman.log"
RUN_LOG="${TEST_TMP}/hivectl.log"
mkdir -p "$FAKE_BIN"

pass_count=0
fail_count=0
CASE=""

pass() {
  printf '  ok: [%s] %s\n' "$CASE" "$1"
  pass_count=$((pass_count + 1))
}

fail() {
  printf '  FAIL: [%s] %s\n' "$CASE" "$1" >&2
  fail_count=$((fail_count + 1))
}

assert_contains() {
  local hay="$1" needle="$2" ctx="$3"
  if [[ "$hay" == *"$needle"* ]]; then pass "$ctx"; else fail "${ctx}: missing '${needle}'"; fi
}

assert_not_contains() {
  local hay="$1" needle="$2" ctx="$3"
  if [[ "$hay" != *"$needle"* ]]; then pass "$ctx"; else fail "${ctx}: unexpectedly found '${needle}'"; fi
}

assert_file_contains() {
  local file="$1" needle="$2" ctx="$3"
  if [[ -f "$file" ]] && grep -qF -- "$needle" "$file"; then
    pass "$ctx"
  else
    fail "${ctx}: '${needle}' not in ${file}"
  fi
}

make_fixture() {
  local dir="${TEST_TMP}/${CASE}"
  rm -rf "$dir"
  mkdir -p "${dir}/bin" "${dir}/home" "${dir}/src/pkg/hivectl/commands"
  cp "$BOOTSTRAP" "${dir}/bin/hivectl-bootstrap.sh"
  cp "${ROOT}/bin/hive-podman-cleanup.sh" "${dir}/bin/hive-podman-cleanup.sh"
  chmod +x "${dir}/bin/hivectl-bootstrap.sh"
  printf '%s\n' "$dir"
}

init_fixture_git() {
  local fixture="$1"
  git -C "$fixture" init -q
  git -C "$fixture" config user.email "test@example.invalid"
  git -C "$fixture" config user.name "Hive Test"
  printf 'package commands\n' >"${fixture}/src/pkg/hivectl/commands/tui.go"
  git -C "$fixture" add bin src
  git -C "$fixture" commit -q -m "initial hivectl"
}

advance_fixture_hivectl() {
  local fixture="$1"
  printf '\n// newer checkout hivectl change\n' >>"${fixture}/src/pkg/hivectl/commands/tui.go"
  git -C "$fixture" add src/pkg/hivectl/commands/tui.go
  git -C "$fixture" commit -q -m "update hivectl"
}

write_fake_hivectl() {
  local path="$1" label="$2"
  cat >"$path" <<EOF_HIVECTL
#!/usr/bin/env bash
if [[ "\${1:-}" == "version" ]]; then
  printf '%s version 1\n' "$label"
  exit 0
fi
printf '%s %s\n' "$label" "\$*" >>"$RUN_LOG"
EOF_HIVECTL
  chmod +x "$path"
}

cat >"${FAKE_BIN}/podman" <<'EOF_PODMAN'
#!/usr/bin/env bash
printf 'podman %s\n' "$*" >>"$FAKE_PODMAN_LOG"
is_unavailable() {
  local image="$1" unavailable="${FAKE_UNAVAILABLE_IMAGE:-}"
  [[ -n "$unavailable" && "$image" == "$unavailable" ]]
}
case "${1:-} ${2:-}" in
  "image inspect")
    [[ "${FAKE_IMAGE_INSPECT_FAIL:-0}" == "1" ]] && exit 125
    is_unavailable "${5:-}" && exit 125
    if [[ "${3:-}" == "--format" && "${4:-}" == *".Labels"* ]]; then
      printf '%s\n' "${FAKE_IMAGE_REVISION:-}"
      exit 0
    fi
    printf '%s\n' "${FAKE_IMAGE_DIGEST:-sha256:fresh}"
    ;;
  "pull "*)
    [[ "${FAKE_PULL_FAIL:-0}" == "1" ]] && exit 125
    is_unavailable "${2:-}" && exit 125
    ;;
  "create --name")
    printf 'ctr\n'
    ;;
  cp*)
    case "${2:-}" in
      *:/usr/local/share/hive/hivectl) ;;
      *) exit 125 ;;
    esac
    dest="${3:?missing destination}"
    cat >"$dest" <<EOF_STAGED
#!/usr/bin/env bash
if [[ "\${1:-}" == "version" ]]; then
  printf 'hivectl %s commit %s\n' "${FAKE_BINARY_VERSION:-staged}" "${FAKE_BINARY_COMMIT:-}"
  exit 0
fi
printf 'staged %s\n' "\$*" >>"$FAKE_HIVECTL_RUN_LOG"
EOF_STAGED
    chmod +x "$dest"
    ;;
  rm*)
    ;;
esac
EOF_PODMAN
chmod +x "${FAKE_BIN}/podman"

run_bootstrap() {
  local fixture="$1"
  shift
  (
    cd "$fixture" || exit 1
    HOME="${fixture}/home" \
    PATH="${FAKE_BIN}:/usr/bin:/bin" \
    FAKE_PODMAN_LOG="$CALL_LOG" \
    FAKE_HIVECTL_RUN_LOG="$RUN_LOG" \
    FAKE_IMAGE_DIGEST="${FAKE_IMAGE_DIGEST:-sha256:fresh}" \
    FAKE_IMAGE_REVISION="${FAKE_IMAGE_REVISION:-}" \
    FAKE_BINARY_VERSION="${FAKE_BINARY_VERSION:-staged}" \
    FAKE_BINARY_COMMIT="${FAKE_BINARY_COMMIT:-}" \
    FAKE_UNAVAILABLE_IMAGE="${FAKE_UNAVAILABLE_IMAGE:-}" \
    HIVECTL_BOOTSTRAP_IMAGE="${HIVECTL_BOOTSTRAP_IMAGE-test-image}" \
    "$fixture/bin/hivectl-bootstrap.sh" "$@"
  )
}

CASE="hivectl-override"
: >"$CALL_LOG"; : >"$RUN_LOG"
fixture="$(make_fixture)"
write_fake_hivectl "${fixture}/override-hivectl" override
(
  cd "$fixture" || exit 1
  HOME="${fixture}/home" \
  PATH="/usr/bin:/bin" \
  HIVECTL="${fixture}/override-hivectl" \
  HIVECTL_BOOTSTRAP_IMAGE="test-image" \
  "$fixture/bin/hivectl-bootstrap.sh" tui
) >"${fixture}/stdout" 2>"${fixture}/stderr"
status=$?
if [[ "$status" -eq 0 ]]; then pass "HIVECTL override exits successfully"; else fail "HIVECTL override exited ${status}"; fi
assert_file_contains "$RUN_LOG" "override tui" "override is used verbatim"
if [[ ! -s "$CALL_LOG" ]]; then pass "override bypasses podman and freshness checks"; else fail "override unexpectedly called podman"; fi
assert_file_contains "${fixture}/stderr" "explicit override bypasses image freshness checks" "override explains freshness bypass"

CASE="fresh-local"
: >"$CALL_LOG"; : >"$RUN_LOG"
fixture="$(make_fixture)"
write_fake_hivectl "${fixture}/bin/hivectl" local
cat >"${fixture}/bin/.hivectl.source" <<'EOF_SOURCE'
image=test-image
path=/usr/local/share/hive/hivectl
digest=sha256:fresh
EOF_SOURCE
run_bootstrap "$fixture" hives list >"${fixture}/stdout" 2>"${fixture}/stderr"
status=$?
if [[ "$status" -eq 0 ]]; then pass "fresh local binary exits successfully"; else fail "fresh local exited ${status}"; fi
assert_file_contains "$RUN_LOG" "local hives list" "fresh ./bin/hivectl is used"
assert_contains "$(cat "$CALL_LOG")" "podman image inspect --format {{.Digest}} test-image" "fresh local compares image digest"
assert_not_contains "$(cat "$CALL_LOG")" "podman create" "fresh local does not re-extract"

CASE="extract-missing"
: >"$CALL_LOG"; : >"$RUN_LOG"
fixture="$(make_fixture)"
run_bootstrap "$fixture" hives list >"${fixture}/stdout" 2>"${fixture}/stderr"
status=$?
if [[ "$status" -eq 0 ]]; then pass "missing binary bootstraps successfully"; else fail "missing binary exited ${status}"; fi
assert_contains "$(cat "$CALL_LOG")" "podman create --name" "missing binary creates extraction container"
assert_contains "$(cat "$CALL_LOG")" "--label io.kubestellar.hive.owned=true" "missing binary labels extraction container for teardown"
assert_contains "$(cat "$CALL_LOG")" "--label io.kubestellar.hive.component=hivectl-bootstrap" "missing binary labels extraction container component"
assert_contains "$(cat "$CALL_LOG")" "podman cp hive-hivectl-bootstrap-" "missing binary copies pinned image path"
assert_file_contains "${fixture}/bin/.hivectl.source" "digest=sha256:fresh" "missing binary records image digest"
assert_file_contains "$RUN_LOG" "staged hives list" "extracted binary handles command"

CASE="stale-local"
: >"$CALL_LOG"; : >"$RUN_LOG"
fixture="$(make_fixture)"
write_fake_hivectl "${fixture}/bin/hivectl" stale
cat >"${fixture}/bin/.hivectl.source" <<'EOF_OLD'
image=test-image
path=/usr/local/share/hive/hivectl
digest=sha256:old
EOF_OLD
FAKE_IMAGE_DIGEST="sha256:new" run_bootstrap "$fixture" hives list >"${fixture}/stdout" 2>"${fixture}/stderr"
status=$?
if [[ "$status" -eq 0 ]]; then pass "stale local refreshes successfully"; else fail "stale local exited ${status}"; fi
assert_contains "$(cat "$CALL_LOG")" "podman cp hive-hivectl-bootstrap-" "stale digest re-extracts"
assert_file_contains "${fixture}/bin/.hivectl.source" "digest=sha256:new" "stale refresh records new digest"
assert_file_contains "$RUN_LOG" "staged hives list" "refreshed binary handles command"


CASE="stale-installed"
: >"$CALL_LOG"; : >"$RUN_LOG"
fixture="$(make_fixture)"
mkdir -p "${fixture}/home/.local/bin"
write_fake_hivectl "${fixture}/home/.local/bin/hivectl" installed
run_bootstrap "$fixture" hives list >"${fixture}/stdout" 2>"${fixture}/stderr"
status=$?
if [[ "$status" -eq 0 ]]; then pass "stale installed binary refreshes successfully"; else fail "stale installed exited ${status}"; fi
assert_file_contains "${fixture}/stderr" "does not match test-image; staging a checkout-local copy instead" "stale installed binary is refused as-is"
assert_file_contains "$RUN_LOG" "staged hives list" "checkout-local image copy handles command instead of stale installed binary"
assert_file_contains "${fixture}/bin/.hivectl.source" "digest=sha256:fresh" "stale installed refresh records image digest"

CASE="no-podman-no-binary"
: >"$CALL_LOG"; : >"$RUN_LOG"
fixture="$(make_fixture)"
(
  cd "$fixture" || exit 1
  HOME="${fixture}/home" \
  PATH="/usr/bin:/bin" \
  HIVECTL_BOOTSTRAP_IMAGE="test-image" \
  "$fixture/bin/hivectl-bootstrap.sh" hives list
) >"${fixture}/stdout" 2>"${fixture}/stderr"
status=$?
if [[ "$status" -ne 0 ]]; then pass "missing podman and binary refuses"; else fail "missing podman unexpectedly succeeded"; fi
assert_file_contains "${fixture}/stderr" "podman is required to bootstrap" "refusal names podman requirement"

CASE="ahead-of-image-refuses"
: >"$CALL_LOG"; : >"$RUN_LOG"
fixture="$(make_fixture)"
init_fixture_git "$fixture"
binary_commit="$(git -C "$fixture" rev-parse HEAD)"
advance_fixture_hivectl "$fixture"
FAKE_BINARY_COMMIT="$binary_commit" run_bootstrap "$fixture" hives list >"${fixture}/stdout" 2>"${fixture}/stderr"
status=$?
if [[ "$status" -eq 66 ]]; then pass "ahead checkout refuses with skew exit"; else fail "ahead checkout exited ${status}, want 66"; fi
assert_file_contains "${fixture}/stderr" "refusing to run a skewed hivectl" "skew refusal names the condition"
assert_file_contains "${fixture}/stderr" "staged binary commit ${binary_commit}" "skew refusal names binary commit"
assert_file_contains "${fixture}/stderr" "you overrode the image with test-image" "skew refusal names selected image override"
assert_file_contains "${fixture}/stderr" "HIVECTL_BOOTSTRAP_IMAGE=" "skew refusal names image override escape hatch"
assert_file_contains "${fixture}/stderr" "HIVECTL=/path/to/hivectl" "skew refusal names explicit binary escape hatch"

CASE="old-source-record-refuses"
: >"$CALL_LOG"; : >"$RUN_LOG"
fixture="$(make_fixture)"
init_fixture_git "$fixture"
binary_commit="$(git -C "$fixture" rev-parse HEAD)"
advance_fixture_hivectl "$fixture"
write_fake_hivectl "${fixture}/bin/hivectl" local
cat >"${fixture}/bin/.hivectl.source" <<'EOF_OLD_RECORD'
image=test-image
path=/usr/local/share/hive/hivectl
digest=sha256:fresh
version<<HIVECTL_VERSION
local version without commit
HIVECTL_VERSION
EOF_OLD_RECORD
FAKE_IMAGE_REVISION="$binary_commit" run_bootstrap "$fixture" hives list >"${fixture}/stdout" 2>"${fixture}/stderr"
status=$?
if [[ "$status" -eq 66 ]]; then pass "old source record refuses with skew exit"; else fail "old source record exited ${status}, want 66"; fi
assert_file_contains "${fixture}/stderr" "staged binary commit ${binary_commit}" "old source record falls back to image revision"

CASE="at-tag-prefers-release-image"
: >"$CALL_LOG"; : >"$RUN_LOG"
fixture="$(make_fixture)"
init_fixture_git "$fixture"
git -C "$fixture" tag v5.4.0
head_commit="$(git -C "$fixture" rev-parse HEAD)"
HIVECTL_BOOTSTRAP_IMAGE="" FAKE_BINARY_COMMIT="$head_commit" run_bootstrap "$fixture" hives list >"${fixture}/stdout" 2>"${fixture}/stderr"
status=$?
if [[ "$status" -eq 0 ]]; then pass "tag checkout bootstraps successfully"; else fail "tag checkout exited ${status}"; fi
assert_file_contains "${fixture}/stderr" "using release image ghcr.io/hivecommons/hive:v5.4.0" "release tag image is announced"
assert_file_contains "${fixture}/bin/.hivectl.source" "image=ghcr.io/hivecommons/hive:v5.4.0" "release tag image is recorded"
assert_file_contains "$RUN_LOG" "staged hives list" "release tag binary handles command"

CASE="at-stable-rev-uses-branch-channel"
: >"$CALL_LOG"; : >"$RUN_LOG"
fixture="$(make_fixture)"
init_fixture_git "$fixture"
git -C "$fixture" checkout -q -b v5
head_commit="$(git -C "$fixture" rev-parse HEAD)"
HIVECTL_BOOTSTRAP_IMAGE="" FAKE_BINARY_COMMIT="$head_commit" run_bootstrap "$fixture" hives list >"${fixture}/stdout" 2>"${fixture}/stderr"
status=$?
if [[ "$status" -eq 0 ]]; then pass "branch channel checkout bootstraps successfully"; else fail "branch channel checkout exited ${status}"; fi
assert_file_contains "${fixture}/bin/.hivectl.source" "image=ghcr.io/hivecommons/hive:stable" "v5 branch falls back to stable channel"
assert_file_contains "$RUN_LOG" "staged hives list" "stable-channel binary handles command"

CASE="image-override-wins"
: >"$CALL_LOG"; : >"$RUN_LOG"
fixture="$(make_fixture)"
init_fixture_git "$fixture"
git -C "$fixture" tag v5.4.0
head_commit="$(git -C "$fixture" rev-parse HEAD)"
HIVECTL_BOOTSTRAP_IMAGE="custom-image" FAKE_BINARY_COMMIT="$head_commit" run_bootstrap "$fixture" hives list >"${fixture}/stdout" 2>"${fixture}/stderr"
status=$?
if [[ "$status" -eq 0 ]]; then pass "image override bootstraps successfully"; else fail "image override exited ${status}"; fi
assert_file_contains "${fixture}/bin/.hivectl.source" "image=custom-image" "explicit image override is recorded"
assert_not_contains "$(cat "$CALL_LOG")" "ghcr.io/hivecommons/hive:v5.4.0" "explicit image override skips release probe"

CASE="offline-release-probe-falls-back"
: >"$CALL_LOG"; : >"$RUN_LOG"
fixture="$(make_fixture)"
init_fixture_git "$fixture"
git -C "$fixture" checkout -q -b v5
git -C "$fixture" tag v5.4.0
head_commit="$(git -C "$fixture" rev-parse HEAD)"
HIVECTL_BOOTSTRAP_IMAGE="" \
  FAKE_UNAVAILABLE_IMAGE="ghcr.io/hivecommons/hive:v5.4.0" \
  FAKE_BINARY_COMMIT="$head_commit" \
  run_bootstrap "$fixture" hives list >"${fixture}/stdout" 2>"${fixture}/stderr"
status=$?
if [[ "$status" -eq 0 ]]; then pass "offline release probe falls back successfully"; else fail "offline release probe exited ${status}"; fi
assert_file_contains "${fixture}/stderr" "falling back to the branch channel" "offline probe fallback is announced"
assert_file_contains "${fixture}/bin/.hivectl.source" "image=ghcr.io/hivecommons/hive:stable" "offline probe records fallback channel"

CASE="justfile-routing"
justfile_content="$(cat "$JUSTFILE")"
assert_contains "$justfile_content" "contribute-tui:" "Justfile defines contribute-tui"
assert_contains "$justfile_content" "exec ./bin/hivectl-bootstrap.sh tui" "contribute-tui routes through bootstrap script"
assert_contains "$justfile_content" "exec ./bin/hivectl-bootstrap.sh hives list" "contribute-hives default routes through bootstrap script"
assert_contains "$justfile_content" "exec ./bin/hivectl-bootstrap.sh hives {{ARGS}}" "contribute-hives args route through bootstrap script"

printf '\n%d passed, %d failed\n' "$pass_count" "$fail_count"
[[ "$fail_count" -eq 0 ]] || exit 1
exit 0
