#!/usr/bin/env bash
# Exercises the real `just contribute-hive` container recipe against stubbed
# Docker and Podman CLIs to pin runtime selection and the launch arguments for
# rootful Docker, rootless Docker, Podman, and the permission-failure fallback
# (#19076). No image, daemon, credential, or network is used.
#
# Run: bash src/deploy/test_contributor_runtime_selection.sh
set -uo pipefail

# shellcheck source=src/deploy/test_lib.sh
. "$(cd "$(dirname "$0")" && pwd)/test_lib.sh"
PASS=0
FAIL=0
pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
fail() {
  echo "  FAIL: $1"
  [ $# -gt 1 ] && [ -n "${2:-}" ] && echo "        $2"
  FAIL=$((FAIL + 1))
}
has() { # label file needle
  if grep -qF -- "$3" "$2" 2>/dev/null; then pass "$1"; else fail "$1" "missing: '$3'"; fi
}
hasnt() {
  if grep -qF -- "$3" "$2" 2>/dev/null; then fail "$1" "unexpectedly present: '$3'"; else pass "$1"; fi
}

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
if ! command -v just >/dev/null 2>&1; then
  hive_test_skip "'just' not installed; cannot exercise the recipe"
  hive_test_report; exit $?
fi

echo "=== contributor runtime selection (#19076) ==="

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
FAKE_HOME="$WORK/home"
STUB_BIN="$WORK/bin"
CALLS="$WORK/calls"
mkdir -p "$FAKE_HOME/.config/hive" "$STUB_BIN" "$WORK/runtime-dir"
printf '%s\n' 'HIVE_HUB=wss://hive.example.test/contribute' \
  'HIVE_REGISTRATION_TOKEN=placeholder-registration-token' 'AGENT_BACKEND=codex' \
  > "$FAKE_HOME/.config/hive/contributor.env"
chmod 600 "$FAKE_HOME/.config/hive/contributor.env"
printf '%s\n' 'GH_TOKEN=placeholder-github-token' > "$FAKE_HOME/.config/hive/gh-auth.env"

# One stub serves both names. Every invocation appends one line:
#   <runtime> <args...>
# and `chown` helper runs also record the mode of the staged contributor.env.
cat > "$STUB_BIN/runtime" <<'STUB'
#!/usr/bin/env bash
name="$(basename "$0")"
echo "$name $*" >> "$STUB_CALLS"
case "${1:-}" in
  info) echo "${STUB_INFO:-[name=seccomp,profile=builtin]}" ;;
  run)
    case "$*" in
      *"--entrypoint chown"*)
        for a in "$@"; do
          case "$a" in
            *hive-config-stage*:/stage/*)
              src="${a%%:/stage/*}"
              { stat -c '%a' "$src/contributor.env" 2>/dev/null \
                || stat -f '%Lp' "$src/contributor.env"; } >> "$STUB_CALLS.mode"
              ;;
          esac
        done ;;
    esac
    echo stub-id ;;
  inspect)
    running="true"
    [ "$name" = "docker" ] && running="${STUB_DOCKER_RUNNING:-true}"
    case "$*" in
      *'.State.Running'*) echo "$running" ;;
      *'.State.ExitCode'*) echo 1 ;;
      *'.State.OOMKilled'*) echo false ;;
      *) exit 1 ;;
    esac ;;
  logs) [ "$name" = "docker" ] && echo "${STUB_DOCKER_LOGS:-}"; exit 0 ;;
  rm|pull) exit 0 ;;
  *) exit 1 ;;
esac
STUB
chmod +x "$STUB_BIN/runtime"
ln -s runtime "$STUB_BIN/docker"
ln -s runtime "$STUB_BIN/podman"
printf '#!/usr/bin/env bash\n[ "${1:-} ${2:-}" = "api user" ] && echo test-contributor\nexit 0\n' > "$STUB_BIN/gh"
printf '#!/usr/bin/env bash\nexit 0\n' > "$STUB_BIN/sleep"
chmod +x "$STUB_BIN/gh" "$STUB_BIN/sleep"

# scenario <forced-runtime-or-empty> [VAR=value...]; output lands in $OUT.
scenario() {
  local forced="$1"; shift
  : > "$CALLS"; rm -f "$CALLS.mode"
  OUT="$(cd "$ROOT" && env -u HIVE_CONTAINER_RUNTIME -u HIVE_RUNTIME_FALLBACK \
    HOME="$FAKE_HOME" PATH="$STUB_BIN:$PATH" XDG_RUNTIME_DIR="$WORK/runtime-dir" \
    STUB_CALLS="$CALLS" HIVE_SKIP_VERSION_CHECK=true HIVE_SKIP_PULL=true \
    ${forced:+HIVE_CONTAINER_RUNTIME=$forced} "$@" just contribute-hive codex 2>&1)"
  RC=$?
}
runline() { grep -E "^$1 run -d" "$CALLS" | head -1; }

echo ""
echo "-- rootful docker: unchanged --"
scenario docker
L="$(runline docker)"; echo "$L" > "$WORK/l"
has "uses docker" "$WORK/l" "docker run -d"
hasnt "no userns flag" "$WORK/l" "--userns"
has "mounts the real config dir" "$WORK/l" "-v $FAKE_HOME/.config/hive:/home/dev/.config/hive "
hasnt "no stage helper" "$CALLS" "--entrypoint chown"

echo ""
echo "-- rootless docker: staged, chowned through the userns, modes kept --"
scenario docker STUB_INFO='[name=seccomp,profile=builtin name=rootless name=cgroupns]'
L="$(runline docker)"; echo "$L" > "$WORK/l"
has "chown helper runs as userns root" "$CALLS" "--user 0:0 --entrypoint chown"
has "helper chowns to uid 1000" "$CALLS" "-R 1000:1000"
hasnt "container does not mount the real config dir" "$WORK/l" "-v $FAKE_HOME/.config/hive:"
has "container mounts the staged config dir" "$WORK/l" "hive-config-stage."
has "staged contributor.env stays 0600" "$CALLS.mode" "600"
has "operator is told why" <(echo "$OUT") "Rootless Docker detected"

echo ""
echo "-- podman: unchanged --"
scenario podman
L="$(runline podman)"; echo "$L" > "$WORK/l"
has "keep-id pinned to uid 1000" "$WORK/l" "--userns=keep-id:uid=1000,gid=1000"
has "real config dir with SELinux label" "$WORK/l" "-v $FAKE_HOME/.config/hive:/home/dev/.config/hive:Z"
hasnt "no stage helper" "$CALLS" "--entrypoint chown"

echo ""
echo "-- auto-detect: docker wins; permission failure falls back to podman --"
scenario "" 
has "auto-detect picks docker first" "$CALLS" "docker run -d"
scenario "" STUB_DOCKER_RUNNING=false STUB_DOCKER_LOGS='source: /home/dev/.config/hive/contributor.env: Permission denied'
has "docker attempted first" "$CALLS" "docker run -d"
has "podman launched after the failure" "$CALLS" "podman run -d"
has "one-line reason printed" <(echo "$OUT") "retrying with podman"

echo ""
echo "-- explicit runtime is never overridden --"
scenario docker STUB_DOCKER_RUNNING=false STUB_DOCKER_LOGS='contributor.env: Permission denied'
hasnt "no podman fallback when docker was forced" "$CALLS" "podman run -d"

hive_test_report
