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
# krun mode (#12199) adds probes the plain launch never makes: the krun
# runtime resolution, the rootless probe, the preflight smoke run (told
# apart from the contributor run by its --entrypoint override), and the
# OCIRuntime lookups contribute-stop uses.
case "${1:-}" in
  --runtime=krun)
    exit "${STUB_KRUN_INFO_RC:-0}"
    ;;
  info)
    echo "${STUB_ROOTLESS:-true}"
    ;;
  run)
    shift
    case " $* " in
      *" --entrypoint "*)
        printf '%s\n' "$@" > "$RUNTIME_CAPTURE.smoke"
        if [ -n "${STUB_SMOKE_FAIL:-}" ]; then
          echo "Error: krun: failed to start the VM" >&2
          exit 126
        fi
        echo "${STUB_GUEST_KERNEL:-6.12.68-krun-guest}"
        echo "krun-marker-${STUB_MARKER:-present}"
        exit 0
        ;;
    esac
    printf '%s\n' "$@" > "$RUNTIME_CAPTURE"
    basename "$0" > "$RUNTIME_CAPTURE.engine"
    # Inspect the staged config before the recipe removes it on exit.
    for arg in "$@"; do
      case "$arg" in
        *:/home/dev/.codex*)
          cat "${arg%%:/home/dev/.codex*}/auth.json" > "$RUNTIME_CAPTURE.auth"
          ;;
      esac
    done
    echo stub-container-id
    ;;
  inspect)
    case "$*" in
      *'.State.Running'*)   echo "${STUB_RUNNING:-true}" ;;
      *'.State.ExitCode'*)  echo "${STUB_EXIT:-0}" ;;
      *'.State.OOMKilled'*) echo "${STUB_OOM:-false}" ;;
      *'.OCIRuntime'*)
        case " ${STUB_KRUN_NAMES:-} " in
          *" ${*: -1} "*) echo krun ;;
          *) echo crun ;;
        esac
        ;;
      *) exit 1 ;;
    esac
    ;;
  ps)
    case " $* " in
      # shellcheck disable=SC2086 # names split on purpose
      *" -a "*) printf '%s\n' ${STUB_PS_ALL:-} ;;
      # shellcheck disable=SC2086
      *) printf '%s\n' ${STUB_PS_RUNNING:-} ;;
    esac
    ;;
  rm)
    shift
    if [ -n "${RUNTIME_CAPTURE:-}" ]; then
      printf '%s %s\n' "$(basename "$0")" "$*" >> "$RUNTIME_CAPTURE.rm"
    fi
    exit 0
    ;;
  # How the recipe follows the contributor: `logs -f` (relay logs) or, in
  # interactive krun mode, `wait` (#12202).
  logs|wait)
    if [ -n "${RUNTIME_CAPTURE:-}" ]; then
      printf '%s\n' "$*" >> "$RUNTIME_CAPTURE.follow"
    fi
    exit 0
    ;;
  pull|stop) exit 0 ;;
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
# uname: -s and -r are overridable (macOS refusal, host kernel); every other
# use falls through to the real binary.
cat > "$STUB_BIN/uname" <<'EOF'
#!/usr/bin/env bash
if [ "${1:-}" = "-s" ] && [ -n "${STUB_UNAME_S:-}" ]; then echo "$STUB_UNAME_S"; exit 0; fi
if [ "${1:-}" = "-r" ] && [ -n "${STUB_HOST_KERNEL:-}" ]; then echo "$STUB_HOST_KERNEL"; exit 0; fi
for d in /usr/bin /bin; do
  if [ -x "$d/uname" ]; then exec "$d/uname" "$@"; fi
done
exit 127
EOF
# systemd-detect-virt is always stubbed, so the CI runner (itself a VM) never
# decides the nested-host warning. Unset STUB_VIRT models bare metal;
# "unavailable" models a check that cannot answer.
cat > "$STUB_BIN/systemd-detect-virt" <<'EOF'
#!/usr/bin/env bash
case "${STUB_VIRT:-none}" in
  none) echo none; exit 1 ;;
  unavailable) exit 127 ;;
  *) echo "$STUB_VIRT"; exit 0 ;;
esac
EOF
chmod +x "$STUB_BIN/gh" "$STUB_BIN/sleep" "$STUB_BIN/uname" "$STUB_BIN/systemd-detect-virt"

# A PATH with every stub and host tool except podman, for the
# "podman not installed" refusal (CI runners ship a real podman).
NOPODMAN_BIN="$WORK/nopodman-bin"
mkdir -p "$NOPODMAN_BIN"
IFS=: read -ra SEARCH_DIRS <<<"$STUB_BIN:$PATH"
for d in "${SEARCH_DIRS[@]}"; do
  for f in "$d"/*; do
    n="${f##*/}"
    [ "$n" = podman ] && continue
    [ -e "$NOPODMAN_BIN/$n" ] && continue
    if [ -x "$f" ] && [ ! -d "$f" ]; then ln -s "$f" "$NOPODMAN_BIN/$n"; fi
  done
done

# An openable stand-in for /dev/kvm (HIVE_KRUN_KVM_DEVICE).
FAKE_KVM="$WORK/kvm"
: > "$FAKE_KVM"

# Everything that could leak from the caller's environment into the run
# argv is unset, so the launch command is reproducible byte for byte.
ENV_UNSET=(
  -u HIVE_CONTAINER_MEMORY -u HIVE_CONTAINER_CPUS -u CODEX_HOME
  -u HIVE_CONTAINER_OCI_RUNTIME -u CONTRIBUTOR_MODE -u HIVE_HUB
  -u HIVE_CONTRIBUTOR_IMAGE -u AGENT_MODEL -u AGENT_REASONING_EFFORT
  -u HIVE_CODEX_AUTO_USE_BANKED_RESET -u HIVE_SESSION -u HIVE_KRUN_KVM_DEVICE
)
for name in ANTHROPIC_API_KEY OPENROUTER_API_KEY OPENAI_API_KEY OPENAI_HOST \
  OPENAI_BASE_PATH GOOGLE_API_KEY GOOSE_API_KEY GOOSE_PROVIDER GOOSE_MODEL \
  KIRO_API_KEY BOBSHELL_API_KEY HIVE_LITELLM_ENDPOINT HIVE_LITELLM_API_KEY \
  KILO_AUTH_CONTENT KILO_CONFIG_CONTENT KILO_API_KEY KILO_ORG_ID META_API_KEY \
  LLM_API_KEY LLM_BASE_URL LLM_MODEL; do
  ENV_UNSET+=(-u "$name")
done

# run_recipe <runtime> "<recipe args>" [VAR=value...]: an empty runtime
# leaves HIVE_CONTAINER_RUNTIME empty, i.e. auto-detection. A later PATH=
# argument replaces the stub PATH.
run_recipe() {
  local runtime="$1" args="$2"
  shift 2
  rm -f "$CAPTURE" "$CAPTURE.smoke" "$CAPTURE.engine" "$CAPTURE.follow"
  (
    cd "$ROOT" || exit 1
    # shellcheck disable=SC2086 # recipe args split on purpose
    env "${ENV_UNSET[@]}" \
      HOME="$FAKE_HOME" PATH="$STUB_BIN:$PATH" \
      XDG_RUNTIME_DIR="$FAKE_RUNTIME_DIR" \
      RUNTIME_CAPTURE="$CAPTURE" HIVE_CONTAINER_RUNTIME="$runtime" \
      HIVE_SKIP_VERSION_CHECK=true HIVE_SKIP_PULL=true \
      "$@" just contribute-hive $args 2>&1
  )
}

run_contributor() {
  local runtime="$1"
  shift
  run_recipe "$runtime" codex "$@"
}

# A krun launch that passes every check unless an argument overrides one.
run_krun() {
  local runtime="$1" args="$2"
  shift 2
  run_recipe "$runtime" "$args" \
    HIVE_CONTAINER_OCI_RUNTIME=krun CONTRIBUTOR_MODE=headless \
    HIVE_KRUN_KVM_DEVICE="$FAKE_KVM" STUB_HOST_KERNEL=6.11.0-host "$@"
}

# The captured argv with the per-run parts (work dir, container suffix)
# replaced by stable placeholders.
normalized_capture() {
  sed -e "s#${WORK}#<WORK>#g" \
    -e 's/hive-contributor-\([a-z]*\)-[0-9a-f]\{8\}/hive-contributor-\1-<id>/g' "${1:-$CAPTURE}"
}

# Drops exactly what krun mode adds: --runtime krun, krun.* annotations, and
# the isolation-class and host-kernel variables (#12200).
without_krun_flags() {
  awk '
    $0 == "--runtime"    { getline; if ($0 != "krun") print "--runtime\n" $0; next }
    $0 == "--annotation" { getline; if ($0 !~ /^krun\./) print "--annotation\n" $0; next }
    $0 == "-e"           { getline; if ($0 !~ /^HIVE_(ISOLATION_CLASS|TEAM_KERNEL_RELEASE)=/) print "-e\n" $0; next }
    { print }
  '
}

started_nothing() {
  local label="$1"
  if [ -e "$CAPTURE" ] || [ -e "$CAPTURE.smoke" ]; then
    fail "$label starts no container"
  else
    pass "$label starts no container"
  fi
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

echo ""
echo "-- golden: unset krun leaves the launch command byte for byte (#12199) --"
GOLDEN_DIR="$ROOT/src/deploy/testdata"
for runtime in docker podman; do
  for value in unset empty; do
    if [ "$value" = unset ]; then
      OUT="$(run_contributor "$runtime")"; RC=$?
    else
      OUT="$(run_contributor "$runtime" HIVE_CONTAINER_OCI_RUNTIME=)"; RC=$?
    fi
    check "$runtime launch with krun $value succeeds" "0" "$RC"
    GOT="$(normalized_capture)"
    WANT="$(cat "$GOLDEN_DIR/contributor_run_${runtime}.golden")"
    if [ "$GOT" = "$WANT" ]; then
      pass "$runtime run argv with krun $value matches the golden"
    else
      fail "$runtime run argv with krun $value matches the golden" \
        "$(diff <(printf '%s\n' "$WANT") <(printf '%s\n' "$GOT") | head -20)"
    fi
    lacks "$runtime banner has no Isolation line with krun $value" "$OUT" "Isolation:"
    contains "$runtime attach hint is unchanged with krun $value" "$OUT" "exec -it"
  done
done

echo ""
echo "-- krun refusals start nothing (#12199) --"
OUT="$(run_krun "" codex HIVE_CONTAINER_OCI_RUNTIME=kata)"; RC=$?
check "kata exits non-zero" "1" "$RC"
contains "kata refusal names the accepted value" "$OUT" "The only accepted value is 'krun'"
contains "kata refusal explains Kata under podman" "$OUT" "Kata Containers is not supported here: it does not run under podman"
started_nothing "kata"

OUT="$(run_krun docker codex)"; RC=$?
check "krun with HIVE_CONTAINER_RUNTIME=docker exits non-zero" "1" "$RC"
contains "docker refusal gives the reason" "$OUT" "runs only under rootless podman"
started_nothing "krun with docker"

OUT="$(run_krun "" codex PATH="$NOPODMAN_BIN")"; RC=$?
check "krun without podman exits non-zero" "1" "$RC"
contains "no-podman refusal gives the reason" "$OUT" "podman is not installed"
started_nothing "krun without podman"

OUT="$(run_krun "" codex STUB_ROOTLESS=false)"; RC=$?
check "krun under rootful podman exits non-zero" "1" "$RC"
contains "rootful refusal gives the reason" "$OUT" "Rootless=false"
started_nothing "krun under rootful podman"

OUT="$(run_krun "" "codex local")"; RC=$?
check "krun with local mode exits non-zero" "1" "$RC"
contains "local refusal gives the reason" "$OUT" "cannot be combined with local mode"
started_nothing "krun with local mode"

OUT="$(run_krun "" pi)"; RC=$?
check "krun with pi exits non-zero" "1" "$RC"
contains "pi refusal names the backend and reason" "$OUT" "does not support backend 'pi'"
contains "pi refusal explains host networking" "$OUT" "host networking"
started_nothing "krun with pi"

OUT="$(run_krun "" omp)"; RC=$?
check "krun with a backend without headless entry exits non-zero" "1" "$RC"
contains "no-headless refusal names the backend" "$OUT" "no headless entry for 'omp'"
contains "no-headless refusal points at interactive mode" "$OUT" "Unset CONTRIBUTOR_MODE"
started_nothing "krun with omp"

OUT="$(run_krun "" codex STUB_UNAME_S=Darwin)"; RC=$?
check "krun on macOS exits non-zero" "1" "$RC"
contains "macOS refusal explains podman's VM" "$OUT" "already run inside a Linux VM"
started_nothing "krun on macOS"

OUT="$(run_krun "" codex HIVE_CONTAINER_CPUS=abc)"; RC=$?
check "krun with HIVE_CONTAINER_CPUS=abc exits non-zero" "1" "$RC"
contains "bad CPU value is named" "$OUT" "got 'abc'"
started_nothing "krun with HIVE_CONTAINER_CPUS=abc"

echo ""
echo "-- krun picks podman without changing docker-first detection (#12199) --"
OUT="$(run_krun "" codex)"; RC=$?
check "krun launch with auto-detection succeeds" "0" "$RC"
check "krun launch uses podman with docker also installed" "podman" "$(cat "$CAPTURE.engine" 2>/dev/null)"
OUT="$(run_contributor "")"; RC=$?
check "plain launch with auto-detection succeeds" "0" "$RC"
check "plain launch still prefers docker" "docker" "$(cat "$CAPTURE.engine" 2>/dev/null)"

echo ""
echo "-- krun preflight failures never start the contributor (#12199) --"
OUT="$(run_krun "" codex STUB_KRUN_INFO_RC=125)"; RC=$?
check "unresolvable krun exits non-zero" "1" "$RC"
contains "unresolvable krun is named" "$OUT" "cannot resolve the 'krun' OCI runtime"
started_nothing "unresolvable krun"

OUT="$(run_krun "" codex HIVE_KRUN_KVM_DEVICE="$WORK/missing-kvm")"; RC=$?
check "missing /dev/kvm exits non-zero" "1" "$RC"
contains "missing /dev/kvm is named" "$OUT" "cannot open $WORK/missing-kvm for reading and writing"
started_nothing "missing /dev/kvm"

# A directory cannot be opened read-write, even by root.
OUT="$(run_krun "" codex HIVE_KRUN_KVM_DEVICE="$FAKE_RUNTIME_DIR")"; RC=$?
check "unopenable /dev/kvm exits non-zero" "1" "$RC"
contains "unopenable /dev/kvm is named" "$OUT" "cannot open $FAKE_RUNTIME_DIR for reading and writing"
started_nothing "unopenable /dev/kvm"

rm -f "$CAPTURE"
OUT="$(run_krun "" codex STUB_SMOKE_FAIL=1)"; RC=$?
check "failed smoke run exits non-zero" "1" "$RC"
contains "failed smoke run is named" "$OUT" "the smoke run of"
if [ -e "$CAPTURE" ]; then fail "failed smoke run starts no contributor"; else pass "failed smoke run starts no contributor"; fi
SMOKE="$(cat "$CAPTURE.smoke" 2>/dev/null)"
for want in --rm --runtime krun --memory 1g --memory-swap --cpus 1 krun.cpus=1 krun.ram_mib=768 --entrypoint; do
  if grep -qxF -- "$want" <<<"$SMOKE"; then pass "smoke run carries $want"
  else fail "smoke run carries $want" "argv: $(tr '\n' ' ' <<<"$SMOKE")"; fi
done

OUT="$(run_krun "" codex STUB_GUEST_KERNEL=6.11.0-host)"; RC=$?
check "guest kernel equal to host exits non-zero" "1" "$RC"
contains "equal kernel is named" "$OUT" "the same as the host's"
if [ -e "$CAPTURE" ]; then fail "equal kernel starts no contributor"; else pass "equal kernel starts no contributor"; fi

OUT="$(run_krun "" codex STUB_MARKER=absent)"; RC=$?
check "absent krun marker exits non-zero" "1" "$RC"
contains "absent marker is named" "$OUT" "/.krun_config.json is absent"
if [ -e "$CAPTURE" ]; then fail "absent marker starts no contributor"; else pass "absent marker starts no contributor"; fi

echo ""
echo "-- the krun launch (#12199) --"
OUT="$(run_recipe podman codex CONTRIBUTOR_MODE=headless)"; RC=$?
check "headless podman reference launch succeeds" "0" "$RC"
REFERENCE="$(normalized_capture)"
OUT="$(run_krun "" codex)"; RC=$?
check "krun launch succeeds" "0" "$RC"
check "krun launch sets the krun runtime" "krun" "$(arg_after --runtime)"
contains "guest gets 75% of the 4g limit" "$(cat "$CAPTURE")" "krun.ram_mib=3072"
contains "vCPUs follow HIVE_CONTAINER_CPUS" "$(cat "$CAPTURE")" "krun.cpus=2"
check "krun keeps the container memory limit" "4g" "$(arg_after --memory)"
GOT="$(normalized_capture | without_krun_flags)"
if [ "$GOT" = "$REFERENCE" ]; then
  pass "krun launch otherwise matches a podman launch"
else
  fail "krun launch otherwise matches a podman launch" \
    "$(diff <(printf '%s\n' "$REFERENCE") <(printf '%s\n' "$GOT") | head -20)"
fi
contains "krun launch passes the isolation class (#12200)" "$(cat "$CAPTURE")" "HIVE_ISOLATION_CLASS=microvm-krun"
contains "krun launch passes the host kernel release (#12200)" "$(cat "$CAPTURE")" "HIVE_TEAM_KERNEL_RELEASE=6.11.0-host"
lacks "plain launch passes no isolation class (#12200)" "$REFERENCE" "HIVE_ISOLATION_CLASS"
lacks "plain launch passes no host kernel release (#12200)" "$REFERENCE" "HIVE_TEAM_KERNEL_RELEASE"
lacks "krun output has no exec attach command" "$OUT" "exec -it"
contains "krun banner has the Isolation line" "$OUT" "Isolation: microVM (krun), guest kernel 6.12.68-krun-guest, 3072 MiB guest of a 4g container limit, 2 vCPUs"
lacks "bare-metal host gets no nested warning" "$OUT" "nested krun is untested"
lacks "headless krun launch gets no TTY (#12202)" "$(cat "$CAPTURE")" "--tty"
lacks "headless krun launch keeps stdin closed (#12202)" "$(cat "$CAPTURE")" "--interactive"
contains "headless krun output still says there is nothing to attach to" "$OUT" "Attach:    none in krun mode (headless; the krun handler refuses exec)."
lacks "headless krun output has no console attach command (#12202)" "$OUT" "podman attach"
lacks "headless krun output has no detach keys (#12202)" "$OUT" "Ctrl-P Ctrl-Q"
contains "headless krun output still shows the relay logs" "$OUT" "Relay logs:"
check "headless krun recipe follows the relay logs" "logs" "$(cut -d' ' -f1 "$CAPTURE.follow" 2>/dev/null)"

OUT="$(run_krun "" codex HIVE_CONTAINER_CPUS=1.5)"; RC=$?
check "krun with 1.5 CPUs succeeds" "0" "$RC"
contains "1.5 CPUs round up to 2 vCPUs" "$(cat "$CAPTURE")" "krun.cpus=2"
check "--cpus keeps the operator's value" "1.5" "$(arg_after --cpus)"

OUT="$(run_krun "" codex HIVE_CONTAINER_CPUS=32)"; RC=$?
check "krun with 32 CPUs succeeds" "0" "$RC"
contains "32 CPUs cap at 16 vCPUs" "$(cat "$CAPTURE")" "krun.cpus=16"

OUT="$(run_krun "" codex HIVE_CONTAINER_CPUS=none HIVE_CONTAINER_MEMORY=none)"; RC=$?
check "krun with no limits succeeds" "0" "$RC"
lacks "no CPU limit sets no vCPU count" "$(cat "$CAPTURE")" "krun.cpus="
lacks "no memory limit sets no guest memory" "$(cat "$CAPTURE")" "krun.ram_mib="
contains "banner says memory falls back to krun's default" "$OUT" "krun's 1 GiB default"
contains "banner says vCPUs follow the host" "$OUT" "vCPUs follow the host's CPU count (at most 16)"

OUT="$(run_krun "" codex HIVE_CONTAINER_MEMORY=6g)"; RC=$?
check "krun with 6g succeeds" "0" "$RC"
contains "6g gives a 4608 MiB guest" "$(cat "$CAPTURE")" "krun.ram_mib=4608"

OUT="$(run_krun "" codex STUB_VIRT=kvm)"; RC=$?
check "krun on a nested host succeeds" "0" "$RC"
contains "nested host gets the warning" "$OUT" "nested krun is untested and may be unstable"
OUT="$(run_krun "" codex STUB_VIRT=unavailable)"; RC=$?
check "krun with no virt check succeeds" "0" "$RC"
lacks "unavailable virt check gives no warning" "$OUT" "nested krun is untested"

echo ""
echo "-- interactive krun mode through the VM console (#12202) --"
for mode in interactive ""; do
  OUT="$(run_krun "" codex CONTRIBUTOR_MODE="$mode")"; RC=$?
  check "krun with CONTRIBUTOR_MODE='$mode' is no longer refused" "0" "$RC"
  lacks "krun with CONTRIBUTOR_MODE='$mode' prints no headless refusal" "$OUT" "requires CONTRIBUTOR_MODE=headless"
  check "krun with CONTRIBUTOR_MODE='$mode' still runs under krun" "krun" "$(arg_after --runtime)"
  check "krun with CONTRIBUTOR_MODE='$mode' still uses podman" "podman" "$(cat "$CAPTURE.engine" 2>/dev/null)"
done
OUT="$(run_krun "" codex CONTRIBUTOR_MODE=interactive)"; RC=$?
check "interactive krun launch succeeds" "0" "$RC"
contains "interactive krun launch keeps stdin open" "$(cat "$CAPTURE")" "--interactive"
contains "interactive krun launch gets a TTY" "$(cat "$CAPTURE")" "--tty"
contains "interactive krun launch passes the isolation class" "$(cat "$CAPTURE")" "HIVE_ISOLATION_CLASS=microvm-krun"
contains "interactive krun output has the attach command" "$OUT" "podman attach hive-contributor-codex-"
contains "interactive krun output has the detach keys" "$OUT" "Ctrl-P Ctrl-Q"
lacks "interactive krun output has no exec command" "$OUT" "exec -it"
lacks "interactive krun output has no tmux attach hint" "$OUT" "tmux attach"
lacks "interactive krun output does not stream the console as relay logs" "$OUT" "Relay logs:"
check "interactive krun recipe waits on the container" "wait" "$(cut -d' ' -f1 "$CAPTURE.follow" 2>/dev/null)"
contains "interactive krun keeps the Isolation line" "$OUT" "Isolation: microVM (krun)"

OUT="$(run_krun "" omp CONTRIBUTOR_MODE=)"; RC=$?
check "interactive-only omp launches in krun mode" "0" "$RC"
check "omp in krun mode runs under krun" "krun" "$(arg_after --runtime)"
contains "omp in krun mode gets a TTY" "$(cat "$CAPTURE")" "--tty"
contains "omp in krun mode prints the attach command" "$OUT" "podman attach hive-contributor-omp-"
lacks "omp in krun mode prints no exec command" "$OUT" "exec -it"

# Interactive krun keeps every refusal: no docker, no local, no pi.
OUT="$(run_krun docker codex CONTRIBUTOR_MODE=interactive)"; RC=$?
check "interactive krun with docker exits non-zero" "1" "$RC"
started_nothing "interactive krun with docker"
OUT="$(run_krun "" "codex local" CONTRIBUTOR_MODE=interactive)"; RC=$?
check "interactive krun with local mode exits non-zero" "1" "$RC"
started_nothing "interactive krun with local mode"
OUT="$(run_krun "" pi CONTRIBUTOR_MODE=interactive)"; RC=$?
check "interactive krun with pi exits non-zero" "1" "$RC"
started_nothing "interactive krun with pi"
OUT="$(run_krun "" codex CONTRIBUTOR_MODE=interactive STUB_ROOTLESS=false)"; RC=$?
check "interactive krun under rootful podman exits non-zero" "1" "$RC"
started_nothing "interactive krun under rootful podman"

echo ""
echo "-- krun set only in contributor.env (#12199) --"
cp "$FAKE_HOME/.config/hive/contributor.env" "$WORK/contributor.env.orig"
printf '%s\n' 'HIVE_CONTAINER_OCI_RUNTIME=krun' 'CONTRIBUTOR_MODE=headless' \
  >> "$FAKE_HOME/.config/hive/contributor.env"
OUT="$(run_recipe "" codex HIVE_KRUN_KVM_DEVICE="$FAKE_KVM" STUB_HOST_KERNEL=6.11.0-host)"; RC=$?
check "krun from contributor.env succeeds" "0" "$RC"
check "krun from contributor.env is a krun launch" "krun" "$(arg_after --runtime)"
OUT="$(run_recipe docker codex)"; RC=$?
check "krun from contributor.env with docker is refused" "1" "$RC"
started_nothing "krun from contributor.env with docker"
cp "$WORK/contributor.env.orig" "$FAKE_HOME/.config/hive/contributor.env"

echo ""
echo "-- krun stop diagnostics (#12199) --"
OUT="$(run_krun "" codex STUB_RUNNING=false STUB_EXIT=137 STUB_OOM=true)"; RC=$?
check "krun startup OOM exits non-zero" "1" "$RC"
contains "krun OOM names VM memory" "$OUT" "VM memory"
OUT="$(run_krun "" codex STUB_RUNNING=true STUB_EXIT=137 STUB_OOM=false)"; RC=$?
check "krun exit 137 keeps the recipe's status" "0" "$RC"
contains "krun exit 137 is reported as a stop" "$OUT" "stopped (exit 137)"
lacks "krun exit 137 does not blame VM memory" "$OUT" "VM memory"
lacks "krun exit 137 does not claim an OOM kill" "$OUT" "OOM-killed"

echo ""
echo "-- contribute-stop removes krun contributors (#12199) --"
PID_RECORD="$FAKE_HOME/.config/hive/contributor-relay.pid"
printf '%s\n' '{"container_runtime":"podman","container_name":"hive-contributor-codex-aaaaaaaa"}' > "$PID_RECORD"
rm -f "$CAPTURE.rm"
OUT="$(
  cd "$ROOT" && env "${ENV_UNSET[@]}" HOME="$FAKE_HOME" PATH="$STUB_BIN:$PATH" \
    RUNTIME_CAPTURE="$CAPTURE" \
    STUB_PS_RUNNING="hive-contributor-codex-aaaaaaaa" \
    STUB_PS_ALL="hive-contributor-codex-aaaaaaaa hive-contributor-codex-bbbbbbbb hive-contributor-codex-cccccccc" \
    STUB_KRUN_NAMES="hive-contributor-codex-aaaaaaaa hive-contributor-codex-bbbbbbbb" \
    just contribute-stop 2>&1
)"; RC=$?
check "contribute-stop succeeds" "0" "$RC"
REMOVED="$(cat "$CAPTURE.rm" 2>/dev/null)"
contains "running krun contributor is removed" "$REMOVED" "podman -f hive-contributor-codex-aaaaaaaa"
contains "stopped krun contributor is removed" "$REMOVED" "podman -f hive-contributor-codex-bbbbbbbb"
lacks "non-krun contributor is left alone" "$REMOVED" "cccccccc"
if [ -e "$PID_RECORD" ]; then fail "relay PID record is deleted"; else pass "relay PID record is deleted"; fi

printf '%s\n' '{"pid":4242}' > "$PID_RECORD"
rm -f "$CAPTURE.rm"
OUT="$(
  cd "$ROOT" && env "${ENV_UNSET[@]}" HOME="$FAKE_HOME" PATH="$STUB_BIN:$PATH" \
    RUNTIME_CAPTURE="$CAPTURE" \
    STUB_PS_ALL="hive-contributor-codex-cccccccc" \
    just contribute-stop 2>&1
)"; RC=$?
check "contribute-stop without krun succeeds" "0" "$RC"
check "contribute-stop without krun removes nothing" "" "$(cat "$CAPTURE.rm" 2>/dev/null)"
if [ -e "$PID_RECORD" ]; then pass "PID record kept without krun"; else fail "PID record kept without krun"; fi
rm -f "$PID_RECORD"

echo ""
echo "-- Codex config staging --"
mkdir -p "$FAKE_HOME/.codex" "$WORK/custom-codex"
printf '%s\n' 'default-auth-fixture' > "$FAKE_HOME/.codex/auth.json"
printf '%s\n' 'custom-auth-fixture' > "$WORK/custom-codex/auth.json"
for runtime in docker podman; do
  rm -f "$CAPTURE.auth"
  OUT="$(run_contributor "$runtime")"; RC=$?
  check "$runtime default config launch succeeds" "0" "$RC"
  check "$runtime stages ~/.codex when CODEX_HOME is unset" \
    "default-auth-fixture" "$(cat "$CAPTURE.auth" 2>/dev/null)"

  rm -f "$CAPTURE.auth"
  OUT="$(run_contributor "$runtime" CODEX_HOME="$WORK/custom-codex")"; RC=$?
  check "$runtime custom config launch succeeds" "0" "$RC"
  check "$runtime stages CODEX_HOME instead of ~/.codex" \
    "custom-auth-fixture" "$(cat "$CAPTURE.auth" 2>/dev/null)"
done

hive_test_report
